package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
)

// TestIndexDocuments_ReadsBeyondTheBudget sends indexDocuments documents whose
// total weight is three times the readSemaphore budget. The consumer releases
// the weight of a normal document when Builder.Add returns, so the reads go
// on. A consumer that keeps the weight until Finish blocks every goroutine of
// the synctest bubble, and synctest.Test fails with a deadlock.
//
// swapReadSemaphoreForTest sets a 4 MiB budget, so 12 MiB of content is
// enough. The default budget is at least 600 MiB.
func TestIndexDocuments_ReadsBeyondTheBudget(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	const chunk int64 = 1 * 1024 * 1024      // 1 MiB
	const totalDocs = 12                     // 12 MiB cumulative > 4 MiB budget

	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	defer goroutineLeakGuard(t)()

	synctest.Test(t, func(t *testing.T) {
		indexDir := t.TempDir()
		source := t.TempDir()

		fileCh := make(chan fileContent)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// The producer takes the weight and then sends, as
		// readOneFolderFileStreaming does. When indexDocuments fails, the
		// deferred cancel stops it before the goroutine guard counts.
		go func() {
			defer close(fileCh)
			payload := bytes.Repeat([]byte("a"), int(chunk))
			for i := 0; i < totalDocs; i++ {
				if err := readSemaphore.Acquire(ctx, chunk); err != nil {
					return
				}
				select {
				case fileCh <- fileContent{
					name:    fmt.Sprintf("d%03d.go", i),
					content: append([]byte("package x\n"), payload...),
					weight:  chunk,
				}:
				case <-ctx.Done():
					readSemaphore.Release(chunk)
					return
				}
			}
		}()

		if _, err := indexDocuments(ctx, indexDir, "pressure_repo", source, fileCh, 2); err != nil {
			t.Fatalf("indexDocuments: %v", err)
		}

		if got := availableWeight(readSemaphore); got != testBudget {
			t.Fatalf("semaphore leak after indexDocuments: got=%d want=%d", got, testBudget)
		}
		// With 12 MiB of content and the 10 MiB shardMax, the build makes at
		// least two shards.
		if n := repositoryShardCount(indexDir, "pressure_repo"); n < 2 {
			t.Fatalf("expected >= 2 shards, got %d", n)
		}
	})
}

// TestIndexDocuments_ReleasesWeightOnShardWriteError checks that a shard write
// error still releases every acquired read weight. The indexDir is read-only,
// so Zoekt cannot write the shard of the document.
func TestIndexDocuments_ReleasesWeightOnShardWriteError(t *testing.T) {
	requireTools(t)
	done := withReadSemLock(t)
	defer done()

	indexDir := t.TempDir()
	if err := os.Chmod(indexDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(indexDir, 0o755) })
	probe := filepath.Join(indexDir, "probe")
	if err := os.WriteFile(probe, nil, 0o644); err == nil {
		_ = os.Remove(probe)
		t.Fatalf("the test user adds a file to the mode-0555 folder %s, so the build cannot fail", indexDir)
	}
	source := t.TempDir()

	// One document of 11 MiB is above shardMax (10 MiB), so Add flushes it to
	// a shard of its own. The shard write fails in the read-only indexDir, and
	// Add returns the error.
	payload := make([]byte, 11*1024*1024)
	for i := range payload {
		payload[i] = byte('p')
	}
	ch := make(chan fileContent, 1)
	ch <- makeFileContent(t, "big.go", payload)
	close(ch)

	_, err := indexDocuments(t.Context(), indexDir, "finishfail_repo", source, ch, 1)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("indexDocuments error=%v, want the permission error of the shard write", err)
	}
	// done checks that the semaphore has its start weight: the error path
	// released the weight.
}

// TestIndexDocuments_NoConsumerWeightDoubleRelease runs indexDocuments three
// times from one start weight, and withReadSemLock checks that the weight
// ends there. A consumer that releases a weight at Add and again after Finish
// fails it.
func TestIndexDocuments_NoConsumerWeightDoubleRelease(t *testing.T) {
	requireTools(t)
	done := withReadSemLock(t)
	defer done()

	indexDir := t.TempDir()
	source := t.TempDir()
	for i := 0; i < 3; i++ {
		ch := make(chan fileContent, 2)
		ch <- makeFileContent(t, fmt.Sprintf("a%d.go", i), []byte("package a\n"))
		ch <- makeFileContent(t, fmt.Sprintf("b%d.go", i), []byte("package b\n"))
		close(ch)
		if _, err := indexDocuments(t.Context(), indexDir, fmt.Sprintf("rep%d", i), source, ch, 1); err != nil {
			t.Fatalf("indexDocuments iter %d: %v", i, err)
		}
	}
}
