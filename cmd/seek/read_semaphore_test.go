package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/sourcegraph/zoekt/index"
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

// availableWeight probes available weight by binary-search with
// TryAcquire/Release. While a waiter is queued, TryAcquire can fail even when
// capacity remains, so active-pipeline callers must use this only as an
// activity signal. After all workers stop, it gives an exact leak check.
// Callers touching the shared readSemaphore MUST hold testReadSemMu for the
// full before-action-after sequence.
func availableWeight(sem *semaphore.Weighted) int64 {
	lo, hi := int64(0), readBudgetBytes
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
// candidate larger than the content limit must become one name-only
// document without touching the semaphore. The test runs in a synctest
// bubble: a reader that waits for read weight fails at once as a deadlock.
func TestReader_OversizeCandidate_NoSemaphoreTouch(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	swapDocumentContentLimitForTest(t, testDocumentContentLimit)
	synctest.Test(t, func(t *testing.T) {
		// Hold the whole read budget, as busy readers would: a reader that
		// acquires any weight waits and sends nothing.
		const budget = 1 << 20
		defer swapReadSemaphoreForTest(budget)()
		if !readSemaphore.TryAcquire(budget) {
			t.Fatal("cannot hold the test read budget")
		}
		defer readSemaphore.Release(budget)
		out := make(chan fileContent, 1)

		// Larger than the swapped content limit: the reader must send it by
		// name only, without Acquiring weight.
		c := folderCandidate{
			name: "huge",
			path: "/nonexistent/huge",
			size: maxInFlightBytes + 1,
		}
		readCtx, fail := context.WithCancelCause(t.Context())
		defer fail(nil)
		readOneFolderFileStreaming(readCtx, c, out, fail)
		close(out)
		if cause := context.Cause(readCtx); cause != nil {
			t.Fatalf("the reader reported a failure: %v", cause)
		}
		sent := 0
		for document := range out {
			sent++
			if document.skipReason != index.SkipReasonTooLarge || document.content != nil || document.weight != 0 {
				t.Fatalf("oversize candidate = %+v, want a name-only TooLarge document", document)
			}
		}
		if sent != 1 {
			t.Fatalf("sent %d documents, want one name-only document", sent)
		}
	})
}

// TestReader_AboveBudgetCandidate_ClampsWeight checks that a document larger
// than the whole read budget does not wait forever in Acquire. The path does
// not exist, so the reader acquires its clamped weight, fails to open the file,
// and must return with the weight released. The test runs in a synctest
// bubble: a reader that waits forever fails at once as a deadlock.
func TestReader_AboveBudgetCandidate_ClampsWeight(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()
	// The budget grows with host memory and can pass the content limit; raise
	// the limit so the candidate takes the weighted read path.
	swapDocumentContentLimitForTest(t, readBudgetBytes+2)

	before := availableWeight(readSemaphore)
	synctest.Test(t, func(t *testing.T) {
		out := make(chan fileContent, 1)
		c := folderCandidate{name: "huge", path: "/nonexistent/huge", size: readBudgetBytes + 1}
		readCtx, fail := context.WithCancelCause(t.Context())
		defer fail(nil)
		readOneFolderFileStreaming(readCtx, c, out, fail)
		close(out)
		if cause := context.Cause(readCtx); cause != nil {
			t.Fatalf("the reader reported a failure: %v", cause)
		}
		for document := range out {
			t.Fatalf("unexpected document for a missing file: %+v", document)
		}
	})
	if got := availableWeight(readSemaphore); got != before {
		t.Fatalf("semaphore leak: before=%d after=%d", before, got)
	}
}

// TestReader_BinaryPrefixSendsNameOnly checks that a large file that starts
// with a NUL byte becomes one binary, name-only document: the reader keeps no
// content and returns its weight.
func TestReader_BinaryPrefixSendsNameOnly(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	// A sparse file with no written bytes reads as NUL bytes from the start.
	path := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 64<<20); err != nil {
		t.Fatal(err)
	}
	before := availableWeight(readSemaphore)
	out := make(chan fileContent, 1)
	readCtx, fail := context.WithCancelCause(t.Context())
	defer fail(nil)
	readOneFolderFileStreaming(readCtx, folderCandidate{name: "model.bin", path: path, size: 64 << 20}, out, fail)
	close(out)
	if cause := context.Cause(readCtx); cause != nil {
		t.Fatalf("the reader reported a failure: %v", cause)
	}
	var got []fileContent
	for document := range out {
		got = append(got, document)
	}
	if len(got) != 1 || got[0].skipReason != index.SkipReasonBinary || got[0].content != nil || got[0].weight != 0 {
		t.Fatalf("documents = %+v, want one binary name-only document", got)
	}
	if after := availableWeight(readSemaphore); after != before {
		t.Fatalf("semaphore leak: before=%d after=%d", before, after)
	}
}
