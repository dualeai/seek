package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestResolveOperands_PathReadErrorsPreserveOperand resolves a missing path
// and a broken symlink: the error message names the operand as it was typed.
func TestResolveOperands_PathReadErrorsPreserveOperand(t *testing.T) {
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
					t.Fatalf("make the broken symlink: %v", err)
				}
				return link
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			operand := tc.operand(t)
			_, err := resolveOperands(t.Context(), []string{operand})
			if err == nil {
				t.Fatal("no error, want a missing path")
			}
			if got, want := formatCLIError(err), "seek: path does not exist: "+strconv.Quote(operand); got != want {
				t.Fatalf("message %q, want %q", got, want)
			}
		})
	}
}

// TestPlanGitCorpus_WalkAndRevParseShareID plans one repository twice. The
// first plan comes from detectGitBoundary, as the walk finds the repository
// (the typed path). The second comes from git rev-parse, as an operand
// resolves (the real path). Both plans must have one corpus ID.
func TestPlanGitCorpus_WalkAndRevParseShareID(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)
	root := initGitRepo(t, "a.txt", "a\n")
	b, status := detectGitBoundary(root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	explicitPaths, err := resolveGitPaths(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := planGitCorpus(b, nil)
	if err != nil {
		t.Fatalf("planGitCorpus: %v", err)
	}
	explicit, err := planGitCorpus(explicitPaths, nil)
	if err != nil {
		t.Fatalf("planGitCorpus: %v", err)
	}
	if discovered.id != explicit.id {
		t.Fatalf("ID mismatch: discovered=%q explicit=%q", discovered.id, explicit.id)
	}
}

func TestPlanGitCorpus_UserCacheLayout(t *testing.T) {
	root := t.TempDir()
	setTestUserCache(t)

	paths := fakeGitPathsForPlanTest(t, root)
	plan, err := planGitCorpus(paths, nil)
	if err != nil {
		t.Fatalf("planGitCorpus: %v", err)
	}

	if plan.kind != corpusKindGit {
		t.Fatalf("expected git corpus, got %q", plan.kind)
	}
	if plan.rootType != rootTypeWorktree {
		t.Fatalf("expected worktree root, got %q", plan.rootType)
	}
	wantRoot := resolvedPath(t, root)
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

func TestPlanGitCorpus_IsStable(t *testing.T) {
	root := t.TempDir()
	setTestUserCache(t)

	paths := fakeGitPathsForPlanTest(t, root)
	first, err := planGitCorpus(paths, nil)
	if err != nil {
		t.Fatalf("planGitCorpus first: %v", err)
	}
	second, err := planGitCorpus(paths, nil)
	if err != nil {
		t.Fatalf("planGitCorpus second: %v", err)
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

	paths := fakeGitPathsForPlanTest(t, root)
	state := repoState{HeadSHA: "abc", RawOutput: "# branch.oid abc\x00"}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}

	gitBefore, err := planGitCorpus(paths, nil)
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

	gitAfter, err := planGitCorpus(paths, nil)
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

// TestGitCorpusFingerprint_ChangesWithGitIdentity checks that the
// fingerprint follows the file identity of the Git common dir. Another
// directory changes it; another path to the same directory does not.
func TestGitCorpusFingerprint_ChangesWithGitIdentity(t *testing.T) {
	root := t.TempDir()
	state := repoState{HeadSHA: "abc", RawOutput: "# branch.oid abc\x00"}
	paths := fakeGitPathsForPlanTest(t, root)
	other := paths
	other.CommonDir = filepath.Join(root, ".git-worktree-common")
	if err := os.MkdirAll(other.CommonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := paths
	alias.CommonDir = filepath.Join(t.TempDir(), "git-link")
	if err := os.Symlink(paths.CommonDir, alias.CommonDir); err != nil {
		t.Fatal(err)
	}

	first := gitCorpusFingerprint(paths, state)
	if gitCorpusFingerprint(other, state) == first {
		t.Fatal("expected git corpus fingerprint to include the Git common dir identity")
	}
	if gitCorpusFingerprint(alias, state) != first {
		t.Fatal("expected git corpus fingerprint to ignore the path to the Git common dir")
	}
}

func TestPlanCorpora_ExternalGitRepoPathUsesGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	externalRepo := initGitRepo(t, "external.go", "package external\n")

	plans, err := planCorpora(context.Background(), []string{externalRepo})
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
	if plan.root != resolvedPath(t, externalRepo) {
		t.Fatalf("external Git root mismatch: got %q want %q", plan.root, resolvedPath(t, externalRepo))
	}
}

func TestPlanCorpora_CurrentGitDefaultDiscoversVisibleNestedGit(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(currentRepo, "nested"), "nested.go", "package nested\n")

	plans, err := planCorpora(context.Background(), []string{currentRepo})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected parent Git plan plus visible nested Git plan, got %d: %#v", len(plans), plans)
	}

	var parentPlan, childPlan *node
	for i := range plans {
		switch plans[i].root {
		case resolvedPath(t, currentRepo):
			parentPlan = &plans[i]
		case resolvedPath(t, nestedRepo):
			childPlan = &plans[i]
		}
	}
	if parentPlan == nil {
		t.Fatalf("expected parent Git plan, got %#v", plans)
	}
	if parentPlan.scope != nil {
		t.Fatalf("parent Git plan should cover the whole node (the child is its own node), got %#v", parentPlan.scope)
	}
	if childPlan == nil {
		t.Fatalf("expected child Git plan, got %#v", plans)
	}
}

func TestPlanCorpora_ExactFilesAndDirectoriesUseScopedGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	cases := []struct {
		name        string
		target      func(t *testing.T) string
		wantKind    corpusKind
		wantRoot    func(target string) string // the check resolves symlinks
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
			wantRoot:    func(string) string { return currentRepo },
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
			wantRoot:    func(string) string { return currentRepo },
			wantRootTyp: rootTypeWorktree,
			wantScope:   true,
		},
		{
			name: "external git exact tracked file",
			target: func(t *testing.T) string {
				externalRepo := initGitRepo(t, "external.go", "package external\n")
				return filepath.Join(externalRepo, "external.go")
			},
			// The repository root is the directory of the file.
			wantKind:    corpusKindGit,
			wantRoot:    func(target string) string { return filepath.Dir(target) },
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
			// The repository root is the parent of the nested directory.
			wantKind:    corpusKindGit,
			wantRoot:    func(target string) string { return filepath.Dir(target) },
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
			wantRoot:    func(target string) string { return target },
			wantRootTyp: rootTypeDirectory,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.target(t)
			plans, err := planCorpora(context.Background(), []string{target})
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
			if want := resolvedPath(t, tc.wantRoot(target)); plan.root != want {
				t.Fatalf("root mismatch: got %q want %q", plan.root, want)
			}
			if gotScope := plan.scope != nil; gotScope != tc.wantScope {
				t.Fatalf("scope presence mismatch: got %v want %v (%#v)", gotScope, tc.wantScope, plan.scope)
			}
		})
	}
}

// TestPlanCorpora_ScopedGitDirectoryReusesCombinedCorpusID checks that the
// plan of a Git subdirectory has the corpus ID of the whole repository. So it
// reads the same index, and it adds a search scope.
func TestPlanCorpora_ScopedGitDirectoryReusesCombinedCorpusID(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	scope := filepath.Join(currentRepo, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}

	unscoped, err := planCorpora(context.Background(), []string{currentRepo})
	if err != nil {
		t.Fatalf("plan unscoped: %v", err)
	}
	scoped, err := planCorpora(context.Background(), []string{scope})
	if err != nil {
		t.Fatalf("plan scoped: %v", err)
	}
	if len(unscoped) != 1 {
		t.Fatalf("expected one unscoped plan, got %#v", unscoped)
	}
	if len(scoped) != 1 {
		t.Fatalf("expected one scoped plan, got %#v", scoped)
	}
	if scoped[0].scope == nil {
		t.Fatal("expected scoped Git directory plan to carry a search scope filter")
	}
	if unscoped[0].id != scoped[0].id {
		t.Fatalf("scoped search must reuse the unscoped combined corpus ID, got %q vs %q", unscoped[0].id, scoped[0].id)
	}
}

// TestPlanCorpora_ScopedGitDirectoriesShareCombinedIndex checks that two
// subdirectories of one repository share the index of the whole repository.
func TestPlanCorpora_ScopedGitDirectoriesShareCombinedIndex(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	platform := filepath.Join(currentRepo, "platform")
	infra := filepath.Join(currentRepo, "infra")
	for _, dir := range []string{platform, infra} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	platformPlans, err := planCorpora(context.Background(), []string{platform})
	if err != nil {
		t.Fatalf("plan platform: %v", err)
	}
	infraPlans, err := planCorpora(context.Background(), []string{infra})
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
}

func TestPlanCorpora_NestedGitDirectoryOperandUsesChildGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(currentRepo, "nested"), "nested.go", "package nested\n")
	nestedScope := filepath.Join(nestedRepo, "pkg")
	if err := os.MkdirAll(nestedScope, 0o755); err != nil {
		t.Fatal(err)
	}

	plans, err := planCorpora(context.Background(), []string{nestedScope})
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
	if plan.root != resolvedPath(t, nestedRepo) {
		t.Fatalf("nested Git root mismatch: got %q want %q", plan.root, resolvedPath(t, nestedRepo))
	}
	if plan.scope == nil {
		t.Fatal("nested Git subdir should produce a scoped child Git plan")
	}
}

func TestPlanCorpora_GitDirectoryOperandDiscoversVisibleNestedGit(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
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

	plans, err := planCorpora(context.Background(), []string{scope})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected parent Git plan plus visible nested Git plan, got %d: %#v", len(plans), plans)
	}

	var parentPlan, childPlan *node
	for i := range plans {
		switch plans[i].root {
		case resolvedPath(t, currentRepo):
			parentPlan = &plans[i]
		case resolvedPath(t, nestedRepo):
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
	if childPlan.scope != nil {
		t.Fatalf("visible nested Git plan should own the whole child root, got %#v", childPlan.scope)
	}
}

func TestPlanCorpora_GitDirectoryOperandDiscoversSubmoduleGitlink(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
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

	plans, err := planCorpora(context.Background(), []string{scope})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}

	var childPlan *node
	for i := range plans {
		if plans[i].root == resolvedPath(t, submoduleRoot) {
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
}

func TestPlanCorpora_ParentFolderAndNestedGitSubdirBroadenChildGitPlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	parent := t.TempDir()
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(parent, "nested"), "nested.go", "package nested\n")
	nestedScope := filepath.Join(nestedRepo, "pkg")
	if err := os.MkdirAll(nestedScope, 0o755); err != nil {
		t.Fatal(err)
	}

	plans, err := planCorpora(context.Background(), []string{parent, nestedScope})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected parent folder plus child Git plan, got %d: %#v", len(plans), plans)
	}

	var folderPlan, gitPlan *node
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
	if gitPlan.root != resolvedPath(t, nestedRepo) {
		t.Fatalf("child Git root mismatch: got %q want %q", gitPlan.root, resolvedPath(t, nestedRepo))
	}
	if gitPlan.scope != nil {
		t.Fatalf("child Git plan should cover the whole child node, got %#v", gitPlan.scope)
	}
	// The parent folder's walk lists no file of the child Git root.
	_, selected, err := folderCorpusState(context.Background(), *folderPlan)
	if err != nil {
		t.Fatalf("walk the parent folder: %v", err)
	}
	for _, candidate := range selected {
		if strings.HasPrefix(candidate.name, "nested/") {
			t.Fatalf("parent folder walk lists %q of the child Git root", candidate.name)
		}
	}
}

func TestPlanCorpora_BroadenedChildGitDiscoversGrandchildOutsideOriginalScope(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

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

	plans, err := planCorpora(context.Background(), []string{parent, childPkg})
	if err != nil {
		t.Fatalf("planCorpora: %v", err)
	}

	var childPlan, grandchildPlan *node
	for i := range plans {
		switch plans[i].root {
		case resolvedPath(t, childRepo):
			childPlan = &plans[i]
		case resolvedPath(t, grandchildRepo):
			grandchildPlan = &plans[i]
		}
	}
	if childPlan == nil {
		t.Fatalf("expected broadened child Git plan, got %#v", plans)
	}
	if childPlan.scope != nil {
		t.Fatalf("child Git plan should cover the whole node (the grandchild is its own node), got %#v", childPlan.scope)
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
	a := filepath.Join(currentRepo, "a")
	ab := filepath.Join(a, "b")
	for _, dir := range []string{a, ab} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	plans, err := planCorpora(context.Background(), []string{a, ab})
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
	if plan.root != resolvedPath(t, currentRepo) {
		t.Fatalf("root mismatch: got %q want %q", plan.root, resolvedPath(t, currentRepo))
	}
	if plan.scope == nil {
		t.Fatal("expected scoped Git plan")
	}
}

func TestPlanCorpora_CurrentGitNestedGitRootUsesChildGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	currentRepo := initGitRepo(t, "current.go", "package current\n")
	nestedRepo := initGitRepoNoRemoteAt(t, filepath.Join(currentRepo, "nested"), "nested.go", "package nested\n")

	plans, err := planCorpora(context.Background(), []string{nestedRepo})
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
	if plan.root != resolvedPath(t, nestedRepo) {
		t.Fatalf("nested Git root mismatch: got %q want %q", plan.root, resolvedPath(t, nestedRepo))
	}
}

func TestPlanCorpora_ExternalGitRootAndNestedOperandsCollapseToSingleGitPlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	externalRepo := initGitRepo(t, "external.go", "package external\n")
	nested := filepath.Join(externalRepo, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedFile := filepath.Join(nested, "file.go")
	if err := os.WriteFile(nestedFile, []byte("package nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The repository root, a nested directory, and a nested file of one
	// repository all go to one Git corpus. The root operand widens the scope
	// to the whole worktree (rootIncluded), so one unscoped Git plan holds
	// all three, with no corpus for the file.
	plans, err := planCorpora(context.Background(), []string{nestedFile, externalRepo, nested})
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
	if plan.root != resolvedPath(t, externalRepo) {
		t.Fatalf("external Git root mismatch: got %q want %q", plan.root, resolvedPath(t, externalRepo))
	}
	if plan.scope != nil {
		t.Fatalf("expected whole-repo scope (nil) when the repo root is an operand, got %#v", plan.scope)
	}
}

// fakeGitPathsForPlanTest gives the Git paths of repoDir with no Git process,
// for planning tests only. Integration tests resolve real Git paths and fail
// when the resolution breaks. It makes the .git directory: a corpus ID uses
// its file identity.
func fakeGitPathsForPlanTest(t testing.TB, repoDir string) gitPaths {
	t.Helper()
	absRepoDir, err := filepath.Abs(repoDir)
	if err != nil {
		absRepoDir = repoDir
	}
	gitDir := filepath.Join(absRepoDir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return gitPaths{RepoDir: absRepoDir, CommonDir: gitDir}
}
