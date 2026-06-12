package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ===========================================================================
// Regression tests
//
// These tests verify that atomic shard swap and LOCK_SH search protection keep
// search results stable across reindex boundaries.
// ===========================================================================

// ---------------------------------------------------------------------------
// Fix #1: Atomic shard swap — no gap during re-indexing
//
// Planned Git corpus search refreshes dirty content through the normal
// freshness path before reading Zoekt shards, so there is no visible gap where
// the old uncommitted marker disappears without the new one being searchable.
// ---------------------------------------------------------------------------

func TestFix_NoShardGapDuringReindexing(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_aaa\n")

	// Create uncommitted edit
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// uncommitted_v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	results, err := runSeekInPlannedGitCorpus(ctx, "uncommitted_v1", paths, plan)
	if err != nil {
		t.Fatalf("initial search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("precondition: uncommitted_v1 must be findable after initial indexing")
	}

	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// uncommitted_v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err = runSeekInPlannedGitCorpus(ctx, "uncommitted_v2", paths, plan)
	if err != nil {
		t.Fatalf("post-reindex search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("FIX FAILED: uncommitted_v2 not findable after re-indexing")
	}

	results, err = runSeekInPlannedGitCorpus(ctx, "uncommitted_v1", paths, plan)
	if err != nil {
		t.Fatalf("old marker search failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("old uncommitted content should be tombstoned, got %d results", len(results))
	}
}

// ---------------------------------------------------------------------------
// Fix #2: acquireSearchLock polls until LOCK_EX is released
//
// acquireSearchLock is the lock primitive used by planned corpus search.
// Since the NB shared lock fails while any LOCK_EX is held, a searcher waits
// for an active indexer to finish before reading shards.
// ---------------------------------------------------------------------------

func TestFix_SharedLockBlocksSearchDuringIndexing(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "test.lock")

	// Acquire LOCK_EX (simulating an indexer)
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(holder); err != nil {
		t.Fatal(err)
	}

	var sharedAcquiredAt atomic.Int64
	done := make(chan struct{})

	// Goroutine tries to acquire shared lock — polls until LOCK_EX released
	go func() {
		defer close(done)
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return
		}
		defer func() {
			unlockFile(f)
			_ = f.Close()
		}()
		// This polls until LOCK_EX is released
		if err := acquireSearchLock(context.Background(), f); err != nil {
			return
		}
		sharedAcquiredAt.Store(time.Now().UnixNano())
	}()

	// Wait to confirm LOCK_SH is blocked
	time.Sleep(50 * time.Millisecond)
	if sharedAcquiredAt.Load() != 0 {
		unlockFile(holder)
		_ = holder.Close()
		t.Fatal("LOCK_SH should block while LOCK_EX is held")
	}

	// Release LOCK_EX and record when
	releaseTime := time.Now().UnixNano()
	unlockFile(holder)
	_ = holder.Close()

	// Wait for goroutine to complete
	<-done

	acquiredTime := sharedAcquiredAt.Load()
	if acquiredTime == 0 {
		t.Fatal("LOCK_SH should have been acquired after LOCK_EX release")
	}
	if acquiredTime < releaseTime {
		t.Fatal("LOCK_SH acquired before LOCK_EX was released")
	}
}

// ---------------------------------------------------------------------------
// Fix #3: Concurrent search+reindex stress — with LOCK_SH protection
//
// Searches through the planned-corpus boundary while lower-level reindexing
// updates shards and holds locks.
// ---------------------------------------------------------------------------

func TestFix_ConcurrentSearchDuringReindex_Stress(t *testing.T) {
	requireTools(t)

	const iterations = 20
	dir := initGitRepo(t, "stable.go", "package main\n// always_findable_fix_test\n")
	for iter := range iterations {
		name := fmt.Sprintf("changing_%d.go", iter)
		content := fmt.Sprintf("package main\n// committed_changing_%d\n", iter)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "add changing fixtures")

	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	// Initial index
	state := gitRepoStateIn(ctx, dir)
	currentState := gitCorpusStateHash(paths, state)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, currentState); err != nil {
		t.Fatalf("initial indexing: %v", err)
	}

	// Verify baseline
	results, err := searchPlannedCorpusForTest(ctx, plan, "always_findable_fix_test")
	if err != nil || len(results) == 0 {
		t.Fatal("precondition: committed content must be findable")
	}

	var missedContent atomic.Int64
	var searchErrors atomic.Int64
	indexErrors := make(chan error, 1)
	recordIndexError := func(err error) {
		select {
		case indexErrors <- err:
		default:
		}
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for iter := range iterations {
			marker := fmt.Sprintf("uncommitted_fix_iter_%d", iter)
			content := fmt.Sprintf("package main\n// %s\n", marker)
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("changing_%d.go", iter)), []byte(content), 0o644); err != nil {
				recordIndexError(fmt.Errorf("write dirty fixture %q: %w", marker, err))
				return
			}
			state := gitRepoStateIn(ctx, dir)
			currentState := gitCorpusStateHash(paths, state)
			if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, currentState); err != nil {
				recordIndexError(fmt.Errorf("refresh dirty fixture %q: %w", marker, err))
				return
			}
		}
	}()

	for i := range iterations {
		wg.Add(1)
		go func(iter int) {
			defer wg.Done()
			<-start
			time.Sleep(time.Duration(1+iter%5) * time.Millisecond)
			res, err := searchPlannedCorpusForTest(ctx, plan, "always_findable_fix_test")
			if err != nil {
				searchErrors.Add(1)
				return
			}
			if len(res) == 0 {
				missedContent.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if missedContent.Load() > 0 {
		t.Errorf("FIX REGRESSION: %d/%d searches missed committed content during concurrent reindexing",
			missedContent.Load(), iterations)
	}
	if searchErrors.Load() > 0 {
		t.Errorf("FIX REGRESSION: %d searches returned errors", searchErrors.Load())
	}
	select {
	case err := <-indexErrors:
		t.Errorf("FIX REGRESSION: concurrent indexing failed: %v", err)
	default:
	}

	latestMarker := fmt.Sprintf("uncommitted_fix_iter_%d", iterations-1)
	latestFiles, err := runSeekInPlannedGitCorpus(ctx, latestMarker, paths, plan)
	if err != nil {
		t.Fatalf("final dirty search failed: %v", err)
	}
	if len(latestFiles) == 0 {
		t.Fatalf("latest dirty marker should be searchable after contention settles")
	}
}
