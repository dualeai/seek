package main

import "golang.org/x/sync/semaphore"

// readWeight is the readSemaphore weight for one document of size bytes. It
// stops at maxReadWeightBytes. The semaphore grants waiters in order, and
// consumers keep their window weight while they wait for more documents. A
// request near the whole budget would therefore wait forever, and every later
// reader would wait behind it. A larger document still gets indexed; its memory
// above the clamp is not counted.
func readWeight(size int64) int64 {
	return min(size, maxReadWeightBytes)
}

// readSemaphore bounds document weight in flight across reader entry points.
// Budget is readBudgetBytes (caps.go); each document takes readWeight of its
// size. Acquire on the reader side; the Release contract lives on
// fileContent.weight (indexer.go).
var readSemaphore = semaphore.NewWeighted(readBudgetBytes)

// largeDocumentSemaphore bounds large shard and ctags jobs across all active
// corpora. It limits concurrency only; every document still gets normal
// content and symbol analysis.
var largeDocumentSemaphore = semaphore.NewWeighted(int64(largeDocumentParallelism))

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
