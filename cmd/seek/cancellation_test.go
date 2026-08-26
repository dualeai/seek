package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestEnsureFolderCorpusFresh_CancelAfterReadActivity starts an indexing
// pass, waits until the semaphore probe observes read activity, then cancels it.
// Every goroutine must stop, and the semaphore must return to its
// starting balance.
//
// goleak.VerifyNone catches any goroutine that wedged on Acquire,
// channel send, or builder I/O. Ignored top-frames are stdlib
// background workers + this test's parent runtime.
func TestEnsureFolderCorpusFresh_CancelAfterReadActivity(t *testing.T) {
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
	// The TryAcquire probe reports activity. A queued waiter can also lower the
	// result, so this broad lifecycle test does not infer exact ownership.
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
	// Cancellation contract is best-effort: the function MAY return
	// nil if it finishes before observing ctx.Done(). What it MUST
	// NOT return is some unrelated error. Accept nil or context.Canceled.
	if ensureErr != nil && !errors.Is(ensureErr, context.Canceled) {
		t.Fatalf("expected nil or context.Canceled, got %v", ensureErr)
	}
}

// TestReadOneFolderFileStreaming_CancelWithoutReceiverReleasesWeight tests
// the worker that owns the Acquire-to-send transfer. With no receiver, the
// worker cannot transfer ownership and must release the weight after cancel.
func TestReadOneFolderFileStreaming_CancelWithoutReceiverReleasesWeight(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	content := []byte("blocked send ownership\n")
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	candidate := folderCandidate{name: "note.txt", path: path, size: int64(len(content))}
	out := make(chan fileContent)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		readOneFolderFileStreaming(ctx, candidate, out)
		close(done)
	}()
	// One worker and a file that fits the budget mean a lower probe result
	// identifies this worker's acquired weight. No receiver can accept it.
	waitUntilSemaphoreBelow(t, readSemaphore, testBudget, 5*time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		dumpGoroutines(t)
		t.Fatal("reader did not stop after cancellation")
	}
	select {
	case item := <-out:
		t.Fatalf("unexpected ownership transfer: %+v", item)
	default:
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after cancel: got=%d want=%d", got, testBudget)
	}
}

// TestIndexDocuments_CancelDuringRange transfers one document, holds a
// second document, cancels the context, then transfers the second one.
// This makes indexDocuments take its canceled range path and release
// both documents' semaphore weight.
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
	defer cancel()

	indexDone := make(chan error, 1)
	go func() {
		_, err := indexDocuments(ctx, indexDir, "cancel_repo", source, fileCh, 2)
		indexDone <- err
	}()

	secondReady := make(chan struct{})
	transferSecond := make(chan struct{})
	producerDone := make(chan error, 1)
	go func() {
		defer close(fileCh)
		payload := []byte("package x\n// cancellation marker\n")
		weight := int64(len(payload))

		if err := readSemaphore.Acquire(ctx, weight); err != nil {
			producerDone <- err
			return
		}
		select {
		case fileCh <- fileContent{name: "first.go", content: payload, weight: weight}:
		case <-ctx.Done():
			readSemaphore.Release(weight)
			producerDone <- ctx.Err()
			return
		}

		if err := readSemaphore.Acquire(ctx, weight); err != nil {
			producerDone <- err
			return
		}
		close(secondReady)
		<-transferSecond
		// The test cancels before this send. A successful send transfers
		// ownership of the acquired weight to indexDocuments.
		fileCh <- fileContent{name: "second.go", content: payload, weight: weight}
		producerDone <- nil
	}()

	select {
	case <-secondReady:
	case err := <-producerDone:
		t.Fatalf("producer stopped before second document was ready: %v", err)
	case <-time.After(10 * time.Second):
		dumpGoroutines(t)
		t.Fatal("producer did not prepare the second document")
	}
	cancel()
	close(transferSecond)

	select {
	case indexErr := <-indexDone:
		if !errors.Is(indexErr, context.Canceled) {
			t.Fatalf("indexDocuments error=%v, want context.Canceled", indexErr)
		}
	case <-time.After(15 * time.Second):
		dumpGoroutines(t)
		t.Fatal("indexDocuments did not unwind within 15s of cancel")
	}
	select {
	case err := <-producerDone:
		if err != nil {
			t.Fatalf("producer error: %v", err)
		}
	case <-time.After(5 * time.Second):
		dumpGoroutines(t)
		t.Fatal("producer did not stop")
	}
	// withReadSemLock done() asserts semaphore returned to baseline.
}
