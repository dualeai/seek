package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

// indexDeltaDocuments adapts a repository name and source to the shared delta
// Builder sink.
func indexDeltaDocuments(
	indexDir string,
	repoName string,
	source string,
	files []fileContent,
	shardMaxBytes int,
	changedPaths []string,
) (bool, error) {
	repository := zoekt.Repository{Name: repoName, Source: source}
	return indexDeltaDocumentsWithRepository(indexDir, repository, files, shardMaxBytes, changedPaths)
}

// indexDeltaDocumentsWithRepository writes fresh content and tombstones for
// changedPaths to one delta Builder. After it creates the Builder, it always
// calls Finish, including after an Add error, so prior shards do not keep
// partial tombstone updates. Zoekt makes each sidecar change visible with its
// own temporary-file rename.
//
// The function releases all readSemaphore weights before it returns. Before it
// creates a Builder, it releases them on error. After creation, it holds them
// until Finish returns. Synchronous folder-delta reads use weight zero.
func indexDeltaDocumentsWithRepository(
	indexDir string,
	repository zoekt.Repository,
	files []fileContent,
	shardMaxBytes int,
	changedPaths []string,
) (bool, error) {
	if err := checkCtagsCached(); err != nil {
		releaseFileContentWeights(files)
		return false, err
	}
	opts := indexBuildOptions(indexDir, 1)
	opts.RepositoryDescription = repository
	if shardMaxBytes > 0 {
		opts.ShardMax = shardMaxBytes
	}
	opts.IsDelta = true

	builder, err := index.NewBuilder(opts)
	if err != nil {
		// NewBuilder failed before any Add: Zoekt holds no Content refs,
		// safe to release immediately.
		releaseFileContentWeights(files)
		return false, fmt.Errorf("create delta builder: %w", err)
	}
	for _, path := range changedPaths {
		builder.MarkFileAsChangedOrRemoved(path)
	}

	var addErr error
	for _, doc := range files {
		if addErr == nil {
			if err := builder.Add(index.Document{
				Name:       doc.name,
				Content:    doc.content,
				Branches:   doc.branches,
				SkipReason: doc.skipReason,
			}); err != nil {
				addErr = fmt.Errorf("add delta document %s: %w", doc.name, err)
			}
		}
	}

	finishErr := builder.Finish()

	// Zoekt no longer reads the document content after Finish returns, so the
	// reserved readSemaphore weight can now be released.
	releaseFileContentWeights(files)

	if addErr != nil {
		return true, addErr
	}
	return true, finishErr
}

// cleanEmptyShards removes prior shards for repoName whose live document count
// is zero (every document has been tombstoned by a later delta).
//
// Zoekt requires contiguous shard numbering: FindAllShards iterates
// sequentially from shard 0 and stops at the first gap, so any deleted
// shard below a live one would orphan the live shards from the delta
// builder's view. To stay safe we only delete the TRAILING suffix of
// empty shards, stopping at the first live shard (or at shard 0
// unconditionally — the base must remain to anchor the numbering).
//
// In practice the newest shard is almost always live, so this is often a no-op
// for rapid-edit chains. The folder and uncommitted callers enforce their own
// shard-count caps and select a full rebuild when a family exceeds its cap.
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
// for repoName. It uses query.Const{true} with a one-document result cap.
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
