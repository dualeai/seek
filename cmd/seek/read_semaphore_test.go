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

// defaultReadSemBudget returns the live readSemaphore budget — the
// value either set by swapReadSemaphoreForTest or production default.
// Acquire-all-then-Release probe via TryAcquire is too disruptive; we
// snapshot the available weight under the assumption that no other
// test holds weight at call time (caller MUST hold testReadSemMu).
func defaultReadSemBudget() int64 { return availableWeight(readSemaphore) }

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

// TestReader_OversizeCandidate_NoSemaphoreTouch — a synthetic
// candidate larger than every per-file cap must be skipped without
// ever touching the semaphore. Under current derivation
// (`maxInFlightBytes = inFlightHeadroomFiles * maxIndexedDocumentBytes`,
// `maxFolderFileSize = maxIndexedDocumentBytes`), the
// maxFolderFileSize guard fires first; the maxInFlightBytes branch
// is defense-in-depth against future drift. Either branch must
// leave the semaphore untouched.
func TestReader_OversizeCandidate_NoSemaphoreTouch(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	before := availableWeight(readSemaphore)
	ctx := t.Context()
	out := make(chan fileContent, 1)

	// Larger than the in-flight ceiling. Production also rejects via
	// the maxFolderFileSize guard, which fires first; either branch
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
