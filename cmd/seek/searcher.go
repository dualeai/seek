package main

import (
	"context"
	"fmt"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"github.com/sourcegraph/zoekt/search"
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
		NumContextLines:    3,
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	return result.Files, nil
}
