package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

// TestEnsureFolderCorpusFresh_CancelAfterReadActivity starts an indexing
// pass and cancels it while a reader holds its read weight: the first file
// open waits for the cancel. Every goroutine must stop, and the semaphore
// must return to its starting balance. The pass can return nil or
// context.Canceled.
//
// goleak.VerifyNone catches any goroutine that stays blocked on Acquire, a
// channel send, or builder I/O. It ignores only the goroutines that ran
// before the test (goleak.IgnoreCurrent).
func TestEnsureFolderCorpusFresh_CancelAfterReadActivity(t *testing.T) {
	requireTools(t)
	// IgnoreCurrent records the goroutines when this defer statement runs,
	// at the top of the test, so a leaked reader is a new goroutine that
	// VerifyNone reports. The test does not use
	// IgnoreTopFunction("internal/poll.runtime_pollWait"): it would hide a
	// leaked reader that stays blocked in os.(*File).Read on slow storage.
	// IgnoreCurrent already records the goroutines of the test runner.
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

	// A reader opens a file after it takes its weight. The first open waits
	// until the test cancels, and the other opens wait behind it.
	reading, canceled := make(chan struct{}), make(chan struct{})
	var first sync.Once
	realOpen := folderOpenFile
	t.Cleanup(func() { folderOpenFile = realOpen })
	folderOpenFile = func(path string) (*os.File, error) {
		first.Do(func() {
			close(reading)
			<-canceled
		})
		return realOpen(path)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var ensureErr error
	done := make(chan struct{})
	go func() {
		_, ensureErr = ensureFolderCorpusFresh(ctx, plan)
		close(done)
	}()
	select {
	case <-reading:
	case <-done:
		t.Fatalf("the pass ended before it read a file: %v", ensureErr)
	}
	cancel()
	close(canceled)
	<-done

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

	// The worker opens the file after it takes its weight. No receiver can
	// accept the document.
	opened := make(chan struct{})
	realOpen := folderOpenFile
	t.Cleanup(func() { folderOpenFile = realOpen })
	folderOpenFile = func(path string) (*os.File, error) {
		close(opened)
		return realOpen(path)
	}

	ctx, cancel := context.WithCancel(t.Context())
	// The build gives the reader a failure function; it cancels the read.
	readCtx, fail := context.WithCancelCause(ctx)
	defer fail(nil)
	done := make(chan struct{})
	go func() {
		readOneFolderFileStreaming(readCtx, candidate, out, fail)
		close(done)
	}()
	select {
	case <-opened:
	case <-done:
		t.Fatal("the reader stopped before it opened the file")
	}
	cancel()
	<-done
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
	requireTools(t)
	done := withReadSemLock(t)
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
	}
	cancel()
	close(transferSecond)

	if indexErr := <-indexDone; !errors.Is(indexErr, context.Canceled) {
		t.Fatalf("indexDocuments error=%v, want context.Canceled", indexErr)
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("producer error: %v", err)
	}
	// withReadSemLock done() asserts semaphore returned to baseline.
}
