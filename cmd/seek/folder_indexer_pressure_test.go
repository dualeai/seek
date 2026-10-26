package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureFolderCorpusFresh_BeyondInFlightBudget — synthesise a
// folder whose cumulative selected bytes exceed the test-local
// readSemaphore budget, then index via the real folder pipeline.
// The consumer must release weight before the terminal Finish so reading
// proceeds; else the build waits until the go test timeout, which prints
// each goroutine.
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
// twice the budget, in 1 MiB files. Each Acquire fits, but the budget holds
// at most four files at once, so the fifth reader waits for a Release from
// the consumer. The beacon search proves that the build is searchable.
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

// TestEnsureFolderCorpusFresh_ManySmallFilesOverBudget — 64 KiB files,
// four times the budget in all: many small Acquire and Release cycles
// instead of a few large ones. The beacon search proves that the build is
// searchable.
func TestEnsureFolderCorpusFresh_ManySmallFilesOverBudget(t *testing.T) {
	const testBudget int64 = 2 * 1024 * 1024 // 2 MiB
	const fileSize int64 = 64 * 1024         // 64 KiB
	const totalBytes int64 = 8 * 1024 * 1024 // 8 MiB = 4× budget
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

	ctx := t.Context()
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

// TestEnsureFolderCorpusFresh_SparseFiles — sparse files (writeSparseFile)
// put the same weight on the read budget as real bytes, with almost no disk
// space and read cost. So the test checks that the build does not wait
// forever on the read budget, with no dependency on real I/O time. A beacon
// search checks that the shards are searchable when the other files are
// mostly NUL bytes.
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
	// order so it lands in the final shard. Asserts the shard chain
	// accepts mixed sparse+real content.
	const beacon = "SPARSE_BEACON_DEADCAFE"
	beaconPath := filepath.Join(root, "z_beacon.txt")
	if err := os.WriteFile(beaconPath, []byte(beacon+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planSynthCorpus(t, root)

	ctx := t.Context()
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
