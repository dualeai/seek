package main

import (
	"context"
	"testing"
	"time"
)

// TestSequentialCorpora_SemaphoreDrainsBetweenPlans — process plans
// sequentially. Every plan must return the global semaphore to its
// starting balance — otherwise plan N+1 starts with reduced budget
// and may wedge once cumulative > remaining.
func TestSequentialCorpora_SemaphoreDrainsBetweenPlans(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	const fileSize int64 = 512 * 1024
	const totalPerCorpus int64 = 8 * 1024 * 1024 // 2× budget per corpus

	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 90*time.Second)()

	for _, label := range []string{"A", "B"} {
		root := writeRandomFolder(t, totalPerCorpus, fileSize)
		plan := planSynthCorpus(t, root)

		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		_, err := ensureFolderCorpusFresh(ctx, plan)
		cancel()
		if err != nil {
			t.Fatalf("corpus %s ensureFolderCorpusFresh: %v", label, err)
		}
		// Indexing-happened guard: a no-op regression would still
		// pass the leak check below. Asserting shard count rules
		// out the silent-success class.
		if n := repositoryShardCount(plan.indexDir, folderRepoName(plan)); n == 0 {
			t.Fatalf("corpus %s: no shards produced", label)
		}
		if got := availableWeight(readSemaphore); got != testBudget {
			t.Fatalf("semaphore leak after corpus %s: got=%d want=%d", label, got, testBudget)
		}
	}
}

// TestSequentialCorpora_ThreeWayMix — large + empty + medium folders
// in sequence. Verifies the empty-corpus branch (zero-doc indexer
// path, indexer.go:768 cleanRepositoryShards) does not leak weight
// and that mixing sizes does not break drain between plans.
func TestSequentialCorpora_ThreeWayMix(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 60*time.Second)()

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
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		_, err := ensureFolderCorpusFresh(ctx, plan)
		cancel()
		if err != nil {
			t.Fatalf("corpus[%s]: %v", rc.label, err)
		}
		// Empty corpus produces 0 shards (zero-doc branch in
		// indexDocuments); large/medium must produce >= 1.
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
