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
// next SHA and time the production native refresh. The sampled chain has at
// most ten advances. If Go's benchmark framework wants more, it resets the
// loop and replays the chain. Each timed iteration advances by one real commit;
// it is not a reflog oscillation between distant points.
//
// SEEK_BENCH_REPO is only used as a read-only source; hard resets happen in the
// temp clone.
func BenchmarkLargeRepo_CommittedAdvance(b *testing.B) {
	sourceRepo := requireBenchRepo(b)
	commits := strings.Fields(gitOutputIn(b, sourceRepo, "rev-list", "--first-parent", "--reverse", "--max-count=11", "HEAD"))
	if len(commits) < 2 {
		b.Skipf("need at least 2 commits in SEEK_BENCH_REPO history, got %d", len(commits))
	}
	for i := 1; i < len(commits); i++ {
		if parent := gitOutputIn(b, sourceRepo, "rev-parse", commits[i]+"^1"); parent != commits[i-1] {
			b.Fatalf("benchmark chain is not a direct first-parent advance: %s parent=%s, want %s", commits[i], parent, commits[i-1])
		}
	}
	base := commits[0]
	chain := commits[1:]

	repoDir := cloneBenchRepoAt(b, sourceRepo, base)
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, repoDir)

	if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
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
			if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
				b.Fatalf("chain replay base index: %v", err)
			}
		}
		gitRunIn(b, repoDir, "reset", "--hard", chain[idx])
		before, ok := readCommittedGitState(plan.cacheDir)
		if !ok {
			b.Fatal("committed benchmark base state is missing")
		}
		wantBefore := base
		if idx > 0 {
			wantBefore = chain[idx-1]
		}
		if before.head.String() != wantBefore {
			b.Fatalf("iteration %d base state head=%s, want direct parent %s", i, before.head, wantBefore)
		}
		b.StartTimer()

		if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
			b.Fatalf("delta advance index (iter %d): %v", i, err)
		}
		b.StopTimer()
		after, ok := readCommittedGitState(plan.cacheDir)
		if !ok || after.head.String() != chain[idx] || after.baseHead != before.baseHead {
			b.Fatalf("iteration %d did not use the direct delta path: before=%+v after=%+v", i, before, after)
		}
		b.StartTimer()
	}
}

// BenchmarkLargeRepo_UncommittedRealistic measures rapid editor saves through
// the full runIndexingWithCache orchestrator. This is the workflow seek has to
// optimize: an editor saving the same file in a loop.
func BenchmarkLargeRepo_UncommittedRealistic(b *testing.B) {
	repoDir := cloneBenchRepoAt(b, requireBenchRepo(b), "HEAD")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, repoDir)
	if _, _, err := ensureGitCorpusFresh(ctx, &plan, paths); err != nil {
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
		state := mustGitRepoStateIn(b, ctx, repoDir)
		preState := gitCorpusStateHash(paths, state)
		if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, preState); err != nil {
			b.Fatalf("reindex iter %d: %v", i, err)
		}
	}
}
