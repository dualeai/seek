package main

import "golang.org/x/sync/semaphore"

// readSemaphore bounds bytes in flight across reader entry points.
// Budget is maxInFlightBytes (caps.go). Acquire on the reader side;
// the Release contract lives on fileContent.weight (indexer.go).
var readSemaphore = semaphore.NewWeighted(maxInFlightBytes)

// largeDocumentSemaphore bounds large shard and ctags jobs across all active
// corpora. It limits concurrency only; every document still gets normal
// content and symbol analysis.
var largeDocumentSemaphore = semaphore.NewWeighted(largeDocumentParallelism)

// releaseFileContentWeights releases readSemaphore weight for every document
// that carries non-zero weight. It is safe on nil, empty, and all-zero slices.
// Call it once when the consumer stops owning the documents. If a Builder
// accepted their Content, wait for Builder.Finish first. An error before
// Builder creation can release the weight immediately. See fileContent.
func releaseFileContentWeights(docs []fileContent) {
	var total int64
	for _, d := range docs {
		total += d.weight
	}
	if total > 0 {
		readSemaphore.Release(total)
	}
}
