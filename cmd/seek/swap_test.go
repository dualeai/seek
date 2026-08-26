package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestTempSwap_RecoversFromIncompleteSwap simulates a crash mid-publish: a
// leftover .swapping marker means the committed family may be torn. The next
// build must run recoverIncompleteSwap (clean the family + force rebuild) and
// return correct results.
func TestTempSwap_RecoversFromIncompleteSwap(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// RECOVER_MARKER\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	// Initial build.
	if files, err := runSeekInPlannedGitCorpus(ctx, "RECOVER_MARKER", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("initial build: files=%v err=%v", files, err)
	}

	// Simulate an interrupted swap: drop a .swapping marker for the committed
	// family. recoverIncompleteSwap should clean committed shards + clear state
	// on the next build, forcing a clean rebuild.
	if err := os.WriteFile(filepath.Join(plan.cacheDir, swappingMarkerFile), []byte("committed"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInPlannedGitCorpus(ctx, "RECOVER_MARKER", paths, plan)
	if err != nil {
		t.Fatalf("search after incomplete-swap recovery: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("recovery rebuild should restore searchable content")
	}
	if _, err := os.Stat(filepath.Join(plan.cacheDir, swappingMarkerFile)); !os.IsNotExist(err) {
		t.Fatalf("swapping marker should be cleared after recovery, stat err=%v", err)
	}
}

// TestTempSwap_SweepsOrphanBuildDir verifies the gc orphan sweep removes a
// leftover temp build dir whose mtime is older than the staleness bound, and
// leaves a fresh one alone.
func TestTempSwap_SweepsOrphanBuildDir(t *testing.T) {
	indexDir := t.TempDir()

	stale := filepath.Join(indexDir, buildDirPrefix+"1-2-3")
	fresh := filepath.Join(indexDir, buildDirPrefix+"9-9-9")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Age the stale dir past the bound.
	old := time.Now().Add(-2 * buildTmpMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	sweepOrphanBuildDirs(indexDir, time.Now().Add(-buildTmpMaxAge))

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale build dir should be swept, stat err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh build dir must NOT be swept, stat err=%v", err)
	}
}

// TestTempSwap_CommittedDeltaKeepsPriorShards verifies the hardlink-seed path:
// a committed rebuild after a new commit STACKS a delta shard rather than doing
// a full rebuild — i.e. the prior generation was seeded into the temp dir so
// zoekt could delta against it. A full rebuild would collapse to a single shard.
func TestTempSwap_CommittedDeltaKeepsPriorShards(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "a.go", "package main\n// DELTA_BASE\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	if _, err := runSeekInPlannedGitCorpus(ctx, "DELTA_BASE", paths, plan); err != nil {
		t.Fatalf("initial build: %v", err)
	}
	base := committedShardCount(t, plan.indexDir)
	if base == 0 {
		t.Fatal("expected committed shards after initial build")
	}

	// New commit adds a file → committed rebuild should delta-stack.
	writeTrackedFile(t, dir, "b.go", "package main\n// DELTA_NEXT\n")
	if files, err := runSeekInPlannedGitCorpus(ctx, "DELTA_NEXT", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("delta rebuild: files=%v err=%v", files, err)
	}
	// Old content must still be found (delta stacked on the seeded base).
	if files, err := runSeekInPlannedGitCorpus(ctx, "DELTA_BASE", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("base content lost after delta rebuild: files=%v err=%v", files, err)
	}
	if got := committedShardCount(t, plan.indexDir); got <= base {
		t.Fatalf("expected delta to stack a shard (>%d), got %d — seed/delta may have regressed to full rebuild", base, got)
	}
}

// committedShardCount counts committed (non-uncommitted) shards in indexDir.
func committedShardCount(t *testing.T, indexDir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range matches {
		if !isUncommittedShard(filepath.Base(m)) {
			n++
		}
	}
	return n
}

// TestTempSwap_RecoversFolderIncompleteSwap covers the folder/wholesale recovery
// path: a leftover .swapping marker with the "all" family must (via the folder
// fast-path swapPending guard) force the build lock + recoverIncompleteSwap,
// which cleans familyAll and rebuilds. Exercises shardFamilyFromLabel→familyAll.
func TestTempSwap_RecoversFolderIncompleteSwap(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("FOLDER_RECOVER_MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("planFolderCorpus: %v", err)
	}

	ctx := context.Background()
	if st, err := ensureFolderCorpusFresh(ctx, plan); err != nil || st != corpusSearchable {
		t.Fatalf("initial folder build: st=%v err=%v", st, err)
	}

	// Simulate an interrupted wholesale (familyAll) swap.
	if err := os.WriteFile(filepath.Join(plan.cacheDir, swappingMarkerFile), []byte(familyAll.label()), 0o644); err != nil {
		t.Fatal(err)
	}

	if st, err := ensureFolderCorpusFresh(ctx, plan); err != nil || st != corpusSearchable {
		t.Fatalf("folder build after marker: st=%v err=%v", st, err)
	}
	if _, err := os.Stat(filepath.Join(plan.cacheDir, swappingMarkerFile)); !os.IsNotExist(err) {
		t.Fatalf("folder swapping marker should be cleared after recovery, stat err=%v", err)
	}
	files, err := searchPlannedCorpusForTest(ctx, plan, "FOLDER_RECOVER_MARKER")
	if err != nil || len(files) == 0 {
		t.Fatalf("folder content lost after recovery: files=%v err=%v", files, err)
	}
}

// TestTempSwap_ReaderExcludedFromTornWindow deterministically proves the publish
// EX lock excludes a concurrent reader from the torn window (live committed
// family deleted, replacements not yet renamed in). The afterDeleteBeforeRename
// hook holds the swap mid-tear while a reader tries to search; the reader must
// block on LOCK_SH and observe the full new generation, never the torn set.
func TestTempSwap_ReaderExcludedFromTornWindow(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "stable.go", "package main\n// TORN_MARKER\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	st := mustGitRepoStateIn(t, ctx, dir)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, st, gitCorpusStateHash(paths, st)); err != nil {
		t.Fatalf("initial build: %v", err)
	}

	readerDone := make(chan int, 1)
	var once sync.Once
	afterDeleteBeforeRenameHook = func() {
		// Torn window is open. Launch a reader and give it time to attempt its
		// glob — it must block on SH because this runs under the publish EX lock.
		once.Do(func() {
			go func() {
				res, err := searchPlannedCorpusForTest(ctx, plan, "TORN_MARKER")
				if err != nil {
					readerDone <- -1
					return
				}
				readerDone <- len(res)
			}()
			time.Sleep(250 * time.Millisecond)
		})
	}
	defer func() { afterDeleteBeforeRenameHook = nil }()

	// New commit → HEAD moves → committed swap → hook fires mid-swap.
	writeTrackedFile(t, dir, "b.go", "package main\n// B\n")
	s := mustGitRepoStateIn(t, ctx, dir)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, s, gitCorpusStateHash(paths, s)); err != nil {
		t.Fatalf("delta build: %v", err)
	}

	got := <-readerDone
	if got <= 0 {
		t.Fatalf("reader observed torn/empty committed set during swap (got %d) — SH exclusion failed", got)
	}
}
