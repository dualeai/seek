package main

import (
	"sync"
	"testing"
	"time"

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

// waitUntilSemaphoreBelow polls availableWeight every 10 ms until it
// drops below `threshold` or the deadline expires. Used by
// cancellation tests as a sync mechanism robust to CI scheduling
// jitter — replaces fixed time.Sleep "wait for producers to start"
// patterns.
func waitUntilSemaphoreBelow(t *testing.T, sem *semaphore.Weighted, threshold int64, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if availableWeight(sem) < threshold {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitUntilSemaphoreBelow: %v deadline exceeded (still at %d, threshold %d)",
		deadline, availableWeight(sem), threshold)
}

// availableWeight probes available weight by binary-search with
// TryAcquire/Release. While a waiter is queued, TryAcquire can fail even when
// capacity remains, so active-pipeline callers must use this only as an
// activity signal. After all workers stop, it gives an exact leak check.
// Callers touching the shared readSemaphore MUST hold testReadSemMu for the
// full before-action-after sequence.
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

// TestReader_OversizeCandidate_NoSemaphoreTouch — a synthetic
// candidate larger than every per-file cap must be skipped without
// ever touching the semaphore. The maxIndexedDocumentBytes guard
// fires first; the maxInFlightBytes branch is defense-in-depth
// against future drift. Either branch must leave the semaphore
// untouched.
func TestReader_OversizeCandidate_NoSemaphoreTouch(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	before := availableWeight(readSemaphore)
	ctx := t.Context()
	out := make(chan fileContent, 1)

	// Larger than the in-flight ceiling. Production also rejects via
	// the maxIndexedDocumentBytes guard, which fires first; either branch
	// must skip the file without Acquiring weight.
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
