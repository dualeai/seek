package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestIndexDocuments_ReadsBeyondTheBudget drives the real indexDocuments
// consumer with a hand-built fileCh whose cumulative weight exceeds the
// readSemaphore budget. The consumer releases the weight of a normal document
// when Builder.Add returns, so reading proceeds. A regression that keeps the
// weight until the terminal Finish wedges, and the goroutineLeakGuard panics
// with a stack dump.
//
// Uses swapReadSemaphoreForTest so the fixture fits in ~12 MiB of
// synthesised payload instead of >600 MiB.
func TestIndexDocuments_ReadsBeyondTheBudget(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	const chunk int64 = 1 * 1024 * 1024      // 1 MiB
	const totalDocs = 12                     // 12 MiB cumulative > 4 MiB budget

	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	defer goroutineLeakGuard(t, 30*time.Second)()

	indexDir := t.TempDir()
	source := t.TempDir()

	fileCh := make(chan fileContent)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

	// Producer that mirrors readOneFolderFileStreaming: Acquire weight
	// then send. Exits cleanly on ctx.Done so the goroutine guard does
	// not flag a leak on the deadlock-failure path.
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

	done := make(chan error, 1)
	go func() {
		_, err := indexDocuments(ctx, indexDir, "pressure_repo", source, fileCh, 2)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("indexDocuments: %v", err)
		}
	case <-ctx.Done():
		dumpGoroutines(t)
		t.Fatal("DEADLOCK: indexDocuments did not return within deadline")
	}

	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after indexDocuments: got=%d want=%d", got, testBudget)
	}
	// With 12 MiB of content and the 10 MiB shardMax, the build makes at
	// least two shards.
	if n := repositoryShardCount(indexDir, "pressure_repo"); n < 2 {
		t.Fatalf("expected >= 2 shards, got %d", n)
	}
}

// TestIndexDocuments_ReleasesWeightOnFinishError checks that a Finish error
// still releases every acquired read weight. It forces a Finish error:
// it writes into a read-only indexDir, with content large enough to make
// Zoekt flush a shard during Add.
//
// TODO: the test skips when err is nil and accepts any other error. A
// narrower check needs a Builder factory to inject (the shard logic is
// inline in indexDocuments), or a test for fs.ErrPermission, which depends on
// how Zoekt wraps its errors.
func TestIndexDocuments_ReleasesWeightOnFinishError(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write to read-only dirs; skipping Finish-error test")
	}
	done := withReadSemLock(t)
	defer done()

	indexDir := t.TempDir()
	if err := os.Chmod(indexDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(indexDir, 0o755) })
	source := t.TempDir()

	// Build a doc that will trigger an internal flush. shardMax=10 MiB,
	// so a single 11 MiB doc forces flush inside Add → Zoekt's buildShard
	// tries to write into a read-only dir → error propagates back through
	// Add (or surfaces at Finish).
	payload := make([]byte, 11*1024*1024)
	for i := range payload {
		payload[i] = byte('p')
	}
	ch := make(chan fileContent, 1)
	ch <- makeFileContent(t, "big.go", payload)
	close(ch)

	_, err := indexDocuments(t.Context(), indexDir, "finishfail_repo", source, ch, 1)
	if err == nil {
		t.Skip("Finish did not error on read-only indexDir; precondition for this test not reached")
	}
	// withReadSemLock done() asserts semaphore returned to baseline —
	// proves the err-path Release fired despite the error.
}

// TestIndexDocuments_NoConsumerWeightDoubleRelease — invariant test:
// running indexDocuments back-to-back from the same baseline must end
// at the same baseline. Catches a future fix that streaming-Releases
// AND still calls bulk Release at the end.
func TestIndexDocuments_NoConsumerWeightDoubleRelease(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
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
