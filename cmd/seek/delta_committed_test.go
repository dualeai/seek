package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// linkCommittedShards links the shard source to the shard numbers from first
// to last and writes the family manifest. So a test gets a large committed
// family with no build for each shard.
func linkCommittedShards(t *testing.T, indexDir string, source familyMember, first, last int) {
	t.Helper()
	for i := first; i <= last; i++ {
		name := fmt.Sprintf("%s.%05d%s", source.prefix, i, shardSuffix)
		if err := os.Link(filepath.Join(indexDir, source.name), filepath.Join(indexDir, name)); err != nil {
			t.Fatalf("link shard %d: %v", i, err)
		}
	}
	scan, err := scanFamily(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(scan.members))
	for _, member := range scan.members {
		names = append(names, member.name)
	}
	if err := writeFamilyManifest(indexDir, names); err != nil {
		t.Fatal(err)
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

// TestDeltaCommitted_ShardThresholdTriggersFullRebuild takes a committed
// family to the delta shard limit and checks the build that comes next. The
// limit reads only the shard count, so links to the first delta shard fill
// the family. A commit and a build for each shard would give the same count.
func TestDeltaCommitted_ShardThresholdTriggersFullRebuild(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "seed.go", "package main\n// threshold_seed\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)

	// commit commits cycle i, builds, and gives the committed shard count.
	commit := func(i int) int {
		t.Helper()
		name := fmt.Sprintf("c%d.go", i)
		body := fmt.Sprintf("package main\n// threshold_cycle_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, dir, "add", ".")
		gitRunIn(t, dir, "commit", "-m", fmt.Sprintf("cycle %d", i))
		reindexGit(t, ctx, paths, plan)
		return committedShardCount(t, plan.indexDir)
	}

	// The full build of the seed gives one shard, and the delta adds one.
	if got := commit(0); got != 2 {
		t.Fatalf("committed shards after the first delta=%d, want 2", got)
	}
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	var delta familyMember
	for _, member := range scan.members {
		if member.shard && !member.uncommitted && member.numbered && member.num == 1 {
			delta = member
		}
	}
	if delta.name == "" {
		t.Fatalf("family %+v has no delta shard one", scan.members)
	}
	linkCommittedShards(t, plan.indexDir, delta, 2, maxCommittedDeltaShards)

	// The check permits a delta while the family holds at most
	// maxCommittedDeltaShards delta shards, so that delta takes the family
	// above the threshold. The next build is a full build of one shard.
	if got := commit(1); got != maxCommittedDeltaShards+2 {
		t.Fatalf("committed shards after the delta at the threshold=%d, want %d", got, maxCommittedDeltaShards+2)
	}
	if got := commit(2); got != 1 {
		t.Fatalf("committed shards after the delta above the threshold=%d, want the full build of 1", got)
	}

	// The full build holds the content of every commit once.
	for i := range 3 {
		pat := fmt.Sprintf("threshold_cycle_%d", i)
		if results, err := searchPlannedCorpusForTest(ctx, plan, pat); err != nil || len(results) != 1 {
			t.Fatalf("cycle %d after the full build: results=%d err=%v, want 1", i, len(results), err)
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
