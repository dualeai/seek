package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkLargeRepo_CommittedAdvance measures the cost of reindexing after
// the repo's HEAD advances by exactly one commit. This is the scenario where
// IsDelta=true wins the most: Zoekt's prepareDeltaBuild diffs the previously
// indexed commit's tree against the new HEAD's tree and only indexes the
// changed blobs. A full rebuild would re-walk the entire tree (17k+ files
// on a kubernetes-sized repo).
//
// Setup: capture a forward chain of 10 commit SHAs ending at the current
// HEAD, reset to the oldest, build a base index, then for each iteration
// `git reset --hard <next sha>` and time indexCommitted. Iterations are
// capped to len(chain)-1; if Go's benchmark framework wants more, it gets a
// loop reset (which behaves like a fresh advance from the oldest). Each
// iteration is a real one-commit advance, not a reflog oscillation between
// two distant positions.
//
// Hard-fails (not just logs) when the committed shard count does not grow
// across the run, because a silent fallback to full rebuild would leave the
// timing alive while invalidating the claim the bench measures the delta
// path. NOTE: this bench mutates SEEK_BENCH_REPO on disk via `git reset
// --hard`. Do not interrupt mid-run; Cleanup restores the original HEAD on
// normal exit.
func BenchmarkLargeRepo_CommittedAdvance(b *testing.B) {
	repoDir := requireBenchRepo(b)
	paths, plan := planGitTestCorpus(b, repoDir)

	origHead := gitOutputIn(b, repoDir, "rev-parse", "HEAD")
	chainRaw := gitOutputIn(b, repoDir, "rev-list", "--reverse", "HEAD~10..HEAD")
	chain := strings.Split(chainRaw, "\n")
	if len(chain) < 2 {
		b.Skipf("need at least 2 commits in the advance chain, got %d", len(chain))
	}
	base := gitOutputIn(b, repoDir, "rev-parse", "HEAD~10")
	gitRunIn(b, repoDir, "reset", "--hard", base)
	b.Cleanup(func() {
		gitRunIn(b, repoDir, "reset", "--hard", origHead)
	})

	if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
		b.Fatalf("cold base index: %v", err)
	}
	startShards := committedShardCount(b, plan.indexDir)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		// chain[0] is one commit ahead of base; advance through the chain.
		// When b.Loop() exhausts the chain (only on -benchtime=Nx with N >
		// len(chain)), reset to base and replay.
		idx := i % len(chain)
		if idx == 0 && i > 0 {
			gitRunIn(b, repoDir, "reset", "--hard", base)
			if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
				b.Fatalf("chain replay base index: %v", err)
			}
		}
		gitRunIn(b, repoDir, "reset", "--hard", chain[idx])
		b.StartTimer()

		if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
			b.Fatalf("delta advance index (iter %d): %v", i, err)
		}
	}
	b.StopTimer()

	endShards := committedShardCount(b, plan.indexDir)
	if endShards <= startShards {
		b.Fatalf("expected committed shard count to grow under IsDelta (start=%d, end=%d) — delta path may have silently fallen back to full rebuild", startShards, endShards)
	}
}

// BenchmarkLargeRepo_UncommittedRealistic measures rapid editor saves through
// the full runIndexingWithCache orchestrator so the manifest binding actually
// chains from one cycle to the next (the per-loop manifest write tags the
// new state and the next gitRepoStateIn picks it up via .state). This is
// the workflow seek has to optimize: an editor saving the same file in a
// loop.
//
// The phase-level BenchmarkLargeRepo_Phases/indexUncommitted_1file does NOT
// chain: cachedState is captured once before the loop, so the delta-path
// manifest binding never matches and every iteration falls back to a full
// rebuild. That bench measures the wasted fallback-attempt overhead, not
// the real workflow.
//
// Hard-fails if the uncommitted shard count stays at 1 after the run, which
// would mean every cycle did a full rebuild (delta never engaged).
func BenchmarkLargeRepo_UncommittedRealistic(b *testing.B) {
	repoDir := requireBenchRepo(b)
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, repoDir)
	if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
		b.Fatalf("initial indexing: %v", err)
	}

	target := findSourceFiles(b, repoDir, 1)[0]
	original, err := os.ReadFile(target)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.WriteFile(target, original, 0o644) })

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		body := fmt.Appendf(original[:len(original):len(original)], "\n// rapid_edit_marker_%d\n", i)
		if err := os.WriteFile(target, body, 0o644); err != nil {
			b.Fatal(err)
		}
		state := gitRepoStateIn(ctx, repoDir)
		preState := gitCorpusStateHash(paths, state)
		if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, preState); err != nil {
			b.Fatalf("reindex iter %d: %v", i, err)
		}
	}
	b.StopTimer()

	count := repositoryShardCount(plan.indexDir, repoUncommitted)
	if count <= 1 {
		b.Fatalf("expected uncommitted shard accumulation under IsDelta, got %d — delta path may have silently fallen back to full rebuild", count)
	}
}

// committedShardCount returns the number of committed-repo shards in
// indexDir. Derived by subtracting the "uncommitted" repo shards from the
// total, which avoids depending on the URL-encoded repository name Zoekt
// uses for the committed prefix.
func committedShardCount(tb testing.TB, indexDir string) int {
	tb.Helper()
	all, err := filepathGlobAllShards(indexDir)
	if err != nil {
		tb.Fatalf("glob shards: %v", err)
	}
	uncommitted := repositoryShardCount(indexDir, repoUncommitted)
	return len(all) - uncommitted
}

func filepathGlobAllShards(indexDir string) ([]string, error) {
	return filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
}
