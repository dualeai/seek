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
// next SHA and time the node refresh that a search runs (gitNode.refresh). The
// sampled chain has at most ten advances. If Go's benchmark framework wants
// more, it resets the loop and replays the chain. Each timed iteration
// advances by one real commit; it is not a reflog oscillation between distant
// points.
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

	if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
		b.Fatalf("cold base index: %v", err)
	}

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		// chain[0] is one commit ahead of base; advance through the chain.
		// When b.Loop() runs more rounds than the chain has advances, reset
		// the scratch clone to base and replay.
		idx := i % len(chain)
		if idx == 0 && i > 0 {
			gitRunIn(b, repoDir, "reset", "--hard", base)
			if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
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

		if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
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
// the node refresh that a search runs (gitNode.refresh): git status, the index
// checks, and the build. This is the workflow seek has to optimize: an editor
// saving the same file in a loop.
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

	published := readStateFile(plan.cacheDir)
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		// Saves alternate in size, so a file system with whole-second times
		// still sees each save.
		body := fmt.Appendf(original[:len(original):len(original)], "\n// rapid_edit_marker_%09d%s\n", i, strings.Repeat("x", 1+i%2))
		if err := os.WriteFile(target, body, 0o644); err != nil {
			b.Fatal(err)
		}
		if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
			b.Fatalf("reindex iter %d: %v", i, err)
		}
		// Each save changes the dirty file's size or mtime, so the refresh
		// publishes a new state.
		b.StopTimer()
		state := readStateFile(plan.cacheDir)
		if state == "" || state == published {
			b.Fatalf("iter %d published no new state: %q", i, state)
		}
		published = state
		b.StartTimer()
	}
}
