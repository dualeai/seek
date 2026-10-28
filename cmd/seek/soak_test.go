package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSoak_LargeFolderStaysFresh makes a 4 GiB sparse folder (1024 files of
// 4 MiB) and runs ensureFolderCorpusFresh on it 1000 times. The first call
// builds the index, and each later call finds it current. It reads the heap
// after each call.
//
// It asserts:
//   - each call ends with no error, and the index has a shard,
//   - the live heap after a GC grows by at most 8 MiB from call 100 to the
//     end,
//   - the HeapAlloc peak stays at or below 1500 MiB (a leak guard),
//   - readSemaphore has its start weight again at the end.
func TestSoak_LargeFolderStaysFresh(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	before := availableWeight(readSemaphore)

	root := t.TempDir()
	const totalFiles = 1024
	const fileSize int64 = 4 * 1024 * 1024 // 4 MiB, so 4 GiB in all
	for i := range totalFiles {
		writeSparseFile(t, filepath.Join(root, fmt.Sprintf("f%05d.bin", i)), fileSize)
	}

	plan := planSynthCorpus(t, root)

	const iterations = 1000
	var peakHeap uint64
	liveHeap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	var warmHeap uint64
	for i := range iterations {
		if _, err := ensureFolderCorpusFresh(t.Context(), plan); err != nil {
			t.Fatalf("ensureFolderCorpusFresh %d: %v", i, err)
		}
		if i == 99 {
			warmHeap = liveHeap()
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		peakHeap = max(peakHeap, ms.HeapAlloc)
	}
	endHeap := liveHeap()
	t.Logf("soak: %d calls, peak heap %d MiB, live heap after call 100 %d KiB, at the end %d KiB", iterations, peakHeap/1024/1024, warmHeap/1024, endHeap/1024)
	// The live heap after a GC must not grow over the warm calls. A ceiling of
	// 8 MiB over 900 calls finds a leak of about 9 KiB per call.
	const liveGrowthCeiling = 8 << 20
	if endHeap > warmHeap+liveGrowthCeiling {
		t.Errorf("live heap grew from %d KiB to %d KiB over %d warm calls", warmHeap/1024, endHeap/1024, iterations-100)
	}
	if n := repositoryShardCount(plan.indexDir, folderRepoName(plan)); n == 0 {
		t.Fatal("no shards produced")
	}

	// This ceiling is a leak guard, not a resource limit: low resource use is
	// no goal of Seek. A leak grows the heap with each call and passes the
	// ceiling. A large working set stays below it.
	const heapCeiling = 1500 * 1024 * 1024
	if peakHeap > heapCeiling {
		t.Errorf("peak HeapAlloc %d MiB > ceiling %d MiB", peakHeap/1024/1024, heapCeiling/1024/1024)
	}
	if got := availableWeight(readSemaphore); got != before {
		t.Errorf("semaphore leak: before=%d after=%d", before, got)
	}
}
