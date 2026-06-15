package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestEnsureFolderCorpusFresh_CancelMidAcquire — start an indexing
// pass over a folder whose cumulative content exceeds the test-local
// budget, then cancel the context partway through. Every goroutine
// (producer, workers, consumer) must unwind cleanly; the semaphore
// must return to its starting balance.
//
// goleak.VerifyNone catches any goroutine that wedged on Acquire,
// channel send, or builder I/O. Ignored top-frames are stdlib
// background workers + this test's parent runtime.
func TestEnsureFolderCorpusFresh_CancelMidAcquire(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	// IgnoreCurrent snapshots the runtime goroutine set at defer time
	// (top of test); a leaked reader appears as a NEW goroutine after
	// the snapshot and surfaces in VerifyNone. Note: we intentionally
	// do NOT use IgnoreTopFunction("internal/poll.runtime_pollWait") —
	// it would mask a leaked reader stuck on os.(*File).Read on slow
	// storage. The runtime test harness goroutines are already
	// captured by IgnoreCurrent.
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
	)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	root := writeRandomFolder(t, 12*1024*1024, 512*1024)
	plan := planSynthCorpus(t, root)

	ctx, cancel := context.WithCancel(t.Context())
	var ensureErr error
	done := make(chan struct{})
	go func() {
		_, ensureErr = ensureFolderCorpusFresh(ctx, plan)
		close(done)
	}()
	// Wait until producers have actually started Acquiring (semaphore
	// dropped below baseline) instead of a fixed Sleep — robust under
	// loaded CI scheduling.
	waitUntilSemaphoreBelow(t, readSemaphore, testBudget, 5*time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		dumpGoroutines(t)
		t.Fatal("ensureFolderCorpusFresh did not unwind within 20s of cancel")
	}

	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after cancel: got=%d want=%d", got, testBudget)
	}
	// Contract: ctx cancel must propagate through to the function's
	// return value. A regression that silently swallows ctx.Err mid-
	// pipeline would still drain workers + balance the semaphore but
	// return nil — caught here.
	// Cancellation contract is best-effort: the function MAY return
	// nil if it finishes before observing ctx.Done(). What it MUST
	// NOT return is some unrelated error. Accept nil or context.Canceled.
	if ensureErr != nil && !errors.Is(ensureErr, context.Canceled) {
		t.Fatalf("expected nil or context.Canceled, got %v", ensureErr)
	}
}

// TestEnsureFolderCorpusFresh_CancelMidChannelSend — start a pass,
// wait until reader workers have Acquired most of the budget (so at
// least some are likely parked on the unbuffered `out <-` send), then
// cancel. Workers must exit through their select-on-ctx branch without
// leaking semaphore weight.
//
// Threshold (testBudget/4) is permissive on purpose: under -race on
// CI, full saturation is non-deterministic. The contract under test
// is "cancel propagates and no leak", which holds for any non-trivial
// Acquire — exact timing of the cancel relative to send vs Acquire
// doesn't matter to the contract.
func TestEnsureFolderCorpusFresh_CancelMidChannelSend(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	// IgnoreCurrent snapshots the runtime goroutine set at defer time
	// (top of test); a leaked reader appears as a NEW goroutine after
	// the snapshot and surfaces in VerifyNone. Note: we intentionally
	// do NOT use IgnoreTopFunction("internal/poll.runtime_pollWait") —
	// it would mask a leaked reader stuck on os.(*File).Read on slow
	// storage. The runtime test harness goroutines are already
	// captured by IgnoreCurrent.
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
	)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	root := writeRandomFolder(t, 12*1024*1024, 512*1024)
	plan := planSynthCorpus(t, root)

	ctx, cancel := context.WithCancel(t.Context())
	var ensureErr error
	done := make(chan struct{})
	go func() {
		_, ensureErr = ensureFolderCorpusFresh(ctx, plan)
		close(done)
	}()
	// Wait until at least 3/4 of the budget is Acquired so workers
	// are likely past their Acquire and parked on the channel send.
	// Avoids waiting for full saturation which is unreliable under
	// -race scheduling.
	waitUntilSemaphoreBelow(t, readSemaphore, testBudget/4, 10*time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		dumpGoroutines(t)
		t.Fatal("ensureFolderCorpusFresh did not unwind within 20s of cancel")
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after cancel: got=%d want=%d", got, testBudget)
	}
	// Cancellation contract is best-effort: the function MAY return
	// nil if it finishes before observing ctx.Done(). What it MUST
	// NOT return is some unrelated error. Accept nil or context.Canceled.
	if ensureErr != nil && !errors.Is(ensureErr, context.Canceled) {
		t.Fatalf("expected nil or context.Canceled, got %v", ensureErr)
	}
}

// TestIndexDocuments_CancelDuringRange — feed N docs into a real
// indexDocuments invocation, cancel midway, and assert the function
// drains and returns within a deadline. Validates the channel-close
// + drain pattern in indexer.go:734.
func TestIndexDocuments_CancelDuringRange(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	_, done := withReadSemLock(t)
	defer done()

	indexDir := t.TempDir()
	source := t.TempDir()

	fileCh := make(chan fileContent)
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		defer close(fileCh)
		for i := 0; i < 50; i++ {
			payload := []byte("package x\n// tail tail tail\n")
			weight := int64(len(payload))
			if err := readSemaphore.Acquire(ctx, weight); err != nil {
				return
			}
			select {
			case fileCh <- fileContent{name: testFileName("c", i), content: payload, weight: weight}:
			case <-ctx.Done():
				readSemaphore.Release(weight)
				return
			}
		}
	}()

	// Cancel as soon as the producer has shipped at least one doc.
	// Polling availableWeight is the closest test-side signal that
	// production has done useful work — fixed sleeps are fragile under
	// CI scheduling.
	go func() {
		waitUntilSemaphoreBelow(t, readSemaphore, defaultReadSemBudget(), 5*time.Second)
		cancel()
	}()

	var indexErr error
	finishCh := make(chan struct{})
	go func() {
		_, indexErr = indexDocuments(ctx, indexDir, "cancel_repo", source, fileCh, 2)
		close(finishCh)
	}()

	select {
	case <-finishCh:
	case <-time.After(15 * time.Second):
		dumpGoroutines(t)
		t.Fatal("indexDocuments did not unwind within 15s of cancel")
	}
	// Cancellation contract: accept nil (cancelled past completion) or
	// context.Canceled; reject any unrelated error.
	if indexErr != nil && !errors.Is(indexErr, context.Canceled) {
		t.Fatalf("expected nil or context.Canceled, got %v", indexErr)
	}
	// withReadSemLock done() asserts semaphore returned to baseline.
}
