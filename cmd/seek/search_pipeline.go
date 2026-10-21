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
// queryPlan in, nodeHits from each node, and one ranked list out.

// Retrieval rule: a semantic hit is a row whose fine MeanMaxSim is at least
// tau_file - delta. A file whose model MeanMaxSim passes tau_file then has its
// rows within the radius while the fine vectors under-estimate the model score
// by at most delta. delta is the p99.9 of (model MeanMaxSim - fine
// MeanMaxSim) over 18,856 window rows of a Kubernetes checkout with a model
// MeanMaxSim of at least 0.55 (p50 0.008, p99 0.069, max 0.149). A per-token
// coarse radius kept every Kubernetes file (delta p99 0.445), so it is not the
// rule.
const semanticRetrievalDelta float32 = 0.1158

// pipelineTauFile is the per-file acceptance threshold, a MeanMaxSim. A
// calibration of 2026-09-14 set it for an earlier rule that admitted a query
// by its best file (65/72 useful and 0/71 wrong-project descriptions on six
// repository families).
const pipelineTauFile float32 = 0.6

func semanticRetrievalRadius(tauFile float32) float32 {
	return tauFile - semanticRetrievalDelta
}

// semanticUSearchDeepenSimilarity is the coarse similarity down to which
// USearch keeps returning keys for a token: an approximation parameter, not a
// count. On a Kubernetes checkout (20 queries) 0.6 kept a median 0.998 (min
// 0.985) of the files that an exact fine scan finds within the radius, and
// the exact top 20 in every query; 0.7 reached the recall target (top-20
// overlap of at least 0.95) only at its limit, 0.8 kept half. That
// measurement read a 127^2 scale, 4.5 % below the cosine (an effective cosine
// of about 0.627); the bound is a cosine, so it keeps a few more rows than
// measured.
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
	// display is what the output prints for the file, and document what the
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

// mergeNodeHits concatenates the hits of every node, sorts them once with the
// tie key, and collapses copies with the same content and match on the sorted
// list: the representative is the copy with the smallest tie key, and the
// other paths follow in order. The same (corpus, path) twice keeps its first,
// best hit. No step cuts a node's hits.
func mergeNodeHits(nodes ...nodeHits) nodeHits {
	total := 0
	for _, hits := range nodes {
		total += len(hits)
	}
	merged := make(nodeHits, 0, total)
	for _, hits := range nodes {
		merged = append(merged, hits...)
	}
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

// candidateFiles is the set that the model scores: the strict all-word files,
// which are admitted and need a model rank, and every file with a semantic hit
// within the retrieval radius, with no count. A relaxed lexical match alone is
// a rank signal, not a candidate: it has no row within the radius, so it
// cannot pass tau_file (relaxed matching returns 2,819 to 31,238 files of a
// Kubernetes checkout for a description). A name-only file (a binary file, a
// macOS dataless file, a missing blob) has no model text: it enters only as a
// strict all-word match and is never model-scored.
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

// acceptFiles admits every strict all-word file and each model-added file
// whose normalized score s(f)/T passes tauFile on its own, with no candidate
// count: the verdict for a file does not depend on the other
// candidates. The order is weighted reciprocal-rank fusion of the lexical
// rank (strict files first, then relaxed) and the model rank (2:1, k = 60).
// scores holds one raw model score per scored candidate; nameOnly files come
// after them in tie order.
func acceptFiles(
	scored nodeHits,
	scores []float32,
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
	lexical := make([]int, len(scored))
	for index := range lexical {
		lexical[index] = index
	}
	slices.SortStableFunc(lexical, func(left, right int) int {
		return compareLexicalRank(scored[left], scored[right])
	})
	lexicalRank := make([]int, len(scored))
	for rank, index := range lexical {
		lexicalRank[index] = rank + 1
	}
	model := make([]int, len(scored))
	for index := range model {
		model[index] = index
	}
	slices.SortStableFunc(model, func(left, right int) int {
		switch {
		case scores[left] > scores[right]:
			return -1
		case scores[left] < scores[right]:
			return 1
		}
		return compareNodeHits(scored[left], scored[right])
	})
	modelRank := make([]int, len(scored))
	for rank, index := range model {
		modelRank[index] = rank + 1
	}
	accepted := make(nodeHits, 0, len(scored)+len(nameOnly))
	for index, hit := range scored {
		if math.IsNaN(float64(scores[index])) || math.IsInf(float64(scores[index]), 0) {
			return ranked{}, fmt.Errorf("invalid model score")
		}
		if !hit.strict && !meanMaxSimPasses(scores[index], activeTokens, tauFile) {
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

// compareLexicalRank orders strict all-word files first, then relaxed lexical
// files, each by their lexical score and the tie key; semantic-only files
// come last.
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

// openedNode is a node whose index is current: what the read step of a route
// reads. state is the Git status that the index holds.
type openedNode struct {
	plan       corpusPlan
	state      repoState
	indexState corpusIndexState
	dirty      dirtyFileSet
}

// openNode makes one node's index current, then runs read on it: the build or
// delta under the node locks, swap recovery, and a folder's stale-index rule
// (a failed update with published shards warns and serves them). A read that
// finds a shard it cannot load drops the family manifest and opens the node
// once more (errShardUnloadable); a second failure returns. Every prepare
// path of a search goes through openNode.
func openNode(
	ctx context.Context,
	plan corpusPlan,
	execution searchExecution,
	read func(openedNode) error,
) error {
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// The attempt saw damage: read the tree again.
			plan.gitState.invalidate()
			plan.folder.invalidate()
		}
		if attempt > 0 {
			execution.opened.forget(plan.id)
		}
		node, err := execution.opened.open(ctx, plan, execution.vectors, func() (openedNode, error) {
			node, err := prepareNode(ctx, plan, execution)
			if err != nil {
				return openedNode{}, asNodeFailure(ctx, err)
			}
			return node, nil
		})
		if err != nil {
			return err
		}
		err = read(node)
		switch {
		case errors.Is(err, errShardUnloadable) && attempt == 0:
			// Invalidate the manifest so the retry rebuilds the committed family
			// or a complete folder family. Keep current shards until publication.
			removeFamilyManifest(plan.indexDir)
			slog.Warn("Index damage detected; rebuilding the corpus", "root", plan.root, "error", err)
			continue
		case errors.Is(err, errShardUnloadable):
			// The repair did not help: every later pass of the command reads
			// this failure, with no third preparation.
			failure := asNodeFailure(ctx, err)
			execution.opened.fail(plan.id, failure)
			return failure
		case errors.Is(err, errGitIndexStateChanged) && attempt == 0:
			// The read saw the tree change (treeChanged dropped the
			// captures): prepare the node from a new capture and read again.
			execution.opened.forget(plan.id)
			continue
		case errors.Is(err, errGitIndexStateChanged):
			execution.opened.forget(plan.id)
			return asNodeFailure(ctx, err)
		case err != nil:
			// A read error fails this node only.
			return asNodeFailure(ctx, err)
		}
		return nil
	}
}

// nodePreparations counts prepareNode calls; tests read it to see one
// preparation per node per command.
var nodePreparations atomic.Int64

// prepareNode makes the index of one node current: the build or delta of a Git
// node, or the build of a folder node. A folder whose update fails while
// shards exist serves them with a warning.
func prepareNode(
	ctx context.Context,
	plan corpusPlan,
	execution searchExecution,
) (openedNode, error) {
	nodePreparations.Add(1)
	node := openedNode{plan: plan}
	switch plan.kind {
	case corpusKindGit:
		planPaths := plan.gitPaths
		if planPaths == nil {
			return openedNode{}, fmt.Errorf("%w: a Git node without Git paths", errInvariant)
		}
		node.plan.gitPaths = planPaths
		// One combined whole-repo index serves both scoped and unscoped
		// searches; scope is applied at search time.
		state, indexState, err := ensureGitCorpusFreshWithExecution(ctx, plan, *planPaths, execution)
		if err != nil {
			return openedNode{}, err
		}
		node.state, node.indexState, node.dirty = state, indexState, dirtyFileSetFromState(state)
	case corpusKindFolder:
		indexState, err := ensureFolderCorpusFreshWithExecution(ctx, plan, execution)
		if err != nil {
			if !shardsExist(plan.indexDir) {
				return openedNode{}, err
			}
			reportStaleIndexWarning(err)
			indexState = corpusStale
		}
		node.indexState = indexState
	default:
		return openedNode{}, fmt.Errorf("%w: unsupported corpus kind %s", errInvariant, plan.kind)
	}
	return node, nil
}

// openedNodes holds the nodes that one command opened: each node is prepared
// once per command, and every retrieval pass of the command reads
// that one preparation, as the snapshot contract says (a search shows the
// tree as captured at its start). A failed preparation is kept too, so the
// node fails once. Nil prepares on every call.
type openedNodes struct {
	mu    sync.Mutex
	nodes map[corpusID]*openedNodeEntry
}

type openedNodeEntry struct {
	mu   sync.Mutex
	done bool
	// vectors reports that the preparation built or checked the node's
	// vectors; a preparation without them cannot serve a route that wants
	// them.
	vectors bool
	node    openedNode
	err     error
}

// open returns the command's preparation of plan, and runs prepare when there
// is none or when it lacks the vectors the caller wants. A cancelled
// preparation and a failed upgrade to vectors are not kept.
func (nodes *openedNodes) open(ctx context.Context, plan corpusPlan, vectors bool, prepare func() (openedNode, error)) (openedNode, error) {
	if nodes == nil {
		return prepare()
	}
	nodes.mu.Lock()
	if nodes.nodes == nil {
		nodes.nodes = make(map[corpusID]*openedNodeEntry)
	}
	entry := nodes.nodes[plan.id]
	if entry == nil {
		entry = &openedNodeEntry{}
		nodes.nodes[plan.id] = entry
	}
	nodes.mu.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.done && vectors && !entry.vectors && entry.err == nil {
		// An upgrade to vectors that fails keeps the good preparation for the
		// routes that need none.
		node, err := prepare()
		if err != nil {
			return openedNode{}, err
		}
		entry.vectors, entry.node = true, node
	}
	if !entry.done {
		node, err := prepare()
		if ctx.Err() != nil {
			return node, err
		}
		entry.done, entry.vectors, entry.node, entry.err = true, vectors, node, err
	}
	if entry.err != nil {
		return openedNode{}, entry.err
	}
	// Another route can give the same corpus another scope.
	node := entry.node
	node.plan = plan
	if node.plan.gitPaths == nil {
		node.plan.gitPaths = entry.node.plan.gitPaths
	}
	return node, nil
}

// plan returns the plan of a node that the command opened, with its Git
// paths.
func (nodes *openedNodes) plan(id corpusID) (corpusPlan, bool) {
	if nodes == nil {
		return corpusPlan{}, false
	}
	nodes.mu.Lock()
	entry := nodes.nodes[id]
	nodes.mu.Unlock()
	if entry == nil {
		return corpusPlan{}, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.done || entry.err != nil {
		return corpusPlan{}, false
	}
	return entry.node.plan, true
}

// fail keeps err as the node's preparation for the rest of the command.
func (nodes *openedNodes) fail(id corpusID, err error) {
	if nodes == nil {
		return
	}
	nodes.mu.Lock()
	if nodes.nodes == nil {
		nodes.nodes = make(map[corpusID]*openedNodeEntry)
	}
	nodes.nodes[id] = &openedNodeEntry{done: true, vectors: true, err: err}
	nodes.mu.Unlock()
}

// forget drops the preparation of a node, so the next route prepares it
// again: a read found damage, or saw the tree change.
func (nodes *openedNodes) forget(id corpusID) {
	if nodes == nil {
		return
	}
	nodes.mu.Lock()
	delete(nodes.nodes, id)
	nodes.mu.Unlock()
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
// the command with exit 2 and is no node failure: a missing ctags, no free file
// descriptor (EMFILE, ENFILE: no property of a node), and a Seek defect.
// The cache root is checked once before the expansion (checkCacheRoot).
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
// maximum-weight documents stay free. Each build takes the two
// windows of its command (the lexical and the semantic consumer), so the
// builds of every command in the process keep the deadlock proof. The
// preparation of a ready node and every search take none.
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
// node and reads it. A node that fails to open is skipped with one warning,
// and the others still answer (exit 0 on matches, 1 on none, never 2 for a
// node failure). Any other error stops the search. A
// cancellation returns ctx.Err() once, with no warning per node. The node
// outputs are concatenated in plan order; the next stage orders them.
func searchNodes[T any](
	ctx context.Context,
	plans []corpusPlan,
	search func(context.Context, corpusPlan) ([]T, dirtyFileSet, error),
) ([]T, dirtyFilesByCorpus, error) {
	type nodeResult struct {
		plan    corpusPlan
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
		if failure, ok := errors.AsType[*nodeOpenError](outcome.err); ok {
			warnSkippedNode(ctx, outcome.plan.displayRoot, failure.err)
			continue
		}
		if outcome.err != nil {
			return nil, nil, outcome.err
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

// skippedNodes holds the roots that one command already warned about: the
// operand resolver, the walks, and every retrieval pass can meet one failed
// node, and it gets one warning. A root is keyed by its file identity,
// so two spellings of one directory are one root.
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

// queryPlan is the first stage of the pipeline: the strict query that
// every node runs, and for a description the plan of the model stages.
type queryPlan struct {
	raw    query.Q
	strict query.Q
	// halves holds the file-name atoms of strict that bare words made.
	halves map[query.Q]struct{}
	// model is set for a description.
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
func strictNode(ctx context.Context, plan corpusPlan, qp queryPlan, config searchConfig, execution searchExecution) ([]corpusSearchResult, dirtyFileSet, error) {
	var results []corpusSearchResult
	var dirty dirtyFileSet
	var nameOnly int64
	err := openNode(ctx, plan, execution, func(node openedNode) error {
		results, dirty, nameOnly = nil, node.dirty, 0
		if node.indexState == corpusKnownEmpty {
			touchPlanUsed(node.plan)
			return nil
		}
		config.contextGitCorpus = node.plan.kind == corpusKindGit
		config.contextDirtyFiles = node.dirty
		// One read lock covers the file listing, the search and the count, so
		// a publish cannot remove a shard between them.
		lock, err := os.OpenFile(filepath.Join(node.plan.cacheDir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return fmt.Errorf("open search lock: %w", err)
		}
		defer func() { _ = lock.Close() }()
		if err := acquireReadLock(ctx, node.plan.indexDir, lock); err != nil {
			return fmt.Errorf("acquire search lock: %w", err)
		}
		defer unlockFile(lock)
		ctx := withReadLockHeld(ctx)
		nodeQP, err := nodeHeaderQuery(ctx, node.plan, qp, execution.operands)
		if err != nil {
			return err
		}
		search := func(q query.Q, searchConfig searchConfig) ([]zoekt.FileMatch, error) {
			return executeParsedSearchScopedDirs(ctx, []string{node.plan.indexDir}, q, node.plan.scope, searchConfig)
		}
		var counted sync.WaitGroup
		counted.Go(func() {
			nameOnly = countNameOnlyFiles(ctx, node.plan, nodeQP.strict, node.dirty, search)
		})
		strictConfig := config
		strictConfig.bm25 = true
		files, err := search(nodeQP.strict, strictConfig)
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

// retrieveNode opens one node and reads it for a description: strict
// lexical retrieval, semantic retrieval within the radius over the rows that
// the query's filters and the node's scope allow, and the relaxed lexical rank
// of each semantic hit, under one read lock on one generation. It gives one
// hit per file with its display result and its model document.
func retrieveNode(ctx context.Context, plan corpusPlan, qp queryPlan, config searchConfig, execution searchExecution) ([]nodeHit, dirtyFileSet, error) {
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

// nodeSource is the semantic source of an opened node: the commit of a Git
// node, the walk state of a folder node.
func nodeSource(node openedNode) (state, source string, ok bool) {
	switch node.plan.kind {
	case corpusKindGit:
		if node.plan.gitPaths == nil {
			return "", "", false
		}
		// A node with no commit has no base; its overlay holds every file.
		state = gitCorpusStateHash(*node.plan.gitPaths, node.state)
		if node.state.HeadSHA == "no-head" {
			return state, "", true
		}
		return state, node.state.HeadSHA, true
	case corpusKindFolder:
		state, _, ok = node.plan.folder.current()
		return state, state, ok
	}
	return "", "", false
}

// readNodeEvidence reads one opened node for a description. Under the node's
// read lock it runs the strict search, the name-only count, semantic retrieval
// on the bound generation and the overlay, and the relaxed lexical rank of
// each semantic file. After the unlock it reads the source of each semantic
// file for its display.
func readNodeEvidence(ctx context.Context, node openedNode, qp queryPlan, config searchConfig, execution searchExecution, nameOnlyFiles *int64) (nodeHits, error) {
	plan := node.plan
	lock, err := os.OpenFile(filepath.Join(plan.cacheDir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open search lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := acquireReadLock(ctx, plan.indexDir, lock); err != nil {
		return nil, fmt.Errorf("acquire search lock: %w", err)
	}
	ctx = withReadLockHeld(ctx)
	locked := true
	defer func() {
		if locked {
			unlockFile(lock)
		}
	}()

	// One capture per node per command: under the lock the index
	// must hold the captured state, and the bound generation that state.
	// The base generation holds the commit; the overlay of a dirty Git node
	// holds the work-tree bytes of its dirty files.
	var generation, overlay *semanticGeneration
	state, source, ok := nodeSource(node)
	if node.indexState == corpusStale {
		// The build failed and the prior index serves with a warning: it
		// holds no captured state, so no bound vectors either.
		slog.Debug("vectors unavailable", "root", plan.displayRoot, "cause", "the index update failed")
		ok = false
	}
	if ok {
		if readStateFile(plan.cacheDir) != state {
			return nil, treeChanged(plan)
		}
		overlaySource := ""
		if plan.kind == corpusKindGit && execution.buildsOverlay(len(node.dirty) > 0) {
			overlaySource = semanticOverlaySource(state)
		}
		if joinedGenerationsMatch(plan.cacheDir, plan.indexDir, state, source, overlaySource) {
			if source != "" {
				generation, err = openJoinedSemanticGeneration(plan.cacheDir, plan.indexDir, source)
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
	// Under the lock: the file sets read the generation that the search reads.
	qp, err = nodeHeaderQuery(ctx, plan, qp, execution.operands)
	if err != nil {
		return nil, err
	}

	modelConfig := rerankModelSearchConfig(config)
	modelConfig.bm25 = true
	modelConfig.contextGitCorpus = plan.kind == corpusKindGit
	modelConfig.contextDirtyFiles = node.dirty
	var strictFiles []zoekt.FileMatch
	var semanticHits []semanticHit
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var searchErr error
		strictFiles, searchErr = executeParsedSearchScopedDirs(groupCtx, []string{plan.indexDir}, qp.strict, plan.scope, modelConfig)
		return searchErr
	})
	group.Go(func() error {
		*nameOnlyFiles = countNameOnlyFiles(groupCtx, plan, qp.strict, node.dirty, func(q query.Q, countConfig searchConfig) ([]zoekt.FileMatch, error) {
			return executeParsedSearchScopedDirs(groupCtx, []string{plan.indexDir}, q, plan.scope, countConfig)
		})
		return nil
	})
	if generation != nil {
		group.Go(func() error {
			hits, scanErr := scanNode(groupCtx, node, qp, generation, execution)
			if scanErr != nil && groupCtx.Err() == nil {
				// The model failed: the node gives its lexical hits, and the
				// model stages fall back to strict order.
				slog.Debug("vectors unavailable", "root", plan.displayRoot, "cause", scanErr)
				return nil
			}
			semanticHits = hits
			return scanErr
		})
	}
	var overlayHits []semanticHit
	if overlay != nil && len(overlay.rows) > 0 {
		group.Go(func() error {
			hits, scanErr := scanOverlay(groupCtx, node, qp, overlay, execution)
			if scanErr != nil && groupCtx.Err() == nil {
				slog.Debug("overlay vectors unavailable", "root", plan.displayRoot, "cause", scanErr)
				return nil
			}
			overlayHits = hits
			return scanErr
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	strict := deduplicateCorpusResults(wrapCorpusResults(plan, strictFiles), dirtyFilesByCorpus{plan.id: node.dirty})
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
	relaxedScores := make(map[string]relaxedLexical, len(pending))
	if len(pending) > 0 {
		opts := searchOpts
		opts.NumContextLines = 0
		relaxedQ := query.NewAnd(qp.model.relaxedQ, &query.FileNameSet{Set: pending})
		// The relaxed rank reads the global average too.
		relaxed, err := executeParsedSearchScopedDirs(ctx, []string{plan.indexDir}, relaxedQ, plan.scope, searchConfig{opts: &opts, bm25: true})
		if err != nil {
			return nil, err
		}
		for _, file := range relaxed {
			if !currentCopy(file, node.dirty.contains(file.FileName)) {
				// The committed copy of a dirty file holds old bytes.
				continue
			}
			if best, ok := relaxedScores[file.FileName]; !ok || file.Score > best.score {
				relaxedScores[file.FileName] = relaxedLexical{score: file.Score, parts: file.Debug}
			}
		}
	}
	// The rows and vectors stay mapped while the generation is open; the
	// evidence reads the source, not the index.
	unlockFile(lock)
	locked = false

	contextLines := max(configuredContextLines(config), searchContextLines)
	switch {
	case len(semanticFiles) == 0:
	case plan.kind == corpusKindGit:
		semanticFiles, err = materializeSemanticCandidates(ctx, plan, *plan.gitPaths, node.state.HeadSHA, semanticFiles, contextLines)
	default:
		semanticFiles, err = materializeFolderSemanticCandidates(ctx, plan, semanticFiles, contextLines)
	}
	if err != nil {
		return nil, err
	}
	// Overlay files print as the dirty files that they are.
	overlayFiles, err = materializeWorkTreeSemanticCandidates(ctx, plan, repoUncommitted, overlayFiles, contextLines)
	if err != nil {
		return nil, err
	}
	return nodeEvidenceHits(plan, node.dirty, generation, overlay, strict, slices.Concat(semanticFiles, overlayFiles), relaxedScores), nil
}

// scanNode runs semantic retrieval on one node through USearch: its filtered
// search over the rows that the mask allows finds the candidates, and the
// exact fine scores rank them with no final count. A row is a semantic hit
// when its fine MeanMaxSim reaches the radius.
func scanNode(ctx context.Context, node openedNode, qp queryPlan, generation *semanticGeneration, execution searchExecution) ([]semanticHit, error) {
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
	var mask *semanticFilterMask
	if keep, err := nodeRowFilter(ctx, node, qp); err != nil {
		return nil, err
	} else if keep != nil {
		built, err := buildSemanticFilterMask(ctx, generation, func(path, _ string) bool { return keep(path) })
		if err != nil {
			return nil, err
		}
		mask = &built
	}
	hits, err := semanticUSearchMasked(ctx, generation, prepared, mask, len(generation.rows))
	if err != nil {
		return nil, err
	}
	need := semanticRetrievalRadius(pipelineTauFile) * float32(tokens)
	kept := hits[:0]
	for _, hit := range hits {
		if hit.score >= need {
			hit.score /= float32(tokens)
			kept = append(kept, hit)
		}
	}
	return kept, nil
}

// scanOverlay scores every overlay row that the query's filters and the
// node's scope allow with the exact fine vectors, on the scale of the base
// rows, and keeps the rows within the radius.
func scanOverlay(ctx context.Context, node openedNode, qp queryPlan, overlay *semanticGeneration, execution searchExecution) ([]semanticHit, error) {
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
	selected, err := nodeRowSelection(ctx, node, qp)
	if err != nil {
		return nil, err
	}
	var hits []semanticHit
	if selected == nil {
		hits, err = exactSemanticSearch(ctx, overlay.vectors, prepared, len(overlay.rows))
	} else {
		var rows []uint64
		for index := range overlay.rows {
			if _, ok := selected[overlay.rows[index].path]; ok {
				rows = append(rows, overlay.rows[index].row)
			}
		}
		hits, err = exactSemanticSortedCandidates(ctx, overlay.vectors, prepared, rows, len(rows))
	}
	if err != nil {
		return nil, err
	}
	need := semanticRetrievalRadius(pipelineTauFile) * float32(tokens)
	kept := hits[:0]
	for _, hit := range hits {
		if hit.score >= need {
			hit.score /= float32(tokens)
			kept = append(kept, hit)
		}
	}
	return kept, nil
}

// nodeRowSelection gives the files that Zoekt selects on the node's own shards
// for the query's filters and the node's scope; nil keeps every file.
func nodeRowSelection(ctx context.Context, node openedNode, qp queryPlan) (map[string]struct{}, error) {
	if len(qp.model.filters) == 0 && node.plan.scope == nil {
		return nil, nil
	}
	opts := searchOpts
	opts.NumContextLines = 0
	filter := &query.Type{Type: query.TypeFileName, Child: query.NewAnd(append([]query.Q{&query.Const{Value: true}}, qp.model.filters...)...)}
	files, err := executeParsedSearchScopedDirs(ctx, []string{node.plan.indexDir}, filter, node.plan.scope, searchConfig{opts: &opts})
	if err != nil {
		return nil, err
	}
	selected := make(map[string]struct{}, len(files))
	for _, file := range files {
		selected[file.FileName] = struct{}{}
	}
	return selected, nil
}

// nodeRowFilter gives the files whose rows semantic retrieval reads: the files
// that Zoekt selects on the node's own shards for the query's filters and the
// node's scope, less the dirty files (their rows hold the commit; the overlay
// adds the dirty bytes). A
// query with no filter and no scope needs no Zoekt call; nil keeps every row.
func nodeRowFilter(ctx context.Context, node openedNode, qp queryPlan) (func(string) bool, error) {
	selected, err := nodeRowSelection(ctx, node, qp)
	if err != nil {
		return nil, err
	}
	if selected == nil && len(node.dirty) == 0 {
		return nil, nil
	}
	return func(path string) bool {
		if selected != nil {
			if _, ok := selected[path]; !ok {
				return false
			}
		}
		return !node.dirty.contains(path)
	}, nil
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
// window that the vectors found. A file's content ID comes from its rows: a
// clean file reads the base rows, a dirty file the overlay rows, and a
// name-only file has none.
func nodeEvidenceHits(
	plan corpusPlan,
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
	best := -1
	for index, match := range file.LineMatches {
		if best < 0 || match.Score > file.LineMatches[best].Score ||
			(match.Score == file.LineMatches[best].Score && match.LineNumber < file.LineMatches[best].LineNumber) {
			best = index
		}
	}
	if best < 0 {
		return "name"
	}
	return fmt.Sprintf("line:%d", file.LineMatches[best].LineNumber)
}

// rankDescription runs the model stages over the merged hits:
// the candidate set, the model score of each candidate in batches, and the
// per-file acceptance. A model error gives the strict order with one debug
// line.
func rankDescription(ctx context.Context, hits nodeHits, qp queryPlan, execution searchExecution) (ranked, error) {
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
		accepted, err = acceptFiles(scored, scores, activeTokens, pipelineTauFile, nameOnly)
		if err == nil {
			slog.Debug("Accepted model candidates", "candidates", len(scored),
				"accepted", len(accepted.hits)-len(nameOnly), "tau_file", pipelineTauFile)
			return accepted, nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ranked{}, ctxErr
	}
	slog.Debug("Model stages failed; using strict order", "provider", semanticProviderName(), "error", err)
	recordSemanticProviderFault(err)
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
