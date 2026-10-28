package main

import (
	"testing"
)

// TestSequentialCorpora_SemaphoreDrainsBetweenPlans builds two folder plans,
// one after the other, each with twice the read budget. Each build must give
// back all its read weight. Else the next plan starts with less budget and
// can block.
func TestSequentialCorpora_SemaphoreDrainsBetweenPlans(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	const fileSize int64 = 512 * 1024
	const totalPerCorpus int64 = 8 * 1024 * 1024 // twice the budget

	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

	for _, label := range []string{"A", "B"} {
		root := writeRandomFolder(t, totalPerCorpus, fileSize)
		plan := planSynthCorpus(t, root)

		if _, err := ensureFolderCorpusFresh(t.Context(), plan); err != nil {
			t.Fatalf("corpus %s ensureFolderCorpusFresh: %v", label, err)
		}
		// A shard proves that this corpus reached the indexing path before the
		// semaphore check.
		if n := repositoryShardCount(plan.indexDir, folderRepoName(plan)); n == 0 {
			t.Fatalf("corpus %s: no shards produced", label)
		}
		if got := availableWeight(readSemaphore); got != testBudget {
			t.Fatalf("semaphore leak after corpus %s: got=%d want=%d", label, got, testBudget)
		}
	}
}

// TestSequentialCorpora_ThreeWayMix processes large, empty, and medium folders
// in sequence. It checks that the known-empty path does not leak weight and
// that each plan releases its weight.
func TestSequentialCorpora_ThreeWayMix(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

	emptyRoot := t.TempDir()
	roots := []struct {
		path  string
		label string
	}{
		{writeRandomFolder(t, 7*1024*1024, 512*1024), "large"},
		{emptyRoot, "empty"},
		{writeRandomFolder(t, 6*1024*1024, 512*1024), "medium"},
	}
	for _, rc := range roots {
		plan := planSynthCorpus(t, rc.path)
		if _, err := ensureFolderCorpusFresh(t.Context(), plan); err != nil {
			t.Fatalf("corpus[%s]: %v", rc.label, err)
		}
		// The empty folder gives no shard: no document reaches the build. The
		// large and medium folders give one or more.
		n := repositoryShardCount(plan.indexDir, folderRepoName(plan))
		if rc.label == "empty" {
			if n != 0 {
				t.Fatalf("corpus[empty]: want 0 shards, got %d", n)
			}
		} else if n == 0 {
			t.Fatalf("corpus[%s]: no shards produced", rc.label)
		}
		if got := availableWeight(readSemaphore); got != testBudget {
			t.Fatalf("semaphore leak after corpus[%s]: got=%d want=%d", rc.label, got, testBudget)
		}
	}
}
