package main

import (
	"sync"
	"testing"

	"golang.org/x/sync/semaphore"
)

// testReadSemMu serializes every test that observes the SHARED
// readSemaphore via availableWeight. Without it, parallel test
// goroutines holding transient weight would skew the binary-search
// reading and produce flaky false-positive "leak detected" failures.
//
// Tests that only use a LOCAL semaphore (constructed in-test) don't
// need this lock.
var testReadSemMu sync.Mutex

// availableWeight returns the currently-available weight on sem by
// binary-search via TryAcquire/Release. Callers touching the shared
// readSemaphore MUST hold testReadSemMu for the entire before→action→
// after sequence; otherwise concurrent acquires bias the reading.
func availableWeight(sem *semaphore.Weighted) int64 {
	lo, hi := int64(0), int64(maxInFlightBytes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if sem.TryAcquire(mid) {
			sem.Release(mid)
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// TestReader_SizeOverCeiling exercises the defensive guard in
// readOneFolderFileStreaming against future cap drift: a synthetic
// candidate larger than maxInFlightBytes must be skipped without ever
// touching the semaphore. The Lstat-cap guard in the production
// readers normally rejects such files before this branch runs; the
// branch exists so a future cap change can't silently hang Acquire
// forever under context.Background() (golang/go#59002).
func TestReader_SizeOverCeiling(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	before := availableWeight(readSemaphore)
	ctx := t.Context()
	out := make(chan fileContent, 1)

	c := folderCandidate{
		name: "huge",
		path: "/nonexistent/huge",
		size: maxInFlightBytes + 1,
	}
	readOneFolderFileStreaming(ctx, c, out)
	close(out)
	for range out {
		t.Fatal("expected no fileContent for oversize candidate")
	}
	if got := availableWeight(readSemaphore); got != before {
		t.Fatalf("semaphore was touched: before=%d after=%d", before, got)
	}
}
