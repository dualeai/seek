package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEnsureFolderCorpusFresh_BeyondInFlightBudget — synthesise a
// folder whose cumulative selected bytes exceed the test-local
// readSemaphore budget, then index via the real folder pipeline.
// Window rotation must Release per-window so reading proceeds; the
// goroutineLeakGuard trips its watchdog if rotation regresses.
//
// Sparse files keep CI cheap: testing.Short() routes writeRandomFolder
// through os.Truncate. Lstat.Size drives the Acquire weight, so
// behaviour matches dense files.
func TestEnsureFolderCorpusFresh_BeyondInFlightBudget(t *testing.T) {
	const testBudget int64 = 4 * 1024 * 1024  // 4 MiB
	const fileSize int64 = 512 * 1024         // 512 KiB
	const totalBytes int64 = 12 * 1024 * 1024 // 12 MiB > budget
	testFolderIndexUnderBudgetPressure(
		t,
		testBudget,
		totalBytes,
		fileSize,
		"BEYOND_BUDGET_BEACON_F00DCAFE",
	)
}

// TestEnsureFolderCorpusFresh_AtBudgetBoundary — cumulative content
// twice the budget. Every Acquire fits but only N-1 readers can hold
// weight at once, so the Nth must wait for a Release from the consumer
// side. Beacon search proves cross-window searchability.
func TestEnsureFolderCorpusFresh_AtBudgetBoundary(t *testing.T) {
	const testBudget int64 = 4 * 1024 * 1024
	const fileSize int64 = 1024 * 1024       // 1 MiB
	const totalBytes int64 = 8 * 1024 * 1024 // 2× budget
	testFolderIndexUnderBudgetPressure(
		t,
		testBudget,
		totalBytes,
		fileSize,
		"AT_BUDGET_BOUNDARY_BEACON_BADC0DE",
	)
}

// TestEnsureFolderCorpusFresh_ManySmallFilesOverBudget — verifies the
// fix handles a different size distribution. Many small files force
// many cheap Acquire/Release cycles instead of a handful of large ones.
// Beacon search proves cross-window searchability under high-fanout.
func TestEnsureFolderCorpusFresh_ManySmallFilesOverBudget(t *testing.T) {
	const testBudget int64 = 2 * 1024 * 1024 // 2 MiB
	const fileSize int64 = 64 * 1024         // 64 KiB
	const totalBytes int64 = 8 * 1024 * 1024 // 8 MiB > 4× budget
	testFolderIndexUnderBudgetPressure(
		t,
		testBudget,
		totalBytes,
		fileSize,
		"MANY_SMALL_FILES_BEACON_FEEDFACE",
	)
}

func testFolderIndexUnderBudgetPressure(
	t *testing.T,
	testBudget int64,
	totalBytes int64,
	fileSize int64,
	beacon string,
) {
	t.Helper()
	defer setupPressureTest(t, testBudget)()

	root := writeRandomFolder(t, totalBytes, fileSize)
	beaconPath := filepath.Join(root, "z_beacon.txt")
	if err := os.WriteFile(beaconPath, []byte(beacon+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := planSynthCorpus(t, root)

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	state, err := ensureFolderCorpusFresh(ctx, plan)
	if err != nil {
		t.Fatalf("ensureFolderCorpusFresh: %v", err)
	}
	if state != corpusSearchable {
		t.Fatalf("folder state=%d, want searchable", state)
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak: got=%d want=%d", got, testBudget)
	}
	results, err := searchPlannedCorpusForTest(ctx, plan, beacon)
	if err != nil {
		t.Fatalf("post-index search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("beacon not found after indexing")
	}
}

// TestEnsureFolderCorpusFresh_SparseFiles — sparse files via Truncate
// reproduce the deadlock with ~zero disk usage and ~zero read cost.
// Useful sanity check that the fix isn't accidentally relying on real
// I/O timing. Includes a beacon search to verify cross-window shard
// chain produces queryable output even when reads return zero bytes.
func TestEnsureFolderCorpusFresh_SparseFiles(t *testing.T) {
	const testBudget int64 = 4 * 1024 * 1024
	defer setupPressureTest(t, testBudget)()

	root := t.TempDir()
	const fileSize int64 = 1024 * 1024
	const totalFiles = 10
	for i := 0; i < totalFiles; i++ {
		writeSparseFile(t, root+"/"+testFileName("sparse", i), fileSize)
	}
	// Beacon written as a normal (non-sparse) file last in scanner
	// order so it lands in the final shard window. Asserts the
	// rotated shard chain accepts mixed sparse+real content.
	const beacon = "SPARSE_BEACON_DEADCAFE"
	beaconPath := filepath.Join(root, "z_beacon.txt")
	if err := os.WriteFile(beaconPath, []byte(beacon+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planSynthCorpus(t, root)

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("ensureFolderCorpusFresh: %v", err)
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak: got=%d want=%d", got, testBudget)
	}
	results, err := searchPlannedCorpusForTest(ctx, plan, beacon)
	if err != nil {
		t.Fatalf("post-index search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("sparse beacon not found post-index; shards may be corrupt")
	}
}
