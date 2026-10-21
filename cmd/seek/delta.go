package main

import (
	"fmt"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
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
// own temporary-file rename. After a successful Finish, guardWrappedShards can
// remove new shards that exceed the Zoekt shard format and write their
// documents again with a second delta Builder.
//
// The function releases all readSemaphore weights before it returns. Before it
// creates a Builder, it releases them on error. After creation, it holds them
// until Finish and the shard guard return. Synchronous folder-delta reads use
// weight zero.
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

	// Add the documents that fill a shard of their own first, so their shards
	// are the first new ones and guardWrappedShards can map them.
	var isolated, later []index.Document
	for _, doc := range files {
		document := index.Document{
			Name:       doc.name,
			Content:    doc.content,
			Branches:   doc.branches,
			SkipReason: doc.skipReason,
		}
		if isolatesShard(document, opts) {
			isolated = append(isolated, document)
		} else {
			later = append(later, document)
		}
	}
	// Only an isolated document can wrap a shard, so the guard needs the shard
	// listing only when there is one.
	firstShard := 0
	if len(isolated) > 0 {
		firstShard = firstBuilderShard(opts)
	}

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
	for _, doc := range append(isolated, later...) {
		if err := builder.Add(doc); err != nil {
			addErr = fmt.Errorf("add delta document %s: %w", doc.Name, err)
			break
		}
	}

	finishErr := builder.Finish()
	if addErr == nil && finishErr == nil {
		finishErr = guardWrappedShards(opts, firstShard, isolated, later)
	}

	// Zoekt no longer reads the document content after Finish returns, so the
	// reserved readSemaphore weight can now be released.
	releaseFileContentWeights(files)

	if addErr != nil {
		return true, addErr
	}
	return true, finishErr
}
