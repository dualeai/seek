package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

const (
	// searchTimeout is the maximum wall-clock time for a single search.
	// Matches the lock acquisition timeout in acquireLock.
	searchTimeout = 60 * time.Second
	// searchContextLines is the number of context lines included before and
	// after each match.
	searchContextLines = 3
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

// loadShards opens all .zoekt shard files in indexDir and returns individual
// zoekt.Searcher instances. This is faster than search.NewDirectorySearcher
// because it skips directory-watcher goroutines, fsnotify setup, and
// ready-channel synchronization — overhead that is unnecessary for a
// one-shot CLI search. Multiple shards are loaded in parallel.
func loadShards(indexDir string) ([]zoekt.Searcher, error) {
	paths, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		return nil, fmt.Errorf("glob shards: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no index shards in %s", indexDir)
	}
	return loadShardPaths(indexDir, paths)
}

func loadShardsOptional(indexDir string) ([]zoekt.Searcher, error) {
	paths, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		return nil, fmt.Errorf("glob shards: %w", err)
	}
	if len(paths) == 0 {
		return nil, nil
	}
	return loadShardPaths(indexDir, paths)
}

func loadShardPaths(indexDir string, paths []string) ([]zoekt.Searcher, error) {
	// Single shard — skip goroutine overhead.
	if len(paths) == 1 {
		s, err := openShard(paths[0])
		if err != nil {
			return nil, fmt.Errorf("load shard %s: %w", paths[0], err)
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
		return nil, loadErr
	}
	if len(searchers) == 0 {
		return nil, fmt.Errorf("no loadable shards in %s", indexDir)
	}
	return searchers, nil
}

func parseSearchQuery(pattern string) (query.Q, error) {
	q, err := query.Parse(pattern)
	if err != nil {
		return nil, fmt.Errorf("parse query %q: %w", pattern, err)
	}
	q = query.Map(q, query.ExpandFileContent)
	return query.Simplify(q), nil
}

func executeParsedSearchScoped(ctx context.Context, indexDir string, userQ query.Q, scope query.Q) ([]zoekt.FileMatch, error) {
	return executeParsedSearchScopedDirs(ctx, []string{indexDir}, userQ, scope)
}

func executeParsedSearchScopedDirs(ctx context.Context, indexDirs []string, userQ query.Q, scope query.Q) ([]zoekt.FileMatch, error) {
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
		return nil, fmt.Errorf("no loadable shards")
	}
	defer func() {
		for _, s := range searchers {
			s.Close()
		}
	}()

	// Fast path: single shard avoids intermediate allFiles slice.
	if len(searchers) == 1 {
		result, err := searchers[0].Search(ctx, q, &searchOpts)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		return cloneFileMatches(result.Files), nil
	}

	// Multiple shards: fan out across goroutines bounded by NumCPU.
	// Sequential iteration cost grows linearly with shard count, which
	// the windowed indexer's rotation can drive into the hundreds for
	// multi-GiB corpora. Parallel dispatch turns the per-shard query
	// cost from sum-of-shards into max-of-shards (up to GOMAXPROCS).
	parallelism := runtime.NumCPU()
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
			r, err := s.Search(ctx, q, &searchOpts)
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
	return cloneFileMatches(allFiles), nil
}

func cloneFileMatches(files []zoekt.FileMatch) []zoekt.FileMatch {
	if len(files) == 0 {
		return nil
	}
	out := make([]zoekt.FileMatch, len(files))
	for i := range files {
		out[i] = cloneFileMatch(files[i])
	}
	return out
}

func cloneFileMatch(in zoekt.FileMatch) zoekt.FileMatch {
	out := in
	out.Branches = cloneStringSlice(in.Branches)
	out.Content = cloneBytes(in.Content)
	out.Checksum = cloneBytes(in.Checksum)

	if len(in.LineMatches) > 0 {
		out.LineMatches = make([]zoekt.LineMatch, len(in.LineMatches))
		for i := range in.LineMatches {
			out.LineMatches[i] = cloneLineMatch(in.LineMatches[i])
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

func cloneLineMatch(in zoekt.LineMatch) zoekt.LineMatch {
	out := in
	out.Line = cloneBytes(in.Line)
	out.Before = cloneBytes(in.Before)
	out.After = cloneBytes(in.After)
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
