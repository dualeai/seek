package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// The cache budget is the one limit on the disk that Seek's indexes use. It
// covers the whole corpora tree, with the unit store. After a search, each
// corpus that it published records its size in a ledger file. Before any
// build, a search checks that its new generations fit. After the search, it
// evicts the unit-store segments and then the least recently used corpora that
// it did not use, until the cache fits again.
const (
	envCacheBudget = "SEEK_CACHE_BUDGET"
	// corpusSizeFile is the ledger entry of one corpus: its bytes on disk.
	corpusSizeFile = ".size"
	// zoektIndexRatio is the measured ratio of Zoekt index bytes to indexed
	// content bytes on the pinned Kubernetes checkout of
	// cicd/bench-semantic.py: 768 MB for 278 MB. Corpora of many small files
	// need more: 7.5 on a folder of 10,000 small text files.
	zoektIndexRatio = 2.76
	// semanticBytesPerContentByte estimates the semantic generation from the
	// content. A measurement on the same checkout with 128-token windows gave
	// 4.0 GB for 278 MB. That is about 3.4 KB per row and one row per 233
	// bytes of content.
	semanticBytesPerContentByte = 14.4
)

// cacheBudgetError stops a search whose new generations do not fit the cache
// budget, before any build. growth is the part that the search adds to the
// corpora it uses. freeBound reports a budget that the 5 % free-space floor
// sets. textFits reports that the same search with no vectors (--lexical-only)
// passes this check.
type cacheBudgetError struct {
	planned   int64
	growth    int64
	budget    int64
	freeBound bool
	textFits  bool
}

func (e *cacheBudgetError) Error() string {
	return fmt.Sprintf("this search needs about %s of index cache (%s new), more than the cache budget of %s",
		humanBytes(e.planned), humanBytes(e.growth), humanBytes(e.budget))
}

// budgetSuggestion gives the budget that the hint of the budget error names.
// It is the next whole GiB above twice the planned size, so the next searches
// fit too.
func budgetSuggestion(planned int64) string {
	const gib = int64(1) << 30
	return strconv.FormatInt(max(1, 2*planned/gib+1), 10) + "G"
}

// cacheVolume returns the capacity and the free bytes of the file system that
// holds dir. Before the first search the cache root does not exist yet, so the
// nearest existing ancestor names the volume.
func cacheVolume(dir string) (capacity, free int64, err error) {
	var st unix.Statfs_t
	for {
		err = unix.Statfs(dir, &st)
		parent := filepath.Dir(dir)
		if !errors.Is(err, unix.ENOENT) || parent == dir {
			break
		}
		dir = parent
	}
	if err != nil {
		return 0, 0, err
	}
	blockSize := int64(st.Bsize)
	return int64(st.Blocks) * blockSize, int64(st.Bavail) * blockSize, nil
}

// cacheBudget returns the byte budget of the corpora tree. It is the smaller
// of 10 % of the cache volume and the bytes that keep 5 % of the volume free.
// The second value counts the current corpora as free space that the cache can
// use again. SEEK_CACHE_BUDGET overrides it. freeBound reports a budget that
// the 5 % free-space floor sets. cacheBudget calls corpora only when the volume
// formula needs the corpora total.
func cacheBudget(cacheRoot string, corpora func() int64) (budget int64, freeBound bool, err error) {
	if value := os.Getenv(envCacheBudget); value != "" {
		budget, err := parseByteSize(value)
		if err != nil {
			return 0, false, fmt.Errorf("%s: %w", envCacheBudget, err)
		}
		return budget, false, nil
	}
	capacity, free, err := cacheVolume(cacheRoot)
	if err != nil {
		return 0, false, err
	}
	budget = budgetFromVolume(capacity, free, corpora())
	return budget, budgetFloorBinds(capacity, budget), nil
}

// budgetFloorBinds reports a budget that the 5 % free-space floor sets: it is
// below the 10 % share of the volume.
func budgetFloorBinds(capacity, budget int64) bool {
	return budget < capacity/10
}

// budgetFromVolume is the default budget (cacheBudget) for a volume's
// capacity and free bytes and the corpora total.
func budgetFromVolume(capacity, free, corporaBytes int64) int64 {
	return max(0, min(capacity/10, corporaBytes+free-capacity/20))
}

// parseByteSize reads a byte count with an optional binary unit: 512, 64K,
// 10M, 20G, 1T (also KB/KiB and so on). Only decimal digits with an optional
// fraction are numbers: NaN, infinity, exponents, and values above the int64
// range are errors.
func parseByteSize(value string) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(value))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	multiplier := int64(1)
	if s != "" {
		if i := strings.IndexByte("KMGTP", s[len(s)-1]); i >= 0 {
			multiplier = int64(1) << (10 * (i + 1))
			s = s[:len(s)-1]
		}
	}
	s = strings.TrimSpace(s)
	invalid := fmt.Errorf("invalid byte size %q (examples: 500M, 20G)", value)
	whole, fraction, _ := strings.Cut(s, ".")
	if whole == "" || strings.Trim(whole, "0123456789") != "" || strings.Trim(fraction, "0123456789") != "" {
		return 0, invalid
	}
	n, err := strconv.ParseFloat(s, 64)
	bytes := n * float64(multiplier)
	if err != nil || bytes >= math.MaxInt64 {
		return 0, invalid
	}
	return int64(bytes), nil
}

// corpusSize returns the ledger size of a corpus directory. It measures a
// corpus again when its ledger entry is missing (a publish that crashed before
// it) or older than its last publish. record writes the measure into the
// ledger; a dry run passes false so that it writes nothing.
func corpusSize(dir string, record bool) int64 {
	if !ledgerStale(dir) {
		if n, err := strconv.ParseInt(readCacheFile(dir, corpusSizeFile), 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	if !record {
		return corpusDirSize(dir)
	}
	return recordCorpusSize(dir)
}

// recordCorpusSize measures a corpus directory, as the gc table does, and
// writes its ledger entry. A corpus with no .used file keeps its directory
// mtime, which is its LRU age.
func recordCorpusSize(dir string) int64 {
	total := corpusDirSize(dir)
	info, statErr := os.Stat(dir)
	_ = writeSharedCacheFile(dir, corpusSizeFile, strconv.FormatInt(total, 10))
	if _, ok := readUsedAt(dir); !ok && statErr == nil {
		_ = os.Chtimes(dir, info.ModTime(), info.ModTime())
	}
	return total
}

// checkCacheBudget stops a search before any build when its new generations
// and the corpora it reuses do not fit the budget. Every other corpus can be
// evicted after the search, and a rebuilt corpus's old generation does not
// count.
func checkCacheBudget(ctx context.Context, plans []node, vectors bool) error {
	// The first pass counts every file by its stat size. Only when that does
	// not fit does the search read file heads to count binary files as 0.
	planned, rebuilds, err := plannedCacheBytes(ctx, plans, vectors, false)
	if err != nil || !rebuilds {
		// With no rebuild the cache cannot grow; the periodic GC handles a
		// budget that shrank.
		return err
	}
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return err
	}
	// The budget does not shrink when the corpora total grows. So a size
	// within the budget of an empty cache fits with no listing of the corpora.
	if budget, _, err := cacheBudget(cacheRoot, func() int64 { return 0 }); err != nil || planned <= budget {
		return err
	}
	budget, freeBound, err := cacheBudget(cacheRoot, func() int64 { return corporaBytes(cacheRoot) })
	if err != nil || planned <= budget {
		return err
	}
	if planned, _, err = plannedCacheBytes(ctx, plans, vectors, true); err != nil || planned <= budget {
		return err
	}
	var published int64
	for _, plan := range plans {
		published += publishedCorpusBytes(plan.cacheDir)
	}
	// A text search runs the same check with no vectors, and none when its
	// index is current.
	textFits := vectors && checkCacheBudget(ctx, plans, false) == nil
	return &cacheBudgetError{planned: planned, growth: max(0, planned-published), budget: budget, freeBound: freeBound, textFits: textFits}
}

// corporaBytes is the ledger total of every corpus plus the unit-store
// segments. A corpora tree that cannot be listed counts 0.
func corporaBytes(cacheRoot string) int64 {
	corpora, err := enumerateCorpusDirs(filepath.Join(cacheRoot, corporaDir))
	if err != nil {
		return 0
	}
	var total int64
	for _, size := range corpusSizes(corpora, true) {
		total += size
	}
	for _, unit := range semanticUnitsSegments(cacheRoot) {
		total += fileSize(unit.path)
	}
	return total
}

// fileSize returns the size of path, or 0 when it cannot be read.
func fileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

// corpusSizes reads the ledger of every corpus at the same time.
func corpusSizes(entries []corpusDirEntry, record bool) map[corpusID]int64 {
	values := make([]int64, len(entries))
	var group errgroup.Group
	group.SetLimit(runtime.GOMAXPROCS(0))
	for at, entry := range entries {
		group.Go(func() error {
			values[at] = corpusSize(entry.path, record)
			return nil
		})
	}
	_ = group.Wait()
	sizes := make(map[corpusID]int64, len(entries))
	for index, entry := range entries {
		sizes[entry.name] = values[index]
	}
	return sizes
}

// plannedCacheBytes estimates the cache that the search needs: for a node
// that the search rebuilds, its new generation; for any other node, its
// ledger size. rebuilds reports whether any node rebuilds. The kind of each
// node gives its estimate (nodeKind.estimate), with the rule of the build: a
// node rebuilds with no published state, a state other than the capture, or
// no generation for the vectors that the search wants. probe reads file heads
// so that binary files count 0; without it every file counts by its size. A
// vector build copies the rows of a node's live generation. So with a live
// generation it adds only the changed content, and with none it adds all of
// it.
//
// Each estimate reads the node's capture with no freshness check. The graph
// expansion gives every node of plans its capture (graphExpansion.plans), and
// checkCacheBudget runs before any route, so no route marked a capture stale.
func plannedCacheBytes(ctx context.Context, plans []node, vectors, probe bool) (int64, bool, error) {
	var planned atomic.Int64
	var rebuilds atomic.Bool
	var group errgroup.Group
	for _, plan := range plans {
		group.Go(func() error {
			bytes, rebuild, err := plan.behavior.estimate(ctx, plan, vectors, probe)
			if rebuild {
				rebuilds.Store(true)
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// The build reports the cause; the estimate counts the node's
				// published size only.
				slog.Debug("cache estimate failed", "root", plan.root, "error", err)
				bytes = publishedCorpusBytes(plan.cacheDir)
			}
			planned.Add(bytes)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return 0, false, err
	}
	return planned.Load(), rebuilds.Load(), nil
}

// estimate gives the folder's next generation from the walk capture: all
// content when nothing is published, else the changed files.
func (*folderNode) estimate(ctx context.Context, plan node, vectors, probe bool) (bytes int64, rebuild bool, err error) {
	published := readStateFile(plan.cacheDir)
	state, selected := plan.folder.stateHash, plan.folder.selected
	// decideFolderBuild uses the same rule. The manifest is read only when
	// the search wants vectors.
	present := vectors && semanticManifestPresent(plan.indexDir, plan.folderSemanticSource(state))
	vectorsMissing := vectorGenerationNeeded(vectors, present, false)
	if published == state && !vectorsMissing {
		return publishedCorpusBytes(plan.cacheDir), false, nil
	}
	if published == "" {
		content, err := folderContentBytes(ctx, selected, probe)
		return generationBytes(content, vectors), true, err
	}
	changed := changedFolderCandidates(plan.cacheDir, published, selected)
	delta, err := folderContentBytes(ctx, changed, probe)
	bytes = publishedCorpusBytes(plan.cacheDir) + int64(float64(delta)*zoektIndexRatio)
	switch {
	case vectorsMissing && semanticManifestPresent(plan.indexDir, plan.folderSemanticSource(published)):
		// The vectors hold the published state: the build adds the
		// changed files.
		bytes += semanticBytes(delta)
	case vectorsMissing:
		// The vectors hold an older state, or none: a text search moved
		// the index alone. Count all content, an overcount at most.
		all, allErr := folderContentBytes(ctx, selected, probe)
		bytes += semanticBytes(all)
		err = errors.Join(err, allErr)
	}
	return bytes, true, err
}

// estimate gives the Git node's next generation from the captured status.
// That is the commit's content when nothing is known, else the commit range
// and the dirty files.
func (g *gitNode) estimate(ctx context.Context, plan node, vectors, probe bool) (bytes int64, rebuild bool, err error) {
	published := readStateFile(plan.cacheDir)
	state := plan.gitState.state
	current := gitCorpusStateHash(g.paths, state)
	// The build's rules (decideGitBuild, vectorGenerationNeeded): a commit
	// builds its base generation, and dirty files build the overlay of their
	// work-tree bytes, also with no commit. A generation is present when its
	// manifest reads, the check of the warm path (joinedGenerationsMatch).
	// Stale is false: the estimate runs before the missing-blob recheck,
	// which writes its records under the build lock.
	wantBase := vectors && state.HeadSHA != gitNoHead
	wantOverlay := vectors && len(state.Files) > 0
	baseMissing := vectorGenerationNeeded(wantBase,
		wantBase && semanticManifestPresent(plan.indexDir, state.HeadSHA), false)
	overlayMissing := vectorGenerationNeeded(wantOverlay,
		wantOverlay && semanticManifestPresent(plan.indexDir, semanticOverlaySource(current)), false)
	if published == current && !baseMissing && !overlayMissing {
		return publishedCorpusBytes(plan.cacheDir), false, nil
	}
	dirty := dirtyContentBytes(g.paths.RepoDir, state.Files, probe)
	if overlayMissing {
		bytes = semanticBytes(dirty)
	}
	// A current text index builds vectors only, so the dirty files add
	// no text index bytes.
	dirtyText := dirty
	if published == current {
		dirtyText = 0
	}
	// committed counts the committed content that a generation adds: the
	// commit range from a commit to HEAD, or all of HEAD for "". A range from
	// HEAD adds nothing. The text index and the base vectors can need the
	// same range, so Git counts each range once.
	counted := map[string]int64{state.HeadSHA: 0}
	committed := func(from string) (int64, error) {
		if n, ok := counted[from]; ok {
			return n, nil
		}
		var n int64
		var err error
		if from == "" {
			n, err = committedContentBytes(ctx, g.paths, state.HeadSHA, probe)
		} else {
			n, err = committedDeltaBytes(ctx, g.paths, from, state.HeadSHA, probe)
		}
		if err == nil {
			counted[from] = n
		}
		return n, err
	}
	// The text index adds all of HEAD when nothing is known, else the commit
	// range from the commit that it holds.
	committedState, known := readCommittedGitState(plan.cacheDir)
	cold := published == "" || !known
	textFrom := string(committedState.head)
	if cold {
		textFrom = ""
	}
	text, err := committed(textFrom)
	if baseMissing {
		// The build reuses the rows of the live base generation, so it embeds
		// the commit range from that generation's commit. A text search moves
		// the text index alone, so that commit can be older than the text
		// index's. With no live base, or a commit that Git no longer has, it
		// counts all of HEAD: an overcount at most.
		source, _ := liveBaseSource(plan.indexDir)
		content, vectorErr := committed(source)
		if vectorErr != nil && source != "" {
			content, vectorErr = committed("")
		}
		bytes += semanticBytes(content)
		err = errors.Join(err, vectorErr)
	}
	if cold {
		return bytes + int64(float64(text+dirty)*zoektIndexRatio), true, err
	}
	bytes += publishedCorpusBytes(plan.cacheDir) + int64(float64(text+dirtyText)*zoektIndexRatio)
	return bytes, true, err
}

// liveBaseSource gives the commit of a base generation in indexDir that the
// current model can reuse.
func liveBaseSource(indexDir string) (string, bool) {
	for _, dir := range semanticGenerationDirsIn(indexDir, "") {
		manifest, err := readSemanticManifestUnchecked(dir)
		if err != nil || !semanticSourceHasGraph(manifest.Source) || validateSemanticManifest(manifest, manifest.Source) != nil {
			continue
		}
		return manifest.Source, true
	}
	return "", false
}

// semanticManifestPresent reports a published generation for source in
// indexDir: a vector build binds it with no model work.
func semanticManifestPresent(indexDir, source string) bool {
	_, err := readSemanticManifest(semanticGenerationDir(indexDir, source), source)
	return err == nil
}

// publishedCorpusBytes is the ledger size of the corpus in dir, or 0 when dir
// does not exist.
func publishedCorpusBytes(dir string) int64 {
	if _, err := os.Stat(dir); err != nil {
		return 0
	}
	return corpusSize(dir, true)
}

// semanticBytes estimates the semantic generation of content bytes.
func semanticBytes(content int64) int64 {
	return int64(float64(content) * semanticBytesPerContentByte)
}

// generationBytes estimates a new generation of content bytes: the Zoekt
// index, plus the semantic generation when the search wants vectors.
func generationBytes(content int64, vectors bool) int64 {
	ratio := zoektIndexRatio
	if vectors {
		ratio += semanticBytesPerContentByte
	}
	return int64(float64(content) * ratio)
}

// changedFolderCandidates returns the files whose stat differs from the
// published manifest: the delta of the new generation. With no readable
// manifest every file counts. Both lists are sorted by name, as in
// folderManifestDiff, so one merge pass finds the entry of each file.
func changedFolderCandidates(cacheDir, published string, selected []folderCandidate) []folderCandidate {
	manifest, ok := readFolderManifest(cacheDir, published)
	if !ok {
		return selected
	}
	files := manifest.Files
	changed := make([]folderCandidate, 0, 16)
	for _, candidate := range selected {
		for len(files) > 0 && files[0].Name < candidate.name {
			files = files[1:]
		}
		if len(files) == 0 || files[0].Name != candidate.name || !folderManifestFileMatchesCandidate(files[0], candidate) {
			changed = append(changed, candidate)
		}
	}
	return changed
}

// folderContentBytes sums the content that the index holds for candidates.
// With probe it reads the head of every file, as the index does, so a binary
// file (indexed by name only) counts 0. Without probe, every file whose
// content the index reads (indexedBytes) counts by its stat size.
func folderContentBytes(ctx context.Context, candidates []folderCandidate, probe bool) (int64, error) {
	if !probe {
		var total int64
		for _, candidate := range candidates {
			total += candidate.indexedBytes()
		}
		return total, nil
	}
	jobs := make(chan folderCandidate)
	var total atomic.Int64
	var wg sync.WaitGroup
	for range fileReadWorkerCount(indexParallelism(), len(candidates)) {
		wg.Go(func() {
			for candidate := range jobs {
				if candidate.indexedBytes() > 0 && !fileIsBinary(candidate.path) {
					total.Add(candidate.size)
				}
			}
		})
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		jobs <- candidate
	}
	close(jobs)
	wg.Wait()
	return total.Load(), ctx.Err()
}

// fileIsBinary applies the index's binary probe: a NUL byte in the first
// binaryProbeBytes. An unreadable file, or one that is no longer regular,
// counts as binary: the index cannot read it either.
func fileIsBinary(path string) bool {
	file, err := openWorkTreeFile(path)
	if err != nil {
		return true
	}
	defer func() { _ = file.Close() }()
	var head [binaryProbeBytes]byte
	n, _ := io.ReadFull(file, head[:])
	return bytes.IndexByte(head[:n], 0) >= 0
}

// dirtyContentBytes sums the dirty files that fit a document; with probe a
// binary file counts 0. A cloud-only file counts 0 with no open: the build
// keeps its name only.
func dirtyContentBytes(repoDir string, files []string, probe bool) int64 {
	var total int64
	for _, name := range files {
		path := filepath.Join(repoDir, filepath.FromSlash(name))
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err == nil &&
			st.Mode&unix.S_IFMT == unix.S_IFREG && !fileIsDataless(path, &st) &&
			documentContentFits(st.Size) && (!probe || !fileIsBinary(path)) {
			total += st.Size
		}
	}
	return total
}

// committedContentBytes sums the committed blobs that fit a document, from one
// git ls-tree. With probe a binary blob counts 0 (committedBlobBytes).
func committedContentBytes(ctx context.Context, paths gitPaths, head string, probe bool) (int64, error) {
	if normalizeCommittedTreeish(head) == gitNoHead {
		return 0, nil
	}
	cmd := newGitCommand(ctx, paths.RepoDir, "ls-tree", "-r", "-l", "-z", "--full-tree", head)
	out, err := gitOutput(ctx, cmd, "git ls-tree")
	if err != nil {
		return 0, err
	}
	var blobs []gitSizedBlob
	for record := range strings.SplitSeq(string(out), "\x00") {
		// "<mode> blob <oid> <size>\t<path>"
		meta, _, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		if size, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
			blobs = append(blobs, gitSizedBlob{oid: fields[2], size: size})
		}
	}
	return committedBlobBytes(ctx, paths, blobs, probe)
}

// committedDeltaBytes sums the regular-file blobs that a commit range adds or
// changes. One git diff-tree lists them, and committedBlobBytes reads their
// sizes.
func committedDeltaBytes(ctx context.Context, paths gitPaths, from, to string, probe bool) (int64, error) {
	// A move to no commit (an orphan branch) has no tree and adds no
	// content. The from side is always a commit: the build records a
	// committed state only for a commit, and only a commit has a base
	// generation.
	if normalizeCommittedTreeish(to) == gitNoHead {
		return 0, nil
	}
	cmd := newGitCommand(ctx, paths.RepoDir, "diff-tree", "-r", "-z", "--no-renames", "--no-commit-id", from, to)
	out, err := gitOutput(ctx, cmd, "git diff-tree")
	if err != nil {
		return 0, err
	}
	var blobs []gitSizedBlob
	for record := range strings.SplitSeq(string(out), "\x00") {
		// ":<old mode> <new mode> <old oid> <new oid> <status>", then the path.
		fields := strings.Fields(strings.TrimPrefix(record, ":"))
		if !strings.HasPrefix(record, ":") || len(fields) != 5 || strings.HasPrefix(fields[4], "D") ||
			(fields[1] != "100644" && fields[1] != "100755") {
			continue
		}
		blobs = append(blobs, gitSizedBlob{oid: fields[3], size: -1})
	}
	return committedBlobBytes(ctx, paths, blobs, probe)
}

// gitSizedBlob is one blob to count: its object ID and its size.
type gitSizedBlob struct {
	oid string
	// size is -1 when the caller does not know it.
	size int64
}

// committedBlobBytes sums the blobs that fit a document. With probe, or with
// an unknown size, it streams the blobs through one git cat-file --batch. A
// streamed blob with a NUL byte in its first binaryProbeBytes is indexed by
// name only and counts 0. This is the test of the index, and Git attributes do
// not change it. Without probe, a known size counts as it is.
func committedBlobBytes(ctx context.Context, paths gitPaths, blobs []gitSizedBlob, probe bool) (int64, error) {
	var total int64
	var stream []gitSizedBlob
	for _, blob := range blobs {
		if probe || blob.size < 0 {
			stream = append(stream, blob)
		} else if documentContentFits(blob.size) {
			total += blob.size
		}
	}
	if len(stream) == 0 {
		return total, nil
	}
	cmd := newGitCommand(ctx, paths.RepoDir, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, gitCommandError(ctx, "git cat-file", err, &stderr)
	}
	go func() {
		buffered := bufio.NewWriter(stdin)
		for _, blob := range stream {
			_, _ = buffered.WriteString(blob.oid + "\n")
		}
		_ = buffered.Flush()
		_ = stdin.Close()
	}()
	reader := bufio.NewReaderSize(stdout, 1<<20)
	head := make([]byte, binaryProbeBytes)
	var readErr error
	for range stream {
		line, err := reader.ReadString('\n')
		if err != nil {
			readErr = err
			break
		}
		// "<oid> <type> <size>" or "<oid> missing"
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			readErr = err
			break
		}
		n, err := io.ReadFull(reader, head[:min(size, int64(len(head)))])
		if err == nil {
			_, err = io.CopyN(io.Discard, reader, size-int64(n)+1)
		}
		if err != nil {
			readErr = err
			break
		}
		if fields[1] == "blob" && documentContentFits(size) && bytes.IndexByte(head[:n], 0) < 0 {
			total += size
		}
	}
	_, _ = io.Copy(io.Discard, reader)
	if err := cmd.Wait(); err != nil {
		return 0, gitCommandError(ctx, "git cat-file", err, &stderr)
	}
	return total, readErr
}

// enforceCacheBudget runs after a search. It records the ledger size of each
// corpus that the search used and whose ledger is stale. Then it evicts the
// oldest unit-store segments and the least recently used corpora that the
// search did not use, until the cache fits (planCorpusEviction). It has no
// time limit: the budget is the only disk limit. It does nothing when the
// search published nothing, or when the cache is on a network or FUSE file
// system (as gc).
func enforceCacheBudget(plans []node) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil || isOnNFS(cacheRoot) {
		return
	}
	used := make(map[corpusID]bool, len(plans))
	published := false
	for _, plan := range plans {
		used[plan.id] = true
		if ledgerStale(plan.cacheDir) {
			recordCorpusSize(plan.cacheDir)
			published = true
		}
	}
	if !published {
		// A search that published nothing did not grow the cache.
		return
	}
	corporaPath := filepath.Join(cacheRoot, corporaDir)
	entries, err := enumerateCorpusDirs(corporaPath)
	if err != nil {
		return
	}
	evict, err := planCorpusEviction(cacheRoot, entries, false, used, true)
	if err != nil || len(evict) == 0 {
		return
	}
	trashDir := filepath.Join(corporaPath, gcTrashDir)
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return
	}
	for _, entry := range entries {
		evictIfPlanned(entry, trashDir, evict)
	}
	evictPlannedUnits(cacheRoot, evict)
	drainTrash(trashDir)
}

// evictPlannedUnits removes the unit-store segments of an eviction plan.
func evictPlannedUnits(cacheRoot string, evict map[corpusID]bool) {
	for _, unit := range semanticUnitsSegments(cacheRoot) {
		if evict[unit.name] {
			_ = os.Remove(unit.path)
		}
	}
}

// ledgerStale reports a corpus that published lexical shards or bound
// vectors after its ledger entry was written, or that has no entry. When it
// reports false, a warm search measures nothing.
func ledgerStale(dir string) bool {
	ledger, ledgerErr := os.Stat(filepath.Join(dir, corpusSizeFile))
	published := false
	// A build that kept dirty files by name writes no state but publishes
	// the committed family and a pending manifest.
	for _, name := range []string{stateFile, joinedGenerationFile, committedGitStateFile, uncommittedManifestFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		published = true
		// An equal time counts as stale: a file system with whole-second
		// times can hold a ledger and a publish in one second.
		if ledgerErr == nil && !ledger.ModTime().After(info.ModTime()) {
			return true
		}
	}
	return published && ledgerErr != nil
}

// processStarted is when this command started. A corpus used since then
// belongs to this command or to one that runs beside it: eviction keeps it.
// The 2-second step covers file systems that store whole or even seconds.
var processStarted = time.Now().Truncate(2 * time.Second)

// usedByARunningCommand reports a corpus whose last use falls between the
// start of this command and now, with the same 2-second step. A .used time
// further in the future proves nothing.
func usedByARunningCommand(usedAt time.Time) bool {
	return !usedAt.Before(processStarted) && !usedAt.After(time.Now().Add(2*time.Second))
}

// planCorpusEviction returns the corpora and the unit-store segments to
// evict. With all, it returns all of them. Else it takes the oldest unit-store
// segments first, then the least recently used corpora, until the total size
// fits the budget. In that mode, it skips a corpus that keep holds or that a
// command used since this one started. Corpus directories of other layout
// versions reach the LRU end first unless an older binary still uses them.
// record writes measured sizes into the ledger; a dry run passes false.
func planCorpusEviction(
	cacheRoot string,
	entries []corpusDirEntry,
	all bool,
	keep map[corpusID]bool,
	record bool,
) (map[corpusID]bool, error) {
	evict := make(map[corpusID]bool)
	units := semanticUnitsSegments(cacheRoot)
	if len(entries) == 0 && len(units) == 0 {
		return evict, nil
	}
	if all {
		for _, entry := range append(entries, units...) {
			evict[entry.name] = true
		}
		return evict, nil
	}
	sizes := corpusSizes(entries, record)
	var total int64
	for _, size := range sizes {
		total += size
	}
	unitSizes := make(map[corpusID]int64, len(units))
	for _, unit := range units {
		unitSizes[unit.name] = fileSize(unit.path)
		total += unitSizes[unit.name]
	}
	budget, _, err := cacheBudget(cacheRoot, func() int64 { return total })
	if err != nil {
		return nil, err
	}
	// Reachability first: no live generation references a unit-store segment,
	// so the segments go before any corpus, oldest first.
	slices.SortStableFunc(units, func(a, b corpusDirEntry) int { return a.usedAt.Compare(b.usedAt) })
	for _, unit := range units {
		if total <= budget {
			break
		}
		evict[unit.name] = true
		total -= unitSizes[unit.name]
	}
	lru := slices.Clone(entries)
	slices.SortStableFunc(lru, func(a, b corpusDirEntry) int { return a.usedAt.Compare(b.usedAt) })
	for _, entry := range lru {
		if total <= budget {
			break
		}
		if keep[entry.name] || usedByARunningCommand(entry.usedAt) {
			continue
		}
		evict[entry.name] = true
		total -= sizes[entry.name]
	}
	return evict, nil
}

// checkCacheRoot makes the corpora directory and checks that Seek can write
// it. Every node keeps its index there. So a cache root that cannot be used is
// an environment error with exit 2, not one warning per node.
func checkCacheRoot() error {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(cacheRoot, corporaDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}
	if err := unix.Access(dir, unix.W_OK|unix.X_OK); err != nil {
		return fmt.Errorf("cache directory %s: %w", dir, err)
	}
	return nil
}
