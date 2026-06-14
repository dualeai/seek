package main

import "golang.org/x/sync/semaphore"

// maxInFlightBytes bounds the total bytes resident in read buffers
// across all reader workers (Git dirty files and folder candidates).
// It is sized to comfortably exceed maxIndexedDocumentBytes so that
// a single max-sized file never blocks Acquire indefinitely under
// a context.Background() caller (golang/go#59002).
const maxInFlightBytes = 64 * 1024 * 1024 // 64 MiB

// readSemaphore caps bytes in flight across both reader entry points
// (cmd/seek/indexer.go::readFilesToChannel and
// cmd/seek/folder_indexer.go::streamFolderFiles). Acquire on the
// reader side; release in the consumer (indexDocuments,
// indexDeltaDocuments) strictly after builder.Finish() returns,
// because Zoekt's async shard writers retain Content references
// until then (zoekt/index/builder.go:659-667 — Finish calls
// b.building.Wait() before returning).
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
