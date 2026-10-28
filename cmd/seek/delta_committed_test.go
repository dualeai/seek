package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// reindexGit runs one build of the Git node at plan for the current state of
// the repository. It passes the state hash that a search gives the build. The
// build picks a committed delta or a full build.
func reindexGit(t testing.TB, ctx context.Context, paths gitPaths, plan node) {
	t.Helper()
	state := mustGitRepoStateIn(t, ctx, paths.RepoDir)
	hash := gitCorpusStateHash(paths, state)
	if err := runIndexingWithCache(ctx, paths, plan.corpusStore, state, hash); err != nil {
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
	gitRunIn(t, dir, "add", ".")
	gitRunIn(t, dir, "commit", "-m", "v2")

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

	// A delta adds one shard to the family. A full build replaces the family,
	// so the count would stay the same.
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
	gitRunIn(t, dir, "add", "-A")
	gitRunIn(t, dir, "commit", "-m", "remove doomed")

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

	gitRunIn(t, dir, "mv", "old.go", "new.go")
	gitRunIn(t, dir, "commit", "-m", "rename")

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
		gitRunIn(t, dir, "add", ".")
		gitRunIn(t, dir, "commit", "-m", fmt.Sprintf("step %d", i))
		reindexGit(t, ctx, paths, plan)
	}

	// Before the reset, each step is found.
	for i := range 5 {
		pat := fmt.Sprintf("rewind_step_%d", i)
		results, _ := searchPlannedCorpusForTest(ctx, plan, pat)
		if len(results) == 0 {
			t.Fatalf("pre-rewind %s must be findable", pat)
		}
	}

	// The first cycle builds in full, and each later cycle adds one delta
	// shard. A full build in each cycle would leave one shard.
	shardsBefore := committedShardCount(t, plan.indexDir)
	if shardsBefore < 2 {
		t.Fatalf("expected delta accumulation across 5 commits, got %d shard(s) — delta path may have silently fallen back", shardsBefore)
	}

	// Reset to step 0 (HEAD~4). The indexed commit stays in the object store,
	// so the native delta can diff it against the new HEAD. The content of the
	// 4 later commits must leave the index.
	gitRunIn(t, dir, "reset", "--hard", "HEAD~4")
	reindexGit(t, ctx, paths, plan)

	for i := range 5 {
		pat := fmt.Sprintf("rewind_step_%d", i)
		results, err := searchPlannedCorpusForTest(ctx, plan, pat)
		if err != nil {
			t.Fatalf("post-rewind search %s: %v", pat, err)
		}
		// Only step 0, at the new HEAD, stays. The reset removed the files
		// of steps 1 to 4.
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

	// Make 10 commits, with a build after each: each build is a delta and
	// adds a shard.
	for i := range 10 {
		name := fmt.Sprintf("step%d.go", i)
		body := fmt.Sprintf("package main\n// rebase_step_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, dir, "add", ".")
		gitRunIn(t, dir, "commit", "-m", fmt.Sprintf("step %d", i))
		reindexGit(t, ctx, paths, plan)
	}

	shardsBefore := committedShardCount(t, plan.indexDir)
	if shardsBefore < 2 {
		t.Fatalf("setup must accumulate multiple delta shards (got %d) — delta path may not be engaged", shardsBefore)
	}

	// Squash the 10 commits into one new commit with the same tree. The
	// indexed commit stays in the object store, so the native delta can diff
	// it against the new HEAD, and the diff is empty. Each marker must still
	// give exactly one file.
	gitRunIn(t, dir, "reset", "--soft", "HEAD~10")
	gitRunIn(t, dir, "commit", "-m", "squashed steps")
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

	dir := initGitRepo(t, "seed.go", "package main\n// threshold_seed\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// Make maxCommittedDeltaShards + 2 commits, with a build after each, and
	// record the committed shard count after each build.
	cycles := maxCommittedDeltaShards + 2
	shardSeries := make([]int, 0, cycles)
	for i := range cycles {
		name := fmt.Sprintf("c%d.go", i)
		body := fmt.Sprintf("package main\n// threshold_cycle_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, dir, "add", ".")
		gitRunIn(t, dir, "commit", "-m", fmt.Sprintf("cycle %d", i))
		reindexGit(t, ctx, paths, plan)
		shardSeries = append(shardSeries, committedShardCount(t, plan.indexDir))
	}

	// The full build of the seed gives one shard, and each delta adds one.
	// The check permits a delta while the family holds at most
	// maxCommittedDeltaShards delta shards, so that delta takes the family
	// above the threshold. The next build is a full build of one shard.
	want := make([]int, 0, cycles)
	for i := range maxCommittedDeltaShards + 1 {
		want = append(want, i+2)
	}
	want = append(want, 1)
	if !slices.Equal(shardSeries, want) {
		t.Fatalf("committed shard counts=%v, want %v", shardSeries, want)
	}

	// The full build holds the content of the earlier deltas: the first, a
	// middle, and the last cycle are found.
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

	// The native committed reader skips gitlinks. The host repository's delta
	// path must remain searchable after a submodule and a later commit are
	// added.
	dir := initGitRepo(t, "app.go", "package main\n// submodule_delta_marker\n")
	subSrc := initEmptyGitRepo(t)
	if err := os.WriteFile(filepath.Join(subSrc, "sub.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, subSrc, "add", ".")
	gitRunIn(t, subSrc, "commit", "-m", "sub initial")

	gitRunIn(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", subSrc, "mysub")
	gitRunIn(t, dir, "commit", "-m", "add submodule")

	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	if err := os.WriteFile(filepath.Join(dir, "added.go"), []byte("package main\n// submodule_delta_added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", ".")
	gitRunIn(t, dir, "commit", "-m", "post-submodule add")
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
			name := fmt.Sprintf("step%d.go", i)
			body := fmt.Sprintf("package main\n// concurrent_delta_step_%d\n", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			gitRunIn(t, dir, "add", ".")
			gitRunIn(t, dir, "commit", "-m", fmt.Sprintf("step %d", i))
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
			res, err := searchPlannedCorpusForTest(ctx, plan, "concurrent_delta_stable")
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
		t.Errorf("%d/%d concurrent searches lost the stable marker during delta cycles", missed.Load(), iterations)
	}
	if searchErr.Load() > 0 {
		t.Errorf("%d concurrent searches errored", searchErr.Load())
	}
}
