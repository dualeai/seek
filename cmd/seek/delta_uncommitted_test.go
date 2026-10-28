package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt/index"
)

// dirtyAndIndex rewrites the working-tree file at relPath with content, runs
// one indexing cycle, and returns the shard count of repoUncommitted after
// it. It sets the mtime from the Go clock, 1 to 5 µs after the current time.
// A file-system clock can give two quick writes of the same size one mtime,
// and statUncommittedCandidates would then see no change.
func dirtyAndIndex(t *testing.T, ctx context.Context, paths gitPaths, plan node, relPath, content string) int {
	t.Helper()
	if err := os.WriteFile(filepath.Join(paths.RepoDir, relPath), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Duration(1+time.Now().UnixNano()%5) * time.Microsecond)
	_ = os.Chtimes(filepath.Join(paths.RepoDir, relPath), future, future)

	reindexGit(t, ctx, paths, plan)
	return repositoryShardCount(plan.indexDir, repoUncommitted)
}

func TestDeltaUncommitted_RapidEditsKeepLatestOnly(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_baseline\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	const cycles = 10
	// The shard count after each cycle. A delta adds one shard per cycle; a
	// full rebuild in its place keeps one shard.
	shardSeries := make([]int, 0, cycles)
	for i := range cycles {
		marker := fmt.Sprintf("dirty_edit_%d", i)
		shards := dirtyAndIndex(t, ctx, paths, plan, "app.go", fmt.Sprintf("package main\n// %s\n", marker))
		shardSeries = append(shardSeries, shards)

		// Only the freshest marker must be visible.
		for prior := range i {
			results, err := searchPlannedCorpusForTest(ctx, plan, fmt.Sprintf("dirty_edit_%d", prior))
			if err != nil {
				t.Fatalf("search prior marker %d: %v", prior, err)
			}
			if len(results) != 0 {
				t.Fatalf("prior marker %d should be tombstoned after edit %d, got %d hits", prior, i, len(results))
			}
		}
		results, err := searchPlannedCorpusForTest(ctx, plan, marker)
		if err != nil {
			t.Fatalf("search current marker %d: %v", i, err)
		}
		if len(results) == 0 {
			t.Fatalf("current marker %d must be findable", i)
		}
	}

	finalShards := shardSeries[len(shardSeries)-1]
	if finalShards > cycles+1 {
		t.Fatalf("shard count %d exceeds delta budget %d", finalShards, cycles+1)
	}
	// Delta-path proof: the shard count must grow over the run. An unseen
	// fallback to full rebuilds would stay at 1 shard throughout, because
	// each rebuild replaces the prior shard set.
	if finalShards <= 1 {
		t.Fatalf("expected uncommitted shard accumulation under delta, got series %v — delta path may have silently fallen back to full rebuild", shardSeries)
	}
	if finalShards < shardSeries[0] {
		t.Fatalf("shard count regressed mid-run %v — delta accumulation broken", shardSeries)
	}
}

// TestDeltaUncommitted_DirtyToCommittedLeavesOneCopy commits the one dirty
// file and indexes again: the search finds one copy of the file, not a
// committed and an uncommitted copy.
func TestDeltaUncommitted_DirtyToCommittedLeavesOneCopy(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// initial_clean\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// Make the file dirty and index.
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// dirty_then_committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	if r, _ := searchPlannedCorpusForTest(ctx, plan, "dirty_then_committed"); len(r) == 0 {
		t.Fatal("dirty marker must be findable")
	}

	// Commit it and index. The dirty set is now empty.
	gitRunIn(t, dir, "add", ".")
	gitRunIn(t, dir, "commit", "-m", "promote dirty to committed")
	reindexGit(t, ctx, paths, plan)

	results, err := searchPlannedCorpusForTest(ctx, plan, "dirty_then_committed")
	if err != nil {
		t.Fatalf("search after commit: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 hit (committed shard), got %d", len(results))
	}
}

func TestDeltaUncommitted_CommittedToDirtyHidesCommittedContent(t *testing.T) {
	requireTools(t)

	// The node read hides the committed copy of a dirty file, so dirty content
	// replaces committed content. Use runSeekInRepo to exercise the public
	// search path.
	dir := initGitRepo(t, "app.go", "package main\n// committed_only_marker\n")

	files, err := runSeekInRepo(t, dir, "committed_only_marker")
	if err != nil {
		t.Fatalf("baseline search: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("committed marker baseline must be findable")
	}

	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// dirty_replaces_committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	committedAfter, _ := runSeekInRepo(t, dir, "committed_only_marker")
	if len(committedAfter) != 0 {
		t.Fatalf("the committed copy of a dirty file still matches: %v", committedAfter)
	}
	dirtyAfter, err := runSeekInRepo(t, dir, "dirty_replaces_committed")
	if err != nil {
		t.Fatalf("dirty search: %v", err)
	}
	if len(dirtyAfter) == 0 {
		t.Fatal("dirty content must be findable through the full run path")
	}
}

func TestDeltaUncommitted_EmptyDirtySetCleansShards(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// empty_dirty_marker\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// Dirty the file, index → uncommitted shards exist.
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// dirty_phase\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	if repositoryShardCount(plan.indexDir, repoUncommitted) == 0 {
		t.Fatal("expected at least one uncommitted shard after dirty cycle")
	}

	// Revert the file to its committed content. The next cycle's dirty set
	// is empty → shards must be cleaned and the manifest dropped.
	gitRunIn(t, dir, "checkout", "--", "app.go")
	reindexGit(t, ctx, paths, plan)

	if got := repositoryShardCount(plan.indexDir, repoUncommitted); got != 0 {
		t.Fatalf("expected uncommitted shards cleared, got %d", got)
	}
	if _, err := os.Stat(filepath.Join(plan.cacheDir, uncommittedManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("manifest should be removed on empty dirty set, stat err=%v", err)
	}
}

func TestDeltaUncommitted_CorruptManifestFallsBackToFullRebuild(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// corrupt_manifest_baseline\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// Three dirty cycles write a manifest and delta shards. With more than one
	// shard before the corruption, the one shard of a full rebuild differs
	// from the prior state.
	for i := range 3 {
		body := fmt.Sprintf("package main\n// pre_corruption_marker_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		reindexGit(t, ctx, paths, plan)
	}
	shardsBefore := repositoryShardCount(plan.indexDir, repoUncommitted)
	if shardsBefore < 2 {
		t.Fatalf("setup must accumulate multiple delta shards, got %d", shardsBefore)
	}

	// Corrupt the manifest with a truncated JSON payload.
	manifestPath := filepath.Join(plan.cacheDir, uncommittedManifestFileName)
	if err := os.WriteFile(manifestPath, []byte(`{"version":1,"state":"x","files":`), 0o644); err != nil {
		t.Fatal(err)
	}

	// readUncommittedManifest returns ok=false for the corrupt manifest, so
	// the next cycle builds the family in full: one shard and a new manifest.
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// post_corruption_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)

	if r, _ := searchPlannedCorpusForTest(ctx, plan, "post_corruption_marker"); len(r) == 0 {
		t.Fatal("post-corruption marker must remain searchable via full rebuild")
	}
	if r, _ := searchPlannedCorpusForTest(ctx, plan, "pre_corruption_marker_2"); len(r) != 0 {
		t.Fatal("pre-corruption marker should not survive a full rebuild")
	}

	// Proof that the full rebuild ran:
	//   1. A full rebuild of one small file writes one uncommitted shard.
	//   2. It writes a valid manifest, whose state replaces the "x" of the
	//      truncated one.
	shardsAfter := repositoryShardCount(plan.indexDir, repoUncommitted)
	if shardsAfter != 1 {
		t.Fatalf("fallback rebuild should leave a single uncommitted shard, got %d (was %d)", shardsAfter, shardsBefore)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest must be regenerated by the fallback, read err=%v", err)
	}
	var manifest uncommittedManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("regenerated manifest must be valid JSON: %v", err)
	}
	if manifest.State == "" || manifest.State == "x" {
		t.Fatalf("regenerated manifest state must be refreshed from a real index cycle, got %q", manifest.State)
	}
}

func TestDeltaUncommitted_ShardThresholdTriggersFullRebuild(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// uthreshold_baseline\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	cycles := maxUncommittedDeltaShards + 2
	shardSeries := make([]int, 0, cycles)
	for i := range cycles {
		shards := dirtyAndIndex(t, ctx, paths, plan, "app.go", fmt.Sprintf("package main\n// uthreshold_cycle_%d\n", i))
		shardSeries = append(shardSeries, shards)
	}

	peak := 0
	for _, n := range shardSeries {
		if n > peak {
			peak = n
		}
	}
	if peak <= 1 {
		t.Fatalf("delta path never stacked extra shards (series=%v) — silent fallback likely", shardSeries)
	}
	if peak < maxUncommittedDeltaShards-1 {
		t.Fatalf("peak shard count %d never approached threshold %d — series=%v", peak, maxUncommittedDeltaShards, shardSeries)
	}
	final := shardSeries[len(shardSeries)-1]
	if final == 0 {
		t.Fatal("expected at least one uncommitted shard after threshold rebuild")
	}
	if final > maxUncommittedDeltaShards {
		t.Fatalf("shard count %d exceeds threshold %d — fallback did not trigger (series=%v)", final, maxUncommittedDeltaShards, shardSeries)
	}
	if final >= peak {
		t.Fatalf("threshold trip should drop shard count (peak=%d, final=%d, series=%v)", peak, final, shardSeries)
	}

	// Latest cycle's content must still be searchable.
	latest := fmt.Sprintf("uthreshold_cycle_%d", cycles-1)
	if r, _ := searchPlannedCorpusForTest(ctx, plan, latest); len(r) == 0 {
		t.Fatalf("latest dirty marker %s must remain findable post-rebuild", latest)
	}
}

func TestDeltaUncommitted_ManifestRecordsEveryDirtyFile(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// manifest_shape_baseline\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\n// a_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("package main\n// b_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)

	// Check that the manifest contains each dirty file with its on-disk stat
	// fingerprint.
	data, err := os.ReadFile(filepath.Join(plan.cacheDir, uncommittedManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest uncommittedManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest.Version != uncommittedManifestVersion {
		t.Fatalf("manifest version: got %d want %d", manifest.Version, uncommittedManifestVersion)
	}

	got := make(map[string]uncommittedManifestEntry, len(manifest.Files))
	for _, e := range manifest.Files {
		got[e.Name] = e
	}
	for _, name := range []string{"a.go", "b.go"} {
		entry, ok := got[name]
		if !ok {
			t.Fatalf("manifest missing dirty file %q (have %d entries)", name, len(manifest.Files))
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("lstat %s: %v", name, err)
		}
		if entry.Size != info.Size() {
			t.Fatalf("manifest entry %q size %d does not match on-disk size %d", name, entry.Size, info.Size())
		}
		// mtime granularity differs across filesystems, but Mtime must be
		// non-zero for a freshly-written file.
		if entry.Mtime == 0 {
			t.Fatalf("manifest entry %q has zero mtime", name)
		}
		if entry.Ino == 0 {
			t.Fatalf("manifest entry %q has zero inode", name)
		}
	}

	// Manifest must be sorted by name so the sorted-merge diff in
	// diffUncommittedAgainstManifest works against future candidate lists.
	for i := 1; i < len(manifest.Files); i++ {
		if manifest.Files[i-1].Name >= manifest.Files[i].Name {
			t.Fatalf("manifest entries not sorted: %q then %q", manifest.Files[i-1].Name, manifest.Files[i].Name)
		}
	}
}

func TestDeltaUncommitted_NewFileAppearsInDelta(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// initial\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// First dirty cycle: edit app.go.
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// app_edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)

	// The second cycle adds a new untracked file: the delta must read it.
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package main\n// brand_new_dirty_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)

	if r, _ := searchPlannedCorpusForTest(ctx, plan, "brand_new_dirty_marker"); len(r) == 0 {
		t.Fatal("newly added dirty file must be findable in second delta cycle")
	}
	if r, _ := searchPlannedCorpusForTest(ctx, plan, "app_edited"); len(r) == 0 {
		t.Fatal("previously dirtied file must remain findable")
	}
}

func TestDeltaUncommitted_DeletedDirtyFileTombstoned(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// keep_me\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// Create two new dirty files and index.
	if err := os.WriteFile(filepath.Join(dir, "first.go"), []byte("package main\n// first_dirty_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.go"), []byte("package main\n// second_dirty_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	if r, _ := searchPlannedCorpusForTest(ctx, plan, "first_dirty_marker"); len(r) == 0 {
		t.Fatal("first marker must be findable before deletion")
	}

	// Delete the first untracked file and edit the second to change the state.
	if err := os.Remove(filepath.Join(dir, "first.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.go"), []byte("package main\n// second_dirty_marker_v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)

	if r, _ := searchPlannedCorpusForTest(ctx, plan, "first_dirty_marker"); len(r) != 0 {
		t.Fatal("deleted dirty file content should be tombstoned")
	}
	if r, _ := searchPlannedCorpusForTest(ctx, plan, "second_dirty_marker_v2"); len(r) == 0 {
		t.Fatal("edited dirty file content should be findable")
	}
}

// TestDeltaUncommitted_RemovalOnlyHidesTheRemovedFile edits one dirty file, so
// a delta shard holds it alone, then removes that file with no other change.
// The removed file must leave the results, and every other file must stay. The
// next delta and a full rebuild must keep these results.
func TestDeltaUncommitted_RemovalOnlyHidesTheRemovedFile(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_base\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package main\n// keep_first_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gone.go"), []byte("package main\n// gone_first_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	if shards := dirtyAndIndex(t, ctx, paths, plan, "gone.go", "package main\n// gone_second_marker\n"); shards < 2 {
		t.Fatalf("uncommitted shards=%d, want a delta shard for the edit", shards)
	}

	matches := func(step string, absent, present []string) {
		t.Helper()
		for _, marker := range absent {
			results, err := searchPlannedCorpusForTest(ctx, plan, marker)
			if err != nil {
				t.Fatalf("%s: search %s: %v", step, marker, err)
			}
			if len(results) != 0 {
				t.Fatalf("%s: %s matches %d files, want none", step, marker, len(results))
			}
		}
		for _, marker := range present {
			results, err := searchPlannedCorpusForTest(ctx, plan, marker)
			if err != nil {
				t.Fatalf("%s: search %s: %v", step, marker, err)
			}
			if len(results) != 1 {
				t.Fatalf("%s: %s matches %d files, want one", step, marker, len(results))
			}
		}
	}

	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	matches("removal",
		[]string{"gone_first_marker", "gone_second_marker"},
		[]string{"keep_first_marker", "committed_base"})

	dirtyAndIndex(t, ctx, paths, plan, "keep.go", "package main\n// keep_second_marker\n")
	matches("next delta",
		[]string{"gone_first_marker", "gone_second_marker", "keep_first_marker"},
		[]string{"keep_second_marker", "committed_base"})

	// With no manifest, the next build reads every dirty file again.
	deleteUncommittedManifest(plan.cacheDir)
	if err := os.WriteFile(filepath.Join(dir, "added.go"), []byte("package main\n// added_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	matches("full rebuild",
		[]string{"gone_first_marker", "gone_second_marker", "keep_first_marker"},
		[]string{"keep_second_marker", "added_marker", "committed_base"})
}

// shardFileState identifies a shard family on disk by file name: the inode of
// each .zoekt file and the content of each .meta sidecar.
type shardFileState struct {
	shards map[string]uint64
	metas  map[string]string
}

// readShardFileState reads the shard files of repoName in indexDir.
func readShardFileState(t *testing.T, indexDir, repoName string) shardFileState {
	t.Helper()
	state := shardFileState{shards: map[string]uint64{}, metas: map[string]string{}}
	for _, shard := range repositoryShardFiles(indexDir, repoName) {
		info, err := os.Stat(shard)
		if err != nil {
			t.Fatal(err)
		}
		state.shards[filepath.Base(shard)] = info.Sys().(*syscall.Stat_t).Ino
		if meta, err := os.ReadFile(shard + ".meta"); err == nil {
			state.metas[filepath.Base(shard)] = string(meta)
		}
	}
	return state
}

// requireTombstoneOnlyDelta fails unless after has the shard files of before,
// each with its inode, and at least one changed .meta sidecar. Then a delta
// wrote only tombstones and rebuilt no shard.
func requireTombstoneOnlyDelta(t *testing.T, before, after shardFileState) {
	t.Helper()
	if !maps.Equal(before.shards, after.shards) {
		t.Fatalf("shard files changed: before=%v after=%v", before.shards, after.shards)
	}
	if maps.Equal(before.metas, after.metas) {
		t.Fatal("no .meta sidecar changed: the delta wrote no tombstone")
	}
}

// TestDeltaUncommitted_RemovalOnlyKeepsShards removes one of two dirty files.
// The delta writes tombstones only: every prior uncommitted shard keeps its
// inode, the shard count stays, and only the .meta sidecars change.
func TestDeltaUncommitted_RemovalOnlyKeepsShards(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "app.go", "package main\n// committed_base\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	keep := "package main\n// keep_marker\n" + strings.Repeat("// filler line\n", 40)
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte(keep), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gone.go"), []byte("package main\n// gone_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	before := readShardFileState(t, plan.indexDir, repoUncommitted)
	if len(before.shards) == 0 {
		t.Fatal("no uncommitted shard")
	}

	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	requireTombstoneOnlyDelta(t, before, readShardFileState(t, plan.indexDir, repoUncommitted))
	if results, err := searchPlannedCorpusForTest(ctx, plan, "gone_marker"); err != nil || len(results) != 0 {
		t.Fatalf("gone_marker matches %d files, want none; error=%v", len(results), err)
	}
}

// requireFullRebuild fails unless after shares no shard file with before: the
// build replaced the family.
func requireFullRebuild(t *testing.T, before, after shardFileState) {
	t.Helper()
	for name, inode := range before.shards {
		if after.shards[name] == inode {
			t.Fatalf("shard %s kept inode %d: the build was a delta", name, inode)
		}
	}
	if len(after.shards) == 0 {
		t.Fatal("no shard after the full rebuild")
	}
}

// requireTombstoneOnlyDeltaOrNewShard fails unless every shard of before keeps
// its inode in after: the build was a delta, which can add shards.
func requireTombstoneOnlyDeltaOrNewShard(t *testing.T, before, after shardFileState) {
	t.Helper()
	for name, inode := range before.shards {
		if after.shards[name] != inode {
			t.Fatalf("shard %s changed inode %d -> %d: the build was not a delta", name, inode, after.shards[name])
		}
	}
}

// compactionFixtureFile is the content of file i of the compaction tests:
// files of one size, so a count of files is a count of bytes.
func compactionFixtureFile(i int) string {
	return fmt.Sprintf("package main\n// compact_marker_%02d\n", i) + strings.Repeat("// filler line\n", 40)
}

// TestDeltaUncommitted_TombstonedBytesCompactTheFamily removes 3 of 10 dirty
// files of one size, then 2 more. The first delta tombstones 3 files against 7
// live ones and stays a delta. With the second, deltas tombstoned 5 files
// against 5 live ones, so the build replaces the family
// (deltaNeedsCompaction).
func TestDeltaUncommitted_TombstonedBytesCompactTheFamily(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two of these files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
	dir := initGitRepo(t, "app.go", "package main\n// committed_base\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("d%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reindexGit(t, ctx, paths, plan)
	remove := func(files ...int) {
		t.Helper()
		for _, i := range files {
			if err := os.Remove(filepath.Join(dir, fmt.Sprintf("d%02d.go", i))); err != nil {
				t.Fatal(err)
			}
		}
		reindexGit(t, ctx, paths, plan)
	}

	before := readShardFileState(t, plan.indexDir, repoUncommitted)
	remove(0, 1, 2)
	requireTombstoneOnlyDelta(t, before, readShardFileState(t, plan.indexDir, repoUncommitted))

	before = readShardFileState(t, plan.indexDir, repoUncommitted)
	remove(3, 4)
	requireFullRebuild(t, before, readShardFileState(t, plan.indexDir, repoUncommitted))
	for i := range 10 {
		results, err := searchPlannedCorpusForTest(ctx, plan, fmt.Sprintf("compact_marker_%02d", i))
		if want := min(max(i-4, 0), 1); err != nil || len(results) != want {
			t.Fatalf("compact_marker_%02d matches %d files, want %d; error=%v", i, len(results), want, err)
		}
	}
}

// TestNameOnlyMarkerBytes checks the bytes that a shard document holds for
// each kind of document (heldContentBytes). A document kept by name only, by
// the reader or by Zoekt's own check, holds Zoekt's NOT-INDEXED marker. Any
// other document holds its content.
func TestNameOnlyMarkerBytes(t *testing.T) {
	late := append(bytes.Repeat([]byte("a"), 9000), 0)
	for _, test := range []struct {
		name string
		doc  fileContent
		held int64
	}{
		{"text", fileContent{content: []byte("package main\n")}, 13},
		{"empty", fileContent{}, 0},
		{"two bytes", fileContent{content: []byte("ab")}, int64(len("NOT-INDEXED: contains too few trigrams"))},
		{"NUL after the reader's probe", fileContent{content: late}, int64(len("NOT-INDEXED: contains binary content"))},
		{"binary by the reader", fileContent{skipReason: index.SkipReasonBinary}, int64(len("NOT-INDEXED: contains binary content"))},
		{"too large", fileContent{skipReason: index.SkipReasonTooLarge}, int64(len("NOT-INDEXED: exceeds the maximum size limit"))},
		{"no bytes", fileContent{skipReason: seekNoBytesReason}, int64(len("NOT-INDEXED: contains too many trigrams"))},
	} {
		if got := heldContentBytes(test.doc); got != test.held {
			t.Errorf("%s: held=%d, want %d", test.name, got, test.held)
		}
	}
}

// binaryFixtureFile is a file of 8,000 bytes with NUL bytes: its shard holds
// a marker of 36 bytes.
var binaryFixtureFile = bytes.Repeat([]byte{0, 1, 2, 3}, 2000)

// textFixture8000 is a text file of 8,000 bytes.
var textFixture8000 = bytes.Repeat([]byte("// text line of about eighty bytes for the large text file of the fixture\n"), 110)[:8000]

// TestDeltaUncommitted_TextFileThatBecomesBinaryCompacts removes 2 of 10 dirty
// text files beside a text file of 8,000 bytes, then makes that file binary.
// Before the reads, the file counts at its size, so the rule keeps a delta.
// After the reads, it counts as its 36-byte marker. The 2 dead text files and
// its old 8,000 bytes then reach the bytes of the 8 live text files and the
// marker. So the family builds in full (deltaNeedsCompaction).
func TestDeltaUncommitted_TextFileThatBecomesBinaryCompacts(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two of these files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
	dir := initGitRepo(t, "app.go", "package main\n// committed_base\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("d%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), textFixture8000, 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	for i := range 2 {
		if err := os.Remove(filepath.Join(dir, fmt.Sprintf("d%02d.go", i))); err != nil {
			t.Fatal(err)
		}
	}
	reindexGit(t, ctx, paths, plan)
	before := readShardFileState(t, plan.indexDir, repoUncommitted)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), binaryFixtureFile, 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	requireFullRebuild(t, before, readShardFileState(t, plan.indexDir, repoUncommitted))
}

// TestDeltaUncommitted_BinaryFileThatBecomesTextStaysADelta removes 4 of 10
// dirty text files beside a binary file, then 2 more while the binary file
// becomes 20,280 bytes of text. Before the reads, the changed file counts at
// an upper bound of its shard bytes, not at its old marker. So the rule keeps
// the delta that the read bytes keep. The 6 dead text files and the old marker
// stay below the live bytes (deltaNeedsCompaction).
func TestDeltaUncommitted_BinaryFileThatBecomesTextStaysADelta(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two of these files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
	dir := initGitRepo(t, "app.go", "package main\n// committed_base\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("d%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), binaryFixtureFile, 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	remove := func(files ...int) {
		t.Helper()
		for _, i := range files {
			if err := os.Remove(filepath.Join(dir, fmt.Sprintf("d%02d.go", i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	remove(0, 1, 2, 3)
	reindexGit(t, ctx, paths, plan)
	before := readShardFileState(t, plan.indexDir, repoUncommitted)
	remove(4, 5)
	text := bytes.Repeat([]byte("// text line of the former binary file\n"), 520)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), text, 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	requireTombstoneOnlyDeltaOrNewShard(t, before, readShardFileState(t, plan.indexDir, repoUncommitted))
}

// TestDeltaUncommitted_BinaryFilesCountTheirMarker removes 8 of 10 dirty text
// files beside a dirty binary file of 8,000 bytes. The binary file counts as
// its marker, so 8 dead text files against 2 live ones and the marker rebuild
// the family (deltaNeedsCompaction).
func TestDeltaUncommitted_BinaryFilesCountTheirMarker(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two of these files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
	dir := initGitRepo(t, "app.go", "package main\n// committed_base\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("d%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), binaryFixtureFile, 0o644); err != nil {
		t.Fatal(err)
	}
	reindexGit(t, ctx, paths, plan)
	before := readShardFileState(t, plan.indexDir, repoUncommitted)
	for i := range 8 {
		if err := os.Remove(filepath.Join(dir, fmt.Sprintf("d%02d.go", i))); err != nil {
			t.Fatal(err)
		}
	}
	reindexGit(t, ctx, paths, plan)
	requireFullRebuild(t, before, readShardFileState(t, plan.indexDir, repoUncommitted))
}

// TestDeltaUncommitted_OnlyAMissingFileIsGone stats a deleted file and a file
// at the root whose name is longer than 255 bytes. Lstat of the long name fails
// with ENAMETOOLONG on Linux and macOS. The file is at the root: below a
// missing folder, Lstat would fail with ENOENT first. Only the deleted file is
// gone, and the delta reads the other one. A gone file gets no manifest entry,
// so when it comes back, the delta tombstones nothing.
func TestDeltaUncommitted_OnlyAMissingFileIsGone(t *testing.T) {
	long := strings.Repeat("z", 300) + ".txt"
	candidates := statUncommittedCandidates(t.TempDir(), []string{"deleted.txt", long})
	if len(candidates) != 2 || !candidates[0].gone || candidates[1].gone {
		t.Fatalf("candidates=%+v, want deleted.txt gone and the long path not", candidates)
	}
	if entries := uncommittedManifestEntries(candidates); len(entries) != 1 || entries[0].Name != long {
		t.Fatalf("entries=%+v, want the long path only", entries)
	}
	toRead, changed, _ := diffUncommittedAgainstManifest(candidates, uncommittedManifest{})
	if len(toRead) != 1 || toRead[0].Name != long || !slices.Equal(changed, []string{long}) {
		t.Fatalf("toRead=%+v changed=%v, want the long path only", toRead, changed)
	}
}

func TestDeltaUncommitted_ConcurrentSearchSeesConsistentResults(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "stable.go", "package main\n// uncommitted_concurrent_stable\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	const iterations = 12
	var missed atomic.Int64
	var searchErr atomic.Int64
	var wg sync.WaitGroup
	// The writer closes reindexing[i] when cycle i starts its reindex, and
	// search i starts then. So search i runs beside cycle i, after the
	// publish of each cycle before it.
	reindexing := make([]chan struct{}, iterations)
	for i := range reindexing {
		reindexing[i] = make(chan struct{})
	}

	wg.Go(func() {
		started := 0
		// A writer that stops early starts the remaining searches.
		defer func() {
			for _, ch := range reindexing[started:] {
				close(ch)
			}
		}()
		for i := range iterations {
			if err := os.WriteFile(filepath.Join(dir, "app.go"), fmt.Appendf(nil, "package main\n// concurrent_uncommitted_%d\n", i), 0o644); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			state, err := gitRepoStateIn(ctx, dir)
			if err != nil {
				t.Errorf("read repository state: %v", err)
				return
			}
			hash := gitCorpusStateHash(paths, state)
			close(reindexing[i])
			started++
			if err := runIndexingWithCache(ctx, paths, plan.corpusStore, state, hash); err != nil {
				t.Errorf("reindex: %v", err)
				return
			}
		}
	})

	for i := range iterations {
		wg.Go(func() {
			<-reindexing[i]
			res, err := searchPlannedCorpusForTest(ctx, plan, "uncommitted_concurrent_stable")
			if err != nil {
				searchErr.Add(1)
				return
			}
			if len(res) == 0 {
				missed.Add(1)
			}
		})
	}
	wg.Wait()

	if missed.Load() > 0 {
		t.Errorf("%d/%d concurrent searches lost the stable committed marker during uncommitted delta cycles", missed.Load(), iterations)
	}
	if searchErr.Load() > 0 {
		t.Errorf("%d concurrent searches errored", searchErr.Load())
	}
}
