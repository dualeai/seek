package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// BenchmarkLargeRepo_CommittedAdvance measures the cost of reindexing after
// the repo's HEAD advances by exactly one commit.
//
// The setup takes a forward chain of recent commit SHAs that ends at the
// current HEAD. It makes a temporary clone under b.TempDir(), checks out a
// local benchmark branch at the oldest commit, and builds a base index. Each
// iteration resets the clone to the next SHA outside the timer, then times the
// node refresh that a search runs (gitNode.prepare). The sampled chain has at
// most ten advances. When b.Loop runs more rounds, the benchmark resets the
// clone to the oldest commit, indexes it outside the timer, and replays the
// chain.
// Each timed iteration advances by one real commit, not between two distant
// commits.
//
// The benchmark only reads SEEK_BENCH_REPO; the hard resets change the
// temporary clone.
func BenchmarkLargeRepo_CommittedAdvance(b *testing.B) {
	sourceRepo := requireBenchRepo(b)
	commits := strings.Fields(gitOutputIn(b, sourceRepo, "rev-list", "--first-parent", "--reverse", "--max-count=11", "HEAD"))
	if len(commits) < 2 {
		b.Fatalf("SEEK_BENCH_REPO needs at least 2 commits in its history, got %d", len(commits))
	}
	for i := 1; i < len(commits); i++ {
		if parent := gitOutputIn(b, sourceRepo, "rev-parse", commits[i]+"^1"); parent != commits[i-1] {
			b.Fatalf("benchmark chain is not a direct first-parent advance: %s parent=%s, want %s", commits[i], parent, commits[i-1])
		}
	}
	base := commits[0]
	chain := commits[1:]

	repoDir := cloneBenchRepoAt(b, sourceRepo, base)
	paths, plan := setupLargeRepoBench(b, repoDir)
	ctx := b.Context()

	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		// chain[0] is one commit ahead of base; advance through the chain.
		// When b.Loop() runs more rounds than the chain has advances, reset
		// the temporary clone to base and replay.
		idx := i % len(chain)
		if idx == 0 && i > 0 {
			gitRunIn(b, repoDir, "reset", "--hard", base)
			if _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
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

		if _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
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

// BenchmarkLargeRepo_UncommittedRealistic measures quick editor saves through
// the node refresh that a search runs (gitNode.prepare): git status, the index
// checks, and the build. The timer also covers the write of each save. Seek
// must make this case fast: an editor that saves one file again and again.
func BenchmarkLargeRepo_UncommittedRealistic(b *testing.B) {
	repoDir := cloneBenchRepoAt(b, requireBenchRepo(b), "HEAD")
	paths, plan := setupLargeRepoBench(b, repoDir)
	ctx := b.Context()

	target := findSourceFiles(b, repoDir, 1)[0]
	original, err := os.ReadFile(target)
	if err != nil {
		b.Fatal(err)
	}

	published := readStateFile(plan.cacheDir)
	for i := 0; b.Loop(); i++ {
		// Saves alternate in size, so a file system with whole-second times
		// still sees each save.
		body := fmt.Appendf(slices.Clip(original), "\n// rapid_edit_marker_%09d%s\n", i, strings.Repeat("x", 1+i%2))
		if err := os.WriteFile(target, body, 0o644); err != nil {
			b.Fatal(err)
		}
		if _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
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
