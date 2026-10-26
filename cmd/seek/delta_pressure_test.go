package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
)

// TestIndexDeltaDocuments_LargeBatchHoldsBudget checks a large delta in one
// process. indexDeltaDocuments holds the weight of every document until
// Finish, so memory holds the full payload. The delta guards bound that
// payload: when it reaches the read window, readUncommittedCandidates
// (indexer.go) and changedFolderDocumentsFromManifest (folder_indexer.go)
// return errDeltaPayloadExceedsWindow, and the caller builds in full.
//
// The test indexes 12 documents of 512 KiB with an 8 MiB budget. It checks
// that the delta completes, that the semaphore has its full budget after
// Finish, and that no goroutine leaks.
func TestIndexDeltaDocuments_LargeBatchHoldsBudget(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 8 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

	indexDir := t.TempDir()
	source := t.TempDir()

	// A delta Builder (IsDelta) needs a prior shard.
	{
		seed := make(chan fileContent, 1)
		seed <- makeFileContent(t, "seed.go", []byte("package seed\nfunc Seed() {}\n"))
		close(seed)
		if _, err := indexDocuments(t.Context(), indexDir, "delta_large_repo", source, seed, 1); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// The 12 documents of 512 KiB hold 6 MiB, below the 8 MiB budget, so each
	// Acquire of makeFileContent returns at once.
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
// indexDeltaDocuments keeps the weight of its documents until Finish. A
// concurrent Acquire for more weight than remains waits for that release.
// When the Acquire returns, its goroutine counts the published shards. Finish
// renames the delta shards into place. So a delta that releases each document
// at Add lets the Acquire return before any delta shard exists.
func TestIndexDeltaDocuments_BlocksConcurrentReader(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

	synctest.Test(t, func(t *testing.T) {
		indexDir := t.TempDir()
		source := t.TempDir()

		// A delta Builder (IsDelta) needs a prior shard.
		{
			seed := make(chan fileContent, 1)
			seed <- makeFileContent(t, "seed.go", []byte("package seed\nfunc Seed() {}\n"))
			close(seed)
			if _, err := indexDocuments(t.Context(), indexDir, "delta_pressure_repo", source, seed, 1); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		seedShards := repositoryShardCount(indexDir, "delta_pressure_repo")

		// Three documents of 1 MiB take 3 MiB of the 4 MiB budget.
		const docCount = 3
		const docSize int64 = 1 * 1024 * 1024
		docs := make([]fileContent, docCount)
		changed := make([]string, docCount)
		payload := bytes.Repeat([]byte("a"), int(docSize))
		for i := 0; i < docCount; i++ {
			name := fmt.Sprintf("delta_%d.go", i)
			docs[i] = makeFileContent(t, name, append([]byte("package x\n"), payload...))
			changed[i] = name
		}

		// The Acquire of 2 MiB waits, because 1 MiB remains.
		shardsAtAcquire := make(chan int, 1)
		go func() {
			if err := readSemaphore.Acquire(t.Context(), 2*1024*1024); err != nil {
				t.Errorf("concurrent Acquire: %v", err)
				shardsAtAcquire <- seedShards
				return
			}
			shards := repositoryShardCount(indexDir, "delta_pressure_repo")
			readSemaphore.Release(2 * 1024 * 1024)
			shardsAtAcquire <- shards
		}()
		// In the bubble, synctest.Wait returns when the Acquire waits in the
		// semaphore queue.
		synctest.Wait()

		if _, err := indexDeltaDocuments(indexDir, "delta_pressure_repo", source, docs, indexParallelism(), changed); err != nil {
			t.Fatalf("indexDeltaDocuments: %v", err)
		}
		if shards := <-shardsAtAcquire; shards <= seedShards {
			t.Fatalf("the concurrent Acquire returned with %d shard(s), the %d of the seed: the delta released its weight before Finish", shards, seedShards)
		}
		if got := availableWeight(readSemaphore); got != testBudget {
			t.Fatalf("semaphore leak after delta: got=%d want=%d", got, testBudget)
		}
	})
}

// TestIndexDeltaDocuments_ReleasesOnBuilderError checks that a delta whose
// Builder fails releases each acquired weight once; withReadSemLock checks the
// weight at the end. With no repository name, NewBuilder fails before any Add,
// and no Builder opens. With a regular file as indexDir, the Builder opens,
// and Finish fails to write the shard.
func TestIndexDeltaDocuments_ReleasesOnBuilderError(t *testing.T) {
	requireTools(t)
	bogusFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bogusFile, []byte("not-a-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		indexDir   string
		repoName   string
		wantOpened bool
	}{
		{name: "NewBuilder", indexDir: t.TempDir(), repoName: ""},
		{name: "Finish", indexDir: bogusFile, repoName: "ctrl_delta_repo", wantOpened: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := withReadSemLock(t)
			defer done()
			docs := []fileContent{
				makeFileContent(t, "ctrl1.go", []byte("package x\n")),
				makeFileContent(t, "ctrl2.go", []byte("package y\n")),
			}
			opened, err := indexDeltaDocuments(tc.indexDir, tc.repoName, t.TempDir(), docs, indexParallelism(), []string{"ctrl1.go", "ctrl2.go"})
			if err == nil || opened != tc.wantOpened {
				t.Fatalf("indexDeltaDocuments opened=%t error=%v, want opened=%t and an error", opened, err, tc.wantOpened)
			}
		})
	}
}
