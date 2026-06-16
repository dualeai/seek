package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanDiscoveredGitCorpus_MatchesExplicitID — a discovered plan
// for the same physical repo as an explicit operand must produce the
// same corpusID so dedup works without coordinating through user-facing
// state. corpusID is keyed on (kind, root_type, root, dev:ino, …).
func TestPlanDiscoveredGitCorpus_MatchesExplicitID(t *testing.T) {
	root := t.TempDir()
	setTestUserCache(t)
	// Build a triad-satisfying .git/ so detectGitBoundary confirms.
	gitDir := filepath.Join(root, ".git")
	for _, sub := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(gitDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(root, root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	discovered, err := planDiscoveredGitCorpus(b)
	if err != nil {
		t.Fatalf("planDiscoveredGitCorpus: %v", err)
	}
	explicit, err := planCurrentGitCorpus(b.toGitPaths())
	if err != nil {
		t.Fatalf("planCurrentGitCorpus: %v", err)
	}
	// Discovered plan's userExplicit must be zero-value (false).
	if discovered.userExplicit {
		t.Fatal("discovered plan must not be userExplicit")
	}
	// IDs must match — same root, same dev:ino, same versioning.
	if discovered.id != explicit.id {
		t.Fatalf("ID mismatch: discovered=%q explicit=%q", discovered.id, explicit.id)
	}
}

func TestPlanCurrentGitCorpus_UserCacheLayout(t *testing.T) {
	root := t.TempDir()
	setTestUserCache(t)

	paths := fakeGitPathsForPlanTest(root)
	plan, err := planCurrentGitCorpus(paths)
	if err != nil {
		t.Fatalf("planCurrentGitCorpus: %v", err)
	}

	if plan.kind != corpusKindGit {
		t.Fatalf("expected git corpus, got %q", plan.kind)
	}
	if plan.rootType != rootTypeWorktree {
		t.Fatalf("expected worktree root, got %q", plan.rootType)
	}
	wantRoot := canonicalCorpusPath(root)
	if plan.root != wantRoot {
		t.Fatalf("expected root %q, got %q", wantRoot, plan.root)
	}
	if plan.id == "" {
		t.Fatal("expected opaque corpus ID")
	}
	if strings.Contains(plan.cacheDir, root) {
		t.Fatalf("cache dir must not be inside repo root: %q", plan.cacheDir)
	}
	if !strings.Contains(plan.cacheDir, filepath.Join("seek", "corpora")) {
		t.Fatalf("cache dir should be under seek/corpora: %q", plan.cacheDir)
	}
	if plan.indexDir != filepath.Join(plan.cacheDir, "index") {
		t.Fatalf("expected index dir under cache dir, got %q", plan.indexDir)
	}
	if plan.scope != nil {
		t.Fatalf("expected unscoped git corpus, got %#v", plan.scope)
	}
}

func TestPlanCurrentGitCorpus_IsStable(t *testing.T) {
	root := t.TempDir()
	setTestUserCache(t)

	paths := fakeGitPathsForPlanTest(root)
	first, err := planCurrentGitCorpus(paths)
	if err != nil {
		t.Fatalf("planCurrentGitCorpus first: %v", err)
	}
	second, err := planCurrentGitCorpus(paths)
	if err != nil {
		t.Fatalf("planCurrentGitCorpus second: %v", err)
	}
	if first.id != second.id {
		t.Fatalf("expected stable corpus ID, got %q then %q", first.id, second.id)
	}
	if first.cacheDir != second.cacheDir {
		t.Fatalf("expected stable cache dir, got %q then %q", first.cacheDir, second.cacheDir)
	}
}

func TestGitCorpusFingerprint_ChangesWithGitIdentity(t *testing.T) {
	root := t.TempDir()
	state := repoState{HeadSHA: "abc", RawOutput: "# branch.oid abc\x00"}
	paths := fakeGitPathsForPlanTest(root)
	other := paths
	other.CommonDir = filepath.Join(root, ".git-worktree-common")

	first := gitCorpusFingerprint(paths, state)
	second := gitCorpusFingerprint(other, state)
	if first == second {
		t.Fatal("expected git corpus fingerprint to include Git common dir")
	}
}

func TestPlanCorpora_ExternalGitRepoPathUsesGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	externalRepo := initGitRepo(t, "external.go", "package external\n")

	plans, err := planCorpora(context.Background(), &currentPaths, []string{externalRepo})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected one external git plan, got %d: %#v", len(plans), plans)
	}
	plan := plans[0]
	if plan.kind != corpusKindGit {
		t.Fatalf("external Git repo should use Git corpus, got kind=%q root=%q", plan.kind, plan.root)
	}
	if plan.rootType != rootTypeWorktree {
		t.Fatalf("external Git repo should use worktree root, got %q", plan.rootType)
	}
	if plan.root != canonicalCorpusPath(externalRepo) {
		t.Fatalf("external Git root mismatch: got %q want %q", plan.root, canonicalCorpusPath(externalRepo))
	}
}

func TestPlanCorpora_ExternalGitRootAndNestedOperandsCollapseToUnscopedPlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	externalRepo := initGitRepo(t, "external.go", "package external\n")
	nested := filepath.Join(externalRepo, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedFile := filepath.Join(nested, "file.go")
	if err := os.WriteFile(nestedFile, []byte("package nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plans, err := planCorpora(context.Background(), &currentPaths, []string{nestedFile, externalRepo, nested})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected one external git plan, got %d: %#v", len(plans), plans)
	}
	plan := plans[0]
	if plan.kind != corpusKindGit {
		t.Fatalf("external Git repo should use Git corpus, got %q", plan.kind)
	}
	if plan.scope != nil {
		t.Fatalf("repo root operand should cover nested operands, got scope %#v", plan.scope)
	}
}

// fakeGitPathsForPlanTest is only for pure corpus planning tests. Integration
// tests should resolve real Git paths and fail if resolution breaks.
func fakeGitPathsForPlanTest(repoDir string) gitPaths {
	absRepoDir, err := filepath.Abs(repoDir)
	if err != nil {
		absRepoDir = repoDir
	}
	gitDir := filepath.Join(absRepoDir, ".git")
	return gitPaths{
		RepoDir:    absRepoDir,
		GitDir:     gitDir,
		CommonDir:  gitDir,
		ConfigPath: filepath.Join(gitDir, "config"),
	}
}
