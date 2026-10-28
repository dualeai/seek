package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"go.uber.org/goleak"
)

// TestEnsureFolderCorpusFresh_CancelAfterReadActivity starts an indexing
// pass and cancels it while a reader holds its read weight. The first file of
// the folder has mode 000. Its reader takes its weight and then writes a debug
// line for the failed open, and the test cancels in that line. Every goroutine
// must stop, and the semaphore must return to its starting balance. The pass
// can return nil or context.Canceled.
//
// goleak.VerifyNone catches any goroutine that stays blocked on Acquire, a
// channel send, or builder I/O. It ignores only the goroutines that ran
// before the test (goleak.IgnoreCurrent).
func TestEnsureFolderCorpusFresh_CancelAfterReadActivity(t *testing.T) {
	requireTools(t)
	// IgnoreCurrent records the goroutines when this defer statement runs, so
	// a leaked reader is a new goroutine that VerifyNone reports. The test
	// does not use IgnoreTopFunction("internal/poll.runtime_pollWait"). It
	// would hide a leaked reader that stays blocked in os.(*File).Read on slow
	// storage.
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
	)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	root := writeRandomFolder(t, 12*1024*1024, 512*1024)
	// The walk sorts the files by name, so the readers take this file first.
	locked := filepath.Join(root, "a-locked.txt")
	writeFixtureFile(t, locked, "zqlockedreader\n")
	chmodUnreadableOrFatal(t, locked, 0)
	plan := planSynthCorpus(t, root)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := captureTestLogsWithAction(t, slog.LevelWarn, "Indexing an unreadable folder file by name only", cancel)
	_, ensureErr := ensureFolderCorpusFresh(ctx, plan)
	if ctx.Err() == nil {
		t.Fatalf("the pass did not read %s, so the test did not cancel: err=%v, logs=%v", locked, ensureErr, logs.Records())
	}

	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after cancel: got=%d want=%d", got, testBudget)
	}
	// A pass that ends before it sees ctx.Done() returns nil. No other error
	// is correct.
	if ensureErr != nil && !errors.Is(ensureErr, context.Canceled) {
		t.Fatalf("expected nil or context.Canceled, got %v", ensureErr)
	}
}

// TestReadOneFolderFileStreaming_CancelWithoutReceiverReleasesWeight tests
// the worker that owns the Acquire-to-send transfer. With no receiver, the
// worker cannot transfer ownership and must release the weight after cancel.
// The worker runs in a synctest bubble, so synctest.Wait returns when the
// worker waits at its send with its weight.
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
	synctest.Test(t, func(t *testing.T) {
		out := make(chan fileContent)
		ctx, cancel := context.WithCancel(t.Context())
		// The build gives the reader a failure function; it cancels the read.
		readCtx, fail := context.WithCancelCause(ctx)
		defer fail(nil)
		done := make(chan struct{})
		go func() {
			readOneFolderFileStreaming(readCtx, candidate, out, fail)
			close(done)
		}()
		// No receiver can accept the document.
		synctest.Wait()
		if held := testBudget - availableWeight(readSemaphore); held != int64(len(content)) {
			t.Fatalf("the worker holds %d bytes of weight at its send, want %d", held, len(content))
		}
		cancel()
		<-done
		select {
		case item := <-out:
			t.Fatalf("unexpected ownership transfer: %+v", item)
		default:
		}
	})
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
	// done (withReadSemLock) checks that the semaphore weight is back to its
	// start value.
}
