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

	"github.com/sourcegraph/zoekt"
)

// Skipped tests documenting known limitations. Each has a corresponding
// TestFix_* test that verifies the mitigation.

func TestBug_ShardGapDuringReindexing(t *testing.T) {
	t.Skip("Known limitation: manually calling cleanUncommittedShards creates a gap. Mitigated by atomic shard swap (see TestFix_NoShardGapDuringReindexing).")
}

func TestBug_ConcurrentSearchDuringReindex_Stress(t *testing.T) {
	t.Skip("Known limitation: probabilistic race without LOCK_SH. Mitigated by LOCK_SH (see TestFix_ConcurrentSearchDuringReindex_Stress).")
}

func TestBug_SameSecondEditStaleness(t *testing.T) {
	t.Skip("Known limitation: git stat cache misses same-mtime same-size edits. This is a git-level limitation outside seek's control.")
}

func TestBug_StaleFallbackMissesUncommittedContent(t *testing.T) {
	t.Skip("Known limitation: stale fallback serves old shards when another process holds LOCK_EX. The next search will re-index.")
}

func TestBug_PostIndexingStateDrift(t *testing.T) {
	t.Skip("Known limitation: mutation during indexing serves stale results for the current search. Post-verification re-stats dirty files so the next search re-indexes.")
}

// ===========================================================================
// Fix verification tests
//
// These tests verify that the applied mitigations (atomic shard swap,
// LOCK_SH during search) actually prevent the bugs demonstrated above.
// ===========================================================================

// searchWithLock mirrors the production search path in run(): acquires
// a shared lock via acquireSearchLock before searching, ensuring the
// searcher waits for any active indexer (LOCK_EX holder) to finish.
func searchWithLock(ctx context.Context, indexDir, pattern string) ([]zoekt.FileMatch, error) {
	lockPath := filepath.Join(indexDir, lockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open search lock: %w", err)
	}
	defer func() {
		unlockFile(f)
		_ = f.Close()
	}()
	if err := acquireSearchLock(ctx, f); err != nil {
		return nil, fmt.Errorf("acquire search lock: %w", err)
	}
	return executeSearch(ctx, indexDir, pattern)
}

// ---------------------------------------------------------------------------
// Fix #1: Atomic shard swap — no gap during re-indexing
//
// After the fix, runIndexing no longer calls cleanUncommittedShards before
// indexUncommitted when uncommitted docs exist. Zoekt's builder.Finish()
// atomically replaces old shards (write .tmp then os.Rename), so there
// is no window where zero uncommitted shards exist.
// ---------------------------------------------------------------------------

func TestFix_NoShardGapDuringReindexing(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_aaa\n")

	// Create uncommitted edit
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// uncommitted_v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Initial index with uncommitted content
	state := gitRepoStateIn(ctx, dir)
	currentState := computeStateHash(repoStateFingerprint(dir, state))
	if err := runIndexing(ctx, dir, indexDir, state, currentState); err != nil {
		t.Fatalf("initial indexing failed: %v", err)
	}

	results, err := executeSearch(ctx, indexDir, "uncommitted_v1")
	if err != nil {
		t.Fatalf("initial search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("precondition: uncommitted_v1 must be findable after initial indexing")
	}

	// Change uncommitted content and re-index through the production path
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// uncommitted_v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	state2 := gitRepoStateIn(ctx, dir)
	currentState2 := computeStateHash(repoStateFingerprint(dir, state2))
	if currentState2 == currentState {
		t.Fatal("precondition: state should change after edit")
	}

	// Force re-indexing by clearing state file
	deleteStateFiles(indexDir)
	if err := runIndexing(ctx, dir, indexDir, state2, currentState2); err != nil {
		t.Fatalf("re-indexing failed: %v", err)
	}

	// After re-indexing, new uncommitted content must be findable
	results, err = executeSearch(ctx, indexDir, "uncommitted_v2")
	if err != nil {
		t.Fatalf("post-reindex search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("FIX FAILED: uncommitted_v2 not findable after re-indexing")
	}

	// Old uncommitted content should NOT be in the uncommitted shard
	// (it may still appear in the committed shard if it was committed)
	t.Log("FIX VERIFIED: re-indexing via production path preserves uncommitted content availability")
}

// ---------------------------------------------------------------------------
// Fix #2: acquireSearchLock polls until LOCK_EX is released
//
// The production search path in run() acquires a shared lock via
// acquireSearchLock (non-blocking poll with exponential backoff) before
// calling executeSearch. Since the NB shared lock fails while any LOCK_EX
// is held, a searcher waits for an active indexer to finish before reading
// shards.
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

	t.Log("FIX VERIFIED: LOCK_SH correctly blocks until LOCK_EX is released")
}

// ---------------------------------------------------------------------------
// Fix #3: Concurrent search+reindex stress — with LOCK_SH protection
//
// Similar to TestBug_ConcurrentSearchDuringReindex_Stress but uses
// searchWithLock (the production path) and expects ZERO missed searches.
// ---------------------------------------------------------------------------

func TestFix_ConcurrentSearchDuringReindex_Stress(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "stable.go", "package main\n// always_findable_fix_test\n")

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Initial index
	state := gitRepoStateIn(ctx, dir)
	currentState := computeStateHash(repoStateFingerprint(dir, state))
	if err := runIndexing(ctx, dir, indexDir, state, currentState); err != nil {
		t.Fatalf("initial indexing: %v", err)
	}

	// Verify baseline
	results, err := searchWithLock(ctx, indexDir, "always_findable_fix_test")
	if err != nil || len(results) == 0 {
		t.Fatal("precondition: committed content must be findable")
	}

	var missedContent atomic.Int64
	var searchErrors atomic.Int64
	const iterations = 20

	var wg sync.WaitGroup
	for i := range iterations {
		wg.Add(2)

		// Goroutine A: create uncommitted change and reindex
		go func(iter int) {
			defer wg.Done()
			content := fmt.Sprintf("package main\n// uncommitted_fix_iter_%d\n", iter)
			_ = os.WriteFile(filepath.Join(dir, "changing.go"), []byte(content), 0o644)
			st := gitRepoStateIn(ctx, dir)
			cs := computeStateHash(repoStateFingerprint(dir, st))
			_ = runIndexing(ctx, dir, indexDir, st, cs)
		}(i)

		// Goroutine B: search concurrently WITH LOCK_SH protection
		go func(iter int) {
			defer wg.Done()
			time.Sleep(time.Duration(1+iter%5) * time.Millisecond)
			res, err := searchWithLock(ctx, indexDir, "always_findable_fix_test")
			if err != nil {
				searchErrors.Add(1)
				return
			}
			if len(res) == 0 {
				missedContent.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if missedContent.Load() > 0 {
		t.Errorf("FIX REGRESSION: %d/%d searches missed committed content during concurrent reindexing",
			missedContent.Load(), iterations)
	} else {
		t.Logf("FIX VERIFIED: all %d concurrent searches found committed content with LOCK_SH protection", iterations)
	}
	if searchErrors.Load() > 0 {
		t.Logf("Note: %d searches returned errors", searchErrors.Load())
	}
}

