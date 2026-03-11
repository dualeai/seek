package main

import (
	"context"
	"fmt"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"github.com/sourcegraph/zoekt/search"
)

const (
	// searchTimeout is the maximum wall-clock time for a single search.
	// Matches the lock acquisition timeout in acquireLock.
	searchTimeout = 60 * time.Second
	// searchContextLines is the number of context lines included before and
	// after each match.
	searchContextLines = 3
)

// executeSearch loads shards and runs a zoekt search against the index.
func executeSearch(ctx context.Context, indexDir, pattern string) ([]zoekt.FileMatch, error) {
	searcher, err := search.NewDirectorySearcher(indexDir)
	if err != nil {
		return nil, fmt.Errorf("load index: %w", err)
	}
	defer searcher.Close()

	q, err := query.Parse(pattern)
	if err != nil {
		return nil, fmt.Errorf("parse query %q: %w", pattern, err)
	}

	q = query.Map(q, query.ExpandFileContent)
	q = query.Simplify(q)

	result, err := searcher.Search(ctx, q, &zoekt.SearchOptions{
		MaxDocDisplayCount: 1000,
		TotalMaxMatchCount: 10000,
		ShardMaxMatchCount: 10000,
		NumContextLines:    searchContextLines,
		UseBM25Scoring:     true,
		MaxWallTime:        searchTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	return result.Files, nil
}
