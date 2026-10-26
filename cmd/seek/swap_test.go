package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	if _, ok := readCommittedGitState(plan.cacheDir); !ok {
		t.Fatal("initial build did not publish committed Git state")
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
	recovered, ok := readCommittedGitState(plan.cacheDir)
	if !ok || recovered.head.String() != gitOutputIn(t, dir, "rev-parse", "HEAD") {
		t.Fatalf("recovered committed Git state=%+v ok=%t", recovered, ok)
	}
}

// TestTempSwap_SweepsOrphanBuildDir verifies the gc orphan sweep removes a
// leftover temp build dir whose mtime is older than the staleness bound, and
// keeps a fresh one.
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

// TestTempSwap_CommittedDeltaKeepsPriorShards verifies the hard-link seed
// path: a committed rebuild after a new commit adds a delta shard and does not
// rebuild in full, so the build linked the prior generation into the temp dir
// and Zoekt built the delta on it. A full rebuild of this repository makes one
// shard, so the shard count does not grow.
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

// TestTempSwap_RecoversFolderIncompleteSwap covers the recovery of a folder
// node: a leftover .swapping marker of the "all" family makes the folder fast
// path (its swapPending check) take the build lock and run
// recoverIncompleteSwap, which reads the label with shardFamilyFromLabel,
// cleans familyAll, and builds again. After the build the marker is gone and
// a search finds the content.
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

// TestTempSwap_ReaderExcludedFromTornWindow checks that the publish EX lock
// keeps a concurrent reader out of the torn window (live committed family
// moved aside, replacements not yet renamed in). afterAsideBeforeRenameHook
// starts a search in that window and waits 250 ms; the reader must block on
// LOCK_SH and find TORN_MARKER. A read of the torn set finds no file.
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
	afterAsideBeforeRenameHook = func() {
		// Torn window is open. Launch a reader and give it time to try its
		// lock: it must block on SH because this runs under the publish EX
		// lock.
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
	defer func() { afterAsideBeforeRenameHook = nil }()

	// New commit → HEAD moves → committed swap → hook fires mid-swap.
	writeTrackedFile(t, dir, "b.go", "package main\n// B\n")
	s := mustGitRepoStateIn(t, ctx, dir)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, s, gitCorpusStateHash(paths, s)); err != nil {
		t.Fatalf("delta build: %v", err)
	}

	var got int
	select {
	case got = <-readerDone:
	case <-time.After(30 * time.Second):
		t.Fatal("no swap ran the hook, so no reader was started")
	}
	if got <= 0 {
		t.Fatalf("reader observed torn/empty committed set during swap (got %d) — SH exclusion failed", got)
	}
}

// TestPublishStepFailureKeepsTheLiveFamily fails one rename of a publish
// after the live family moved aside: the hook removes the last shard of the
// build. It runs for each node kind. The swap moves the live family back, so
// the search serves it whole, with a warning and no .swapping; the edit is not
// in it. The next search publishes the edit.
func TestPublishStepFailureKeepsTheLiveFamily(t *testing.T) {
	requireTools(t)
	for _, test := range []struct {
		kind string
		fam  shardFamily
	}{
		{"git", familyCommitted},
		{"folder", familyAll},
	} {
		t.Run(test.kind, func(t *testing.T) {
			setTestUserCache(t)
			swapShardMaxForTest(t, 4096)
			root := t.TempDir()
			write := func(index int, marker string) {
				writeFileAt(t, root, fmt.Sprintf("f%d.txt", index), marker+"\n"+strings.Repeat(fmt.Sprintf("fill %d of the torn swap fixture\n", index), 128))
			}
			for index := range 4 {
				write(index, "zqtorn")
			}
			if test.kind == "git" {
				gitInitCommitAll(t, root)
			}
			search := func() string {
				t.Helper()
				out, err := seek(t, "--lexical-only", "zqtorn", root)
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				return out
			}
			search()
			// Edit the last file, so that its new copy is in the last shard.
			write(3, "zqtorn zqtornedit")
			if test.kind == "git" {
				gitRunIn(t, root, "commit", "-q", "-am", "edit")
			}
			plans, err := planCorpora(t.Context(), []string{root})
			if err != nil || len(plans) != 1 {
				t.Fatalf("plans=%d err=%v", len(plans), err)
			}
			plan := plans[0]
			var removed int
			afterAsideBeforeRenameHook = func() {
				dirs, _ := filepath.Glob(filepath.Join(plan.indexDir, buildDirPrefix+"*"))
				for _, dir := range dirs {
					shards, err := familyShardFiles(dir, test.fam)
					if err != nil || len(shards) < 2 {
						t.Errorf("build %s: shards=%v err=%v, want 2 or more", dir, shards, err)
						continue
					}
					if err := os.Remove(shards[len(shards)-1]); err != nil {
						t.Error(err)
						continue
					}
					removed++
				}
			}
			t.Cleanup(func() { afterAsideBeforeRenameHook = nil })
			logs := captureTestLogs(t, slog.LevelWarn)
			out := search()
			afterAsideBeforeRenameHook = nil
			if removed != 1 {
				t.Fatalf("the hook removed %d shards, want 1", removed)
			}
			warned := false
			for _, record := range logs.Records() {
				warned = warned || record.Message == "Index update failed; using the existing index"
			}
			for index := range 4 {
				if !warned || !strings.Contains(out, fmt.Sprintf("f%d.txt", index)) {
					t.Fatalf("want the whole live family with a warning: warned=%t, f%d.txt missing:\n%s", warned, index, out)
				}
			}
			if label := readCacheFile(plan.cacheDir, swappingMarkerFile); label != "" {
				t.Fatalf(".swapping=%q after a rolled-back swap, want none", label)
			}
			if edited, err := seek(t, "--lexical-only", "zqtornedit", root); err != nil || !strings.Contains(edited, "f3.txt") {
				t.Fatalf("the next search must publish the edit: err=%v\n%s", err, edited)
			}
		})
	}
}

// TestRecoverIncompleteSwapCleansTornFamily makes a torn family by hand, as a
// crash during a publish leaves it, and runs the swap repair. The repair
// removes the shards and sidecars of the family that .swapping names, the
// family manifest and the state files, and keeps the delta base record. A
// full rebuild cannot show this: the next publish removes every live member
// of the family anyway.
func TestRecoverIncompleteSwapCleansTornFamily(t *testing.T) {
	for _, test := range []struct {
		label           string
		uncommittedGone bool
	}{
		{"committed", false},
		{"all", true},
	} {
		t.Run(test.label, func(t *testing.T) {
			cacheDir := t.TempDir()
			indexDir := filepath.Join(cacheDir, "index")
			committed := []string{"repo_v16.00000.zoekt", "repo_v16.00000.zoekt.meta", "repo_v16.00002.zoekt"}
			uncommitted := "uncommitted_v16.00000.zoekt"
			for _, name := range append(slices.Clone(committed), uncommitted, familyManifestFile) {
				writeFixtureFile(t, filepath.Join(indexDir, name), "x")
			}
			state := []string{stateFile, headFile, emptyFile, swappingMarkerFile}
			for _, name := range append(slices.Clone(state), committedGitStateFile) {
				writeFixtureFile(t, filepath.Join(cacheDir, name), "x")
			}
			writeFixtureFile(t, filepath.Join(cacheDir, swappingMarkerFile), test.label)
			corpusStore{cacheDir: cacheDir, indexDir: indexDir}.recoverIncompleteSwap()
			exists := func(path string) bool {
				_, err := os.Stat(path)
				return err == nil
			}
			for _, name := range append(slices.Clone(committed), familyManifestFile) {
				if exists(filepath.Join(indexDir, name)) {
					t.Errorf("%s stays after the repair", name)
				}
			}
			for _, name := range state {
				if exists(filepath.Join(cacheDir, name)) {
					t.Errorf("%s stays after the repair", name)
				}
			}
			if got := exists(filepath.Join(indexDir, uncommitted)); got == test.uncommittedGone {
				t.Errorf("uncommitted shard exists=%t, want %t", got, !test.uncommittedGone)
			}
			if !exists(filepath.Join(cacheDir, committedGitStateFile)) {
				t.Error("the repair removed the delta base record")
			}
		})
	}
}

// TestPublishMarksOnlyASwapThatMovedFiles publishes into an index directory
// that cannot be listed: the scan fails before any move, so no .swapping
// stays and the next build keeps the family. Then the index directory takes
// no new entry after the live shard moved aside: the rename in and the move
// back fail, the family is torn, and .swapping names it for repair.
func TestPublishMarksOnlyASwapThatMovedFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	const shard = "folder_x_v16.00000.zoekt"
	store := corpusStore{cacheDir: t.TempDir(), indexDir: filepath.Join(t.TempDir(), "index")}
	buildDir := filepath.Join(filepath.Dir(store.indexDir), "build")
	for _, dir := range []string{store.indexDir, buildDir} {
		writeFixtureFile(t, filepath.Join(dir, shard), "shard")
	}
	t.Cleanup(func() { _ = os.Chmod(store.indexDir, 0o755) })
	if err := os.Chmod(store.indexDir, 0o333); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishShardFamilyLocked(buildDir, familyAll); err == nil {
		t.Fatal("the publish listed an index directory that cannot be read")
	}
	if label := readCacheFile(store.cacheDir, swappingMarkerFile); label != "" {
		t.Fatalf(".swapping=%q after a scan error, want none", label)
	}
	if err := os.Chmod(store.indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	afterAsideBeforeRenameHook = func() {
		if err := os.Chmod(store.indexDir, 0o555); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterAsideBeforeRenameHook = nil })
	if _, err := store.publishShardFamilyLocked(buildDir, familyAll); err == nil {
		t.Fatal("the publish renamed into an index directory that takes no entry")
	}
	if label := readCacheFile(store.cacheDir, swappingMarkerFile); label != "all" {
		t.Fatalf(".swapping=%q after a failed move back, want all", label)
	}
}
