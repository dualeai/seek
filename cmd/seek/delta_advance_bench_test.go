package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// BenchmarkLargeRepo_CommittedAdvance measures the cost of reindexing after
// the repo's HEAD advances by exactly one commit.
//
// Setup: capture a forward chain of recent commit SHAs ending at the current
// HEAD, create a scratch clone under b.TempDir(), check out a local benchmark
// branch at the oldest commit, build a base index, then reset the clone to each
// next SHA and time indexCommitted. Iterations are capped to len(chain)-1; if
// Go's benchmark framework wants more, it gets a loop reset (which behaves
// like a fresh advance from the oldest). Each iteration is a real one-commit
// advance, not a reflog oscillation between two distant positions.
//
// SEEK_BENCH_REPO is only used as a read-only source; hard resets happen in the
// temp clone.
func BenchmarkLargeRepo_CommittedAdvance(b *testing.B) {
	sourceRepo := requireBenchRepo(b)
	commits := strings.Fields(gitOutputIn(b, sourceRepo, "rev-list", "--reverse", "--max-count=11", "HEAD"))
	if len(commits) < 2 {
		b.Skipf("need at least 2 commits in SEEK_BENCH_REPO history, got %d", len(commits))
	}
	base := commits[0]
	chain := commits[1:]

	repoDir := cloneBenchRepoAt(b, sourceRepo, base)
	paths, plan := planGitTestCorpus(b, repoDir)

	if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
		b.Fatalf("cold base index: %v", err)
	}

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		// chain[0] is one commit ahead of base; advance through the chain.
		// When b.Loop() exhausts the chain (only on -benchtime=Nx with N >
		// len(chain)), reset the scratch clone to base and replay.
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
}

// BenchmarkLargeRepo_UncommittedRealistic measures rapid editor saves through
// the full runIndexingWithCache orchestrator. This is the workflow seek has to
// optimize: an editor saving the same file in a loop.
func BenchmarkLargeRepo_UncommittedRealistic(b *testing.B) {
	repoDir := cloneBenchRepoAt(b, requireBenchRepo(b), "HEAD")
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
}
