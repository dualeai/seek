package main

import "golang.org/x/sync/semaphore"

// readSemaphore bounds bytes in flight across reader entry points.
// Budget is maxInFlightBytes (caps.go). Acquire on the reader side;
// the Release contract lives on fileContent.weight (indexer.go).
//
// TODO(perf): per-corpus sharding to eliminate cross-corpus mutex
// contention. Under N-way corpus indexing this single global
// `semaphore.Weighted` becomes a contention point: every Acquire /
// Release takes the package-internal sync.Mutex. With corpusWorkerCap=4
// concurrent corpora × indexParallelism=min(NumCPU,16) reader
// goroutines, up to ~64 contenders race on one mutex per file read;
// for a 1 GiB corpus at ~10 KiB avg file size that's ~200k
// Acquire/Release pairs per corpus.
//
// Solution: shard into N sub-semaphores keyed by corpusID hash
// (e.g. 4 shards = 600 MiB each at the current 2400 MiB budget). Each
// corpus binds to one shard for its lifetime → zero cross-corpus
// mutex contention. Windowed-fit invariant still holds per-shard
// because shard >= window + 2*doc when N matches the worker cap.
//
// Why deferred: no profile evidence today. `go test -mutexprofile` on
// TestRunCorpusPool_NWayConcurrent has not been captured; if the
// mutex is <1% of wall time the sharding refactor (touches every
// reader Acquire/Release site in indexer.go + folder_indexer.go +
// delta.go) is pure cost. Revisit IF a real `pprof -mutex` capture
// shows >5% wall time inside `(*Weighted).Acquire`.
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
