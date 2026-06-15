package main

import "golang.org/x/sync/semaphore"

// readSemaphore bounds bytes in flight across reader entry points.
// Budget is maxInFlightBytes (caps.go). Acquire on the reader side;
// the Release contract lives on fileContent.weight (indexer.go).
var readSemaphore = semaphore.NewWeighted(maxInFlightBytes)

// releaseFileContentWeights Releases readSemaphore weight for every
// doc carrying non-zero weight. Safe on nil/empty/all-zero slices.
// Caller must invoke exactly once per doc set, AFTER the
// builder.Finish() that may have referenced the underlying Content
// (Zoekt's b.building.Wait inside Finish joins the shard writers).
func releaseFileContentWeights(docs []fileContent) {
	var total int64
	for _, d := range docs {
		total += d.weight
	}
	if total > 0 {
		readSemaphore.Release(total)
	}
}
