package main

import "golang.org/x/sync/semaphore"

// readWeight is the readSemaphore weight for one document of size bytes. It
// stops at maxReadWeightBytes. The semaphore grants waiters in order, and
// consumers keep their window weight while they wait for more documents. A
// request near the whole budget would therefore wait forever, and every later
// reader would wait behind it. Seek still indexes a larger document, but the
// semaphore does not count its memory above the clamp.
func readWeight(size int64) int64 {
	return min(size, maxReadWeightBytes)
}

// readSemaphore bounds the weight of the documents that are read and not yet
// released, across every reader entry point. Its budget is readBudgetBytes
// (caps.go), and each document takes readWeight of its size. The readers
// acquire the weight; the doc of fileContent.weight (indexer.go) gives the
// release rules.
var readSemaphore = semaphore.NewWeighted(readBudgetBytes)

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
