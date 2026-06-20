package main

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestIndexDeltaDocuments_LargeBatchHoldsBudget — documents the
// single-process behaviour: indexDeltaDocuments still holds all
// pre-Acquired weight until Finish(). With one process and no
// concurrent reader, this is correct (not a deadlock) but it does
// pin RSS to the slice's full byte count. The bound is enforced by
// the windowed-fallback guard in tryUncommittedDelta (indexer.go) —
// large dirty payloads route to the windowed indexDocuments instead.
//
// Asserts: (a) delta completes in bounded time, (b) the semaphore
// returns to baseline after Finish, (c) peak NumGoroutine stays
// modest (no leaked workers).
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

	if _, err := indexDeltaDocuments(indexDir, "delta_large_repo", source, docs, shardMax, changed); err != nil {
		t.Fatalf("indexDeltaDocuments: %v", err)
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak: got=%d want=%d", got, testBudget)
	}
}

// TestIndexDeltaDocuments_BlocksConcurrentReader — indexDeltaDocuments
// uses single-builder bulk-Release-after-Finish. A concurrent Acquire
// requesting weight beyond the remaining budget must wait until the
// terminal Finish releases the held weight. The hard ordering assertion
// at the bottom (acquireDone NOT Before deltaReturned) catches a
// regression that flipped to streaming Release.
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
	// Use channels to establish happens-before instead of time.Now()
	// ordering — otherwise a scheduling-delay race could let the
	// goroutine record acquireDone BEFORE the main goroutine records
	// deltaReturned even if Release fired during Finish.
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
	// Ensure the Acquire is actively pending (semaphore exhausted by
	// pre-Acquired delta weight + the new 2 MiB request) before
	// indexDeltaDocuments runs. Otherwise the goroutine could be
	// scheduled AFTER delta finishes, succeed instantly, and
	// false-pass the timing assertion.
	waitUntilSemaphoreBelow(t, readSemaphore, 1, 5*time.Second)

	deltaDone := make(chan error, 1)
	go func() {
		_, err := indexDeltaDocuments(indexDir, "delta_pressure_repo", source, docs, shardMax, changed)
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

// TestIndexDeltaDocuments_ReleasesOnNewBuilderError — when NewBuilder
// fails before any Add, every pre-Acquired weight in the input slice
// must still be Released exactly once via the early-return path in
// indexDeltaDocuments.
func TestIndexDeltaDocuments_ReleasesOnNewBuilderError(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	_, done := withReadSemLock(t)
	defer done()

	docs := []fileContent{
		makeFileContent(t, "ctrl1.go", []byte("package x\n")),
		makeFileContent(t, "ctrl2.go", []byte("package y\n")),
	}
	bogusFile := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(bogusFile, []byte("not-a-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := indexDeltaDocuments(bogusFile, "ctrl_delta_repo", t.TempDir(), docs, shardMax, []string{"ctrl1.go", "ctrl2.go"})
	if err == nil {
		t.Fatal("expected NewBuilder failure with regular-file indexDir")
	}
}
