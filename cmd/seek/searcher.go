package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

const (
	// searchTimeout is the maximum wall-clock time for a single search.
	// Matches the pollLock timeout used by the lock acquisition helpers.
	searchTimeout = 60 * time.Second
	// searchContextLines is the number of context lines included before and
	// after each match.
	searchContextLines    = 3
	maxSearchContextLines = 512
)

// searchOpts are the zoekt search options used for every search. Defined
// at package level to avoid per-search heap allocation.
var searchOpts = zoekt.SearchOptions{
	// MaxDocDisplayCount is intentionally left at 0 (unlimited). Display
	// limiting is handled by seek's --limit/-n flag during formatting,
	// which applies after dedup and BM25 sort. A zoekt-level display cap
	// would silently drop low-ranked files before seek or downstream
	// pipes (| grep, | head) see them, causing false negatives.
	// Search work is bounded by TotalMaxMatchCount and ShardMaxMatchCount.
	TotalMaxMatchCount: 10000,
	ShardMaxMatchCount: 10000,
	NumContextLines:    searchContextLines,
	UseBM25Scoring:     true,
	MaxWallTime:        searchTimeout,
}

type searchConfig struct {
	opts              *zoekt.SearchOptions
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

func explicitSearchConfig(contextLines int, afterOnly bool) searchConfig {
	opts := searchOpts
	opts.NumContextLines = contextLines
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
// The caller normally holds the shared publish lock, so this scan cannot
// interleave with a successful swap. acquireReadLock can permit an unlocked
// read after its timeout; that path scans the shards that remain. This
// function needs names only and does not load size metadata.
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
	results := make(chan shardLoadResult, len(paths))
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			s, err := openShard(path)
			if err != nil {
				results <- shardLoadResult{path: path, err: err}
				return
			}
			results <- shardLoadResult{searcher: s, path: path}
		}(p)
	}
	wg.Wait()
	close(results)

	searchers := make([]zoekt.Searcher, 0, len(paths))
	var loadErr error
	for result := range results {
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
	raw, err := query.Parse(pattern)
	if err != nil {
		var ok bool
		raw, ok = parsePlainDescriptionFallback(pattern)
		if !ok {
			return nil, nil, &querySyntaxError{query: pattern, cause: err}
		}
	}
	expanded := query.Map(raw, query.ExpandFileContent)
	return raw, query.Simplify(expanded), nil
}

// parsePlainDescriptionFallback keeps programming names such as C++ usable in
// descriptive search. Zoekt parses bare terms as regular expressions, so a
// valid programming name can otherwise fail before the semantic path starts.
// Only use this path after Zoekt rejects the query, and reject all explicit
// query syntax so malformed filters and regular expressions stay errors.
func parsePlainDescriptionFallback(pattern string) (query.Q, bool) {
	terms := strings.Fields(pattern)
	if len(terms) < 2 {
		return nil, false
	}
	children := make([]query.Q, 0, len(terms))
	for _, term := range terms {
		if strings.EqualFold(term, "or") {
			return nil, false
		}
		parsed, err := query.Parse(term)
		if err == nil {
			if substring, ok := parsed.(*query.Substring); ok &&
				!substring.FileName && !substring.Content && substring.Pattern == term {
				children = append(children, substring)
				continue
			}
		}
		if !plainProgrammingName(term) {
			return nil, false
		}
		children = append(children, &query.Substring{
			Pattern:       term,
			CaseSensitive: strings.ToLower(term) != term,
		})
	}
	return query.NewAnd(children...), true
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
		q = query.NewAnd(q, scope)
		q = query.Simplify(q)
	}

	var searchers []zoekt.Searcher
	for _, indexDir := range indexDirs {
		loaded, err := loadShardsOptional(indexDir)
		if err != nil {
			for _, s := range searchers {
				s.Close()
			}
			return nil, fmt.Errorf("load index: %w", err)
		}
		searchers = append(searchers, loaded...)
	}
	if len(searchers) == 0 {
		return nil, fmt.Errorf("%w: no loadable shards", errShardUnloadable)
	}
	defer func() {
		for _, s := range searchers {
			s.Close()
		}
	}()

	// Fast path: single shard avoids intermediate allFiles slice.
	if len(searchers) == 1 {
		result, err := searchers[0].Search(ctx, q, config.opts)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
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
			results[i] = shardResult{files: r.Files}
		}(i, s)
	}
	wg.Wait()
	var allFiles []zoekt.FileMatch
	for _, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("search: %w", r.err)
		}
		allFiles = append(allFiles, r.files...)
	}
	return cloneFileMatches(allFiles, config), nil
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
