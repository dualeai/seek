package main

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestIndexDeltaDocuments_LargeBatchHoldsBudget checks a large delta in one
// process: indexDeltaDocuments holds all pre-acquired weight until Finish.
// With no concurrent reader, this is correct (not a deadlock), but memory
// holds the full payload. The delta guards bound the payload:
// readUncommittedCandidates (indexer.go) and changedFolderDocumentsFromManifest
// (folder_indexer.go) send a payload that reaches the window to the windowed
// indexDocuments.
//
// The test indexes 12 documents of 512 KiB with an 8 MiB budget. It checks
// that (a) the delta completes within the 30 s guard deadline, (b) the
// semaphore has its full budget after Finish, and (c) no goroutine leaks.
func TestIndexDeltaDocuments_LargeBatchHoldsBudget(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 8 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 30*time.Second)()

	indexDir := t.TempDir()
	source := t.TempDir()

	// Seed prior shard required by IsDelta.
	{
		seed := make(chan fileContent, 1)
		seed <- makeFileContent(t, "seed.go", []byte("package seed\nfunc Seed() {}\n"))
		close(seed)
		if _, err := indexDocuments(t.Context(), indexDir, "delta_large_repo", source, seed, 1); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Total = 6 MiB across 12 × 512 KiB docs — fits well within the
	// 8 MiB budget so all pre-Acquires succeed.
	const docCount = 12
	const docSize int64 = 512 * 1024
	docs := make([]fileContent, docCount)
	changed := make([]string, docCount)
	payload := bytes.Repeat([]byte("a"), int(docSize))
	for i := 0; i < docCount; i++ {
		name := fmt.Sprintf("dlarge_%d.go", i)
		docs[i] = makeFileContent(t, name, append([]byte("package x\n"), payload...))
		changed[i] = name
	}

	if _, err := indexDeltaDocuments(indexDir, "delta_large_repo", source, docs, indexParallelism(), changed); err != nil {
		t.Fatalf("indexDeltaDocuments: %v", err)
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak: got=%d want=%d", got, testBudget)
	}
}

// TestIndexDeltaDocuments_BlocksConcurrentReader checks that
// indexDeltaDocuments releases all weight in bulk after the Finish of its one
// builder. A concurrent Acquire for more weight than remains must wait until
// that release. The select at the end fails when the Acquire completes while
// the delta still runs, so it catches a change to a release per document.
func TestIndexDeltaDocuments_BlocksConcurrentReader(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 30*time.Second)()

	indexDir := t.TempDir()
	source := t.TempDir()

	// Seed prior shard so the delta builder has something to chain on.
	{
		seed := make(chan fileContent, 1)
		seed <- makeFileContent(t, "seed.go", []byte("package seed\nfunc Seed() {}\n"))
		close(seed)
		if _, err := indexDocuments(t.Context(), indexDir, "delta_pressure_repo", source, seed, 1); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Build a delta payload that consumes most of the budget.
	const docCount = 3
	const docSize int64 = 1 * 1024 * 1024 // 1 MiB each → 3 MiB total
	docs := make([]fileContent, docCount)
	changed := make([]string, docCount)
	payload := bytes.Repeat([]byte("a"), int(docSize))
	for i := 0; i < docCount; i++ {
		name := fmt.Sprintf("delta_%d.go", i)
		docs[i] = makeFileContent(t, name, append([]byte("package x\n"), payload...))
		changed[i] = name
	}

	acquireDone := make(chan error, 1)

	// Background concurrent Acquire — wants 2 MiB. The makeFileContent
	// calls above already pre-Acquired 3 MiB, so the remaining budget
	// is 1 MiB and this Acquire blocks immediately. It can only proceed
	// after indexDeltaDocuments returns its weight via the bulk-Release
	// after Finish.
	//
	// The channels acquireDone and deltaDone give the order of the two
	// events. Time stamps cannot: after a correct release after Finish, the
	// Acquire can complete before indexDeltaDocuments returns.
	goroutineStarted := make(chan struct{})
	go func() {
		close(goroutineStarted)
		if err := readSemaphore.Acquire(t.Context(), 2*1024*1024); err != nil {
			acquireDone <- err
			return
		}
		readSemaphore.Release(2 * 1024 * 1024)
		acquireDone <- nil
	}()
	<-goroutineStarted
	// Wait until the Acquire waits in the queue before indexDeltaDocuments
	// runs: with a waiter, TryAcquire fails, so availableWeight reads 0.
	// Otherwise the goroutine could run after the delta finishes, succeed at
	// once, and pass the order check falsely.
	waitUntilSemaphoreBelow(t, readSemaphore, 1, 5*time.Second)

	deltaDone := make(chan error, 1)
	go func() {
		_, err := indexDeltaDocuments(indexDir, "delta_pressure_repo", source, docs, indexParallelism(), changed)
		deltaDone <- err
	}()

	select {
	case err := <-acquireDone:
		if err != nil {
			t.Fatalf("concurrent Acquire: %v", err)
		}
		select {
		case err := <-deltaDone:
			if err != nil {
				t.Fatalf("indexDeltaDocuments: %v", err)
			}
		default:
			err := <-deltaDone
			if err != nil {
				t.Fatalf("indexDeltaDocuments: %v", err)
			}
			t.Fatal("concurrent Acquire completed while delta indexing was still running")
		}
	case err := <-deltaDone:
		if err != nil {
			t.Fatalf("indexDeltaDocuments: %v", err)
		}
		select {
		case err := <-acquireDone:
			if err != nil {
				t.Fatalf("concurrent Acquire: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Acquire did not complete after delta returned its weight")
		}
	}

	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after delta: got=%d want=%d", got, testBudget)
	}

	// indexDeltaDocuments uses bulk-Release-after-Finish by design (the
	// payload size is bounded by the delta guards upstream). Therefore
	// a concurrent Acquire that exceeds the remaining budget MUST wait
	// for the delta to return its weight. The select above allows the
	// legitimate edge where Release happens after Finish but just before
	// the delta goroutine sends deltaDone; it fails only if Acquire wins
	// while delta indexing is still running.
	//
	// If indexDeltaDocuments is ever refactored to streaming-Release,
	// this test should move to an explicit early-unblock assertion.
}

// TestIndexDeltaDocuments_ReleasesOnNewBuilderError gives indexDeltaDocuments
// a regular file as indexDir, so NewBuilder fails before any Add. The early
// return of indexDeltaDocumentsWithRepository must release each pre-acquired
// weight once; withReadSemLock checks the weight at the end.
func TestIndexDeltaDocuments_ReleasesOnNewBuilderError(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	done := withReadSemLock(t)
	defer done()

	docs := []fileContent{
		makeFileContent(t, "ctrl1.go", []byte("package x\n")),
		makeFileContent(t, "ctrl2.go", []byte("package y\n")),
	}
	bogusFile := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(bogusFile, []byte("not-a-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := indexDeltaDocuments(bogusFile, "ctrl_delta_repo", t.TempDir(), docs, indexParallelism(), []string{"ctrl1.go", "ctrl2.go"})
	if err == nil {
		t.Fatal("expected NewBuilder failure with regular-file indexDir")
	}
}
