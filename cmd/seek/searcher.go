package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	// limiting is handled by seek's --limit/-n flag in formatResults,
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

	// Single shard — skip goroutine overhead.
	if len(paths) == 1 {
		s, err := openShard(paths[0])
		if err != nil {
			return nil, fmt.Errorf("no loadable shards in %s", indexDir)
		}
		return []zoekt.Searcher{s}, nil
	}

	// Multiple shards — load in parallel to overlap mmap + metadata parsing.
	type result struct {
		idx int
		s   zoekt.Searcher
	}
	results := make(chan result, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(idx int, path string) {
			defer wg.Done()
			s, err := openShard(path)
			if err != nil {
				return
			}
			results <- result{idx: idx, s: s}
		}(i, p)
	}
	wg.Wait()
	close(results)

	searchers := make([]zoekt.Searcher, 0, len(paths))
	for r := range results {
		_ = r.idx // preserve order is not needed — BM25 sorts results
		searchers = append(searchers, r.s)
	}
	if len(searchers) == 0 {
		return nil, fmt.Errorf("no loadable shards in %s", indexDir)
	}
	return searchers, nil
}

// executeSearch loads shards and runs a zoekt search against the index.
// Shards are loaded directly via mmap (skipping the directory-watcher
// infrastructure) for minimal latency on the warm search path.
func executeSearch(ctx context.Context, indexDir, pattern string) ([]zoekt.FileMatch, error) {
	// Parse query before loading shards — fail fast on invalid patterns
	// without wasting mmap syscalls.
	q, err := query.Parse(pattern)
	if err != nil {
		return nil, fmt.Errorf("parse query %q: %w", pattern, err)
	}
	q = query.Map(q, query.ExpandFileContent)
	q = query.Simplify(q)

	searchers, err := loadShards(indexDir)
	if err != nil {
		return nil, fmt.Errorf("load index: %w", err)
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
		return result.Files, nil
	}

	// Multiple shards: search each and merge results.
	var allFiles []zoekt.FileMatch
	for _, s := range searchers {
		result, err := s.Search(ctx, q, &searchOpts)
		if err != nil {
			continue
		}
		allFiles = append(allFiles, result.Files...)
	}
	return allFiles, nil
}
