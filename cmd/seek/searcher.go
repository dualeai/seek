package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"

	grafanaregexp "github.com/grafana/regexp"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/sync/errgroup"
	"log/slog"
	"path/filepath"
)

// searchContextLines is the number of context lines included before and after
// each match.
const searchContextLines = 3

// searchOpts are the zoekt search options used for every search. Defined
// at package level to avoid per-search heap allocation.
var searchOpts = zoekt.SearchOptions{
	// MaxDocDisplayCount is intentionally left at 0 (unlimited). Display
	// limiting is handled by seek's --limit/-n flag during formatting,
	// which applies after dedup and BM25 sort. A zoekt-level display cap
	// would silently drop low-ranked files before seek or downstream
	// pipes (| grep, | head) see them, causing false negatives.
	//
	// Seek searches each shard with Zoekt's per-shard searcher, which reads
	// only ShardMaxMatchCount. It stays at math.MaxInt so that every match in
	// a shard counts: 0 would become Zoekt's default of 100,000. SetDefaults
	// then sets the total match budget to ten times this value, which
	// overflows, but only the search package's aggregator reads that budget.
	ShardMaxMatchCount: math.MaxInt,
	NumContextLines:    searchContextLines,
	UseBM25Scoring:     true,
}

type searchConfig struct {
	opts *zoekt.SearchOptions
	// bm25 marks a strict search whose hits the command rescores with one
	// global average file length.
	bm25              bool
	afterOnly         bool
	contextMatchLimit int
	contextFileLimit  int
	resultFileLimit   int
	contextGitCorpus  bool
	contextDirtyFiles dirtyFileSet
}

// querySyntaxError identifies a query that Zoekt could not parse. The original
// query and wrapped cause let the CLI explain the input and retain Zoekt's
// diagnostic in verbose output.
type querySyntaxError struct {
	query string
	cause error
}

func (e *querySyntaxError) Error() string {
	return fmt.Sprintf("parse query %q: %v", e.query, e.cause)
}

func (e *querySyntaxError) Unwrap() error {
	return e.cause
}

func defaultSearchConfig() searchConfig {
	return searchConfig{opts: &searchOpts}
}

// explicitSearchConfig limits the context count to zoektShardFormatBytes: no
// document holds more lines than a shard holds bytes, so the cap shows the
// same lines. Zoekt adds the line number to this count, and a value near
// math.MaxInt would overflow and drop every line after the match.
func explicitSearchConfig(contextLines int, afterOnly bool) searchConfig {
	opts := searchOpts
	opts.NumContextLines = min(contextLines, zoektShardFormatBytes)
	return searchConfig{opts: &opts, afterOnly: afterOnly}
}

// openShard opens a single .zoekt shard file and returns a Searcher.
func openShard(path string) (zoekt.Searcher, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	iFile, err := index.NewIndexFile(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	s, err := index.NewSearcher(iFile)
	if err != nil {
		iFile.Close()
		return nil, err
	}
	return s, nil
}

// loadShards opens all .zoekt entries in indexDir and returns individual
// zoekt.Searcher instances. A one-shot CLI search does not need directory
// watchers or ready-channel synchronization. Multiple shards load in parallel.
func loadShards(indexDir string) ([]zoekt.Searcher, error) {
	paths, err := searchableShardPaths(indexDir)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		// An index dir that holds no shard at all is damage the caller can
		// repair, not a query failure to report.
		return nil, fmt.Errorf("%w: no index shards in %s", errShardUnloadable, indexDir)
	}
	return loadShardPaths(indexDir, paths)
}

func loadShardsOptional(indexDir string) ([]zoekt.Searcher, error) {
	paths, err := searchableShardPaths(indexDir)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	return loadShardPaths(indexDir, paths)
}

// searchableShardPaths lists the non-directory .zoekt entries that a search
// will try to open.
//
// The caller holds the shared publish lock, so this scan cannot interleave
// with a successful swap. This function needs names only and does not load
// size metadata.
func searchableShardPaths(indexDir string) ([]string, error) {
	return familyShardNames(indexDir)
}

func loadShardPaths(indexDir string, paths []string) ([]zoekt.Searcher, error) {
	// Single shard — skip goroutine overhead.
	if len(paths) == 1 {
		s, err := openShard(paths[0])
		if err != nil {
			// Mark a single-shard load failure as index damage too.
			return nil, fmt.Errorf("%w: load shard %s: %w", errShardUnloadable, paths[0], err)
		}
		return []zoekt.Searcher{s}, nil
	}

	type shardLoadResult struct {
		searcher zoekt.Searcher
		path     string
		err      error
	}

	// Multiple shards — load in parallel to overlap mmap + metadata parsing.
	// The searchers keep the path order, which the BM25 rescore reads.
	results := make([]shardLoadResult, len(paths))
	var wg sync.WaitGroup
	for index, p := range paths {
		wg.Add(1)
		go func(index int, path string) {
			defer wg.Done()
			s, err := openShard(path)
			if err != nil {
				results[index] = shardLoadResult{path: path, err: err}
				return
			}
			results[index] = shardLoadResult{searcher: s, path: path}
		}(index, p)
	}
	wg.Wait()

	searchers := make([]zoekt.Searcher, 0, len(paths))
	var loadErr error
	for _, result := range results {
		if result.err != nil {
			if loadErr == nil {
				loadErr = fmt.Errorf("load shard %s: %w", result.path, result.err)
			}
			continue
		}
		searchers = append(searchers, result.searcher)
	}
	if loadErr != nil {
		for _, s := range searchers {
			s.Close()
		}
		return nil, fmt.Errorf("%w: %w", errShardUnloadable, loadErr)
	}
	if len(searchers) == 0 {
		return nil, fmt.Errorf("%w: no loadable shards in %s", errShardUnloadable, indexDir)
	}
	return searchers, nil
}

// errShardUnloadable marks a shard that exists but cannot be opened: truncated,
// unreadable, a directory, or a dangling symlink. The caller treats this as
// index damage, invalidates the family manifest, and retries the search.
//
// This repair covers the committed family: dropping the manifest makes
// needCommitted fire and that family be rebuilt. It does not cover the
// uncommitted family, whose rebuild is driven by the working tree's own state
// hash and shard contiguity, none of which notices a present, contiguous,
// corrupt shard. Such an uncommitted shard can fail until the working tree
// changes.
var errShardUnloadable = errors.New("index damage")

func parseSearchQueryForms(pattern string) (query.Q, query.Q, error) {
	raw, strict, _, err := parseQueryHalves(pattern)
	return raw, strict, err
}

// parseQueryHalves parses pattern and expands each bare word into a file-name
// and a content atom, as Zoekt does. halves holds the file-name atoms that the
// expansion made: they keep Zoekt's meaning, so only an explicit file: atom
// reads the header.
func parseQueryHalves(pattern string) (raw, strict query.Q, halves map[query.Q]struct{}, err error) {
	raw, err = query.Parse(pattern)
	if err != nil {
		var ok bool
		raw, ok = parsePlainDescriptionFallback(pattern)
		if !ok {
			return nil, nil, nil, &querySyntaxError{query: pattern, cause: err}
		}
	}
	halves = map[query.Q]struct{}{}
	expanded := query.Map(raw, func(q query.Q) query.Q {
		out := query.ExpandFileContent(q)
		if or, ok := out.(*query.Or); ok && out != q {
			for _, child := range or.Children {
				if fileNameAtom(child) {
					halves[child] = struct{}{}
				}
			}
		}
		return out
	})
	strict = query.Simplify(expanded)
	if err := compileRegexpAtoms(strict); err != nil {
		return nil, nil, nil, &querySyntaxError{query: pattern, cause: err}
	}
	return raw, strict, halves, nil
}

// compileRegexpAtoms compiles each regexp atom of q as Zoekt does
// (index/matchtree.go regexpPattern and MustCompile): a regexp that Go's
// parser rejects after Zoekt's simplification, such as a{1,501}, is a query
// error, not a panic in the search.
func compileRegexpAtoms(q query.Q) error {
	var err error
	check := func(atom *query.Regexp) {
		if err != nil || atom == nil {
			return
		}
		pattern := atom.RegexpString()
		if !atom.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		if _, compileErr := grafanaregexp.Compile(pattern); compileErr != nil {
			// Go's message ends with the whole expanded expression.
			message, _, _ := strings.Cut(compileErr.Error(), ": `")
			err = errors.New(message)
		}
	}
	query.VisitAtoms(q, func(atom query.Q) {
		switch typed := atom.(type) {
		case *query.Regexp:
			check(typed)
		case *query.Symbol:
			if expr, ok := typed.Expr.(*query.Regexp); ok {
				check(expr)
			}
		}
	})
	return err
}

// parsePlainDescriptionFallback keeps programming names such as C++ usable.
// Zoekt parses bare terms as regular expressions, so a valid programming name
// can otherwise fail the whole query. Only after Zoekt rejects the query, it
// quotes each programming name (C++, X[], X(), with :: qualifiers) and parses
// the rewritten query again, so filters keep their meaning. A name
// such as at() is also a valid expression, but next to C++ it means the
// literal name. The quoted term parses to the literal name, which the model
// reads.
func parsePlainDescriptionFallback(pattern string) (query.Q, bool) {
	var rewritten strings.Builder
	changed := false
	for at := 0; at < len(pattern); {
		// Zoekt splits words on space and tab; keep every separator as typed.
		end := at
		for end < len(pattern) && pattern[end] != ' ' && pattern[end] != '\t' {
			end++
		}
		term := pattern[at:end]
		if plainProgrammingName(term) {
			// QuoteMeta leaves ':', which would make "content::X()" a filter.
			term = strings.ReplaceAll(regexp.QuoteMeta(term), ":", `\:`)
			changed = true
		}
		rewritten.WriteString(term)
		if end < len(pattern) {
			rewritten.WriteByte(pattern[end])
			end++
		}
		at = end
	}
	if !changed {
		return nil, false
	}
	parsed, err := query.Parse(rewritten.String())
	return parsed, err == nil
}

func plainProgrammingName(term string) bool {
	for _, suffix := range []string{"++", "[]", "()"} {
		if strings.HasSuffix(term, suffix) {
			return plainQualifiedIdentifier(strings.TrimSuffix(term, suffix))
		}
	}
	return false
}

func plainQualifiedIdentifier(name string) bool {
	for _, part := range strings.Split(name, "::") {
		if part == "" {
			return false
		}
		for index, character := range part {
			if character == '_' || unicode.IsLetter(character) ||
				(index > 0 && unicode.IsDigit(character)) {
				continue
			}
			return false
		}
	}
	return true
}

func executeParsedSearchScopedDirs(
	ctx context.Context,
	indexDirs []string,
	userQ query.Q,
	scope query.Q,
	config searchConfig,
) ([]zoekt.FileMatch, error) {
	q := userQ
	if scope != nil {
		// Zoekt gathers no match under a file-name type, so the scope filters
		// the files and adds no file-name term to their BM25 score: a file in
		// the scope scores as in an unscoped search. Unlike a double
		// negation, it keeps Zoekt's shard and file skips.
		q = query.NewAnd(q, &query.Type{Type: query.TypeFileName, Child: scope})
		q = query.Simplify(q)
	}

	var searchers []zoekt.Searcher
	var shardPaths []string
	for _, indexDir := range indexDirs {
		paths, err := searchableShardPaths(indexDir)
		var loaded []zoekt.Searcher
		if err == nil && len(paths) > 0 {
			loaded, err = loadShardPaths(indexDir, paths)
		}
		if err != nil {
			for _, s := range searchers {
				s.Close()
			}
			return nil, fmt.Errorf("load index: %w", err)
		}
		searchers = append(searchers, loaded...)
		shardPaths = append(shardPaths, paths...)
	}
	if len(searchers) == 0 {
		return nil, fmt.Errorf("%w: no loadable shards", errShardUnloadable)
	}
	parts, err := bm25ShardParts(ctx, searchers, shardPaths, q, &config)
	if err != nil {
		for _, s := range searchers {
			s.Close()
		}
		return nil, err
	}
	return searchLoadedShards(ctx, searchers, q, config, parts)
}

// bm25ShardParts prepares the rescore of a strict search: it records
// each shard's counts for the command's average, asks Zoekt for its score
// debug line, and returns the function that rebuilds each hit's parts. Other
// searches get nil.
func bm25ShardParts(ctx context.Context, searchers []zoekt.Searcher, paths []string, q query.Q, config *searchConfig) (func(int, []zoekt.FileMatch), error) {
	command, ok := ctx.Value(bm25CommandKey{}).(*bm25Command)
	if !ok || !config.bm25 || config.opts == nil || !config.opts.UseBM25Scoring || len(paths) != len(searchers) {
		return nil, nil
	}
	if command.nodes == 1 && len(paths) == 1 {
		// One node of one shard: Zoekt's average is the command's, so
		// Zoekt's scores are final.
		return nil, nil
	}
	records := map[string]*shardStatsRecord{}
	shards := make([]shardLengthStats, len(paths))
	var group errgroup.Group
	group.SetLimit(runtime.GOMAXPROCS(0))
	for index, path := range paths {
		indexDir := filepath.Dir(path)
		record, ok := records[indexDir]
		if !ok {
			record = readShardStatsRecord(indexDir)
			records[indexDir] = record
		}
		group.Go(func() error {
			stats, err := record.stats(ctx, path, searchers[index])
			if err != nil {
				return err
			}
			shards[index] = stats
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// A shard whose counts Seek cannot read keeps Zoekt's scores, and
		// the command's average gets none of this search's counts.
		slog.Debug("BM25 shard counts unavailable", "error", err)
		return nil, nil
	}
	// The counts replace those of an earlier search of the same index
	// directories, such as a repaired attempt.
	command.set(paths, shards)
	averages := make([]float64, len(paths))
	for index, stats := range shards {
		averages[index] = stats.average()
	}
	for indexDir, record := range records {
		var live []string
		for _, path := range paths {
			if filepath.Dir(path) == indexDir {
				live = append(live, path)
			}
		}
		record.save(live)
	}
	opts := *config.opts
	opts.DebugScore = true
	config.opts = &opts
	// The clone keeps context lines for the best files by score. With
	// one node these shards are all of the command's, so each hit gets its
	// final score here (with one shard Zoekt's score is final). Otherwise the
	// command rescore can reorder the files after the clone, and every strict
	// file keeps its context.
	global, final := 0.0, false
	if command.nodes == 1 {
		global, final = shardsAverage(shards)
		command.final.Store(final)
	} else {
		config.contextFileLimit = 0
	}
	bq := compileBM25Query(q)
	rebuild := func(file *zoekt.FileMatch, average float64) {
		if !bm25Parts(file, bq, average) {
			command.kept.Add(1)
			return
		}
		command.rebuilt.Add(1)
		if !final {
			return
		}
		if score, ok := bm25Rescore(*file, global); ok {
			file.Score = score
		}
	}
	return func(index int, files []zoekt.FileMatch) {
		// One hit's rebuild reads only that hit: the hits of one shard
		// spread over the host's CPUs, so the shard with the most ambiguous
		// lines does not hold the search.
		workers := min(runtime.GOMAXPROCS(0), max(1, len(files)/bm25FilesPerWorker))
		var group sync.WaitGroup
		for worker := range workers {
			group.Go(func() {
				for file := worker; file < len(files); file += workers {
					rebuild(&files[file], averages[index])
				}
			})
		}
		group.Wait()
	}, nil
}

// bm25FilesPerWorker is the share of a shard's hits below which one more
// goroutine costs more than it saves: 4 halves the rebuild phase on
// Kubernetes, whose shards hold few hits each.
const bm25FilesPerWorker = 4

// searchLoadedShards closes each searcher after all search workers return.
//
// NOTE(zoekt): a query with a `query.Type` other than type:file panics inside
// index.Searcher.Search (index/matchtree.go:1298, log.Panicf("type %T")).
// Seek leaves the crash until Zoekt fixes it.
// TODO: add the upstream Zoekt issue link once it is filed.
func searchLoadedShards(
	ctx context.Context,
	searchers []zoekt.Searcher,
	q query.Q,
	config searchConfig,
	parts func(int, []zoekt.FileMatch),
) ([]zoekt.FileMatch, error) {
	defer func() {
		for _, s := range searchers {
			s.Close()
		}
	}()

	// Fast path: single shard avoids intermediate allFiles slice.
	if len(searchers) == 1 {
		result, err := searchers[0].Search(ctx, q, config.opts)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		if parts != nil {
			parts(0, result.Files)
		}
		roundScores(result.Files)
		return cloneFileMatches(result.Files, config), nil
	}

	// Multiple shards: fan out across goroutines bounded by NumCPU.
	// Sequential iteration cost grows linearly with shard count, which
	// the windowed indexer's rotation can drive into the hundreds for
	// multi-GiB corpora. Parallel dispatch turns the per-shard query
	// cost from sum-of-shards into max-of-shards (up to GOMAXPROCS).
	parallelism := runtime.GOMAXPROCS(0)
	if parallelism < 1 {
		parallelism = 1
	}
	if parallelism > len(searchers) {
		parallelism = len(searchers)
	}
	type shardResult struct {
		files []zoekt.FileMatch
		err   error
	}
	results := make([]shardResult, len(searchers))
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, s := range searchers {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, s zoekt.Searcher) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := s.Search(ctx, q, config.opts)
			if err != nil {
				results[i] = shardResult{err: err}
				return
			}
			// The parts read every match, before the display limits drop any.
			if parts != nil {
				parts(i, r.Files)
			}
			roundScores(r.Files)
			results[i] = shardResult{files: r.Files}
		}(i, s)
	}
	wg.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	var allFiles []zoekt.FileMatch
	for _, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("search: %w", r.err)
		}
		allFiles = append(allFiles, r.files...)
	}
	return cloneFileMatches(allFiles, config), nil
}

// roundScores rounds each score once, after the parts read the exact value:
// Zoekt sums a score over a Go map, so equal true scores differ in their last
// bits from run to run, and rounded ties sort by name.
func roundScores(files []zoekt.FileMatch) {
	for index := range files {
		files[index].Score = math.Round(files[index].Score*1e9) / 1e9
	}
}

func cloneFileMatches(files []zoekt.FileMatch, config searchConfig) []zoekt.FileMatch {
	if len(files) == 0 {
		return nil
	}
	contextFiles := displayedContextFiles(files, config)
	var resultFiles map[int]struct{}
	if config.resultFileLimit > 0 {
		if config.resultFileLimit == config.contextFileLimit {
			resultFiles = contextFiles
		} else {
			resultFiles = selectedFileIndexes(files, config, config.resultFileLimit)
		}
	}
	capacity := len(files)
	if resultFiles != nil {
		capacity = len(resultFiles)
	}
	out := make([]zoekt.FileMatch, 0, capacity)
	for i := range files {
		if resultFiles != nil {
			if _, keep := resultFiles[i]; !keep {
				continue
			}
		}
		_, keepContext := contextFiles[i]
		out = append(out, cloneFileMatch(
			files[i],
			config.contextMatchLimit,
			config.afterOnly,
			contextFiles == nil || keepContext,
		))
	}
	return out
}

// displayedContextFiles returns the files from this corpus that can still be
// shown after the global file limit. Deduplication and dirty-file suppression
// are corpus-local, so a global top N file must be in its corpus's top N. A
// nil map means that all context must be copied.
func displayedContextFiles(files []zoekt.FileMatch, config searchConfig) map[int]struct{} {
	return selectedFileIndexes(files, config, config.contextFileLimit)
}

// selectedFileIndexes returns the best valid file indexes for one corpus after
// deduplication and dirty-file suppression. A nil map means that all input
// files remain selected.
func selectedFileIndexes(
	files []zoekt.FileMatch,
	config searchConfig,
	limit int,
) map[int]struct{} {
	if limit <= 0 || len(files) <= limit {
		return nil
	}

	byPath := make(map[string]dedupEntry, len(files))
	for i, file := range files {
		isUncommitted := config.contextGitCorpus && file.Repository == repoUncommitted
		chooseDedupEntry(byPath, file.FileName, i, isUncommitted)
	}

	candidates := make([]int, 0, len(byPath))
	for _, entry := range byPath {
		file := files[entry.idx]
		if !entry.uncommitted && config.contextDirtyFiles.contains(file.FileName) {
			continue
		}
		candidates = append(candidates, entry.idx)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := files[candidates[i]]
		right := files[candidates[j]]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		return left.FileName < right.FileName
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	selected := make(map[int]struct{}, len(candidates))
	for _, index := range candidates {
		selected[index] = struct{}{}
	}
	return selected
}

func cloneFileMatch(
	in zoekt.FileMatch,
	contextMatchLimit int,
	afterOnly bool,
	keepFileContext bool,
) zoekt.FileMatch {
	out := in
	out.Branches = cloneStringSlice(in.Branches)
	out.Content = cloneBytes(in.Content)
	out.Checksum = cloneBytes(in.Checksum)

	if len(in.LineMatches) > 0 {
		var contextLines map[int]struct{}
		if keepFileContext {
			contextLines = displayedContextLines(in.LineMatches, contextMatchLimit)
		}
		out.LineMatches = make([]zoekt.LineMatch, len(in.LineMatches))
		for i := range in.LineMatches {
			_, keepContext := contextLines[in.LineMatches[i].LineNumber]
			out.LineMatches[i] = cloneLineMatch(
				in.LineMatches[i],
				keepFileContext && (contextLines == nil || keepContext),
				afterOnly,
			)
		}
	}

	if len(in.ChunkMatches) > 0 {
		out.ChunkMatches = make([]zoekt.ChunkMatch, len(in.ChunkMatches))
		for i := range in.ChunkMatches {
			out.ChunkMatches[i] = cloneChunkMatch(in.ChunkMatches[i])
		}
	}

	return out
}

// displayedContextLines returns the source lines whose context can reach the
// formatter. A nil map means that all context must be copied. Search results
// refer to shard memory, so every displayed byte must be copied before close.
func displayedContextLines(matches []zoekt.LineMatch, limit int) map[int]struct{} {
	if limit <= 0 || len(matches) <= limit {
		return nil
	}
	displayed, _ := normalizeLineMatches(matches, limit)
	lines := make(map[int]struct{}, len(displayed))
	for _, match := range displayed {
		lines[match.LineNumber] = struct{}{}
	}
	return lines
}

func cloneLineMatch(in zoekt.LineMatch, keepContext, afterOnly bool) zoekt.LineMatch {
	out := in
	out.Line = cloneBytes(in.Line)
	if keepContext {
		if afterOnly {
			out.Before = nil
		} else {
			out.Before = cloneBytes(in.Before)
		}
		out.After = cloneBytes(in.After)
	} else {
		out.Before = nil
		out.After = nil
	}
	if len(in.LineFragments) > 0 {
		out.LineFragments = make([]zoekt.LineFragmentMatch, len(in.LineFragments))
		for i := range in.LineFragments {
			out.LineFragments[i] = cloneLineFragmentMatch(in.LineFragments[i])
		}
	}
	return out
}

func cloneLineFragmentMatch(in zoekt.LineFragmentMatch) zoekt.LineFragmentMatch {
	out := in
	if in.SymbolInfo != nil {
		symbol := *in.SymbolInfo
		out.SymbolInfo = &symbol
	}
	return out
}

func cloneChunkMatch(in zoekt.ChunkMatch) zoekt.ChunkMatch {
	out := in
	out.Content = cloneBytes(in.Content)
	if len(in.Ranges) > 0 {
		out.Ranges = make([]zoekt.Range, len(in.Ranges))
		copy(out.Ranges, in.Ranges)
	}
	if len(in.SymbolInfo) > 0 {
		out.SymbolInfo = make([]*zoekt.Symbol, len(in.SymbolInfo))
		for i, symbol := range in.SymbolInfo {
			if symbol == nil {
				continue
			}
			copied := *symbol
			out.SymbolInfo[i] = &copied
		}
	}
	return out
}

func cloneBytes(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
