package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dirtyAndIndex rewrites the working-tree file at relPath with content and
// runs one indexing cycle. Returns the post-cycle shard count for
// repoUncommitted so tests can assert on shard accumulation under successive
// edits. The mtime is forced into the future to defeat filesystems that
// collapse two writes within the same nanosecond, which would otherwise hide
// the modification from statUncommittedCandidates.
func dirtyAndIndex(t *testing.T, ctx context.Context, paths gitPaths, plan corpusPlan, relPath, content string) int {
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
	// Shard count snapshots after each cycle. Used to distinguish delta-path
	// behaviour (monotonic growth +1 per cycle, modulo empty-shard cleanup)
	// from a hypothetical silent fallback to full rebuild (would stay at 1).
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
	// Delta-path proof: the shard count must grow over the run. A silently
	// degraded full-rebuild path would stay at 1 shard throughout because
	// each rebuild replaces the prior shard set.
	if finalShards <= 1 {
		t.Fatalf("expected uncommitted shard accumulation under delta, got series %v — delta path may have silently fallen back to full rebuild", shardSeries)
	}
	if finalShards < shardSeries[0] {
		t.Fatalf("shard count regressed mid-run %v — delta accumulation broken", shardSeries)
	}
}

func TestDeltaUncommitted_DirtyToCommittedTombstonesUncommittedShard(t *testing.T) {
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

	// Commit it and index. The uncommitted shard must tombstone the file so
	// the committed shard's content wins and we never get duplicate hits.
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "promote dirty to committed")
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

	// Dirty content shadows committed content at the presentation layer
	// (formatter.go drops committed hits for paths that appear in the
	// dirty set, see formatter.go:110). Use runSeekInRepo so the test
	// exercises the same code path users see.
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
		t.Fatalf("committed content must be shadowed by dirty edit at the formatter, got %v", committedAfter)
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
	gitRun(t, dir, "checkout", "--", "app.go")
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

	// Establish a manifest + a few delta shards by chaining several dirty
	// cycles. We need shardsBefore > 1 so the fallback's "single fresh
	// shard" outcome is unambiguously distinguishable from the prior state.
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

	// Next cycle should detect the corruption (readUncommittedManifest
	// returns ok=false), fall back to the full rebuild, and produce a fresh
	// single base shard plus a regenerated manifest.
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

	// Direct proof the fallback path ran:
	//   1. full rebuild via indexDocuments emits exactly one shard for the
	//      uncommitted repo, so shard count must collapse.
	//   2. the fallback writes a fresh manifest with valid JSON whose state
	//      field is non-empty (was overwritten from the truncated payload).
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
	if testing.Short() {
		t.Skip("threshold test does maxUncommittedDeltaShards+2 cycles; skipped under -short")
	}

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

	// Read the manifest directly and assert every dirty file appears with a
	// real stat fingerprint matching the on-disk file. Anything weaker (e.g.
	// just checking the file exists) lets a future bug silently truncate or
	// scramble the manifest without failing this test.
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

	// Second cycle adds a brand-new untracked file — the delta must pick it up.
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

	// Delete the first untracked file and edit the second to drift the state.
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
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
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
			if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, hash); err != nil {
				t.Errorf("reindex: %v", err)
				return
			}
		}
	}()

	for i := range iterations {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			time.Sleep(time.Duration(1+idx%4) * time.Millisecond)
			res, err := searchPlannedCorpusForTest(ctx, plan, "uncommitted_concurrent_stable")
			if err != nil {
				searchErr.Add(1)
				return
			}
			if len(res) == 0 {
				missed.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if missed.Load() > 0 {
		t.Errorf("%d/%d concurrent searches lost the stable committed marker during uncommitted delta cycles", missed.Load(), iterations)
	}
	if searchErr.Load() > 0 {
		t.Errorf("%d concurrent searches errored", searchErr.Load())
	}
}
