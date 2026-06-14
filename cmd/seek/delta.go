package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

// indexDeltaDocuments writes a delta shard for the given repository: a new
// shard containing fresh content for changed files, plus tombstone entries in
// every prior shard's .meta sidecar for paths the caller marks via
// changedPaths (renames, content changes, deletions).
//
// The builder always Finishes (even on Add errors) so prior shards are not
// left with partial tombstone updates. Concurrent searchers keep the old .meta
// view until the atomic rename lands (see Zoekt index/builder.go:706-780).
func indexDeltaDocuments(
	indexDir string,
	repoName string,
	source string,
	files []fileContent,
	shardMaxBytes int,
	changedPaths []string,
) (bool, error) {
	opts := indexBuildOptions(indexDir, 1)
	opts.RepositoryDescription.Name = repoName
	opts.RepositoryDescription.Source = source
	if shardMaxBytes > 0 {
		opts.ShardMax = shardMaxBytes
	}
	opts.IsDelta = true

	builder, err := index.NewBuilder(opts)
	if err != nil {
		return false, fmt.Errorf("create delta builder: %w", err)
	}
	for _, path := range changedPaths {
		builder.MarkFileAsChangedOrRemoved(path)
	}

	var addErr error
	for _, doc := range files {
		if addErr == nil {
			if err := builder.Add(index.Document{
				Name:    doc.name,
				Content: doc.content,
			}); err != nil {
				addErr = fmt.Errorf("add delta document %s: %w", doc.name, err)
			}
		}
	}

	finishErr := builder.Finish()
	if addErr != nil {
		return true, addErr
	}
	return true, finishErr
}

// cleanEmptyShards removes prior shards for repoName whose live document count
// is zero (every document has been tombstoned by a later delta).
//
// Zoekt requires contiguous shard numbering: its FindAllShards iterates
// sequentially from shard 0 and stops at the first gap (Zoekt
// index/builder.go:507-528 + index/read.go:507-528), so any deleted shard
// below a live one would orphan the live shards from the delta builder's
// view. To stay safe we only delete the TRAILING suffix of empty shards,
// stopping at the first live shard (or at shard 0 unconditionally — the base
// must remain to anchor the numbering).
//
// In practice the newest shard is almost always live (it was just written by
// the prior cycle), so this is effectively a no-op for rapid-edit chains.
// Compaction in that scenario is delegated to the per-repo
// DeltaShardNumberFallbackThreshold guard (Zoekt gitindex/index.go:831-843).
func cleanEmptyShards(ctx context.Context, indexDir, repoName string) {
	shards := repositoryShardFiles(indexDir, repoName)
	for i := len(shards) - 1; i > 0; i-- {
		empty, err := shardHasNoLiveDocuments(ctx, shards[i], repoName)
		if err != nil || !empty {
			return
		}
		paths, err := index.IndexFilePaths(shards[i])
		if err != nil {
			return
		}
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
}

// shardHasNoLiveDocuments returns true when shard contains no live documents
// for repoName. Uses Zoekt's query.Const{true} as the cheapest occupancy probe
// (single-doc cap via SearchOptions).
func shardHasNoLiveDocuments(ctx context.Context, shard, repoName string) (bool, error) {
	searcher, err := openShard(shard)
	if err != nil {
		return false, err
	}
	defer searcher.Close()

	opts := zoekt.SearchOptions{
		TotalMaxMatchCount: 1,
		ShardMaxMatchCount: 1,
		MaxDocDisplayCount: 1,
		MaxWallTime:        searchTimeout,
	}
	result, err := searcher.Search(ctx, &query.Const{Value: true}, &opts)
	if err != nil {
		return false, err
	}
	for _, file := range result.Files {
		if file.Repository == repoName {
			return false, nil
		}
	}
	return true, nil
}
