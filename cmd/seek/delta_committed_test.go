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

// reindexGit runs one full indexing cycle (committed + uncommitted) against
// the given repo and corpus plan. Returns the post-index shard count for
// repoUncommitted to let callers assert on shard accumulation.
func reindexGit(t *testing.T, ctx context.Context, paths gitPaths, plan corpusPlan) {
	t.Helper()
	state := mustGitRepoStateIn(t, ctx, paths.RepoDir)
	hash := gitCorpusStateHash(paths, state)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, hash); err != nil {
		t.Fatalf("runIndexingWithCache: %v", err)
	}
}

func TestDeltaCommitted_ModifyFileTombstonesOldContent(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "foo.go", "package main\n// delta_modify_v1\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	reindexGit(t, ctx, paths, plan)

	results, err := searchPlannedCorpusForTest(ctx, plan, "delta_modify_v1")
	if err != nil || len(results) == 0 {
		t.Fatalf("baseline v1 must be findable: results=%d err=%v", len(results), err)
	}
	shardsBefore := committedShardCount(t, plan.indexDir)

	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package main\n// delta_modify_v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "v2")

	reindexGit(t, ctx, paths, plan)

	v1, err := searchPlannedCorpusForTest(ctx, plan, "delta_modify_v1")
	if err != nil {
		t.Fatalf("search v1 after delta: %v", err)
	}
	if len(v1) != 0 {
		t.Fatalf("v1 should be tombstoned, got %d hits", len(v1))
	}
	v2, err := searchPlannedCorpusForTest(ctx, plan, "delta_modify_v2")
	if err != nil || len(v2) == 0 {
		t.Fatalf("v2 must be findable post-delta: results=%d err=%v", len(v2), err)
	}

	// Delta-path proof: each delta cycle stacks exactly one new shard on
	// top of the existing set (Zoekt index/builder.go:585). A silent
	// fallback to a non-delta rebuild would leave shardsAfter == shardsBefore
	// because the old shard would be replaced rather than supplemented.
	shardsAfter := committedShardCount(t, plan.indexDir)
	if shardsAfter != shardsBefore+1 {
		t.Fatalf("expected delta path to stack +1 shard (before=%d, after=%d) — non-delta rebuild would replace rather than append", shardsBefore, shardsAfter)
	}
}

func TestDeltaCommitted_DeleteFileRemovesFromIndex(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "doomed.go", "package main\n// delta_delete_marker\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	reindexGit(t, ctx, paths, plan)
	if results, _ := searchPlannedCorpusForTest(ctx, plan, "delta_delete_marker"); len(results) == 0 {
		t.Fatal("baseline must find marker")
	}

	if err := os.Remove(filepath.Join(dir, "doomed.go")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "remove doomed")

	reindexGit(t, ctx, paths, plan)

	results, err := searchPlannedCorpusForTest(ctx, plan, "delta_delete_marker")
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("deleted file content should be tombstoned, got %d hits", len(results))
	}
}

func TestDeltaCommitted_RenameKeepsContentAtNewPath(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "old.go", "package main\n// delta_rename_marker\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	reindexGit(t, ctx, paths, plan)

	gitRun(t, dir, "mv", "old.go", "new.go")
	gitRun(t, dir, "commit", "-m", "rename")

	reindexGit(t, ctx, paths, plan)

	results, err := searchPlannedCorpusForTest(ctx, plan, "delta_rename_marker")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 hit at new path, got %d", len(results))
	}
	if results[0].FileName != "new.go" {
		t.Fatalf("expected hit at new.go, got %s", results[0].FileName)
	}
}

func TestDeltaCommitted_HeadRewindFallsBackCleanly(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "base.go", "package main\n// rewind_base\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	for i := range 5 {
		name := fmt.Sprintf("step%d.go", i)
		body := fmt.Sprintf("package main\n// rewind_step_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", ".")
		gitRun(t, dir, "commit", "-m", fmt.Sprintf("step %d", i))
		reindexGit(t, ctx, paths, plan)
	}

	// At HEAD we should find everything.
	for i := range 5 {
		pat := fmt.Sprintf("rewind_step_%d", i)
		results, _ := searchPlannedCorpusForTest(ctx, plan, pat)
		if len(results) == 0 {
			t.Fatalf("pre-rewind %s must be findable", pat)
		}
	}

	// Delta-path proof: five HEAD-advancing reindex cycles must have stacked
	// shards (one per delta). A non-delta build would replace shards each
	// time, leaving the count at 1.
	shardsBefore := committedShardCount(t, plan.indexDir)
	if shardsBefore < 2 {
		t.Fatalf("expected delta accumulation across 5 commits, got %d shard(s) — delta path may have silently fallen back", shardsBefore)
	}

	// Hard reset to base — the prior indexed commits still exist in the
	// repo's reflog, so prepareDeltaBuild can diff against them. After reset,
	// 4 commits' worth of content should disappear from the index.
	gitRun(t, dir, "reset", "--hard", "HEAD~4")
	reindexGit(t, ctx, paths, plan)

	for i := range 5 {
		pat := fmt.Sprintf("rewind_step_%d", i)
		results, err := searchPlannedCorpusForTest(ctx, plan, pat)
		if err != nil {
			t.Fatalf("post-rewind search %s: %v", pat, err)
		}
		// Only step_0 (HEAD after reset) should remain. Steps 1..4 were
		// rolled back, their files no longer exist on disk.
		if i == 0 {
			if len(results) == 0 {
				t.Fatalf("step 0 must remain after rewind to HEAD~4")
			}
		} else {
			if len(results) != 0 {
				t.Fatalf("step %d should be gone after rewind, got %d hits", i, len(results))
			}
		}
	}

	if base, _ := searchPlannedCorpusForTest(ctx, plan, "rewind_base"); len(base) == 0 {
		t.Fatal("base content must still be findable after rewind")
	}
}

func TestDeltaCommitted_RebaseLeavesNoDuplicateHits(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "base.go", "package main\n// rebase_base\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// Build a 10-commit chain. Indexing after each commit exercises the
	// delta path and accumulates shards.
	for i := range 10 {
		name := fmt.Sprintf("step%d.go", i)
		body := fmt.Sprintf("package main\n// rebase_step_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", ".")
		gitRun(t, dir, "commit", "-m", fmt.Sprintf("step %d", i))
		reindexGit(t, ctx, paths, plan)
	}

	shardsBefore := committedShardCount(t, plan.indexDir)
	if shardsBefore < 2 {
		t.Fatalf("setup must accumulate multiple delta shards (got %d) — delta path may not be engaged", shardsBefore)
	}

	// Squash + recommit equivalent content under a new history. The prior
	// indexed commits remain reachable via the reflog, so Zoekt's
	// prepareDeltaBuild succeeds; the working tree contents are unchanged
	// so the diff reports no changed files and the cycle is a near-noop.
	// The invariant we care about is search correctness: every previously
	// committed marker still resolves to exactly one file.
	gitRun(t, dir, "reset", "--soft", "HEAD~10")
	gitRun(t, dir, "commit", "-m", "squashed steps")
	reindexGit(t, ctx, paths, plan)

	for i := range 10 {
		pat := fmt.Sprintf("rebase_step_%d", i)
		results, err := searchPlannedCorpusForTest(ctx, plan, pat)
		if err != nil {
			t.Fatalf("search %s: %v", pat, err)
		}
		if len(results) != 1 {
			t.Fatalf("expected exactly 1 hit for %s after rebase, got %d", pat, len(results))
		}
	}
}

func TestDeltaCommitted_ShardThresholdTriggersFullRebuild(t *testing.T) {
	requireTools(t)
	if testing.Short() {
		t.Skip("threshold test makes 65+ commits; skipped under -short")
	}

	dir := initGitRepo(t, "seed.go", "package main\n// threshold_seed\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// Produce maxCommittedDeltaShards + 2 commits, indexing after each.
	// Record shard count every cycle so we can prove three things:
	//   (a) delta path is engaged and stacks shards (count grows past 1)
	//   (b) shard count reaches the threshold (proves seek's growth path)
	//   (c) the (threshold+1)-th cycle drops the count back to a small
	//       number (proves Zoekt's DeltaShardNumberFallbackThreshold guard
	//       fired and seek did NOT keep stacking past the cap).
	cycles := maxCommittedDeltaShards + 2
	shardSeries := make([]int, 0, cycles)
	for i := range cycles {
		name := fmt.Sprintf("c%d.go", i)
		body := fmt.Sprintf("package main\n// threshold_cycle_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", ".")
		gitRun(t, dir, "commit", "-m", fmt.Sprintf("cycle %d", i))
		reindexGit(t, ctx, paths, plan)
		shardSeries = append(shardSeries, committedShardCount(t, plan.indexDir))
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
	if peak < maxCommittedDeltaShards-1 {
		// Allow some slack for empty-shard cleanup but the run must climb
		// at least near the cap to prove the threshold path is meaningful.
		t.Fatalf("peak shard count %d never approached threshold %d — series=%v", peak, maxCommittedDeltaShards, shardSeries)
	}
	finalShards := shardSeries[len(shardSeries)-1]
	if finalShards == 0 {
		t.Fatal("expected at least one committed shard after threshold rebuild")
	}
	if finalShards > maxCommittedDeltaShards {
		t.Fatalf("shard count %d exceeds threshold %d — fallback did not trigger (series=%v)", finalShards, maxCommittedDeltaShards, shardSeries)
	}
	if finalShards >= peak {
		t.Fatalf("threshold trip should drop shard count (peak=%d, final=%d, series=%v)", peak, finalShards, shardSeries)
	}

	// Search across the threshold boundary: the freshest and oldest cycles
	// must both be findable, proving the rebuild absorbed prior deltas.
	for _, i := range []int{0, cycles / 2, cycles - 1} {
		pat := fmt.Sprintf("threshold_cycle_%d", i)
		results, _ := searchPlannedCorpusForTest(ctx, plan, pat)
		if len(results) == 0 {
			t.Fatalf("cycle %d must remain findable post-rebuild", i)
		}
	}
}

func TestDeltaCommitted_SubmoduleHostStaysSearchable(t *testing.T) {
	requireTools(t)

	// Reuse the submodule fixture pattern from git_edge_test.go:568. Zoekt
	// only refuses delta builds for submodules when the caller passes
	// Options.Submodules=true (zoekt/gitindex/index.go:818). Seek does NOT
	// enable submodule walking, so the host repo's delta path runs normally
	// — the submodule directory looks like an opaque blob to gitindex. The
	// invariant we care about is that adding the submodule plus a follow-up
	// commit does not break search.
	dir := initGitRepo(t, "app.go", "package main\n// submodule_delta_marker\n")
	subSrc := initEmptyGitRepo(t)
	if err := os.WriteFile(filepath.Join(subSrc, "sub.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, subSrc, "add", ".")
	gitRunIn(t, subSrc, "commit", "-m", "sub initial")

	gitRun(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", subSrc, "mysub")
	gitRun(t, dir, "commit", "-m", "add submodule")

	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	if err := os.WriteFile(filepath.Join(dir, "added.go"), []byte("package main\n// submodule_delta_added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "post-submodule add")
	reindexGit(t, ctx, paths, plan)

	if results, _ := searchPlannedCorpusForTest(ctx, plan, "submodule_delta_marker"); len(results) == 0 {
		t.Fatal("host marker must remain findable with submodule present")
	}
	if results, _ := searchPlannedCorpusForTest(ctx, plan, "submodule_delta_added"); len(results) == 0 {
		t.Fatal("post-submodule added file must be findable")
	}
}

func TestDeltaCommitted_ConcurrentSearchSeesConsistentResults(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "stable.go", "package main\n// concurrent_delta_stable\n")
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
			name := fmt.Sprintf("step%d.go", i)
			body := fmt.Sprintf("package main\n// concurrent_delta_step_%d\n", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			gitRun(t, dir, "add", ".")
			gitRun(t, dir, "commit", "-m", fmt.Sprintf("step %d", i))
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
			res, err := searchPlannedCorpusForTest(ctx, plan, "concurrent_delta_stable")
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
		t.Errorf("%d/%d concurrent searches lost the stable marker during delta cycles", missed.Load(), iterations)
	}
	if searchErr.Load() > 0 {
		t.Errorf("%d concurrent searches errored", searchErr.Load())
	}
}
