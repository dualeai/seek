package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

// The one search pipeline moves three stage types between its stages: a
// queryPlan in, nodeHits from each node, and one ranked list out. A query with
// no model stage gets corpusSearchResult values from each node (strictNode)
// and has no ranked list.

// Retrieval rule: a semantic hit is a row whose summed fine MaxSim F reaches
// S0 + (tau_file - delta) * T (semanticRetrievalFloor). S0 is the
// empty-document score of the query (semanticQueryEmbedding.emptyScore), and T
// is its token count. Take a row whose window has a model lift (S - S0) / T
// that passes tau_file. That row is within the radius while the fine vectors
// under-estimate the model score S by at most delta per token.
//
// The measurement of delta used the earlier acceptance rule, 0.6 on the score
// per token. delta is the p99.9 of (model MeanMaxSim - fine MeanMaxSim) over
// 18,856 window rows of a Kubernetes checkout. Each row had a model MeanMaxSim
// of at least 0.55. The gap had a p50 of 0.008, a p99 of 0.069 and a maximum
// of 0.149. A per-token coarse radius kept every Kubernetes file (delta p99
// 0.445), so it is not the rule.
//
// The measurement of pipelineTauFile gives a larger p99.9, 0.1413. It used
// the 3,906 rows of both corpora with a model lift of at least 0.15. But at
// tau_file 0.25, each delta from 0.10 to 0.15 kept a row of each admitted file
// in the radius. At 0.1158, a short description gets 15.4 (Seek) and 9.4
// (Kubernetes) candidate files. At 0.1413, it gets 26.8 and 27.7. Fewer
// candidates make a shorter search, so delta stays 0.1158.
//
// The gap has two causes. Each row stores 20 of a median 91 token vectors.
// In 64 % of the rows, the row input also differs from the input that the
// model scores. A measurement of 2026-10-26 on the whole Kubernetes checkout
// tried a smaller gap with 20 descriptions. Rows built from the model's input
// with 20 vectors (delta 0.06) lost 12 or 13 printed files. 48 vectors (delta
// 0.04) lost 1 file and made a 51-word question 20 to 32 % slower. They also
// grew the index from 4.2 to 7.6 GB. So the rows keep 20 vectors.
const semanticRetrievalDelta float32 = 0.1158

// pipelineTauFile is the per-file acceptance threshold: the lift of the model
// score above the empty-document score, per query token (meanMaxSimPasses).
// A measurement of 2026-10-22 set it with the model of
// TestRerankAcceptanceCalibrationContract on Core ML (the CPU provider
// differs by at most 0.0005). The corpora were the 97 production files of
// Seek (5,142 rows) and every 48th production Go file of Kubernetes at
// 912ec3583d77 (298 files, 17,355 rows). The rows had ctags symbols, as the
// index builds them. The queries were 58 descriptions. 42 were 14 behaviors
// of Seek in 3 lengths. 12 had no answer, 3 were nonsense, and 1 was a
// sentence of 51 tokens with no topic.
//
// The descriptions from other projects (e-mail, HTML pages, images), the
// nonsense, and the sentence with no topic must admit no file. Their largest
// lift is 0.2002 (Kubernetes), so 0.25 admits none of them. At 0.25, the
// descriptions of the 14 Seek behaviors find their files for 8 of 14 short
// forms. They find them for 1 of 14 questions and 0 of 14 long forms, and
// admit 16 false files. The earlier rule, 0.6 on the score per token, found 8,
// 8 and 14 with 1,418 false files. It admitted almost every file for a long
// query, because each token with no topic adds to the score of any text. The
// lift per token falls as the query grows, so a long description finds few
// files.
//
// A description of one or two words adds few files by meaning. T counts the
// 3 special tokens of each window (CLS, the query marker, SEP). Their lift
// is about 0 or less: a median of -0.10 per token for one word. So the 1 to
// 4 content tokens of such a description must lift the sum by 1.0 to 1.75
// or more alone. A probe of 2026-10-23 used the same model and corpora. It
// found the files of 6 of 21 one-word and 9 of 17 two-word labelled
// descriptions of the Seek behaviors. On a corpus of 222 Seek files, 15
// one-word descriptions added 12 files by meaning (111 with the earlier
// rule). A lift per content token uses T without the special tokens and a
// tau_file of 0.40. It finds the files of 13 of the 21 and 13 of the 17
// descriptions. With it, the 15 one-word descriptions add 150 files. But a
// description of 1 to 4 content tokens then scores more candidates, so it is
// slower. On the whole Kubernetes checkout, "request authentication flow" in
// staging/ takes 1.5 s against 1.0 s, and "logging" 2.8 s against 1.1 s.
// Fewer candidates make a shorter search, so T counts every scoring token.
//
// On the whole Kubernetes checkout (31,250 files), warm, the earlier rule
// gave a question of 51 words 24,138 candidates and 9.1 s. The lift gives 81
// candidates and 1.7 s. A short description with a filter gets 1 to 4 % more
// candidates, with no change of time that the measurement can show.
const pipelineTauFile float32 = 0.25

// semanticRetrievalRadius is the fine lift per query token above the
// empty-document score that a semantic hit reaches (semanticRetrievalFloor).
const semanticRetrievalRadius = pipelineTauFile - semanticRetrievalDelta

// semanticRetrievalFloor gives the summed fine MaxSim that a row reaches to be
// a semantic hit. It is the empty-document score of the query plus the radius
// times its scoring token count (semanticQueryTokenCount). The caller counts
// the tokens once and also uses the count to turn sums into means.
func semanticRetrievalFloor(emptyScore float32, tokens int) float32 {
	return emptyScore + semanticRetrievalRadius*float32(tokens)
}

// semanticUSearchDeepenSimilarity is the coarse similarity down to which
// USearch keeps returning keys for a token: an approximation parameter, not a
// count. In a shard that does not saturate, only keys at or above it are
// candidates. A measurement on a Kubernetes checkout used 20 queries and the
// radius of an earlier rule (a fine MeanMaxSim of 0.4842 per token). At 0.6,
// USearch kept a median 0.998 (min 0.985) of the files that an exact fine scan
// finds within the radius. It also kept the exact top 20 in every query. 0.7
// reached the recall target (top-20 overlap of at least 0.95) only at its
// limit, and 0.8 kept half. That measurement read a 127^2 scale, 4.5 % below
// the cosine (an effective cosine of about 0.627). The bound applies to
// USearch's cosine of the int8 codes. For keys at a cosine of 0.5 or more, it
// differs from the f32 cosine by a mean under 0.001 and by at most 0.012. So
// the bound keeps a few more rows than measured. On the same checkout in
// October 2026, descriptions of 1 to 51 words with no filter gave all 1,347,164
// rows to the fine scan. 373 of the 379 shards saturated, and the 6 smallest (4
// to 598 rows) gave all their rows. So the bound changed no result for them.
var semanticUSearchDeepenSimilarity float32 = 0.6

// nodeHit is one file of one node: the unit of merge, duplicate collapse,
// model scoring, and acceptance. Its identity includes the node: the dedup
// key is (corpus ID, path).
type nodeHit struct {
	corpus      corpusID
	displayRoot string
	path        string
	// contentID is the SHA-256 of the file content from its semantic rows. A
	// name-only file has none and never collapses.
	contentID semanticContentID
	// match names the evidence: two copies collapse only with the same
	// content and the same match.
	match string
	// start is the start of the first matched unit, the last tie key.
	start uint64
	score float32
	// strict marks a strict all-word lexical match; lexical, a relaxed one;
	// semantic, a unit within the retrieval radius.
	strict   bool
	lexical  bool
	semantic bool
	nameOnly bool
	// lexicalParts are the BM25 parts of a relaxed match (bm25Parts), which
	// the command rescore reads.
	lexicalParts string
	// also holds the other collapsed copies, in tie order.
	also []corpusCopy
	// display is what the output prints for the file. document is what the
	// model reads: the window that the vectors found, or the best lexical
	// match with its context.
	display  corpusSearchResult
	document rerankDocument
}

type nodeHits []nodeHit

// ranked is the pipeline output: the accepted files in display order.
type ranked struct {
	hits nodeHits
}

func (hit nodeHit) displayPath() string {
	return filepath.Join(hit.displayRoot, hit.path)
}

// compareNodeHits is the one tie key: score, then the display path,
// then the unit start. Corpus and generation IDs are never order keys, so the
// order does not depend on how the search splits into nodes.
func compareNodeHits(left, right nodeHit) int {
	switch {
	case left.score > right.score:
		return -1
	case left.score < right.score:
		return 1
	}
	if c := strings.Compare(left.displayPath(), right.displayPath()); c != 0 {
		return c
	}
	switch {
	case left.start < right.start:
		return -1
	case left.start > right.start:
		return 1
	}
	return 0
}

// mergeNodeHits sorts a copy of the hits of every node once with the tie key.
// It then collapses copies with the same content and match on the sorted
// list. The representative is the copy with the smallest tie key, and the
// other paths follow in order. The same (corpus, path) twice keeps its first,
// best hit. No step cuts a node's hits.
func mergeNodeHits(hits nodeHits) nodeHits {
	merged := slices.Clone(hits)
	sortNodeHits(merged)
	type fileKey struct {
		corpus corpusID
		path   string
	}
	type copyKey struct {
		content semanticContentID
		match   string
	}
	seenFiles := make(map[fileKey]struct{}, len(merged))
	representative := make(map[copyKey]int, len(merged))
	out := merged[:0]
	for _, hit := range merged {
		file := fileKey{corpus: hit.corpus, path: hit.path}
		if _, seen := seenFiles[file]; seen {
			continue
		}
		seenFiles[file] = struct{}{}
		if hit.contentID != (semanticContentID{}) && !hit.nameOnly {
			key := copyKey{content: hit.contentID, match: hit.match}
			if at, ok := representative[key]; ok {
				out[at].also = append(out[at].also, corpusCopy{displayRoot: hit.displayRoot, path: hit.path})
				continue
			}
			representative[key] = len(out)
		}
		out = append(out, hit)
	}
	return out
}

func sortNodeHits(hits nodeHits) {
	slices.SortStableFunc(hits, compareNodeHits)
}

// candidateFiles gives the set that the model scores. The set holds the strict
// all-word files, which are admitted and need a model rank. It also holds
// every file with a semantic hit within the retrieval radius, with no count. A
// relaxed lexical match alone is a rank signal, not a candidate. It has no row
// within the radius, so it cannot pass tau_file. Relaxed matching returns
// 2,819 to 31,238 files of a Kubernetes checkout for a description. A
// name-only file (a binary file, a macOS dataless file, a missing blob) has no
// model text. It enters only as a strict all-word match, and the model never
// scores it.
func candidateFiles(hits nodeHits) (scored nodeHits, nameOnly nodeHits) {
	for _, hit := range hits {
		switch {
		case hit.nameOnly && hit.strict:
			nameOnly = append(nameOnly, hit)
		case hit.nameOnly:
		case hit.strict || hit.semantic:
			scored = append(scored, hit)
		}
	}
	return scored, nameOnly
}

// acceptFiles admits every strict all-word file. It also admits each
// model-added file whose lift (s(f) - emptyScore)/T passes tauFile on its own,
// with no candidate count. So the verdict for a file does not depend on the
// other candidates. The order is weighted reciprocal-rank fusion of the
// lexical rank (strict files first, then relaxed) and the model rank (2:1,
// k = 60). scores holds one raw model score per scored candidate. The nameOnly
// files come after them in tie order.
func acceptFiles(
	scored nodeHits,
	scores []float32,
	emptyScore float32,
	activeTokens int,
	tauFile float32,
	nameOnly nodeHits,
) (ranked, error) {
	if len(scores) != len(scored) {
		return ranked{}, fmt.Errorf("model scores %d candidates, want %d", len(scores), len(scored))
	}
	if activeTokens <= 0 {
		return ranked{}, fmt.Errorf("semantic query has no active tokens")
	}
	lexicalRank := ranks(len(scored), func(left, right int) int {
		return compareLexicalRank(scored[left], scored[right])
	})
	// Semantic-only files share the last lexical rank, so the model alone
	// orders them; their vector score is not a lexical signal.
	lexicalFiles := 0
	for _, hit := range scored {
		if hit.strict || hit.lexical {
			lexicalFiles++
		}
	}
	for index, hit := range scored {
		if !hit.strict && !hit.lexical {
			lexicalRank[index] = lexicalFiles + 1
		}
	}
	modelRank := ranks(len(scored), func(left, right int) int {
		switch {
		case scores[left] > scores[right]:
			return -1
		case scores[left] < scores[right]:
			return 1
		}
		return compareNodeHits(scored[left], scored[right])
	})
	accepted := make(nodeHits, 0, len(scored)+len(nameOnly))
	for index, hit := range scored {
		if math.IsNaN(float64(scores[index])) || math.IsInf(float64(scores[index]), 0) {
			return ranked{}, fmt.Errorf("invalid model score")
		}
		if !hit.strict && !meanMaxSimPasses(scores[index], emptyScore, activeTokens, tauFile) {
			continue
		}
		fused := hit
		fused.score = float32(rerankLexicalWeight/(rerankRRFConstant+float64(lexicalRank[index])) +
			1/(rerankRRFConstant+float64(modelRank[index])))
		accepted = append(accepted, fused)
	}
	sortNodeHits(accepted)
	accepted = append(accepted, nameOnly...)
	return ranked{hits: accepted}, nil
}

// ranks gives each of n items its rank from 1 in the stable order of compare,
// which compares two item indexes.
func ranks(n int, compare func(left, right int) int) []int {
	order := make([]int, n)
	for index := range order {
		order[index] = index
	}
	slices.SortStableFunc(order, compare)
	rank := make([]int, n)
	for position, index := range order {
		rank[index] = position + 1
	}
	return rank
}

// compareLexicalRank orders strict all-word files first, then relaxed lexical
// files, each by their lexical score and the tie key. Semantic-only files
// come last, and acceptFiles gives them one shared rank.
func compareLexicalRank(left, right nodeHit) int {
	rank := func(hit nodeHit) int {
		switch {
		case hit.strict:
			return 0
		case hit.lexical:
			return 1
		}
		return 2
	}
	if c := rank(left) - rank(right); c != 0 {
		return c
	}
	return compareNodeHits(left, right)
}

// openedNode is a prepared node: what the read step of a route reads. Its
// index is current, or with indexState corpusStale the last complete one (a
// failed build, a peer's build). state is the Git status of the capture.
type openedNode struct {
	plan       node
	state      repoState
	indexState corpusIndexState
	dirty      dirtyFileSet
	// version gives the state that the index must hold at read time. It also
	// gives the semantic source of that state: the commit of a Git node, the
	// walk state of a folder node. nil reads text only.
	version func() (state, source string, ok bool)
	// attempt counts the reads of this node in one openNode: 0, or 1 after a
	// repair or a tree change.
	attempt int
	// repair collects the repairs that the read asks for; openNode runs them
	// after the read releases its lock.
	repair *nodeRepair
}

// nodeRepair is a repair that a node read asks for. A read holds the node's
// read lock, and a repair takes the publish lock, which waits for every
// reader: so the read only records it.
type nodeRepair struct {
	// vectors is set when a read finds vector data that no shard repair
	// fixes. That data is rows that still do not match their shard copy
	// after the shard repair, or a graph shard that fails its hash.
	vectors atomic.Bool
}

// openNode makes one node's index current, then runs read on it. The
// preparation runs the build or delta under the node locks, swap recovery,
// and the stale-index rule. That rule (servesStale) serves the published
// shards of a failed update with a warning. A read can find a shard that it
// cannot load (errShardUnloadable). Then openNode removes the family manifest
// that the read saw (removeDamagedFamilyManifest) and opens the node once
// more. A second failure returns. A read that sees the tree
// change opens the node once more from a new capture. When a read records
// vector damage (nodeRepair), openNode removes the node's vector generations
// after the read (removeDamagedVectors). Every prepare path of a search goes
// through openNode.
func openNode(
	ctx context.Context,
	plan node,
	execution searchExecution,
	read func(openedNode) error,
) error {
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// The first read saw damage or a tree change: read the tree
			// again.
			plan.gitState.invalidate()
			plan.folder.invalidate()
		}
		opened, err := prepareNode(ctx, plan, execution)
		if err != nil {
			return asNodeFailure(ctx, err)
		}
		opened.attempt, opened.repair = attempt, &nodeRepair{}
		err = read(opened)
		if opened.repair.vectors.Load() {
			removeDamagedVectors(ctx, plan)
		}
		retry := errors.Is(err, errShardUnloadable) || errors.Is(err, errGitIndexStateChanged)
		switch {
		case retry && attempt == 0:
			if errors.Is(err, errShardUnloadable) {
				// Invalidate the manifest so the retry rebuilds the committed
				// family or a complete folder family. Keep current shards until
				// publication.
				removeDamagedFamilyManifest(ctx, plan, err)
				slog.Warn("Index damage detected; rebuilding the corpus", "root", plan.root, "error", err)
			}
			continue
		case retry:
			// The repair or the new capture did not help.
			return asNodeFailure(ctx, err)
		}
		// The not-indexed check reads this node, also after a read error.
		execution.opened.add(plan.id)
		if err != nil {
			// A read error fails this node only.
			return asNodeFailure(ctx, err)
		}
		return nil
	}
}

// removeDamagedVectors removes the node's vector generations, so the next
// preparation builds them again. The shard repair rebuilds only the Zoekt
// family. The open check of a generation trusts rows that its build recorded.
// So without this removal, damaged rows give the same mismatch on every
// search. The open check does not read the graph shards either. So the next
// bind would serve a damaged graph shard again, and each search would hash it
// and fall back to the exact vectors. The windows of the removed generations
// go to the unit store first. The rebuild copies every window whose file and
// content still match. It embeds only the damaged rows again, because their
// content does not match. The graphs are built again from the rows. The
// removal takes the publish lock, as removeDamagedFamilyManifest does. It
// also takes the node's build lock with no wait. A peer build can still link
// graph files of these generations, so the removal skips while one runs. That
// build's rows are good, because it does not copy the damaged rows: their
// content does not match. It also hashes a graph shard whose recorded check
// fails, and links it only when the hash matches.
func removeDamagedVectors(ctx context.Context, plan node) {
	build, ok, err := tryBuildLock(plan.cacheDir)
	if err != nil || !ok {
		slog.Debug("Kept the damaged vector index", "root", plan.root, "cause", "a build runs", "error", err)
		return
	}
	defer releaseLock(build)
	lock, err := acquirePublishLock(ctx, plan.cacheDir)
	if err != nil {
		slog.Debug("Kept the damaged vector index", "root", plan.root, "error", err)
		return
	}
	defer releaseLock(lock)
	removeJoinedGeneration(plan.cacheDir)
	archiveSupersededUnits(plan.indexDir)
	removeSemanticGenerationsExcept(plan.indexDir, nil)
	slog.Warn("Vector index damage detected; rebuilding the vectors", "root", plan.root)
}

// removeDamagedFamilyManifest removes the family manifest that a damaged read
// saw, so the next preparation rebuilds the family. A peer can publish a new
// family, with a new manifest, after the read released its lock. So the
// removal takes the publish lock, which waits for every reader of the node.
// It removes the manifest only when it is still the file that the read saw.
// On cancellation or eviction it removes nothing.
func removeDamagedFamilyManifest(ctx context.Context, plan node, err error) {
	damage, ok := errors.AsType[*shardDamage](err)
	if !ok || damage.manifest == nil {
		return
	}
	lock, err := acquirePublishLock(ctx, plan.cacheDir)
	if err != nil {
		slog.Debug("Kept the family manifest of the damaged index", "root", plan.root, "error", err)
		return
	}
	defer releaseLock(lock)
	// A publish writes a new file and renames it; the time also covers a
	// reused inode.
	current, err := os.Stat(filepath.Join(plan.indexDir, familyManifestFile))
	if err == nil && os.SameFile(current, damage.manifest) && current.ModTime().Equal(damage.manifest.ModTime()) {
		removeFamilyManifest(plan.indexDir)
	}
}

// nodePreparations counts prepareNode calls; tests read it to see one
// preparation per node per command.
var nodePreparations atomic.Int64

// prepareNode makes the index of one node current through its kind
// (nodeKind.prepare) and counts the preparation.
func prepareNode(
	ctx context.Context,
	plan node,
	execution searchExecution,
) (openedNode, error) {
	nodePreparations.Add(1)
	if plan.behavior == nil {
		return openedNode{}, fmt.Errorf("%w: the %s node %s has no kind behavior", errInvariant, plan.kind, plan.root)
	}
	return plan.behavior.prepare(ctx, plan, execution)
}

// openedNodes records the nodes that one command opened, for the not-indexed
// check (warnUnindexedFileOperands). openNode records a node after a read that
// saw no damage and no tree change, also when that read failed. A node whose
// preparation failed is not recorded. Nil records nothing.
type openedNodes struct {
	mu  sync.Mutex
	ids map[corpusID]struct{}
}

// add records that the command opened the node.
func (nodes *openedNodes) add(id corpusID) {
	if nodes == nil {
		return
	}
	nodes.mu.Lock()
	defer nodes.mu.Unlock()
	if nodes.ids == nil {
		nodes.ids = make(map[corpusID]struct{})
	}
	nodes.ids[id] = struct{}{}
}

// has reports whether the command opened the node.
func (nodes *openedNodes) has(id corpusID) bool {
	if nodes == nil {
		return false
	}
	nodes.mu.Lock()
	defer nodes.mu.Unlock()
	_, ok := nodes.ids[id]
	return ok
}

// nodeOpenError marks a node that failed to open: its build, its Git state, a
// read error, or damage that the repair retry did not fix. The orchestrator
// skips such a node with one warning.
type nodeOpenError struct {
	err error
}

func (e *nodeOpenError) Error() string { return e.err.Error() }
func (e *nodeOpenError) Unwrap() error { return e.err }

// asNodeFailure marks err as a failure of one node. A cancellation and
// an environment failure (environmentFailure) stay as they are and end the
// command.
func asNodeFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	if interruptedChild(err) {
		return context.Canceled
	}
	if environmentFailure(err) {
		return err
	}
	return &nodeOpenError{err: err}
}

// errInvariant marks a Seek defect: a state that no input can cause. It ends
// the command instead of hiding a node.
var errInvariant = errors.New("internal error")

// environmentFailure reports an error that every node would meet, so it ends
// the command with exit 2 and is no node failure. Such errors are a missing
// ctags, no free file descriptor (EMFILE, ENFILE: no property of a node), and
// a Seek defect. The cache root is checked once before the expansion
// (checkCacheRoot).
func environmentFailure(err error) bool {
	if _, ctags := errors.AsType[*ctagsUnavailableError](err); ctags {
		return true
	}
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, errInvariant)
}

// interruptedChild reports a child process (Git, ctags) that a terminal
// signal stopped. The same signal reaches Seek, so the command is being
// cancelled: the node did not fail and gets no warning.
func interruptedChild(err error) bool {
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false
	}
	switch status.Signal() {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP:
		return true
	}
	return false
}

// buildWindowBytes is the read budget that running node builds take: two
// maximum-weight documents stay free. Each build takes the two windows of its
// command (the lexical and the semantic consumer). So the builds of every
// command in the process keep the deadlock proof. The preparation of a ready
// node and every search take none.
var (
	buildWindowBytes = max(readBudgetBytes-2*maxReadWeightBytes, 1)
	buildWindows     = semaphore.NewWeighted(buildWindowBytes)
)

// acquireBuildSlot waits for one build slot and returns its release. A build
// takes the slot before its build lock.
func acquireBuildSlot(ctx context.Context) (func(), error) {
	// A slot wait under a read lock could wait forever, as a build lock wait
	// could.
	if err := checkLockOrder(ctx); err != nil {
		return nil, err
	}
	weight := min(max(2*indexWindow(ctx), 1), buildWindowBytes)
	if err := buildWindows.Acquire(ctx, weight); err != nil {
		return nil, err
	}
	return func() { buildWindows.Release(weight) }, nil
}

// searchNodes runs every node of the graph at the same time: search opens one
// node and reads it. It skips a node that fails to open, with one warning.
// The others still answer: exit 0 on matches, 1 on none, never 2 for a node
// failure. Any other error stops the search. A cancellation returns ctx.Err()
// once, with no warning per node. It joins the node outputs in plan order; the
// next stage orders them.
func searchNodes[T any](
	ctx context.Context,
	plans []node,
	search func(context.Context, node) ([]T, dirtyFileSet, error),
) ([]T, dirtyFilesByCorpus, error) {
	type nodeResult struct {
		plan    node
		results []T
		dirty   dirtyFileSet
		err     error
		run     bool
	}
	outcomes := make([]nodeResult, len(plans))
	seen := make(map[corpusID]bool, len(plans))
	group, groupCtx := errgroup.WithContext(ctx)
	for index, plan := range plans {
		// A physical corpus reached through two paths runs once.
		if seen[plan.id] {
			continue
		}
		seen[plan.id] = true
		group.Go(func() error {
			results, dirty, err := search(groupCtx, plan)
			outcomes[index] = nodeResult{plan: plan, results: results, dirty: dirty, err: err, run: true}
			if _, opening := errors.AsType[*nodeOpenError](err); err != nil && !opening && groupCtx.Err() == nil {
				return err
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var all []T
	dirtyByCorpus := make(dirtyFilesByCorpus)
	for _, outcome := range outcomes {
		if !outcome.run {
			continue
		}
		// Wait or the ctx.Err() check returned every other error.
		if failure, ok := errors.AsType[*nodeOpenError](outcome.err); ok {
			warnSkippedNode(ctx, outcome.plan.displayRoot, failure.err)
			continue
		}
		if len(outcome.dirty) > 0 {
			dirtyByCorpus[outcome.plan.id] = outcome.dirty
		}
		all = append(all, outcome.results...)
	}
	return all, dirtyByCorpus, nil
}

// warnSkippedNode writes the one warning of a node that the search skips. A
// cancellation is silent.
func warnSkippedNode(ctx context.Context, root string, err error) {
	if ctx.Err() != nil || interruptedChild(err) {
		slog.Debug("search node failed after cancellation", "root", root, "error", err)
		return
	}
	if warned, ok := ctx.Value(skippedNodesKey{}).(*skippedNodes); ok && !warned.first(root) {
		return
	}
	cause := strings.ReplaceAll(strings.TrimPrefix(formatCLIError(err), "seek: "), "\n", "; ")
	slog.Warn("Skipped a search node; its results are missing", "root", root, "cause", cause)
}

// skippedNodes holds the roots that one command already warned about. The
// operand resolver, the walks, and the node search can meet one failed node,
// and it gets one warning. A root is keyed by its file identity, so two
// spellings of one directory are one root.
type skippedNodes struct {
	mu    sync.Mutex
	roots map[string]struct{}
}

type skippedNodesKey struct{}

// withSkippedNodes gives the command's context its warning registry.
func withSkippedNodes(ctx context.Context) context.Context {
	return context.WithValue(ctx, skippedNodesKey{}, &skippedNodes{roots: make(map[string]struct{})})
}

// first records root and reports whether it is new.
func (nodes *skippedNodes) first(root string) bool {
	key := canonicalCorpusPath(root)
	if identity, err := filesystemIdentity(key); err == nil {
		key = identity
	}
	nodes.mu.Lock()
	defer nodes.mu.Unlock()
	if _, seen := nodes.roots[key]; seen {
		return false
	}
	nodes.roots[key] = struct{}{}
	return true
}

// queryPlan is the first stage of the pipeline: the strict query that every
// node runs. For a query that gives the model text, it also holds the plan of
// the model stages.
type queryPlan struct {
	raw    query.Q
	strict query.Q
	// halves holds the file-name atoms of strict that bare words made.
	halves map[query.Q]struct{}
	// model is set for a query that gives the model text (planRerankQuery).
	model *rerankQueryPlan
}

// planQuery parses pattern into the stage-one plan. It plans the model stages
// only when policy enables semantic search.
func planQuery(pattern string, policy searchPolicy) (queryPlan, error) {
	raw, strict, halves, err := parseQueryHalves(pattern)
	if err != nil {
		return queryPlan{}, err
	}
	plan := queryPlan{raw: raw, strict: strict, halves: halves}
	if policy.semanticEnabled() {
		if model, ok := planRerankQuery(raw); ok {
			plan.model = &model
		}
	}
	return plan, nil
}

// strictNode opens one node and runs the strict query on it: the route of a
// query with no model stage.
func strictNode(ctx context.Context, plan node, qp queryPlan, config searchConfig, execution searchExecution) ([]corpusSearchResult, dirtyFileSet, error) {
	var results []corpusSearchResult
	var dirty dirtyFileSet
	var nameOnly int64
	err := openNode(ctx, plan, execution, func(node openedNode) error {
		results, dirty, nameOnly = nil, node.dirty, 0
		if node.indexState == corpusKnownEmpty {
			touchPlanUsed(node.plan)
			return nil
		}
		// One read lock and one shard load cover the file listing, the search
		// and the count, so a publish cannot remove a shard between them.
		read, ctx, err := openNodeRead(ctx, node.plan, node.dirty)
		if err != nil {
			return err
		}
		defer read.close()
		nodeQP, err := nodeHeaderQuery(ctx, read, qp, execution.operands)
		if err != nil {
			return err
		}
		var counted sync.WaitGroup
		counted.Go(func() {
			nameOnly = countNameOnlyFiles(ctx, read, nodeQP.strict)
		})
		strictConfig := config
		strictConfig.bm25 = true
		files, err := read.search(ctx, nodeQP.strict, strictConfig)
		counted.Wait()
		if err != nil {
			return err
		}
		touchPlanUsed(node.plan)
		results = wrapCorpusResults(node.plan, files)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	addNameOnlyFiles(ctx, nameOnly)
	return results, dirty, nil
}

// retrieveNode opens one node and reads it for a model query, under one read
// lock on one generation. The read runs strict lexical retrieval and the
// relaxed lexical rank of each semantic hit. It also runs semantic retrieval
// within the radius, over the rows that the query's filters and the node's
// scope allow. It gives one hit per file with its display result and its
// model document.
func retrieveNode(ctx context.Context, plan node, qp queryPlan, config searchConfig, execution searchExecution) ([]nodeHit, dirtyFileSet, error) {
	var hits nodeHits
	var dirty dirtyFileSet
	var nameOnly int64
	err := openNode(ctx, plan, execution, func(node openedNode) error {
		hits, dirty, nameOnly = nil, node.dirty, 0
		if node.indexState == corpusKnownEmpty {
			touchPlanUsed(node.plan)
			return nil
		}
		var readErr error
		hits, readErr = readNodeEvidence(ctx, node, qp, config, execution, &nameOnly)
		if readErr == nil {
			touchPlanUsed(node.plan)
		}
		return readErr
	})
	if err != nil {
		return nil, nil, err
	}
	addNameOnlyFiles(ctx, nameOnly)
	return hits, dirty, nil
}

// readNodeEvidence reads one opened node for a model query. Under the node's
// read lock it runs the strict search, the name-only count, and semantic
// retrieval on the bound generation and the overlay. It then reads the
// relaxed lexical rank of each semantic file, and the text of each semantic
// file from the shards (readSemanticEvidence).
func readNodeEvidence(ctx context.Context, node openedNode, qp queryPlan, config searchConfig, execution searchExecution, nameOnlyFiles *int64) (nodeHits, error) {
	plan := node.plan
	read, ctx, err := openNodeRead(ctx, plan, node.dirty)
	if err != nil {
		return nil, err
	}
	defer read.close()

	// One capture per node per command: under the lock the index
	// must hold the captured state, and the bound generation that state.
	// The base generation of a Git node holds the commit; the overlay of a
	// dirty Git node holds the work-tree bytes of its dirty files.
	var generation, overlay *semanticGeneration
	var state, source string
	var ok bool
	switch {
	case node.indexState == corpusStale:
		// The build failed or a peer builds: the prior index holds no
		// captured state, so no bound vectors either.
		slog.Debug("vectors unavailable", "root", plan.displayRoot, "cause", "the index is not current")
	case node.version != nil:
		state, source, ok = node.version()
	}
	if ok && readStateFile(plan.cacheDir) != state {
		if node.attempt == 0 {
			return nil, treeChanged(plan)
		}
		// The tree changed again after the retry's capture, as a file
		// written on each save does. The node serves its text with no
		// vectors, as a stale index does.
		slog.Debug("vectors unavailable", "root", plan.displayRoot, "cause", "the tree changed again")
		ok = false
	}
	if ok {
		overlaySource := ""
		// Only the Git prepare sets dirty files.
		if execution.buildsOverlay(len(node.dirty) > 0) {
			overlaySource = semanticOverlaySource(state)
		}
		if plan.joinedGenerationsMatch(state, source, overlaySource) {
			if source != "" {
				generation, err = plan.openJoinedSemanticGeneration(source)
			}
			if generation != nil {
				// The descriptor removal alone gives no new graph: the next
				// bind would serve the same file.
				clearDescriptor := generation.onUSearchDamage
				generation.onUSearchDamage = func() {
					clearDescriptor()
					node.repair.vectors.Store(true)
				}
			}
			if err == nil && overlaySource != "" {
				overlay, err = openSemanticGenerationForSearch(semanticGenerationDir(plan.indexDir, overlaySource), overlaySource)
				if err != nil {
					// Damage clears the descriptor, as for the base, so the
					// next build repairs the overlay.
					removeJoinedGeneration(plan.cacheDir)
				}
			}
		} else {
			err = errors.New("no bound generation")
		}
		if err != nil {
			slog.Debug("vectors unavailable", "root", plan.displayRoot, "cause", err)
			if generation != nil {
				_ = generation.Close()
			}
			generation, overlay = nil, nil
		}
	}
	if generation != nil {
		defer func() { _ = generation.Close() }()
	}
	if overlay != nil {
		defer func() { _ = overlay.Close() }()
	}
	// Under the lock: the file sets read the shards that the search reads.
	qp, err = nodeHeaderQuery(ctx, read, qp, execution.operands)
	if err != nil {
		return nil, err
	}

	modelConfig := rerankModelSearchConfig(config)
	modelConfig.bm25 = true
	var strictFiles []zoekt.FileMatch
	var semanticHits []semanticHit
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var searchErr error
		strictFiles, searchErr = read.search(groupCtx, qp.strict, modelConfig)
		return searchErr
	})
	group.Go(func() error {
		*nameOnlyFiles = countNameOnlyFiles(groupCtx, read, qp.strict)
		return nil
	})
	// Both scans select their rows with one file-name query.
	selection := sync.OnceValues(func() (map[string]struct{}, error) {
		return nodeRowSelection(groupCtx, read, qp)
	})
	if generation != nil {
		group.Go(func() error {
			hits, scanErr := scanNode(groupCtx, node, qp, generation, execution, selection)
			if scanErr != nil && groupCtx.Err() == nil {
				// The model failed: the node gives its lexical hits, and the
				// model stages fall back to strict order.
				slog.Debug("vectors unavailable", "root", plan.displayRoot, "cause", scanErr)
				execution.model.recordFault(scanErr)
				return nil
			}
			semanticHits = hits
			return scanErr
		})
	}
	var overlayHits []semanticHit
	if overlay != nil && len(overlay.rows) > 0 {
		group.Go(func() error {
			hits, scanErr := scanOverlay(groupCtx, qp, overlay, execution, selection)
			if scanErr != nil && groupCtx.Err() == nil {
				slog.Debug("overlay vectors unavailable", "root", plan.displayRoot, "cause", scanErr)
				execution.model.recordFault(scanErr)
				return nil
			}
			overlayHits = hits
			return scanErr
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	strict := wrapCorpusResults(plan, strictFiles)
	semanticFiles, err := semanticHitFiles(generation, semanticHits)
	if err != nil {
		return nil, err
	}
	overlayFiles, err := semanticHitFiles(overlay, overlayHits)
	if err != nil {
		return nil, err
	}

	// The relaxed lexical rank of each semantic file that is no strict match:
	// a rank signal, not a candidate.
	strictPaths := make(map[string]struct{}, len(strict))
	for _, result := range strict {
		strictPaths[result.file.FileName] = struct{}{}
	}
	pending := make(map[string]struct{}, len(semanticFiles)+len(overlayFiles))
	for _, candidate := range slices.Concat(semanticFiles, overlayFiles) {
		if _, ok := strictPaths[candidate.unit.path]; !ok {
			pending[candidate.unit.path] = struct{}{}
		}
	}
	// The relaxed rank and the text of the semantic files are two searches of
	// the loaded shards, so they run at the same time. On Kubernetes this
	// saves about 25 ms of a 1.9 s description search.
	relaxedScores := make(map[string]relaxedLexical, len(pending))
	var evidence []semanticFileCandidate
	tail, tailCtx := errgroup.WithContext(ctx)
	if len(pending) > 0 {
		tail.Go(func() error {
			opts := searchOpts
			opts.NumContextLines = 0
			relaxedQ := query.NewAnd(qp.model.relaxedQ, &query.FileNameSet{Set: pending})
			// The relaxed rank reads the global average too.
			relaxed, err := read.search(tailCtx, relaxedQ, searchConfig{opts: &opts, bm25: true})
			if err != nil {
				return err
			}
			for _, file := range relaxed {
				if best, ok := relaxedScores[file.FileName]; !ok || file.Score > best.score {
					relaxedScores[file.FileName] = relaxedLexical{score: file.Score, parts: file.Debug}
				}
			}
			return nil
		})
	}
	// The base rows exclude the dirty files, and the overlay rows hold only
	// them, so each path has one candidate. The current-copy clause of the
	// read gives an overlay file its work-tree copy.
	tail.Go(func() error {
		var err error
		evidence, err = readSemanticEvidence(tailCtx, read, node, slices.Concat(semanticFiles, overlayFiles),
			max(config.opts.NumContextLines, searchContextLines))
		return err
	})
	if err := tail.Wait(); err != nil {
		return nil, err
	}
	// The rows and vectors stay mapped while the generation is open.
	read.close()
	return nodeEvidenceHits(plan, node.dirty, generation, overlay, strict, evidence, relaxedScores), nil
}

// scanNode runs semantic retrieval on one node through USearch. Its filtered
// search over the rows that the mask allows finds the candidates. The exact
// fine scores rank them with no final count.
func scanNode(ctx context.Context, node openedNode, qp queryPlan, generation *semanticGeneration, execution searchExecution, selection func() (map[string]struct{}, error)) ([]semanticHit, error) {
	return scanWithinRadius(ctx, qp, execution, selection, func(prepared *semanticQueryEmbedding, selected map[string]struct{}, floor float32) ([]semanticHit, error) {
		var mask *semanticFilterMask
		if keep := nodeRowFilter(selected, node.dirty); keep != nil {
			built, err := buildSemanticFilterMask(ctx, generation, keep)
			if err != nil {
				return nil, err
			}
			mask = &built
		}
		return semanticUSearchMasked(ctx, generation, prepared, mask, floor)
	})
}

// scanOverlay scores with the exact fine vectors every overlay row that the
// query's filters and the node's scope allow. The scores use the scale of the
// base rows.
func scanOverlay(ctx context.Context, qp queryPlan, overlay *semanticGeneration, execution searchExecution, selection func() (map[string]struct{}, error)) ([]semanticHit, error) {
	return scanWithinRadius(ctx, qp, execution, selection, func(prepared *semanticQueryEmbedding, selected map[string]struct{}, floor float32) ([]semanticHit, error) {
		if selected == nil {
			return exactSemanticSearch(ctx, overlay.vectors, prepared, floor)
		}
		var rows []uint64
		for index := range overlay.rows {
			if _, ok := selected[overlay.rows[index].path]; ok {
				rows = append(rows, overlay.rows[index].row)
			}
		}
		return exactSemanticSortedCandidates(ctx, overlay.vectors, prepared, rows, floor)
	})
}

// scanWithinRadius runs one scan with the command's query and the node's row
// selection. The scan keeps the rows whose summed fine MaxSim reaches
// semanticRetrievalFloor. Each kept row then takes its fine MeanMaxSim as its
// score. The scorer applies this floor before it sorts, so it sorts only the
// kept rows. On Kubernetes (1.3 million rows), a sort of every scored row
// costs 6 to 9 % of a description search.
func scanWithinRadius(
	ctx context.Context,
	qp queryPlan,
	execution searchExecution,
	selection func() (map[string]struct{}, error),
	scan func(prepared *semanticQueryEmbedding, selected map[string]struct{}, floor float32) ([]semanticHit, error),
) ([]semanticHit, error) {
	_, prepared, err := execution.model.prepareQuery(ctx, qp.model.modelQuery)
	if err != nil {
		return nil, err
	}
	if prepared == nil {
		return nil, errRerankUnavailable
	}
	tokens, err := semanticQueryTokenCount(prepared)
	if err != nil {
		return nil, err
	}
	floor := semanticRetrievalFloor(prepared.emptyScore, tokens)
	selected, err := selection()
	if err != nil {
		return nil, err
	}
	hits, err := scan(prepared, selected, floor)
	if err != nil {
		return nil, err
	}
	for index := range hits {
		hits[index].score /= float32(tokens)
	}
	return hits, nil
}

// nodeRowSelection gives the files that Zoekt selects on the node's own shards
// for the query's filters and the node's scope; nil keeps every file. The read
// searches only the current copy of each file, as for the text results. So a
// committed copy that matches a filter does not select the work-tree copy.
func nodeRowSelection(ctx context.Context, read *nodeRead, qp queryPlan) (map[string]struct{}, error) {
	if len(qp.model.filters) == 0 && read.plan.scope == nil {
		return nil, nil
	}
	opts := searchOpts
	opts.NumContextLines = 0
	filter := &query.Type{Type: query.TypeFileName, Child: query.NewAnd(append([]query.Q{&query.Const{Value: true}}, qp.model.filters...)...)}
	files, err := read.search(ctx, filter, searchConfig{opts: &opts})
	if err != nil {
		return nil, err
	}
	selected := make(map[string]struct{}, len(files))
	for _, file := range files {
		selected[file.FileName] = struct{}{}
	}
	return selected, nil
}

// nodeRowFilter gives the files whose rows semantic retrieval reads. These are
// the selected files (nodeRowSelection; nil selects every file), less the
// dirty files. The rows of a dirty file hold the commit, and the overlay adds
// the dirty bytes. nil keeps every row.
func nodeRowFilter(selected map[string]struct{}, dirty dirtyFileSet) func(string) bool {
	if selected == nil && len(dirty) == 0 {
		return nil
	}
	return func(path string) bool {
		if selected != nil {
			if _, ok := selected[path]; !ok {
				return false
			}
		}
		return !dirty.contains(path)
	}
}

// semanticHitFiles gives one candidate per file: its best row (the hits come
// best first). No count cuts them.
func semanticHitFiles(generation *semanticGeneration, hits []semanticHit) ([]semanticFileCandidate, error) {
	if generation == nil || len(hits) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(hits))
	var files []semanticFileCandidate
	for _, hit := range hits {
		if hit.row >= uint64(len(generation.rows)) {
			return nil, fmt.Errorf("semantic row %d is out of range", hit.row)
		}
		unit := generation.rows[hit.row]
		if unit.row != hit.row || unit.path == "" {
			return nil, fmt.Errorf("semantic row %d has invalid identity", hit.row)
		}
		if _, ok := seen[unit.path]; ok {
			continue
		}
		seen[unit.path] = struct{}{}
		files = append(files, semanticFileCandidate{unit: unit, proxyScore: hit.score})
	}
	return files, nil
}

// relaxedLexical is the best relaxed lexical match of a file: its score and
// its BM25 parts.
type relaxedLexical struct {
	score float64
	parts string
}

// generationContentIDs adds the content ID of each wanted path of generation
// to ids.
func generationContentIDs(generation *semanticGeneration, wanted map[string]struct{}, ids map[string]semanticContentID) {
	if generation == nil || len(wanted) == 0 {
		return
	}
	previous := ""
	for index := range generation.rows {
		row := &generation.rows[index]
		if row.path == previous {
			continue
		}
		previous = row.path
		if _, ok := wanted[row.path]; ok {
			ids[row.path] = row.contentID
		}
	}
}

// nodeEvidenceHits builds the node's hits: each strict file, and each file of
// a semantic hit. A file with both keeps its lexical display and scores the
// window that the vectors found. A file's content ID comes from its rows. A
// clean file reads the base rows, a dirty file the overlay rows, and a
// name-only file has none.
func nodeEvidenceHits(
	plan node,
	dirty dirtyFileSet,
	generation *semanticGeneration,
	overlay *semanticGeneration,
	strict []corpusSearchResult,
	semanticFiles []semanticFileCandidate,
	relaxedScores map[string]relaxedLexical,
) nodeHits {
	// A clean path takes its content ID from the base rows, a dirty path from
	// the overlay rows, so identical dirty copies collapse too.
	clean := make(map[string]struct{}, len(strict))
	changed := make(map[string]struct{})
	for _, result := range strict {
		if dirty.contains(result.file.FileName) {
			changed[result.file.FileName] = struct{}{}
		} else {
			clean[result.file.FileName] = struct{}{}
		}
	}
	contentIDs := make(map[string]semanticContentID, len(strict))
	generationContentIDs(generation, clean, contentIDs)
	overlayIDs := make(map[string]semanticContentID, len(changed))
	generationContentIDs(overlay, changed, overlayIDs)
	hits := make(nodeHits, 0, len(strict)+len(semanticFiles))
	byPath := make(map[string]int, len(strict))
	for _, result := range strict {
		path := result.file.FileName
		hit := nodeHit{
			corpus:      plan.id,
			displayRoot: plan.displayRoot,
			path:        path,
			score:       float32(result.file.Score),
			strict:      true,
			match:       lexicalMatchKey(result.file),
			display:     result,
			document:    newRerankDocument(result),
		}
		if !dirty.contains(path) {
			id, ok := contentIDs[path]
			hit.contentID = id
			// A clean file with no rows has no content that Zoekt indexes:
			// it enters as a strict name match and is never model-scored.
			hit.nameOnly = generation != nil && !ok
		} else {
			hit.contentID = overlayIDs[path]
		}
		byPath[path] = len(hits)
		hits = append(hits, hit)
	}
	for _, candidate := range semanticFiles {
		if at, ok := byPath[candidate.unit.path]; ok {
			hits[at].semantic = true
			hits[at].start = candidate.unit.start
			hits[at].document = candidate.document
			continue
		}
		relaxed, lexical := relaxedScores[candidate.unit.path]
		score := relaxed.score
		if !lexical {
			score = float64(candidate.proxyScore)
		}
		hits = append(hits, nodeHit{
			corpus:       plan.id,
			displayRoot:  plan.displayRoot,
			path:         candidate.unit.path,
			contentID:    candidate.unit.contentID,
			match:        fmt.Sprintf("unit:%d-%d", candidate.unit.start, candidate.unit.end),
			start:        candidate.unit.start,
			score:        float32(score),
			lexical:      lexical,
			lexicalParts: relaxed.parts,
			semantic:     true,
			display:      candidate.result,
			document:     candidate.document,
		})
	}
	return hits
}

// lexicalMatchKey names a file's best lexical match for duplicate collapse:
// two copies with the same content match the same line.
func lexicalMatchKey(file zoekt.FileMatch) string {
	best := bestLineMatch(file.LineMatches)
	if best < 0 {
		return "name"
	}
	return fmt.Sprintf("line:%d", file.LineMatches[best].LineNumber)
}

// rankModelQuery runs the model stages over the merged hits:
// the candidate set, the model score of each candidate in batches, and the
// per-file acceptance. A model error gives the strict order with one debug
// line.
func rankModelQuery(ctx context.Context, hits nodeHits, qp queryPlan, execution searchExecution) (ranked, error) {
	merged := mergeNodeHits(hits)
	scored, nameOnly := candidateFiles(merged)
	if len(scored) == 0 {
		return ranked{hits: nameOnly}, nil
	}
	model, prepared, err := execution.model.prepareQuery(ctx, qp.model.modelQuery)
	if err == nil && (model == nil || prepared == nil) {
		err = errRerankUnavailable
	}
	var scores []float32
	if err == nil {
		documents := make([]rerankDocument, len(scored))
		for index, hit := range scored {
			documents[index] = hit.document
		}
		scores, err = model.ScoresWithSemanticQuery(ctx, prepared, documents)
	}
	var activeTokens int
	if err == nil {
		activeTokens, err = semanticQueryTokenCount(prepared)
	}
	if err == nil {
		var accepted ranked
		accepted, err = acceptFiles(scored, scores, prepared.emptyScore, activeTokens, pipelineTauFile, nameOnly)
		if err == nil {
			slog.Debug("Accepted model candidates", "candidates", len(scored),
				"accepted", len(accepted.hits)-len(nameOnly), "tau_file", pipelineTauFile,
				"empty_score", prepared.emptyScore)
			return accepted, nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ranked{}, ctxErr
	}
	slog.Debug("Model stages failed; using strict order", "provider", semanticProviderName(), "error", err)
	execution.model.recordFault(err)
	var strict nodeHits
	for _, hit := range merged {
		if hit.strict {
			strict = append(strict, hit)
		}
	}
	return ranked{hits: strict}, nil
}

// presentRanked turns the ranked hits into display results in rank order.
func presentRanked(r ranked) []corpusSearchResult {
	out := make([]corpusSearchResult, len(r.hits))
	for index, hit := range r.hits {
		result := hit.display
		result.rankOverride = index + 1
		result.also = hit.also
		out[index] = result
	}
	return out
}
