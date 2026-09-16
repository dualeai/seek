package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDrift_KeepsTheCommittedSemanticGeneration checks that a build interrupted
// by a working-tree change keeps its finished committed vectors.
//
// The generation is keyed by the commit, is built from a validated snapshot and
// never reads the working tree, so a working-tree change cannot invalidate it.
// Discarding it means embedding the same commit again on the next search.
//
// It also checks the prune: publishing without binding must not leave a
// generation behind for every drifted build.
func TestDrift_KeepsTheCommittedSemanticGeneration(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "app.go", "package main\n// DRIFT_SEMANTIC_MARKER\n")
	setTestUserCache(t)
	paths, plan := planGitTestCorpus(t, repo)
	ctx := context.Background()

	scorer := &hybridTestScorer{
		vectorForUnit: func(semanticUnit) semanticVector {
			// Change the working tree while the build runs, so the post-build
			// state check disagrees and the drift branch runs.
			writeFileAt(t, repo, "late.go", "package main\n// arrived during the build\n")
			return semanticVector{1: 1}
		},
	}
	execution := testSemanticSearchExecution(t, func(context.Context) (semanticModel, error) {
		return scorer, nil
	})

	state := mustGitRepoStateIn(t, ctx, repo)
	// Drive the corpus entry point rather than the indexer: it derives the
	// state hash itself, so this test does not restate how production builds
	// it. The drift branch reports a stale index, which is expected here.
	_, _ = ensureCombinedGitCorpusWithExecution(ctx, plan, paths, state, execution)

	dirs := semanticGenerationDirs(t, plan.indexDir)
	if len(dirs) == 0 {
		t.Fatal("the drifted build discarded its committed semantic generation")
	}
	if len(dirs) > 1 {
		t.Fatalf("the drifted build left %d generations behind: %v", len(dirs), dirs)
	}
	if !semanticGenerationPresent(plan.indexDir, state.HeadSHA) {
		t.Fatal("the kept generation is not the one built for the indexed commit")
	}
	// Unbound: nothing may bind it on the drifted run.
	if joinedGenerationMatches(plan.cacheDir, plan.indexDir, readStateFile(plan.cacheDir), state.HeadSHA) {
		t.Fatal("the drifted build bound a generation to an untrusted state")
	}

	// The prune must actually run. Put a second, unrelated generation beside
	// the kept one and drift again: the unbound publish must remove it, or
	// every drifted HEAD would leave one more generation behind.
	stale := semanticGenerationDir(plan.indexDir, "0000000000000000000000000000000000000000")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := len(semanticGenerationDirs(t, plan.indexDir)); got != 2 {
		t.Fatalf("setup left %d generations, want 2", got)
	}

	writeFileAt(t, repo, "third.go", "package main\n// forces another build\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-m", "third")
	nextState := mustGitRepoStateIn(t, ctx, repo)
	_, _ = ensureCombinedGitCorpusWithExecution(ctx, plan, paths, nextState, execution)

	after := semanticGenerationDirs(t, plan.indexDir)
	for _, dir := range after {
		if dir == stale {
			t.Fatalf("the unbound publish kept an unrelated generation: %v", after)
		}
	}
	if len(after) != 1 {
		t.Fatalf("the unbound publish left %d generations: %v", len(after), after)
	}
}

// semanticGenerationDirs lists the semantic generation directories in indexDir.
func semanticGenerationDirs(tb testing.TB, indexDir string) []string {
	tb.Helper()
	dirs, err := filepath.Glob(filepath.Join(indexDir, semanticGenerationPrefix+"*"))
	if err != nil {
		tb.Fatalf("list semantic generations: %v", err)
	}
	return dirs
}
