package main

import (
	"errors"
	"fmt"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
)

// deltaShardMin is the smallest shard target of a delta Builder. Each shard
// starts its own ctags process and allocates its own postings, so a smaller
// shard costs more time than it saves. Fewer shards also keep a family below
// the delta shard caps for longer. On an 11 MB folder delta with 18 CPUs, a
// 256 KiB floor was not faster and made 1.8 times the shards. Floors of 2 MiB
// and 4 MiB were 2 and 3 times slower than 1 MiB.
const deltaShardMin = 1 << 20 // 1 MiB

// deltaShardMax returns the shard target of a delta Builder: the payload of
// files divided by parallelism, so that each Builder worker builds one shard.
// With a fixed target, one CPU builds a large delta as one shard. An 11 MB
// commit took 3.7 s as one 10 MB shard, and 1.2 to 1.5 s with this split. The
// target is at least deltaShardMin and at most shardMax, the target of a full
// build. The upper bound keeps a shard with no isolated document far below the
// limit of the Zoekt shard format (guardWrappedShards). Zoekt does not hash
// ShardMax, so a delta stays compatible with its base.
func deltaShardMax(files []fileContent, parallelism int) int {
	payload := 0
	for _, doc := range files {
		payload += len(doc.name) + len(doc.content)
	}
	return min(shardMax, max(deltaShardMin, payload/max(parallelism, 1)))
}

// errDeltaCompaction stops a delta that deltaNeedsCompaction rejects; the
// caller then builds the family in full.
var errDeltaCompaction = errors.New("deltas tombstoned as many bytes as the family holds")

// deltaNeedsCompaction reports whether a family builds in full instead of in a
// delta: tombstoned reaches both live and one full shard (shardMax).
// tombstoned is the content bytes that deltas tombstoned since the last full
// build, this delta included; live is the content bytes of the live files.
// Both count the bytes that the shard documents hold (heldContentBytes). A
// file kept by name only counts as its NOT-INDEXED marker, because a search
// reads only that marker. A delta adds tombstones but never removes a
// document. A delta of removals only adds no shard, so a run of such deltas
// never reaches a shard cap. Without this rule, dead documents stay in
// every shard and every .meta, and each search reads them. After a removal of
// 36,000 of 40,000 files (140 MB of dead content), a warm search took 63 ms
// and 155 MiB. After the full build that this rule starts, it took 47 ms and
// 55 MiB. The index shrank from 396 MB to 38 MB, and the full build took
// about 4 s once. That is about 0.1 ms per dead MB. So less than one shard of
// dead content costs a search less than about 1 ms. A delta of a small family
// stays a delta.
func deltaNeedsCompaction(tombstoned, live int64) bool {
	return tombstoned >= max(live, int64(shardMax))
}

// notIndexedMarkerBytes is the length of the content that Zoekt stores for a
// document that it keeps by name only: zoektNotIndexedMarker and the
// explanation of reason (zoektSkipExplanation).
func notIndexedMarkerBytes(reason index.SkipReason) int64 {
	return int64(len(zoektNotIndexedMarker) + len(zoektSkipExplanation(reason)))
}

// unreadFileMarker is the marker value of a file that a delta has not read
// yet. Such a file counts at an upper bound of its shard bytes: its size or
// the largest marker (maxNotIndexedMarkerBytes), whichever is larger. So the
// live bytes before the reads are never below the exact ones. The rule of
// deltaNeedsCompaction then never fires before the reads when the read bytes
// would keep a delta. A manifest never stores this value.
const unreadFileMarker int64 = -1

// maxNotIndexedMarkerBytes is the largest marker that Zoekt stores for a
// document kept by name only (notIndexedMarkerBytes).
var maxNotIndexedMarkerBytes = notIndexedMarkerBytes(index.SkipReasonTooLarge)

// shardHeldBytes is the content bytes that the shard document of a file of size
// bytes holds. A file kept by name only holds its NOT-INDEXED marker: a file
// with a marker, a file of which Seek has no bytes (noBytes), or a file too
// large for the index. Any other file holds its size. marker is the marker
// bytes that a read found (nameOnlyMarkerBytes), zero when the shard holds the
// content, or unreadFileMarker. An unread file counts at the larger of that
// value and the largest marker (maxNotIndexedMarkerBytes).
func shardHeldBytes(marker, size int64, noBytes bool) int64 {
	held := size
	switch {
	case marker > 0:
		held = marker
	case noBytes:
		held = notIndexedMarkerBytes(seekNoBytesReason)
	case !documentContentFits(size):
		held = notIndexedMarkerBytes(index.SkipReasonTooLarge)
	}
	if marker == unreadFileMarker {
		return max(held, maxNotIndexedMarkerBytes)
	}
	return held
}

// nameOnlyMarkerBytes returns the bytes of the marker that the shard document
// of doc holds in place of its content. It returns zero when the shard holds
// the content. The skip reason comes from the reader, or from Zoekt's own
// check that Builder.Add applies. That check catches content with a NUL byte,
// or of one or two bytes. A binary file of any size then counts as its marker
// in deltaNeedsCompaction, because a search reads only that marker.
func nameOnlyMarkerBytes(doc fileContent) int64 {
	reason := doc.skipReason
	if reason == index.SkipReasonNone {
		var checker index.DocChecker
		reason = checker.Check(doc.content, int(documentContentLimit), false)
	}
	if reason == index.SkipReasonNone {
		return 0
	}
	return notIndexedMarkerBytes(reason)
}

// heldContentBytes returns the content bytes that the shard document of doc
// holds: its marker (nameOnlyMarkerBytes) or its content.
func heldContentBytes(doc fileContent) int64 {
	if marker := nameOnlyMarkerBytes(doc); marker > 0 {
		return marker
	}
	return int64(len(doc.content))
}

// indexDeltaDocuments adapts a repository name and source to the shared delta
// Builder sink.
func indexDeltaDocuments(
	indexDir string,
	repoName string,
	source string,
	files []fileContent,
	parallelism int,
	changedPaths []string,
) (bool, error) {
	repository := zoekt.Repository{Name: repoName, Source: source}
	return indexDeltaDocumentsWithRepository(indexDir, repository, files, parallelism, changedPaths)
}

// indexDeltaDocumentsWithRepository writes new content and tombstones for
// changedPaths to one delta Builder, and returns true once it has made the
// Builder. As in a full build, the Builder builds up to parallelism shards at
// the same time. Its shard target comes from deltaShardMax, which states the
// measured gain of one shard job per CPU.
//
// files can be empty: Zoekt accepts a delta with no documents and then writes
// only the tombstones into the sidecars of the prior shards. So a delta of
// removals only needs no full rebuild until deltaNeedsCompaction asks for one.
// For one removed file of 3,000, a full rebuild took 7.9 s, and the
// tombstone-only delta took 0.1 s.
//
// After it creates the Builder, it always calls Finish, including after an Add
// error, so prior shards do not keep partial tombstone updates. Zoekt makes
// each sidecar change visible with its own temporary-file rename. After a
// successful Finish, guardWrappedShards can remove new shards that exceed the
// Zoekt shard format and write their documents again with a second delta
// Builder.
//
// The function releases all readSemaphore weights before it returns. Before it
// creates a Builder, it releases them on error. After creation, it holds them
// until Finish and the shard guard return. Synchronous folder-delta reads use
// weight zero.
func indexDeltaDocumentsWithRepository(
	indexDir string,
	repository zoekt.Repository,
	files []fileContent,
	parallelism int,
	changedPaths []string,
) (bool, error) {
	if err := checkCtagsCached(); err != nil {
		releaseFileContentWeights(files)
		return false, err
	}
	opts := indexBuildOptions(indexDir, max(parallelism, 1))
	opts.RepositoryDescription = repository
	opts.ShardMax = deltaShardMax(files, parallelism)
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

	var builder *index.Builder
	err := zoektShardCall(func() (err error) {
		builder, err = index.NewBuilder(opts)
		return err
	})
	if err != nil {
		// NewBuilder failed before any Add, so Zoekt holds no Content
		// reference, and the weights can go now.
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

	finishErr := zoektShardCall(builder.Finish)
	if finishErr != nil {
		// A Finish that stops part way, as on a damaged record, leaves its
		// shards under their temporary names. A delta can write into the live
		// family (the uncommitted one), so they go now.
		removeRepositoryShardTemps(indexDir, repository.Name)
	}
	if addErr == nil && finishErr == nil {
		finishErr = guardWrappedShards(opts, firstShard, isolated, later)
	}

	// Zoekt and the shard guard are done with the document content, so
	// release the reserved readSemaphore weight here.
	releaseFileContentWeights(files)

	if addErr != nil {
		return true, addErr
	}
	return true, finishErr
}
