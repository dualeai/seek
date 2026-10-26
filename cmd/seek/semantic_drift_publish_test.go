package main

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDrift_BuildPublishesItsCaptureAndTheNextSearchSeesTheChange changes the
// work tree while a build runs. The build publishes the state that it
// captured and binds its committed vectors, with no second git status. The
// next search captures the change and indexes it.
func TestDrift_BuildPublishesItsCaptureAndTheNextSearchSeesTheChange(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "app.go", "package main\n// DRIFT_SEMANTIC_MARKER\n")
	setTestUserCache(t)
	paths, plan := planGitTestCorpus(t, repo)
	ctx := context.Background()

	scorer := &hybridTestScorer{
		vectorForUnit: func(semanticUnit) semanticVector {
			writeFileAt(t, repo, "late.go", "package main\n// DRIFT_LATE_MARKER arrived during the build\n")
			return semanticVector{1: 1}
		},
	}
	execution := testSemanticSearchExecution(t, modelFactory(scorer))
	state := mustGitRepoStateIn(t, ctx, repo)
	if _, err := (&gitNode{paths: paths}).refreshState(ctx, plan, state, execution); err != nil {
		t.Fatal(err)
	}
	if got := readStateFile(plan.cacheDir); got != gitCorpusStateHash(paths, state) {
		t.Fatalf("the build published state %q, want its capture", got)
	}
	if dirs := semanticGenerationDirs(t, plan.indexDir); len(dirs) != 1 ||
		!plan.joinedGenerationsMatch(readStateFile(plan.cacheDir), state.HeadSHA, "") {
		t.Fatalf("the build did not bind its committed generation: %v", dirs)
	}
	next := mustGitRepoStateIn(t, ctx, repo)
	if gitCorpusStateHash(paths, next) == gitCorpusStateHash(paths, state) {
		t.Fatal("the change during the build did not change the next capture")
	}
	if got := budgetSearchFiles(t, "DRIFT_LATE_MARKER", repo); got != 1 {
		t.Fatalf("the next search found %d files, want late.go", got)
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
