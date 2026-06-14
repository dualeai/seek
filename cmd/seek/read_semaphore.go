package main

import "golang.org/x/sync/semaphore"

// readSemaphore bounds bytes in flight across both reader entry
// points (cmd/seek/indexer.go::readFilesToChannel and
// cmd/seek/folder_indexer.go::streamFolderFiles). Budget is defined
// by maxInFlightBytes in cmd/seek/caps.go as a derived multiple of
// maxIndexedDocumentBytes.
//
// Acquire on the reader side; Release in the consumer
// (indexDocuments / indexDeltaDocuments) strictly after
// builder.Finish() returns, because Zoekt's async shard writers
// retain Content references until then.
var readSemaphore = semaphore.NewWeighted(maxInFlightBytes)

// sumFileContentWeights returns the total Acquire weight to release
// for the given docs. Synchronous folder-delta reads leave weight at
// zero, which contributes nothing.
func sumFileContentWeights(docs []fileContent) int64 {
	var total int64
	for _, d := range docs {
		total += d.weight
	}
	return total
}

// releaseFileContentWeights releases readSemaphore weight for every
// doc that carries a non-zero weight. Safe on nil, empty, or
// all-zero-weight slices. Must be called exactly once per doc set,
// and only AFTER any builder.Finish() that may have referenced the
// underlying Content slices.
func releaseFileContentWeights(docs []fileContent) {
	total := sumFileContentWeights(docs)
	if total > 0 {
		readSemaphore.Release(total)
	}
}
