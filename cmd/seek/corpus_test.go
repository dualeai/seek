package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDefaultSearchRoot_OutsideGitReturnsTypedError(t *testing.T) {
	requireGit(t)
	t.Chdir(t.TempDir())
	paths, err := resolveDefaultSearchRoot(t.Context(), nil)
	if paths != nil {
		t.Fatalf("paths=%+v, want nil", paths)
	}
	if _, ok := errors.AsType[*searchRootError](err); !ok {
		t.Fatalf("error=%v, want searchRootError", err)
	}
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Fatalf("error=%v, want wrapped Git exit error", err)
	}
}

func TestResolveDefaultSearchRoot_ExplicitOperandNeedsNoGitRoot(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Chdir(t.TempDir())
	paths, err := resolveDefaultSearchRoot(t.Context(), []string{"."})
	if err != nil || paths != nil {
		t.Fatalf("paths=%+v error=%v, want nil paths and error", paths, err)
	}
}

func TestResolveDefaultSearchRoot_MissingGitReturnsTypedError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Chdir(t.TempDir())
	paths, err := resolveDefaultSearchRoot(t.Context(), nil)
	if paths != nil {
		t.Fatalf("paths=%+v, want nil", paths)
	}
	if _, ok := errors.AsType[*gitUnavailableError](err); !ok {
		t.Fatalf("error=%v, want gitUnavailableError", err)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("error=%v, want wrapped exec.ErrNotFound", err)
	}
}

func TestResolveDefaultSearchRoot_CancellationIsNotAPathError(t *testing.T) {
	requireGit(t)
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	paths, err := resolveDefaultSearchRoot(ctx, nil)
	if paths != nil {
		t.Fatalf("paths=%+v, want nil", paths)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
	if _, ok := errors.AsType[*searchRootError](err); ok {
		t.Fatalf("error=%v, must not be searchRootError", err)
	}
}

func TestCollectExternalOperands_PathReadErrorsPreserveOperand(t *testing.T) {
	tests := []struct {
		name    string
		operand func(t *testing.T) string
	}{
		{
			name: "missing path",
			operand: func(t *testing.T) string {
				t.Chdir(t.TempDir())
				return filepath.Join("nested", "missing")
			},
		},
		{
			name: "broken symlink",
			operand: func(t *testing.T) string {
				root := t.TempDir()
				link := filepath.Join(root, "broken-link")
				if err := os.Symlink(filepath.Join(root, "missing-target"), link); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				return link
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			operand := tc.operand(t)
			_, err := collectExternalOperands(t.Context(), []string{operand})
			pathErr, ok := errors.AsType[*pathOperandError](err)
			if !ok {
				t.Fatalf("error=%v, want pathOperandError", err)
			}
			if pathErr.operation != pathOperandRead || pathErr.operand != operand {
				t.Fatalf("error=%+v, want read operation for raw operand %q", pathErr, operand)
			}
			if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("error=%v, want fs.ErrNotExist", err)
			}
		})
	}
}

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

func TestZoektCompatibilityVersion_RotatesCacheKeys(t *testing.T) {
	root := t.TempDir()
	setTestUserCache(t)

	paths := fakeGitPathsForPlanTest(root)
	state := repoState{HeadSHA: "abc", RawOutput: "# branch.oid abc\x00"}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}

	gitBefore, err := planCurrentGitCorpus(paths)
	if err != nil {
		t.Fatalf("plan Git corpus before upgrade: %v", err)
	}
	folderBefore, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("plan folder corpus before upgrade: %v", err)
	}
	optionsBefore := indexOptionsHash()
	gitStateBefore := gitCorpusStateHash(paths, state)
	folderStateBefore := finishFolderStateHash(newFolderStateHasher(folderBefore))

	original := zoektCompatibilityVersion
	zoektCompatibilityVersion = original + "-upgrade-test"
	t.Cleanup(func() { zoektCompatibilityVersion = original })

	gitAfter, err := planCurrentGitCorpus(paths)
	if err != nil {
		t.Fatalf("plan Git corpus after upgrade: %v", err)
	}
	folderAfter, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("plan folder corpus after upgrade: %v", err)
	}

	checks := []struct {
		name   string
		before string
		after  string
	}{
		{"index options hash", optionsBefore, indexOptionsHash()},
		{"Git corpus ID", string(gitBefore.id), string(gitAfter.id)},
		{"Git cache directory", gitBefore.cacheDir, gitAfter.cacheDir},
		{"Git state hash", gitStateBefore, gitCorpusStateHash(paths, state)},
		{"folder corpus ID", string(folderBefore.id), string(folderAfter.id)},
		{"folder cache directory", folderBefore.cacheDir, folderAfter.cacheDir},
		{"folder state hash", folderStateBefore, finishFolderStateHash(newFolderStateHasher(folderAfter))},
	}
	for _, check := range checks {
		if check.before == check.after {
			t.Errorf("%s did not change after a Zoekt compatibility upgrade", check.name)
		}
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

func TestPlanCorpora_CurrentGitDefaultDiscoversVisibleNestedGit(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(currentRepo, "nested"), "nested.go", "package nested\n")

	plans, err := planCorpora(context.Background(), &currentPaths, nil)
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected parent Git plan plus visible nested Git plan, got %d: %#v", len(plans), plans)
	}

	var parentPlan, childPlan *corpusPlan
	for i := range plans {
		switch plans[i].root {
		case canonicalCorpusPath(currentRepo):
			parentPlan = &plans[i]
		case canonicalCorpusPath(nestedRepo):
			childPlan = &plans[i]
		}
	}
	if parentPlan == nil {
		t.Fatalf("expected parent Git plan, got %#v", plans)
	}
	if !parentPlan.userExplicit {
		t.Fatal("default current Git plan should be user-explicit")
	}
	if parentPlan.scope == nil {
		t.Fatal("expected parent Git plan to exclude visible nested Git root")
	}
	if childPlan == nil {
		t.Fatalf("expected child Git plan, got %#v", plans)
	}
	if childPlan.userExplicit {
		t.Fatal("visible nested Git plan discovered from default root should not be user-explicit")
	}
}

func TestPlanCorpora_ExactFilesAndDirectoriesUseScopedGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	cases := []struct {
		name        string
		target      func(t *testing.T) string
		wantKind    corpusKind
		wantRoot    func(target string) string
		wantRootTyp rootType
		wantScope   bool
	}{
		{
			name: "current git exact tracked file",
			target: func(t *testing.T) string {
				target := filepath.Join(currentRepo, "nested", "file.go")
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("package nested\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitRunIn(t, currentRepo, "add", ".")
				gitRunIn(t, currentRepo, "commit", "-m", "nested")
				return target
			},
			wantKind:    corpusKindGit,
			wantRoot:    func(string) string { return canonicalCorpusPath(currentRepo) },
			wantRootTyp: rootTypeWorktree,
			wantScope:   true,
		},
		{
			name: "current git non-root directory",
			target: func(t *testing.T) string {
				target := filepath.Join(currentRepo, "nested-dir")
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				return target
			},
			wantKind:    corpusKindGit,
			wantRoot:    func(string) string { return canonicalCorpusPath(currentRepo) },
			wantRootTyp: rootTypeWorktree,
			wantScope:   true,
		},
		{
			name: "external git exact tracked file",
			target: func(t *testing.T) string {
				externalRepo := initGitRepo(t, "external.go", "package external\n")
				return filepath.Join(externalRepo, "external.go")
			},
			wantKind: corpusKindGit,
			wantRoot: func(target string) string {
				paths, err := resolveGitPaths(context.Background(), filepath.Dir(target))
				if err != nil {
					t.Fatalf("resolve external git paths: %v", err)
				}
				return canonicalCorpusPath(paths.RepoDir)
			},
			wantRootTyp: rootTypeWorktree,
			wantScope:   true,
		},
		{
			name: "external git non-root directory",
			target: func(t *testing.T) string {
				externalRepo := initGitRepo(t, "external.go", "package external\n")
				target := filepath.Join(externalRepo, "nested")
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				return target
			},
			wantKind: corpusKindGit,
			wantRoot: func(target string) string {
				paths, err := resolveGitPaths(context.Background(), target)
				if err != nil {
					t.Fatalf("resolve external git paths: %v", err)
				}
				return canonicalCorpusPath(paths.RepoDir)
			},
			wantRootTyp: rootTypeWorktree,
			wantScope:   true,
		},
		{
			name: "outside git directory",
			target: func(t *testing.T) string {
				target := filepath.Join(t.TempDir(), "plain")
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				return target
			},
			wantKind:    corpusKindFolder,
			wantRoot:    func(target string) string { return canonicalCorpusPath(target) },
			wantRootTyp: rootTypeDirectory,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.target(t)
			plans, err := planCorpora(context.Background(), &currentPaths, []string{target})
			if err != nil {
				t.Fatalf("planCorpora: %v", err)
			}
			if len(plans) != 1 {
				t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
			}
			plan := plans[0]
			if plan.kind != tc.wantKind {
				t.Fatalf("kind mismatch: got %q root=%q want %q", plan.kind, plan.root, tc.wantKind)
			}
			if plan.rootType != tc.wantRootTyp {
				t.Fatalf("root type mismatch: got %q want %q", plan.rootType, tc.wantRootTyp)
			}
			if want := tc.wantRoot(target); plan.root != want {
				t.Fatalf("root mismatch: got %q want %q", plan.root, want)
			}
			if gotScope := plan.scope != nil; gotScope != tc.wantScope {
				t.Fatalf("scope presence mismatch: got %v want %v (%#v)", gotScope, tc.wantScope, plan.scope)
			}
		})
	}
}

// A scoped Git directory plan reuses the unscoped combined corpus ID and index
// directory. It adds a search scope, a Git path scope, and an over-cap fallback.
func TestPlanCorpora_ScopedGitDirectoryReusesCombinedCorpusID(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	scope := filepath.Join(currentRepo, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}

	unscoped, err := planCorpora(context.Background(), &currentPaths, nil)
	if err != nil {
		t.Fatalf("plan unscoped: %v", err)
	}
	scoped, err := planCorpora(context.Background(), &currentPaths, []string{scope})
	if err != nil {
		t.Fatalf("plan scoped: %v", err)
	}
	if len(unscoped) != 1 {
		t.Fatalf("expected one unscoped plan, got %#v", unscoped)
	}
	if len(scoped) != 1 {
		t.Fatalf("expected one scoped plan, got %#v", scoped)
	}
	if scoped[0].dirtyScope == nil {
		t.Fatal("expected scoped Git directory plan to carry dirty scope")
	}
	if scoped[0].scope == nil {
		t.Fatal("expected scoped Git directory plan to carry a search scope filter")
	}
	if unscoped[0].id != scoped[0].id {
		t.Fatalf("scoped search must reuse the unscoped combined corpus ID, got %q vs %q", unscoped[0].id, scoped[0].id)
	}
	if scoped[0].scopedIndexDir == "" {
		t.Fatal("expected scoped plan to carry an over-cap fallback dir")
	}
}

// Distinct scopes of one repository share the combined whole-repo index and
// use different search scopes and over-cap fallback directories.
func TestPlanCorpora_ScopedGitDirectoriesShareCombinedIndex(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	platform := filepath.Join(currentRepo, "platform")
	infra := filepath.Join(currentRepo, "infra")
	for _, dir := range []string{platform, infra} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	platformPlans, err := planCorpora(context.Background(), &currentPaths, []string{platform})
	if err != nil {
		t.Fatalf("plan platform: %v", err)
	}
	infraPlans, err := planCorpora(context.Background(), &currentPaths, []string{infra})
	if err != nil {
		t.Fatalf("plan infra: %v", err)
	}
	if len(platformPlans) != 1 || len(infraPlans) != 1 {
		t.Fatalf("expected one plan per scope, got platform=%#v infra=%#v", platformPlans, infraPlans)
	}
	platformPlan := platformPlans[0]
	infraPlan := infraPlans[0]
	if platformPlan.indexDir == "" || infraPlan.indexDir == "" {
		t.Fatalf("expected scoped plans to have a combined index, got platform=%#v infra=%#v", platformPlan, infraPlan)
	}
	if platformPlan.indexDir != infraPlan.indexDir {
		t.Fatalf("distinct scopes must share one combined index, got %q vs %q", platformPlan.indexDir, infraPlan.indexDir)
	}
	if platformPlan.scopedIndexDir == "" || infraPlan.scopedIndexDir == "" {
		t.Fatalf("expected scoped plans to carry an over-cap fallback dir, got platform=%#v infra=%#v", platformPlan, infraPlan)
	}
	if platformPlan.scopedIndexDir == infraPlan.scopedIndexDir {
		t.Fatalf("distinct scopes must not share the over-cap fallback dir %q", platformPlan.scopedIndexDir)
	}
}

func TestPlanCorpora_NestedGitDirectoryOperandUsesChildGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(currentRepo, "nested"), "nested.go", "package nested\n")
	nestedScope := filepath.Join(nestedRepo, "pkg")
	if err := os.MkdirAll(nestedScope, 0o755); err != nil {
		t.Fatal(err)
	}

	plans, err := planCorpora(context.Background(), &currentPaths, []string{nestedScope})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected one nested Git plan, got %d: %#v", len(plans), plans)
	}
	plan := plans[0]
	if plan.kind != corpusKindGit {
		t.Fatalf("nested Git subdir should use Git corpus, got kind=%q root=%q", plan.kind, plan.root)
	}
	if plan.root != canonicalCorpusPath(nestedRepo) {
		t.Fatalf("nested Git root mismatch: got %q want %q", plan.root, canonicalCorpusPath(nestedRepo))
	}
	if plan.scope == nil {
		t.Fatal("nested Git subdir should produce a scoped child Git plan")
	}
	if !plan.userExplicit {
		t.Fatal("direct nested Git directory operand should be user-explicit")
	}
}

func TestPlanCorpora_GitDirectoryOperandDiscoversVisibleNestedGit(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	scope := filepath.Join(currentRepo, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, currentRepo, "add", "scope")
	gitRunIn(t, currentRepo, "commit", "-m", "add scope")
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(scope, "nested"), "nested.go", "package nested\n")

	plans, err := planCorpora(context.Background(), &currentPaths, []string{scope})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected parent Git plan plus visible nested Git plan, got %d: %#v", len(plans), plans)
	}

	var parentPlan, childPlan *corpusPlan
	for i := range plans {
		switch plans[i].root {
		case canonicalCorpusPath(currentRepo):
			parentPlan = &plans[i]
		case canonicalCorpusPath(nestedRepo):
			childPlan = &plans[i]
		}
	}
	if parentPlan == nil {
		t.Fatalf("expected parent Git plan, got %#v", plans)
	}
	if childPlan == nil {
		t.Fatalf("expected child Git plan, got %#v", plans)
	}
	if parentPlan.scope == nil {
		t.Fatal("expected parent Git plan to stay scoped and exclude child Git root")
	}
	if !parentPlan.userExplicit {
		t.Fatal("parent Git directory operand should be user-explicit")
	}
	if childPlan.scope != nil {
		t.Fatalf("visible nested Git plan should own the whole child root, got %#v", childPlan.scope)
	}
	if childPlan.userExplicit {
		t.Fatal("visible nested Git plan discovered from parent operand should not be user-explicit")
	}
}

func TestPlanCorpora_GitDirectoryOperandDiscoversSubmoduleGitlink(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	scope := filepath.Join(currentRepo, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, currentRepo, "add", "scope")
	gitRunIn(t, currentRepo, "commit", "-m", "add scope")

	subSrc := initGitRepoNoRemote(t, "sub.go", "package sub\n")
	gitRunIn(t, currentRepo, "-c", "protocol.file.allow=always", "submodule", "add", subSrc, filepath.Join("scope", "sub"))
	gitRunIn(t, currentRepo, "commit", "-m", "add submodule")
	submoduleRoot := filepath.Join(scope, "sub")

	plans, err := planCorpora(context.Background(), &currentPaths, []string{scope})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}

	var childPlan *corpusPlan
	for i := range plans {
		if plans[i].root == canonicalCorpusPath(submoduleRoot) {
			childPlan = &plans[i]
			break
		}
	}
	if childPlan == nil {
		t.Fatalf("expected submodule Git plan, got %#v", plans)
	}
	if childPlan.scope != nil {
		t.Fatalf("submodule Git plan should own the whole child root, got %#v", childPlan.scope)
	}
	if childPlan.userExplicit {
		t.Fatal("submodule Git plan discovered from parent operand should not be user-explicit")
	}
}

func TestPlanCorpora_ParentFolderAndNestedGitSubdirBroadenChildGitPlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	parent := t.TempDir()
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(parent, "nested"), "nested.go", "package nested\n")
	nestedScope := filepath.Join(nestedRepo, "pkg")
	if err := os.MkdirAll(nestedScope, 0o755); err != nil {
		t.Fatal(err)
	}

	plans, err := planCorpora(context.Background(), &currentPaths, []string{parent, nestedScope})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected parent folder plus child Git plan, got %d: %#v", len(plans), plans)
	}

	var folderPlan, gitPlan *corpusPlan
	for i := range plans {
		switch plans[i].kind {
		case corpusKindFolder:
			folderPlan = &plans[i]
		case corpusKindGit:
			gitPlan = &plans[i]
		}
	}
	if folderPlan == nil {
		t.Fatalf("expected parent folder plan, got %#v", plans)
	}
	if gitPlan == nil {
		t.Fatalf("expected child Git plan, got %#v", plans)
	}
	if gitPlan.root != canonicalCorpusPath(nestedRepo) {
		t.Fatalf("child Git root mismatch: got %q want %q", gitPlan.root, canonicalCorpusPath(nestedRepo))
	}
	if gitPlan.scope != nil {
		t.Fatalf("child Git plan should broaden to the root covered by parent folder exclusion, got %#v", gitPlan.scope)
	}
	if len(folderPlan.excludeRoots) != 1 || folderPlan.excludeRoots[0] != canonicalCorpusPath(nestedRepo) {
		t.Fatalf("parent folder should exclude child Git root, got %#v", folderPlan.excludeRoots)
	}
}

func TestPlanCorpora_BroadenedChildGitDiscoversGrandchildOutsideOriginalScope(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	parent := t.TempDir()
	childRepo := initEmptyGitRepoNoRemoteAt(t, filepath.Join(parent, "child"))
	childPkg := filepath.Join(childRepo, "pkg")
	if err := os.MkdirAll(childPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childPkg, "app.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, childRepo, "add", "pkg")
	gitRunIn(t, childRepo, "commit", "-m", "add pkg")
	grandchildRepo := initGitRepoNoRemoteAt(t, filepath.Join(childRepo, "other", "grand"), "grand.go", "package grand\n")

	plans, err := planCorpora(context.Background(), &currentPaths, []string{parent, childPkg})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}

	var childPlan, grandchildPlan *corpusPlan
	for i := range plans {
		switch plans[i].root {
		case canonicalCorpusPath(childRepo):
			childPlan = &plans[i]
		case canonicalCorpusPath(grandchildRepo):
			grandchildPlan = &plans[i]
		}
	}
	if childPlan == nil {
		t.Fatalf("expected broadened child Git plan, got %#v", plans)
	}
	if childPlan.scope == nil {
		t.Fatalf("child Git plan should carry a search exclusion for the grandchild, got %#v", childPlan)
	}
	if grandchildPlan == nil {
		t.Fatalf("expected grandchild Git plan discovered after broadening, got %#v", plans)
	}
	if grandchildPlan.scope != nil {
		t.Fatalf("grandchild Git plan should own whole root, got %#v", grandchildPlan.scope)
	}
}

func TestPlanCorpora_SameGitDirectoryOperandsMergeIntoOneScopedPlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	a := filepath.Join(currentRepo, "a")
	ab := filepath.Join(a, "b")
	for _, dir := range []string{a, ab} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	plans, err := planCorpora(context.Background(), &currentPaths, []string{a, ab})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected one merged Git plan, got %d: %#v", len(plans), plans)
	}
	plan := plans[0]
	if plan.kind != corpusKindGit {
		t.Fatalf("expected Git plan, got kind=%q root=%q", plan.kind, plan.root)
	}
	if plan.root != canonicalCorpusPath(currentRepo) {
		t.Fatalf("root mismatch: got %q want %q", plan.root, canonicalCorpusPath(currentRepo))
	}
	if plan.scope == nil {
		t.Fatal("expected scoped Git plan")
	}
}

func TestPlanCorpora_CurrentGitNestedGitRootUsesChildGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	currentPaths, err := resolveGitPaths(context.Background(), currentRepo)
	if err != nil {
		t.Fatalf("resolve current git paths: %v", err)
	}
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(currentRepo, "nested"), "nested.go", "package nested\n")

	plans, err := planCorpora(context.Background(), &currentPaths, []string{nestedRepo})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected one nested Git plan, got %d: %#v", len(plans), plans)
	}
	plan := plans[0]
	if plan.kind != corpusKindGit {
		t.Fatalf("nested Git root should use Git corpus, got kind=%q root=%q", plan.kind, plan.root)
	}
	if plan.root != canonicalCorpusPath(nestedRepo) {
		t.Fatalf("nested Git root mismatch: got %q want %q", plan.root, canonicalCorpusPath(nestedRepo))
	}
	if !plan.userExplicit {
		t.Fatal("direct nested Git root operand should be user-explicit")
	}
}

func TestPlanCorpora_ExternalGitRootAndNestedOperandsCollapseToSingleGitPlan(t *testing.T) {
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

	// The repo root, a nested directory, and a nested file of the SAME repo
	// all route to one git corpus. The repo-root operand widens the scope to
	// the whole worktree (rootIncluded), so a single unscoped git plan owns
	// everything — no separate per-file corpus.
	plans, err := planCorpora(context.Background(), &currentPaths, []string{nestedFile, externalRepo, nested})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected one collapsed git plan, got %d: %#v", len(plans), plans)
	}
	plan := plans[0]
	if plan.kind != corpusKindGit {
		t.Fatalf("expected git corpus, got %q", plan.kind)
	}
	if plan.root != canonicalCorpusPath(externalRepo) {
		t.Fatalf("external Git root mismatch: got %q want %q", plan.root, canonicalCorpusPath(externalRepo))
	}
	if plan.scope != nil {
		t.Fatalf("expected whole-repo scope (nil) when the repo root is an operand, got %#v", plan.scope)
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
