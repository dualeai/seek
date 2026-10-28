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
// current HEAD, and makes a temporary clone under b.TempDir(). Each round
// takes the next advance of the chain. Outside the timer, it resets the clone
// to the parent commit and builds a new base index there, in a new cache.
// Then it resets the clone to the child commit and times the node refresh
// that a search runs (gitNode.prepare). The sampled chain has at most ten
// advances; more rounds replay it.
//
// Each round starts from a new base, so the deltas do not stack. Stacked
// deltas keep the content that they replace. When that dead content reaches
// one full shard, the refresh builds in full (deltaNeedsCompaction), and how
// soon that happens depends on the size of the sampled commits. A new base
// makes each round time the same one-commit delta for any history.
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

	repoDir := cloneBenchRepoAt(b, sourceRepo, commits[0])
	ctx := b.Context()

	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		// commits[idx] is the parent of commits[idx+1].
		idx := i % (len(commits) - 1)
		parent, child := commits[idx], commits[idx+1]
		gitRunIn(b, repoDir, "reset", "--hard", parent)
		paths, plan := setupLargeRepoBench(b, repoDir)
		gitRunIn(b, repoDir, "reset", "--hard", child)
		before, ok := readCommittedGitState(plan.cacheDir)
		if !ok {
			b.Fatal("committed benchmark base state is missing")
		}
		if before.head.String() != parent || before.baseHead.String() != parent {
			b.Fatalf("iteration %d base state head=%s base=%s, want a new base at the parent %s",
				i, before.head, before.baseHead, parent)
		}
		b.StartTimer()

		if _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
			b.Fatalf("delta advance index (iter %d): %v", i, err)
		}
		b.StopTimer()
		after, ok := readCommittedGitState(plan.cacheDir)
		if !ok || after.head.String() != child || after.baseHead != before.baseHead {
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
