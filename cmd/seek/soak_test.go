//go:build soak

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestSoak_LargeFolderForFiveMinutes is a long test that runs only with the
// soak build tag and SEEK_SOAK=1. It makes a 4 GiB sparse folder (1024 files
// of 4 MiB) and runs ensureFolderCorpusFresh in a loop for 5 minutes. It
// reads the memory statistics every 5 seconds.
//
// It asserts:
//   - the HeapAlloc peak stays at or below 1500 MiB (a leak guard),
//   - the iteration count increases at least once in each 2 minutes
//     (progressDeadline),
//   - at least one iteration completes,
//   - readSemaphore has its start weight again at the end.
//
// Run it with:
//
//	SEEK_SOAK=1 go test -tags=soak ./cmd/seek -run TestSoak_LargeFolderForFiveMinutes -timeout=15m
func TestSoak_LargeFolderForFiveMinutes(t *testing.T) {
	if os.Getenv("SEEK_SOAK") != "1" {
		t.Skip("set SEEK_SOAK=1 to run soak test")
	}
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	before := availableWeight(readSemaphore)

	root := t.TempDir()
	const totalFiles = 1024
	const fileSize int64 = 4 * 1024 * 1024 // 4 MiB → 4 GiB total
	for i := 0; i < totalFiles; i++ {
		writeSparseFile(t, filepath.Join(root, fmt.Sprintf("f%05d.bin", i)), fileSize)
	}

	plan := planSynthCorpus(t, root)

	const soakDuration = 5 * time.Minute
	const sampleInterval = 5 * time.Second
	// The first build of the 4 GiB sparse folder is slow (one ctags process
	// per shard); the later iterations find the index current. The 2 minute
	// deadline lets the first build finish and still finds a loop that stops.
	const progressDeadline = 2 * time.Minute

	ctx, cancel := context.WithTimeout(t.Context(), soakDuration+1*time.Minute)
	defer cancel()

	var iterations atomic.Int64
	doneSoak := make(chan struct{})
	go func() {
		defer close(doneSoak)
		deadline := time.Now().Add(soakDuration)
		for time.Now().Before(deadline) {
			if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
				t.Errorf("ensureFolderCorpusFresh: %v", err)
				return
			}
			iterations.Add(1)
			if ctx.Err() != nil {
				return
			}
		}
	}()

	var peakHeap uint64
	lastProgressIters := int64(0)
	lastProgressTime := time.Now()
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-doneSoak:
			goto end
		case <-ctx.Done():
			t.Fatal("soak context deadline exceeded")
		case <-ticker.C:
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peakHeap {
				peakHeap = ms.HeapAlloc
			}
			iter := iterations.Load()
			if iter > lastProgressIters {
				lastProgressIters = iter
				lastProgressTime = time.Now()
			} else if time.Since(lastProgressTime) > progressDeadline {
				dumpGoroutines(t)
				t.Fatalf("no forward progress in %v (iter stuck at %d)", progressDeadline, iter)
			}
			t.Logf("soak: iter=%d heap=%d MiB peak=%d MiB", iter, ms.HeapAlloc/1024/1024, peakHeap/1024/1024)
		}
	}

end:
	// A leak guard, not a resource limit (low resource use is no goal of
	// Seek): a leak grows the heap with each iteration and passes the
	// ceiling; a large working set stays below it.
	const heapCeiling = 1500 * 1024 * 1024
	if peakHeap > heapCeiling {
		t.Errorf("peak HeapAlloc %d MiB > ceiling %d MiB", peakHeap/1024/1024, heapCeiling/1024/1024)
	}
	if iterations.Load() == 0 {
		t.Error("soak loop never completed a single iteration")
	}
	if got := availableWeight(readSemaphore); got != before {
		t.Errorf("semaphore leak: before=%d after=%d", before, got)
	}
}
