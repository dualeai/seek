package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTempSwap_RecoversFromIncompleteSwap leaves a .swapping marker of the
// committed family, as a crash during a publish does. The marker makes the
// next search build again, after lockBuild repairs the family
// (recoverIncompleteSwap). The search then finds the content, the marker is
// gone, and the committed Git state names HEAD.
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

	// A publish that stopped during its swap leaves this marker.
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

// TestTempSwap_SweepsOrphanBuildDir runs the gc orphan sweep of one corpus
// (sweepCorpusOrphans). It removes a build directory older than buildTmpMaxAge
// and keeps a younger one. While a build holds the build lock, it keeps both.
func TestTempSwap_SweepsOrphanBuildDir(t *testing.T) {
	cacheDir := t.TempDir()
	entry := corpusDirEntry{name: "c", path: cacheDir}
	indexDir := filepath.Join(cacheDir, "index")
	now := time.Now()
	stale := filepath.Join(indexDir, buildDirPrefix+"1-2-3")
	fresh := filepath.Join(indexDir, buildDirPrefix+"9-9-9")
	for path, age := range map[string]time.Duration{stale: buildTmpMaxAge + time.Minute, fresh: buildTmpMaxAge - time.Minute} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}

	build, ok, err := tryBuildLock(cacheDir)
	if err != nil || !ok {
		t.Fatalf("take the build lock: ok=%t err=%v", ok, err)
	}
	sweepCorpusOrphans(entry, now)
	releaseLock(build)
	if !exists(stale) || !exists(fresh) {
		t.Fatalf("the sweep removed a build directory while a build held the lock: stale=%t fresh=%t", exists(stale), exists(fresh))
	}

	sweepCorpusOrphans(entry, now)
	if exists(stale) {
		t.Fatal("the sweep kept a build directory older than buildTmpMaxAge")
	}
	if !exists(fresh) {
		t.Fatal("the sweep removed a build directory younger than buildTmpMaxAge")
	}
}

// TestTempSwap_CommittedDeltaKeepsPriorShards checks the hard-link seed of a
// committed delta. After a new commit, the build links the prior shards into
// its build directory, and Zoekt adds a delta shard to them. A full rebuild of
// this repository makes one shard, so only a delta adds to the shard count.
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

	// A new commit adds a file, so the committed build adds a delta shard.
	writeTrackedFile(t, dir, "b.go", "package main\n// DELTA_NEXT\n")
	if files, err := runSeekInPlannedGitCorpus(ctx, "DELTA_NEXT", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("delta rebuild: files=%v err=%v", files, err)
	}
	// The delta builds on the linked shards, so the old content stays.
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

// TestTempSwap_RecoversFolderIncompleteSwap leaves a .swapping marker of the
// "all" family in a folder node. The marker makes the folder skip its warm
// fast path (swapPending) and take the build lock. lockBuild then runs
// recoverIncompleteSwap, which reads the label (shardFamilyFromLabel), cleans
// familyAll, and clears the state, so the folder builds again. After the
// build, the marker is gone and a search finds the content.
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

	// A publish of familyAll that stopped during its swap leaves this marker.
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

// TestTempSwap_ReaderExcludedFromTornWindow takes the publish lock and moves
// the live committed family aside, as a swap does before it renames the new
// family in. A reader that starts in this torn window must wait for the lock,
// which its debug line shows. It must not read the torn family, where it finds
// no file. When the family is back and the lock is free, the reader finds
// TORN_MARKER.
func TestTempSwap_ReaderExcludedFromTornWindow(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "stable.go", "package main\n// TORN_MARKER\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	st := mustGitRepoStateIn(t, ctx, dir)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, st, gitCorpusStateHash(paths, st)); err != nil {
		t.Fatalf("initial build: %v", err)
	}
	_, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitPublish, filepath.Join(plan.cacheDir, lockFile))
	pub, err := acquirePublishLock(ctx, plan.cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	restore := tearFamily(t, plan, familyCommitted)
	type result struct {
		files int
		err   error
	}
	done := make(chan result, 1)
	go func() {
		files, err := searchPlannedCorpusForTest(ctx, plan, "TORN_MARKER")
		done <- result{files: len(files), err: err}
	}()
	select {
	case <-waiting:
	case got := <-done:
		restore()
		releaseLock(pub)
		t.Fatalf("a reader read the torn family during the publish: files=%d err=%v", got.files, got.err)
	}
	restore()
	releaseLock(pub)
	if got := <-done; got.err != nil || got.files == 0 {
		t.Fatalf("the reader after the publish: files=%d err=%v, want TORN_MARKER", got.files, got.err)
	}
}

// tearFamily moves the shards and sidecars of fam out of the index directory
// of plan, as swapFiles does before it renames the new family in. The caller
// holds the publish lock. The returned restore moves them back.
func tearFamily(t *testing.T, plan node, fam shardFamily) (restore func()) {
	t.Helper()
	shards := familyShardFilesForTest(t, plan.indexDir, fam)
	if len(shards) == 0 {
		t.Fatal("the family has no shard to move aside")
	}
	aside, err := os.MkdirTemp(plan.cacheDir, "torn-")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range shards {
		if err := os.Rename(path, filepath.Join(aside, filepath.Base(path))); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		t.Helper()
		for _, path := range shards {
			if err := os.Rename(filepath.Join(aside, filepath.Base(path)), path); err != nil {
				t.Errorf("move %s back: %v", filepath.Base(path), err)
			}
		}
		_ = os.Remove(aside)
	}
}

// TestPublishStepFailureKeepsTheLiveFamily fails one rename of a publish
// after the live family moved aside. The edit adds a shard to the family, and
// a folder that the test user cannot empty holds the path of that shard. It
// runs for each node kind. The swap moves the live family back, so the search
// serves it whole, with a warning and no .swapping; the edit is not in it.
// When the folder is gone, the next search publishes the edit.
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
			// The edit adds the shard after the last one (a delta). The scan
			// of the live family skips folders, so the swap meets the folder
			// only when it renames that shard in.
			sc, err := scanFamily(plan.indexDir)
			if err != nil {
				t.Fatal(err)
			}
			prefix, last := "", -1
			for _, member := range sc.members {
				if member.shard && member.numbered && inFamily(test.fam, member) && member.num > last {
					prefix, last = member.prefix, member.num
				}
			}
			blocker := filepath.Join(plan.indexDir, fmt.Sprintf("%s.%05d%s", prefix, last+1, shardSuffix))
			writeFixtureFile(t, filepath.Join(blocker, "kept"), "the swap cannot remove this folder\n")
			unlock := lockFolderTree(t, blocker)
			logs := captureTestLogs(t, slog.LevelWarn)
			out := search()
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
			unlock()
			if err := os.RemoveAll(blocker); err != nil {
				t.Fatal(err)
			}
			if edited, err := seek(t, "--lexical-only", "zqtornedit", root); err != nil || !strings.Contains(edited, "f3.txt") {
				t.Fatalf("the next search must publish the edit: err=%v\n%s", err, edited)
			}
		})
	}
}

// TestRecoverIncompleteSwapCleansTornFamily makes a torn family by hand, as a
// crash during a publish leaves it, and runs the swap repair. The repair
// removes the shards and sidecars of the family that .swapping names. It also
// removes the family manifest and the state files, and keeps the delta base
// record. A full rebuild cannot show this: the next publish removes every
// live member of the family anyway.
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
// that cannot be listed. The scan fails before any move, so no .swapping stays
// and the next build keeps the family. TestPublishMarksAFailedMoveBack (macOS)
// covers a move back that fails.
func TestPublishMarksOnlyASwapThatMovedFiles(t *testing.T) {
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
	if _, err := os.ReadDir(store.indexDir); err == nil {
		t.Fatal("the test user lists a mode-0333 directory, so the scan cannot fail")
	}
	if _, err := store.publishShardFamilyLocked(buildDir, familyAll); err == nil {
		t.Fatal("the publish listed an index directory that cannot be read")
	}
	if label := readCacheFile(store.cacheDir, swappingMarkerFile); label != "" {
		t.Fatalf(".swapping=%q after a scan error, want none", label)
	}
}
