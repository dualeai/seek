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
// family deleted, replacements not yet renamed in). afterDeleteBeforeRenameHook
// holds the swap mid-tear while a reader tries to search; the reader must
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

// TestPublishStepFailureServesTheTornFamily fails one rename of a publish
// after the live family is deleted: the hook removes the last shard of the
// build. It runs for each node kind. The search serves the shards that the
// swap renamed, with a warning, and .swapping names the torn family; the
// edited file is in neither copy. The next search repairs.
func TestPublishStepFailureServesTheTornFamily(t *testing.T) {
	requireTools(t)
	for _, test := range []struct {
		kind  string
		fam   shardFamily
		label string
	}{
		{"git", familyCommitted, "committed"},
		{"folder", familyAll, "all"},
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
				out, err := captureStdout(t, func() error { return run(t.Context(), "zqtorn", []string{root}) })
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
			afterDeleteBeforeRenameHook = func() {
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
			t.Cleanup(func() { afterDeleteBeforeRenameHook = nil })
			logs := captureTestLogs(t, slog.LevelWarn)
			out := search()
			afterDeleteBeforeRenameHook = nil
			if removed != 1 {
				t.Fatalf("the hook removed %d shards, want 1", removed)
			}
			warned := false
			for _, record := range logs.Records() {
				warned = warned || record.Message == "Index update failed; using the existing index"
			}
			if !warned || !strings.Contains(out, "f0.txt") || strings.Contains(out, "f3.txt") {
				t.Fatalf("want the torn family with a warning and no edited file: warned=%t output:\n%s", warned, out)
			}
			if label := readCacheFile(plan.cacheDir, swappingMarkerFile); label != test.label {
				t.Fatalf(".swapping=%q, want %q", label, test.label)
			}
			out = search()
			for index := range 4 {
				if !strings.Contains(out, fmt.Sprintf("f%d.txt", index)) {
					t.Fatalf("the repair must find f%d.txt:\n%s", index, out)
				}
			}
			if _, err := os.Stat(filepath.Join(plan.cacheDir, swappingMarkerFile)); !os.IsNotExist(err) {
				t.Fatalf(".swapping stays after the repair: %v", err)
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
