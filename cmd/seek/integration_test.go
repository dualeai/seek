package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testUserCacheMarkerEnv = "SEEK_TEST_USER_CACHE_CONFIGURED"

// requireTools fails the test/benchmark if git or universal-ctags is not
// available. A failure here means the detection pipeline in checkCtags is
// broken — ctags should be auto-detected whether the binary is named
// "universal-ctags" or "ctags".
func requireTools(tb testing.TB) {
	tb.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		tb.Fatal("requires git on PATH")
	}
	if err := checkCtags(); err != nil {
		tb.Fatalf("requires universal-ctags: %v", err)
	}
}

func initEmptyGitRepo(tb testing.TB) string {
	tb.Helper()
	dir := initEmptyGitRepoNoRemote(tb)
	// zoekt's gitindex.IndexGitRepo derives the repo name from the remote URL.
	// Most tests keep this remote so committed shard prefixes stay stable.
	gitRunIn(tb, dir, "remote", "add", "origin", "https://github.com/test/repo.git")
	return dir
}

func initEmptyGitRepoNoRemote(tb testing.TB) string {
	tb.Helper()
	dir := tb.TempDir()
	return initEmptyGitRepoNoRemoteAt(tb, dir)
}

func initEmptyGitRepoNoRemoteAt(tb testing.TB, dir string) string {
	tb.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatal(err)
	}
	gitRunIn(tb, dir, "init")
	gitRunIn(tb, dir, "config", "user.email", "test@test.com")
	gitRunIn(tb, dir, "config", "user.name", "Test")
	return dir
}

// initGitRepo creates a temp git repo with a single committed file.
// Returns the repo directory. The caller's working directory is unchanged.
func initGitRepo(tb testing.TB, fileName, content string) string {
	tb.Helper()
	dir := initEmptyGitRepo(tb)

	// Write and commit the file
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o644); err != nil {
		tb.Fatal(err)
	}
	gitRunIn(tb, dir, "add", ".")
	gitRunIn(tb, dir, "commit", "-m", "initial")

	return dir
}

func initGitRepoNoRemote(tb testing.TB, fileName, content string) string {
	tb.Helper()
	dir := initEmptyGitRepoNoRemote(tb)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o644); err != nil {
		tb.Fatal(err)
	}
	gitRunIn(tb, dir, "add", ".")
	gitRunIn(tb, dir, "commit", "-m", "initial")
	return dir
}

func initGitRepoNoRemoteAt(tb testing.TB, dir, fileName, content string) string {
	tb.Helper()
	initEmptyGitRepoNoRemoteAt(tb, dir)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o644); err != nil {
		tb.Fatal(err)
	}
	gitRunIn(tb, dir, "add", ".")
	gitRunIn(tb, dir, "commit", "-m", "initial")
	return dir
}

func setTestUserCache(tb testing.TB) string {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))
	tb.Setenv("SEEK_CACHE_DIR", "")
	tb.Setenv(testUserCacheMarkerEnv, "1")
	return home
}

func ensureTestUserCache(tb testing.TB) {
	tb.Helper()
	if os.Getenv(testUserCacheMarkerEnv) != "" {
		return
	}
	setTestUserCache(tb)
}

func planGitTestCorpus(tb testing.TB, repoDir string) (gitPaths, corpusPlan) {
	tb.Helper()
	setTestUserCache(tb)

	paths, err := resolveGitPaths(context.Background(), repoDir)
	if err != nil {
		tb.Fatalf("resolveGitPaths: %v", err)
	}
	plan, err := planCurrentGitCorpus(paths)
	if err != nil {
		tb.Fatalf("planCurrentGitCorpus: %v", err)
	}
	if err := os.MkdirAll(plan.cacheDir, 0o755); err != nil {
		tb.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
		tb.Fatalf("mkdir index dir: %v", err)
	}
	return paths, plan
}

func planFolderTestCorpus(tb testing.TB, root string) corpusPlan {
	tb.Helper()
	setTestUserCache(tb)

	info, err := os.Lstat(root)
	if err != nil {
		tb.Fatalf("lstat folder root: %v", err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		tb.Fatalf("planFolderCorpus: %v", err)
	}
	if err := os.MkdirAll(plan.cacheDir, 0o755); err != nil {
		tb.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
		tb.Fatalf("mkdir index dir: %v", err)
	}
	return plan
}

// runSeekInRepo searches a repository through run and unwraps the visible file
// headers for tests that only care which files matched.
func runSeekInRepo(t *testing.T, repoDir, pattern string) ([]string, error) {
	t.Helper()

	ensureTestUserCache(t)
	t.Chdir(repoDir)
	out, err := captureStdout(t, func() error {
		return run(context.Background(), pattern, nil, 0, 0)
	})
	if errors.Is(err, errNoMatch) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return formattedOutputFileNamesForTest(out), nil
}

func formattedOutputFileNamesForTest(output string) []string {
	var fileNames []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		header := strings.TrimPrefix(line, "## ")
		if name, _, ok := strings.Cut(header, " ("); ok {
			fileNames = append(fileNames, name)
		}
	}
	return fileNames
}

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %q: %v", pattern, err)
	}
	return matches
}

// runSeekInPlannedGitCorpus is a white-box helper for tests and benchmarks that
// need to reuse a precomputed corpus plan. Integration tests should prefer
// runSeekInRepo or run so they exercise CLI planning, freshness, search, and
// formatting together.
func runSeekInPlannedGitCorpus(
	ctx context.Context,
	pattern string,
	paths gitPaths,
	plan corpusPlan,
) ([]string, error) {
	query, err := parseSearchQuery(pattern)
	if err != nil {
		return nil, err
	}
	results, _, err := prepareAndSearchCorpus(ctx, plan, &paths, query)
	if err != nil {
		return nil, err
	}

	var fileNames []string
	for _, result := range results {
		fileNames = append(fileNames, result.file.FileName)
	}
	return fileNames, nil
}

func initScopedGitRepo(t *testing.T, marker string) string {
	t.Helper()

	dir := initGitRepo(t, "seed.go", "package seed\n")
	for _, name := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		content := []byte("package " + name + "\n// " + marker + "\n")
		if err := os.WriteFile(filepath.Join(dir, name, "app.go"), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "add scoped files")
	return dir
}

func assertScopedRunIncludesOnly(t *testing.T, query, operand string) {
	t.Helper()

	out, err := captureStdout(t, func() error {
		return run(context.Background(), query, []string{operand}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertScopedOutputIncludesOnlyA(t, out)
}

func assertScopedOutputIncludesOnlyA(t *testing.T, out string) {
	t.Helper()

	if !strings.Contains(out, "## a/app.go") && !strings.Contains(out, "## app.go") {
		t.Fatalf("expected scoped output to include app.go from a/, got:\n%s", out)
	}
	if strings.Contains(out, "## b/app.go") || strings.Contains(out, "package b") {
		t.Fatalf("expected scoped output to exclude b/app.go, got:\n%s", out)
	}
}

// gitRun executes a git command in dir, failing the test on error.
func gitRun(t testing.TB, dir string, args ...string) {
	t.Helper()
	gitRunIn(t, dir, args...)
}

// gitRunIn executes a git command in the specified directory.
func gitRunIn(t testing.TB, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// gitOutputIn runs a git command in dir and returns its trimmed stdout.
// Test/bench helper; fails the test on git error.
func gitOutputIn(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return strings.TrimRight(string(out), "\n\r")
}

func gitCurrentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "symbolic-ref", "--quiet", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git current branch failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func initGitWorktree(t *testing.T, fileName, content string) (string, string) {
	t.Helper()

	repoDir := initGitRepo(t, fileName, content)
	gitRunIn(t, repoDir, "branch", "worktree-branch")

	worktreeRoot := t.TempDir()
	worktreeDir := filepath.Join(worktreeRoot, "wt")
	gitRunIn(t, repoDir, "worktree", "add", worktreeDir, "worktree-branch")

	return repoDir, worktreeDir
}

func TestResolveGitPaths_Worktree(t *testing.T) {
	requireTools(t)

	repoDir, worktreeDir := initGitWorktree(t, "app.go", "package main\n// worktree_base\n")
	paths, err := resolveGitPaths(context.Background(), worktreeDir)
	if err != nil {
		t.Fatalf("resolveGitPaths: %v", err)
	}
	resolvedRepoDir, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		resolvedRepoDir = repoDir
	}
	resolvedWorktreeDir, err := filepath.EvalSymlinks(worktreeDir)
	if err != nil {
		resolvedWorktreeDir = worktreeDir
	}

	if paths.RepoDir != resolvedWorktreeDir {
		t.Fatalf("expected RepoDir %q, got %q", resolvedWorktreeDir, paths.RepoDir)
	}
	if !strings.Contains(paths.GitDir, "/.git/worktrees/") {
		t.Fatalf("expected worktree git dir, got %q", paths.GitDir)
	}
	if paths.CommonDir != filepath.Join(resolvedRepoDir, ".git") {
		t.Fatalf("expected common git dir %q, got %q", filepath.Join(resolvedRepoDir, ".git"), paths.CommonDir)
	}
	if paths.ConfigPath != filepath.Join(resolvedRepoDir, ".git", "config") {
		t.Fatalf("expected shared config path %q, got %q", filepath.Join(resolvedRepoDir, ".git", "config"), paths.ConfigPath)
	}
}

func TestIntegration_SearchCleanRepo(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "hello.go", `package main

import "fmt"

func main() {
	fmt.Println("findme_marker_123")
	}
	`)
	setTestUserCache(t)
	t.Chdir(dir)

	// Search for committed content
	out, err := captureStdout(t, func() error {
		return run(context.Background(), "findme_marker_123", nil, 0, 0)
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(out, "## hello.go") || !strings.Contains(out, "findme_marker_123") {
		t.Fatalf("expected committed content in output, got:\n%s", out)
	}

	// Search for non-existent content
	out, err = captureStdout(t, func() error {
		return run(context.Background(), "nothere_xyz_999", nil, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("expected no-match, got err=%v out=%q", err, out)
	}
}

func TestRun_UsesUserCache(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// unique_marker_user_cache\n")
	setTestUserCache(t)
	t.Chdir(dir)

	if _, err := captureStdout(t, func() error {
		return run(context.Background(), "unique_marker_user_cache", nil, 0, 0)
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	paths, err := resolveGitPaths(context.Background(), dir)
	if err != nil {
		t.Fatalf("resolveGitPaths: %v", err)
	}
	plan, err := planCurrentGitCorpus(paths)
	if err != nil {
		t.Fatalf("planCurrentGitCorpus: %v", err)
	}
	if readStateFile(plan.cacheDir) == "" {
		t.Fatalf("expected state file in user cache corpus %q", plan.cacheDir)
	}
	shards, err := filepath.Glob(filepath.Join(plan.indexDir, "*.zoekt"))
	if err != nil {
		t.Fatalf("glob shards: %v", err)
	}
	if len(shards) == 0 {
		t.Fatalf("expected shards in user cache index %q", plan.indexDir)
	}
}

func TestRun_CurrentRepoDirectoryPathOperandScopesSearch(t *testing.T) {
	requireTools(t)

	dir := initScopedGitRepo(t, "shared_scope_marker")
	setTestUserCache(t)
	t.Chdir(dir)

	assertScopedRunIncludesOnly(t, "shared_scope_marker", "a")
}

func TestRun_CurrentRepoRootPathOperandSearchesWholeRepo(t *testing.T) {
	requireTools(t)

	dir := initScopedGitRepo(t, "root_scope_marker")
	setTestUserCache(t)
	t.Chdir(dir)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "root_scope_marker", []string{"."}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"## a/app.go", "## b/app.go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected root operand output to include %s, got:\n%s", want, out)
		}
	}
}

func TestRun_GitRootAndChildOperandsDoNotDuplicateChildResults(t *testing.T) {
	requireTools(t)

	cases := []struct {
		name    string
		operand func(dir, childDir string) string
	}{
		{
			name: "directory",
			operand: func(_, childDir string) string {
				return childDir
			},
		},
		{
			name: "file",
			operand: func(_, childDir string) string {
				return filepath.Join(childDir, "out.go")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := "root_child_" + tc.name + "_overlap_marker"
			dir := initGitRepo(t, "seed.go", "package seed\n")
			childDir := filepath.Join(dir, "generated")
			if err := os.MkdirAll(childDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(childDir, "out.go"), []byte("package generated\n// "+marker+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRun(t, dir, "add", ".")
			gitRun(t, dir, "commit", "-m", "add generated file")

			setTestUserCache(t)
			t.Chdir(t.TempDir())

			out, err := captureStdout(t, func() error {
				return run(context.Background(), marker, []string{dir, tc.operand(dir, childDir)}, 0, 0)
			})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if count := strings.Count(out, marker); count != 1 {
				t.Fatalf("expected child result once, got %d occurrences:\n%s", count, out)
			}
		})
	}
}

func TestRun_MissingPathOperandFails(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// missing_path_marker\n")
	t.Chdir(dir)

	err := run(context.Background(), "missing_path_marker", []string{"does-not-exist"}, 0, 0)
	if err == nil {
		t.Fatal("expected missing path operand to fail")
	}
	if !strings.Contains(err.Error(), "read path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_ExternalFolderPathWorksOutsideGit(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(folder, "note.txt"),
		[]byte("external_folder_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_folder_marker", []string{folder}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## note.txt") {
		t.Fatalf("expected external folder result, got:\n%s", out)
	}
}

func TestRun_ExternalDuplicateAndNestedPathOperandsDeduplicateResults(t *testing.T) {
	requireTools(t)

	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "note.txt"), []byte("dedup_operand_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "dedup_operand_marker", []string{
			root,
			root,
			nested,
			filepath.Join(nested, "note.txt"),
		}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Count(out, "## nested/note.txt") != 1 {
		t.Fatalf("expected one visible result for collapsed operands, got:\n%s", out)
	}
}

func TestRun_InvalidQueryDoesNotCreateV2Cache(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "note.txt"), []byte("invalid_query_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	home := setTestUserCache(t)
	t.Chdir(t.TempDir())

	_, err := captureStdout(t, func() error {
		return run(context.Background(), "(", []string{folder}, 0, 0)
	})
	if err == nil {
		t.Fatal("expected invalid query to fail")
	}
	if !strings.Contains(err.Error(), "parse query") {
		t.Fatalf("expected parse query error, got %v", err)
	}

	cacheRoot := filepath.Join(home, "xdg-cache", "seek")
	if _, err := os.Stat(cacheRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid query should not create v2 cache at %q; stat err=%v", cacheRoot, err)
	}
}

func TestRun_ExternalGitRepoPathUsesGitCorpus(t *testing.T) {
	requireTools(t)

	externalRepo := initGitRepo(t, "app.go", "package main\n// external_git_folder_marker\n")
	if err := os.WriteFile(
		filepath.Join(externalRepo, ".git", "info", "seek-marker.txt"),
		[]byte("external_git_folder_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_git_folder_marker", []string{externalRepo}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## app.go") {
		t.Fatalf("expected external Git repo to be searched, got:\n%s", out)
	}
	if strings.Contains(out, ".git/") {
		t.Fatalf("external Git search should skip .git metadata, got:\n%s", out)
	}
}

func TestRun_ExternalGitRepoPathUsesGitIgnore(t *testing.T) {
	requireTools(t)

	externalRepo := initGitRepo(t, "app.go", "package main\n// external_git_visible_marker\n")
	if err := os.WriteFile(filepath.Join(externalRepo, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, externalRepo, "add", ".gitignore")
	gitRunIn(t, externalRepo, "commit", "-m", "add gitignore")
	if err := os.MkdirAll(filepath.Join(externalRepo, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(externalRepo, "ignored", "secret.go"),
		[]byte("package secret\n// external_gitignored_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_gitignored_marker", []string{externalRepo}, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("external Git repo should honor .gitignore, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "external_gitignored_marker") {
		t.Fatalf("ignored external Git file must not be searchable, got:\n%s", out)
	}
}

func TestRun_ExternalGitRepoPathDoesNotBudgetIgnoredFiles(t *testing.T) {
	requireTools(t)

	externalRepo := initGitRepo(t, "app.go", "package main\n// external_git_budget_marker\n")
	if err := os.WriteFile(filepath.Join(externalRepo, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, externalRepo, "add", ".gitignore")
	gitRunIn(t, externalRepo, "commit", "-m", "add gitignore")
	ignoredDir := filepath.Join(externalRepo, "ignored")
	if err := os.MkdirAll(ignoredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ignoredLarge := filepath.Join(ignoredDir, "huge.dat")
	if err := os.WriteFile(ignoredLarge, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(ignoredLarge, maxCorpusIndexedBytes+1); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_git_budget_marker", []string{externalRepo}, 0, 0)
	})
	if err != nil {
		t.Fatalf("external Git repo should not budget ignored files, got err=%v out=%q", err, out)
	}
	if !strings.Contains(out, "external_git_budget_marker") {
		t.Fatalf("expected tracked file result, got:\n%s", out)
	}
}

func TestRun_ExternalGitSubdirPathScopesSearch(t *testing.T) {
	requireTools(t)

	externalRepo := initScopedGitRepo(t, "external_git_scope_marker")
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_git_scope_marker", []string{filepath.Join(externalRepo, "a")}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertScopedOutputIncludesOnlyA(t, out)
}

func TestRun_ExternalGitExactFilePathScopesSearch(t *testing.T) {
	requireTools(t)

	externalRepo := initScopedGitRepo(t, "external_git_exact_scope_marker")
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(
			context.Background(),
			"external_git_exact_scope_marker",
			[]string{filepath.Join(externalRepo, "a", "app.go")},
			0,
			0,
		)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertScopedOutputIncludesOnlyA(t, out)
}

func TestRun_CurrentGitExactIgnoredFileOperandSearchesLiteralFile(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "app.go", "package main\n// visible\n")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", ".gitignore")
	gitRunIn(t, repo, "commit", "-m", "add gitignore")
	ignoredDir := filepath.Join(repo, "ignored")
	if err := os.MkdirAll(ignoredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ignoredDir, "target.txt")
	if err := os.WriteFile(target, []byte("literal_ignored_file_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignoredDir, "sibling.txt"), []byte("literal_ignored_file_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "literal_ignored_file_marker", []string{target}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "target.txt") {
		t.Fatalf("expected exact ignored file result, got:\n%s", out)
	}
	if strings.Contains(out, "sibling.txt") {
		t.Fatalf("exact ignored file search should not include sibling, got:\n%s", out)
	}
}

func TestRun_ExternalGitExactIgnoredFileOperandSearchesLiteralFile(t *testing.T) {
	requireTools(t)

	externalRepo := initGitRepo(t, "app.go", "package main\n// visible\n")
	if err := os.WriteFile(filepath.Join(externalRepo, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, externalRepo, "add", ".gitignore")
	gitRunIn(t, externalRepo, "commit", "-m", "add gitignore")
	ignoredDir := filepath.Join(externalRepo, "ignored")
	if err := os.MkdirAll(ignoredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ignoredDir, "target.txt")
	if err := os.WriteFile(target, []byte("external_literal_ignored_file_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignoredDir, "sibling.txt"), []byte("external_literal_ignored_file_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_literal_ignored_file_marker", []string{target}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "target.txt") {
		t.Fatalf("expected exact ignored file result, got:\n%s", out)
	}
	if strings.Contains(out, "sibling.txt") {
		t.Fatalf("exact ignored file search should not include sibling, got:\n%s", out)
	}
}

func TestRun_CurrentGitIgnoredDirectoryOperandSearchesContent(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "app.go", "package main\n// visible\n")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("scratch/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", ".gitignore")
	gitRunIn(t, repo, "commit", "-m", "add gitignore")
	scratch := filepath.Join(repo, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "note.txt"), []byte("literal_ignored_dir_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "literal_ignored_dir_marker", []string{scratch}, 0, 0)
	})
	if err != nil {
		t.Fatalf("explicitly selected ignored directory should be searched as a folder, got err=%v", err)
	}
	if !strings.Contains(out, "literal_ignored_dir_marker") {
		t.Fatalf("explicitly selected ignored directory content should be searched, got:\n%s", out)
	}
}

func TestRun_ExternalGitIgnoredDirectoryOperandSearchesContent(t *testing.T) {
	requireTools(t)

	externalRepo := initGitRepo(t, "app.go", "package main\n// visible\n")
	if err := os.WriteFile(filepath.Join(externalRepo, ".gitignore"), []byte("scratch/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, externalRepo, "add", ".gitignore")
	gitRunIn(t, externalRepo, "commit", "-m", "add gitignore")
	scratch := filepath.Join(externalRepo, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "note.txt"), []byte("external_literal_ignored_dir_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_literal_ignored_dir_marker", []string{scratch}, 0, 0)
	})
	if err != nil {
		t.Fatalf("explicitly selected ignored directory should be searched as a folder, got err=%v", err)
	}
	if !strings.Contains(out, "external_literal_ignored_dir_marker") {
		t.Fatalf("explicitly selected ignored directory content should be searched, got:\n%s", out)
	}
}

func TestRun_GitSubdirOperandDoesNotBudgetIgnoredFolderArtifacts(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "platform")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package platform\n// scoped_visible_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".gitignore"), []byte(".data/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "platform")
	gitRunIn(t, repo, "commit", "-m", "add scoped platform")

	ignoredData := filepath.Join(scope, ".data")
	if err := os.MkdirAll(ignoredData, 0o755); err != nil {
		t.Fatal(err)
	}
	size := int64(maxIndexedDocumentBytes)
	count := maxFolderIndexedBytes/maxIndexedDocumentBytes + 1
	for i := range count {
		path := filepath.Join(ignoredData, fmt.Sprintf("artifact_%03d.bin", i))
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "scoped_visible_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("Git-scoped subdir should ignore large artifacts, got err=%v out=%q", err, out)
	}
	if !strings.Contains(out, "scoped_visible_marker") {
		t.Fatalf("expected tracked scoped result, got:\n%s", out)
	}
}

func TestRun_GitSubdirOperandDoesNotBudgetUnignoredSiblingDirtyArtifacts(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "platform")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package platform\n// scoped_dirty_visible_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "platform")
	gitRunIn(t, repo, "commit", "-m", "add scoped platform")

	sibling := filepath.Join(repo, "other")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	size := int64(maxIndexedDocumentBytes)
	count := maxFolderIndexedBytes/maxIndexedDocumentBytes + 1
	for i := range count {
		path := filepath.Join(sibling, fmt.Sprintf("artifact_%03d.bin", i))
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "scoped_dirty_visible_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("Git-scoped subdir should ignore out-of-scope dirty artifacts, got err=%v out=%q", err, out)
	}
	if !strings.Contains(out, "scoped_dirty_visible_marker") {
		t.Fatalf("expected tracked scoped result, got:\n%s", out)
	}
}

func TestRun_GitSubdirOperandDoesNotBudgetTrackedSiblingArtifacts(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "platform")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	inScopeContent := "package platform\n// scoped_tracked_visible_marker\n"
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte(inScopeContent), 0o644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(repo, "other")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "large.go"), []byte(strings.Repeat("x", len(inScopeContent)*4)), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-m", "add scoped and sibling tracked files")

	oldLimit := gitCorpusIndexedByteLimit
	gitCorpusIndexedByteLimit = int64(len(inScopeContent) + 8)
	defer func() { gitCorpusIndexedByteLimit = oldLimit }()

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "scoped_tracked_visible_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("Git-scoped subdir should ignore out-of-scope tracked artifacts, got err=%v out=%q", err, out)
	}
	if !strings.Contains(out, "scoped_tracked_visible_marker") {
		t.Fatalf("expected tracked scoped result, got:\n%s", out)
	}
	if strings.Contains(out, "other/large.go") {
		t.Fatalf("out-of-scope tracked sibling leaked, got:\n%s", out)
	}
}

func TestEnsureGitDirtyLayerFresh_CachesDeletedOnlyScope(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "platform")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(scope, "deleted.go")
	if err := os.WriteFile(deleted, []byte("package platform\n// deleted_only_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "platform")
	gitRunIn(t, repo, "commit", "-m", "add deleted scope file")
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	plans, err := planCorpora(context.Background(), &paths, []string{scope})
	if err != nil {
		t.Fatalf("plan scoped corpus: %v", err)
	}
	if len(plans) != 1 || plans[0].dirtyScope == nil {
		t.Fatalf("expected one scoped Git plan, got %#v", plans)
	}
	state, err := gitRepoStateInScope(context.Background(), paths.RepoDir, plans[0].dirtyScope)
	if err != nil {
		t.Fatalf("scoped state: %v", err)
	}
	if len(state.Files) != 1 || state.Files[0] != "platform/deleted.go" {
		t.Fatalf("expected deleted file in scoped dirty state, got %#v", state.Files)
	}

	first, err := ensureGitDirtyLayerFresh(context.Background(), plans[0], paths, state)
	if err != nil {
		t.Fatalf("first dirty refresh: %v", err)
	}
	if first != corpusKnownEmpty {
		t.Fatalf("deleted-only dirty overlay should be known empty, got %v", first)
	}
	cachedState := readStateFile(plans[0].dirtyCacheDir)
	if cachedState == "" {
		t.Fatal("deleted-only dirty overlay should cache its empty state")
	}
	if got := readEmptyStateFile(plans[0].dirtyCacheDir); got != cachedState {
		t.Fatalf("deleted-only dirty overlay should cache an explicit empty marker, got %q want %q", got, cachedState)
	}
	if shardsExist(plans[0].dirtyIndexDir) {
		t.Fatalf("deleted-only dirty overlay should not create shards in %s", plans[0].dirtyIndexDir)
	}

	second, err := ensureGitDirtyLayerFresh(context.Background(), plans[0], paths, state)
	if err != nil {
		t.Fatalf("second dirty refresh: %v", err)
	}
	if second != corpusKnownEmpty {
		t.Fatalf("cached deleted-only dirty overlay should stay known empty, got %v", second)
	}
}

func TestEnsureGitCommittedLayerFresh_RebuildsMissingShardsWithoutEmptyMarker(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "platform")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package platform\n// committed_layer_rebuild_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "platform")
	gitRunIn(t, repo, "commit", "-m", "add scoped platform")

	setTestUserCache(t)
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	plans, err := planCorpora(context.Background(), &paths, []string{scope})
	if err != nil {
		t.Fatalf("plan scoped corpus: %v", err)
	}
	if len(plans) != 1 || plans[0].dirtyScope == nil {
		t.Fatalf("expected one scoped Git plan, got %#v", plans)
	}
	state, err := gitRepoStateInScope(context.Background(), paths.RepoDir, plans[0].dirtyScope)
	if err != nil {
		t.Fatalf("scoped state: %v", err)
	}
	first, err := ensureGitCommittedLayerFresh(context.Background(), plans[0], paths, state.HeadSHA)
	if err != nil {
		t.Fatalf("first committed refresh: %v", err)
	}
	if first != corpusSearchable || !shardsExist(plans[0].committedIndexDir) {
		t.Fatalf("expected searchable committed layer with shards, got state=%v index=%s", first, plans[0].committedIndexDir)
	}
	for _, shard := range mustGlob(t, filepath.Join(plans[0].committedIndexDir, "*.zoekt")) {
		if err := os.Remove(shard); err != nil {
			t.Fatal(err)
		}
	}
	if readEmptyStateFile(plans[0].committedCacheDir) != "" {
		t.Fatal("non-empty committed layer should not have an empty marker")
	}

	second, err := ensureGitCommittedLayerFresh(context.Background(), plans[0], paths, state.HeadSHA)
	if err != nil {
		t.Fatalf("second committed refresh: %v", err)
	}
	if second != corpusSearchable || !shardsExist(plans[0].committedIndexDir) {
		t.Fatalf("missing shards should rebuild instead of caching empty, got state=%v index=%s", second, plans[0].committedIndexDir)
	}
}

func TestEnsureGitCommittedLayerFresh_DetectsHeadDriftBeforeStateWrite(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "platform")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package platform\n// committed_head_a_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "platform")
	gitRunIn(t, repo, "commit", "-m", "add scoped platform")

	setTestUserCache(t)
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	plans, err := planCorpora(context.Background(), &paths, []string{scope})
	if err != nil {
		t.Fatalf("plan scoped corpus: %v", err)
	}
	state, err := gitRepoStateInScope(context.Background(), paths.RepoDir, plans[0].dirtyScope)
	if err != nil {
		t.Fatalf("scoped state: %v", err)
	}

	if err := os.WriteFile(filepath.Join(scope, "next.go"), []byte("package platform\n// committed_head_b_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "platform/next.go")
	gitRunIn(t, repo, "commit", "-m", "move scoped head")

	_, err = ensureGitCommittedLayerFresh(context.Background(), plans[0], paths, state.HeadSHA)
	if !errors.Is(err, errScopedLayerStateChanged) {
		t.Fatalf("expected scoped layer drift error, got %v", err)
	}
	if got := readStateFile(plans[0].committedCacheDir); got != "" {
		t.Fatalf("committed state should not be written after head drift, got %q", got)
	}
}

func TestEnsureGitCommittedLayerFresh_ReusesLayerAfterOutOfScopeCommit(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "platform")
	sibling := filepath.Join(repo, "infra")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package platform\n// out_of_scope_reuse_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "infra.go"), []byte("package infra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-m", "add scoped platform and sibling")

	setTestUserCache(t)
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	plans, err := planCorpora(context.Background(), &paths, []string{scope})
	if err != nil {
		t.Fatalf("plan scoped corpus: %v", err)
	}
	state, err := gitRepoStateInScope(context.Background(), paths.RepoDir, plans[0].dirtyScope)
	if err != nil {
		t.Fatalf("scoped state: %v", err)
	}
	if got, err := ensureGitCommittedLayerFresh(context.Background(), plans[0], paths, state.HeadSHA); err != nil {
		t.Fatalf("first committed refresh: %v", err)
	} else if got != corpusSearchable {
		t.Fatalf("first committed refresh=%v, want searchable", got)
	}
	shards := mustGlob(t, filepath.Join(plans[0].committedIndexDir, "*.zoekt"))
	if len(shards) == 0 {
		t.Fatal("first committed refresh should create shards")
	}
	shardModTimes := make(map[string]int64, len(shards))
	for _, shard := range shards {
		info, err := os.Stat(shard)
		if err != nil {
			t.Fatal(err)
		}
		shardModTimes[filepath.Base(shard)] = info.ModTime().UnixNano()
	}

	if err := os.WriteFile(filepath.Join(sibling, "outside.go"), []byte("package infra\n// out of scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "infra/outside.go")
	gitRunIn(t, repo, "commit", "-m", "commit outside scoped path")
	nextState, err := gitRepoStateInScope(context.Background(), paths.RepoDir, plans[0].dirtyScope)
	if err != nil {
		t.Fatalf("next scoped state: %v", err)
	}
	if nextState.HeadSHA == state.HeadSHA {
		t.Fatal("expected HEAD to change after out-of-scope commit")
	}
	if got, err := ensureGitCommittedLayerFresh(context.Background(), plans[0], paths, nextState.HeadSHA); err != nil {
		t.Fatalf("second committed refresh: %v", err)
	} else if got != corpusSearchable {
		t.Fatalf("second committed refresh=%v, want searchable", got)
	}
	if got := readHeadFile(plans[0].committedCacheDir); got != nextState.HeadSHA {
		t.Fatalf("committed head=%q, want %q", got, nextState.HeadSHA)
	}
	if got, want := readStateFile(plans[0].committedCacheDir), scopedCommittedLayerStateHash(paths, plans[0].dirtyScope, nextState.HeadSHA); got != want {
		t.Fatalf("committed state=%q, want %q", got, want)
	}
	for _, shard := range mustGlob(t, filepath.Join(plans[0].committedIndexDir, "*.zoekt")) {
		info, err := os.Stat(shard)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := info.ModTime().UnixNano(), shardModTimes[filepath.Base(shard)]; got != want {
			t.Fatalf("out-of-scope commit should reuse shard %s, mtime got %d want %d", shard, got, want)
		}
	}
	matches, err := executeUnscopedShardSearchForTest(context.Background(), plans[0].committedIndexDir, "out_of_scope_reuse_marker")
	if err != nil {
		t.Fatalf("search reused committed shard: %v", err)
	}
	if len(matches) != 1 || matches[0].FileName != "platform/app.go" {
		t.Fatalf("expected reused committed shard result, got %#v", matches)
	}
}

func TestEnsureGitCommittedLayerFresh_PathspecEnvCannotHideScopedChange(t *testing.T) {
	requireTools(t)
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "scope[1]")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n// pathspec_env_old_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "scope[1]")
	gitRunIn(t, repo, "commit", "-m", "add scoped literal path")

	setTestUserCache(t)
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	plans, err := planCorpora(context.Background(), &paths, []string{scope})
	if err != nil {
		t.Fatalf("plan scoped corpus: %v", err)
	}
	state, err := gitRepoStateInScope(context.Background(), paths.RepoDir, plans[0].dirtyScope)
	if err != nil {
		t.Fatalf("scoped state: %v", err)
	}
	if got, err := ensureGitCommittedLayerFresh(context.Background(), plans[0], paths, state.HeadSHA); err != nil {
		t.Fatalf("first committed refresh: %v", err)
	} else if got != corpusSearchable {
		t.Fatalf("first committed refresh=%v, want searchable", got)
	}

	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n// pathspec_env_new_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "scope[1]/app.go")
	gitRunIn(t, repo, "commit", "-m", "change scoped literal path")
	nextState, err := gitRepoStateInScope(context.Background(), paths.RepoDir, plans[0].dirtyScope)
	if err != nil {
		t.Fatalf("next scoped state: %v", err)
	}
	if got, err := ensureGitCommittedLayerFresh(context.Background(), plans[0], paths, nextState.HeadSHA); err != nil {
		t.Fatalf("second committed refresh: %v", err)
	} else if got != corpusSearchable {
		t.Fatalf("second committed refresh=%v, want searchable", got)
	}

	oldMatches, err := executeUnscopedShardSearchForTest(context.Background(), plans[0].committedIndexDir, "pathspec_env_old_marker")
	if err != nil {
		t.Fatalf("search old marker: %v", err)
	}
	if len(oldMatches) != 0 {
		t.Fatalf("old scoped content should not survive in reused shard, got %#v", oldMatches)
	}
	newMatches, err := executeUnscopedShardSearchForTest(context.Background(), plans[0].committedIndexDir, "pathspec_env_new_marker")
	if err != nil {
		t.Fatalf("search new marker: %v", err)
	}
	if len(newMatches) != 1 || newMatches[0].FileName != "scope[1]/app.go" {
		t.Fatalf("expected reindexed scoped content under pathspec env, got %#v", newMatches)
	}
}

func TestEnsureGitCommittedLayerFresh_UnbornRepoKnownEmpty(t *testing.T) {
	requireGit(t)

	repo := initEmptyGitRepoNoRemote(t)
	scope := filepath.Join(repo, "platform")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	plans, err := planCorpora(context.Background(), &paths, []string{scope})
	if err != nil {
		t.Fatalf("plan scoped corpus: %v", err)
	}
	state, err := gitRepoStateInScope(context.Background(), paths.RepoDir, plans[0].dirtyScope)
	if err != nil {
		t.Fatalf("scoped state: %v", err)
	}
	got, err := ensureGitCommittedLayerFresh(context.Background(), plans[0], paths, state.HeadSHA)
	if err != nil {
		t.Fatalf("unborn committed refresh: %v", err)
	}
	if got != corpusKnownEmpty {
		t.Fatalf("unborn committed layer should be known empty, got %v", got)
	}
}

func TestSearchPlannedScopedCorpusRejectsLayerStateMismatch(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "platform")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package platform\n// state_mismatch_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "platform")
	gitRunIn(t, repo, "commit", "-m", "add scoped platform")

	setTestUserCache(t)
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	plans, err := planCorpora(context.Background(), &paths, []string{scope})
	if err != nil {
		t.Fatalf("plan scoped corpus: %v", err)
	}
	_, ready, err := ensureGitCorpusFresh(context.Background(), plans[0], paths)
	if err != nil {
		t.Fatalf("ensure scoped corpus: %v", err)
	}
	if ready != corpusSearchable {
		t.Fatalf("expected searchable corpus, got %v", ready)
	}
	plans[0].committedStateHash = readStateFile(plans[0].committedCacheDir)
	if plans[0].committedStateHash == "" {
		t.Fatal("expected committed layer to persist state before mismatch check")
	}
	plans[0].dirtyStateHash = readStateFile(plans[0].dirtyCacheDir)
	if plans[0].dirtyStateHash == "" {
		t.Fatal("expected dirty layer to persist state before mismatch check")
	}
	plans[0].committedStateHash = "0000000000000000"

	q, err := parseSearchQuery("state_mismatch_marker")
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	_, err = searchPlannedCorpusParsed(context.Background(), plans[0], q)
	if !errors.Is(err, errScopedLayerStateChanged) {
		t.Fatalf("expected scoped layer state mismatch, got %v", err)
	}
}

func TestRun_GitDirectoryOperandTreatsPathspecMetacharactersLiterally(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "scope[1]")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "committed.go"), []byte("package scope\n// literal_pathspec_committed_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "scope[1]")
	gitRunIn(t, repo, "commit", "-m", "add literal pathspec scope")
	if err := os.WriteFile(filepath.Join(scope, "dirty.go"), []byte("package scope\n// literal_pathspec_dirty_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "literal_pathspec_committed_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run committed literal pathspec marker: %v", err)
	}
	if !strings.Contains(out, "scope[1]/committed.go") {
		t.Fatalf("expected literal pathspec committed result, got:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "literal_pathspec_dirty_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run dirty literal pathspec marker: %v", err)
	}
	if !strings.Contains(out, "scope[1]/dirty.go") {
		t.Fatalf("expected literal pathspec dirty result, got:\n%s", out)
	}
}

func TestRun_GitDirectoryOperandDiscoversVisibleNestedGit(t *testing.T) {
	requireTools(t)

	parent := initGitRepo(t, "app.go", "package main\n// parent_visible_marker\n")
	scope := filepath.Join(parent, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n// scope_visible_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, parent, "add", "scope")
	gitRunIn(t, parent, "commit", "-m", "add scope")

	nested := filepath.Join(scope, "nested")
	initGitRepoNoRemoteAt(t, nested, "src.go", "package nested\n// visible_nested_git_marker\n")
	if err := os.WriteFile(filepath.Join(nested, ".gitignore"), []byte(".venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, nested, "add", ".gitignore")
	gitRunIn(t, nested, "commit", "-m", "add gitignore")
	venv := filepath.Join(nested, ".venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "secret.py"), []byte("# visible_nested_ignored_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	outsideNested := initGitRepoNoRemoteAt(t, filepath.Join(parent, "outside", "nested"), "src.go", "package outside\n// visible_nested_git_marker\n")
	_ = outsideNested

	setTestUserCache(t)
	t.Chdir(parent)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "visible_nested_git_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run nested marker: %v", err)
	}
	if !strings.Contains(out, "scope/nested/src.go") {
		t.Fatalf("expected visible nested Git content under scoped directory, got:\n%s", out)
	}
	if count := strings.Count(out, "visible_nested_git_marker"); count != 1 {
		t.Fatalf("expected visible nested Git result once, got %d occurrences:\n%s", count, out)
	}
	if strings.Contains(out, "outside/nested/src.go") {
		t.Fatalf("out-of-scope nested Git content leaked, got:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "visible_nested_ignored_marker", []string{scope}, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("nested Git ignore should be honored, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "visible_nested_ignored_marker") {
		t.Fatalf("nested ignored content leaked, got:\n%s", out)
	}
}

func TestRun_GitDirectoryOperandDiscoversNestedSubmoduleRecursively(t *testing.T) {
	requireTools(t)

	grandSrc := initGitRepoNoRemote(t, "grand.go", "package grand\n// recursive_submodule_marker\n")
	nestedSrc := initGitRepoNoRemote(t, "nested.go", "package nested\n")
	gitRunIn(t, nestedSrc, "-c", "protocol.file.allow=always", "submodule", "add", grandSrc, "grand")
	gitRunIn(t, nestedSrc, "commit", "-m", "add grand submodule")

	parent := initGitRepo(t, "app.go", "package main\n")
	scope := filepath.Join(parent, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, parent, "add", "scope")
	gitRunIn(t, parent, "commit", "-m", "add scope")
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", nestedSrc, filepath.Join("scope", "nested"))
	gitRunIn(t, parent, "commit", "-m", "add nested submodule")
	gitRunIn(t, filepath.Join(parent, "scope", "nested"), "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")

	setTestUserCache(t)
	t.Chdir(parent)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "recursive_submodule_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run recursive submodule marker: %v", err)
	}
	if !strings.Contains(out, "scope/nested/grand/grand.go") {
		t.Fatalf("expected recursively discovered submodule content, got:\n%s", out)
	}
	if count := strings.Count(out, "recursive_submodule_marker"); count != 1 {
		t.Fatalf("expected recursive submodule result once, got %d occurrences:\n%s", count, out)
	}
}

func TestRun_FolderOperandDiscoversNestedSubmoduleRecursively(t *testing.T) {
	requireTools(t)

	grandSrc := initGitRepoNoRemote(t, "grand.go", "package grand\n// folder_recursive_submodule_marker\n")
	parent := t.TempDir()
	nested := initGitRepoNoRemoteAt(t, filepath.Join(parent, "nested"), "nested.go", "package nested\n")
	gitRunIn(t, nested, "-c", "protocol.file.allow=always", "submodule", "add", grandSrc, "grand")
	gitRunIn(t, nested, "commit", "-m", "add grand submodule")
	gitRunIn(t, nested, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "folder_recursive_submodule_marker", []string{parent}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run folder recursive submodule marker: %v", err)
	}
	if !strings.Contains(out, "grand.go") {
		t.Fatalf("expected folder-discovered recursive submodule content, got:\n%s", out)
	}
	if count := strings.Count(out, "folder_recursive_submodule_marker"); count != 1 {
		t.Fatalf("expected folder recursive submodule result once, got %d occurrences:\n%s", count, out)
	}
}

func TestRun_FolderOperandDiscoversLinkedWorktree(t *testing.T) {
	requireTools(t)

	repo := initGitRepoNoRemote(t, "base.go", "package base\n")
	gitRunIn(t, repo, "branch", "linked")
	parent := t.TempDir()
	linked := filepath.Join(parent, "linked")
	gitRunIn(t, repo, "worktree", "add", linked, "linked")
	if err := os.WriteFile(filepath.Join(linked, ".gitignore"), []byte(".cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "linked.go"), []byte("package linked\n// folder_linked_worktree_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, linked, "add", ".gitignore", "linked.go")
	gitRunIn(t, linked, "commit", "-m", "add linked marker")
	cacheDir := filepath.Join(linked, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "secret.txt"), []byte("folder_linked_worktree_ignored_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "folder_linked_worktree_marker", []string{parent}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run folder linked worktree marker: %v", err)
	}
	if !strings.Contains(out, "linked.go") {
		t.Fatalf("expected folder-discovered linked worktree content, got:\n%s", out)
	}
	if count := strings.Count(out, "folder_linked_worktree_marker"); count != 1 {
		t.Fatalf("expected linked worktree result once, got %d occurrences:\n%s", count, out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "folder_linked_worktree_ignored_marker", []string{parent}, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("linked worktree ignored file should not match through folder parent, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "folder_linked_worktree_ignored_marker") {
		t.Fatalf("linked worktree ignored content leaked, got:\n%s", out)
	}
}

func TestRun_GitDirectoryOperandDiscoversLinkedWorktree(t *testing.T) {
	requireTools(t)

	parent := initGitRepo(t, "app.go", "package main\n")
	scope := filepath.Join(parent, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, parent, "add", "scope")
	gitRunIn(t, parent, "commit", "-m", "add scope")

	gitRunIn(t, parent, "branch", "linked")
	linked := filepath.Join(scope, "linked")
	gitRunIn(t, parent, "worktree", "add", linked, "linked")
	if err := os.WriteFile(filepath.Join(linked, ".gitignore"), []byte(".cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "linked.go"), []byte("package linked\n// git_scope_linked_worktree_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, linked, "add", ".gitignore", "linked.go")
	gitRunIn(t, linked, "commit", "-m", "add linked marker")
	cacheDir := filepath.Join(linked, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "secret.txt"), []byte("git_scope_linked_worktree_ignored_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(parent)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "git_scope_linked_worktree_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run scoped linked worktree marker: %v", err)
	}
	if !strings.Contains(out, "scope/linked/linked.go") {
		t.Fatalf("expected linked worktree content under scoped directory, got:\n%s", out)
	}
	if count := strings.Count(out, "git_scope_linked_worktree_marker"); count != 1 {
		t.Fatalf("expected linked worktree result once, got %d occurrences:\n%s", count, out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "git_scope_linked_worktree_ignored_marker", []string{scope}, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("linked worktree ignored file should not match through scoped Git parent, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "git_scope_linked_worktree_ignored_marker") {
		t.Fatalf("linked worktree ignored content leaked, got:\n%s", out)
	}
}

func TestRun_GitDirectoryOperandDiscoversSubmoduleGitlink(t *testing.T) {
	requireTools(t)

	parent := initGitRepo(t, "app.go", "package main\n// parent_visible_marker\n")
	scope := filepath.Join(parent, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n// scope_visible_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, parent, "add", "scope")
	gitRunIn(t, parent, "commit", "-m", "add scope")

	subSrc := initGitRepoNoRemote(t, "sub.go", "package sub\n// submodule_scope_marker\n")
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", subSrc, filepath.Join("scope", "sub"))
	gitRunIn(t, parent, "commit", "-m", "add submodule")

	setTestUserCache(t)
	t.Chdir(parent)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "submodule_scope_marker", []string{scope}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run submodule marker: %v", err)
	}
	if !strings.Contains(out, "scope/sub/sub.go") {
		t.Fatalf("expected submodule content under scoped directory, got:\n%s", out)
	}
	if count := strings.Count(out, "submodule_scope_marker"); count != 1 {
		t.Fatalf("expected submodule result once, got %d occurrences:\n%s", count, out)
	}
}

func TestRun_GitDirectoryOperandDoesNotDiscoverIgnoredNestedGit(t *testing.T) {
	requireTools(t)

	parent := initGitRepo(t, "app.go", "package main\n// parent_visible_marker\n")
	scope := filepath.Join(parent, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package scope\n// scope_visible_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, ".gitignore"), []byte("scope/vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, parent, "add", ".gitignore", "scope")
	gitRunIn(t, parent, "commit", "-m", "add scope and ignore")

	nested := filepath.Join(scope, "vendor", "nested")
	initGitRepoNoRemoteAt(t, nested, "src.go", "package nested\n// parent_ignored_nested_marker\n")

	setTestUserCache(t)
	t.Chdir(parent)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "parent_ignored_nested_marker", []string{scope}, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("parent-ignored nested Git should not match through scoped parent, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "parent_ignored_nested_marker") {
		t.Fatalf("parent-ignored nested Git content leaked, got:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "parent_ignored_nested_marker", []string{nested}, 0, 0)
	})
	if err != nil {
		t.Fatalf("explicit ignored nested Git should still work: %v", err)
	}
	if !strings.Contains(out, "parent_ignored_nested_marker") {
		t.Fatalf("expected explicit ignored nested Git content, got:\n%s", out)
	}
}

func TestRun_GitDirectoryAndExactFileOperandDoNotDuplicateExactFile(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package seed\n")
	scope := filepath.Join(repo, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(scope, "target.go")
	if err := os.WriteFile(target, []byte("package scope\n// git_dir_exact_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "outside.go"), []byte("package outside\n// git_dir_exact_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-m", "add scoped and outside files")

	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "git_dir_exact_marker", []string{scope, target}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if count := strings.Count(out, "git_dir_exact_marker"); count != 1 {
		t.Fatalf("expected exact file result once, got %d occurrences:\n%s", count, out)
	}
	if !strings.Contains(out, "scope/target.go") {
		t.Fatalf("expected target file result, got:\n%s", out)
	}
	if strings.Contains(out, "outside.go") {
		t.Fatalf("out-of-scope file leaked, got:\n%s", out)
	}
}

func TestRun_CurrentGitDefaultDiscoversVisibleNestedGit(t *testing.T) {
	requireTools(t)

	parent := initGitRepo(t, "app.go", "package main\n// parent_visible_marker\n")
	nested := filepath.Join(parent, "nested")
	initGitRepoNoRemoteAt(t, nested, "src.go", "package nested\n// default_visible_nested_marker\n")

	setTestUserCache(t)
	t.Chdir(parent)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "default_visible_nested_marker", nil, 0, 0)
	})
	if err != nil {
		t.Fatalf("run nested marker: %v", err)
	}
	if !strings.Contains(out, "nested/src.go") {
		t.Fatalf("expected default search to include visible nested Git content, got:\n%s", out)
	}
	if count := strings.Count(out, "default_visible_nested_marker"); count != 1 {
		t.Fatalf("expected default nested Git result once, got %d occurrences:\n%s", count, out)
	}
}

func TestRun_GitRootOperandDiscoversSubmoduleGitlink(t *testing.T) {
	requireTools(t)

	parent := initGitRepo(t, "app.go", "package main\n// parent_visible_marker\n")
	subSrc := initGitRepoNoRemote(t, "sub.go", "package sub\n// root_submodule_marker\n")
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", subSrc, "sub")
	gitRunIn(t, parent, "commit", "-m", "add submodule")

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "root_submodule_marker", []string{parent}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run submodule marker: %v", err)
	}
	if !strings.Contains(out, "sub/sub.go") {
		t.Fatalf("expected Git root operand to include submodule content, got:\n%s", out)
	}
	if count := strings.Count(out, "root_submodule_marker"); count != 1 {
		t.Fatalf("expected root submodule result once, got %d occurrences:\n%s", count, out)
	}
}

func TestRun_CurrentGitDefaultDoesNotDiscoverIgnoredNestedGit(t *testing.T) {
	requireTools(t)

	parent := initGitRepo(t, "app.go", "package main\n// parent_visible_marker\n")
	if err := os.WriteFile(filepath.Join(parent, ".gitignore"), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, parent, "add", ".gitignore")
	gitRunIn(t, parent, "commit", "-m", "add gitignore")
	nested := filepath.Join(parent, "vendor", "nested")
	initGitRepoNoRemoteAt(t, nested, "src.go", "package nested\n// default_ignored_nested_marker\n")

	setTestUserCache(t)
	t.Chdir(parent)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "default_ignored_nested_marker", nil, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("parent-ignored nested Git should not match through default search, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "default_ignored_nested_marker") {
		t.Fatalf("parent-ignored nested Git content leaked, got:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "default_ignored_nested_marker", []string{nested}, 0, 0)
	})
	if err != nil {
		t.Fatalf("explicit ignored nested Git should still work: %v", err)
	}
	if !strings.Contains(out, "default_ignored_nested_marker") {
		t.Fatalf("expected explicit ignored nested Git content, got:\n%s", out)
	}
}

func TestRun_IgnoredFolderOperandSearchesContentAndDiscoversNestedGit(t *testing.T) {
	requireTools(t)

	parent := initGitRepo(t, "app.go", "package main\n// visible\n")
	if err := os.WriteFile(filepath.Join(parent, ".gitignore"), []byte("scratch/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, parent, "add", ".gitignore")
	gitRunIn(t, parent, "commit", "-m", "add gitignore")

	scratch := filepath.Join(parent, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "plain.txt"), []byte("scratch_plain_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(scratch, "nested")
	initGitRepoNoRemoteAt(t, nested, "src.go", "package nested\n// nested_committed_marker\n")
	if err := os.WriteFile(filepath.Join(nested, ".gitignore"), []byte(".venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, nested, "add", ".gitignore")
	gitRunIn(t, nested, "commit", "-m", "add gitignore")
	venv := filepath.Join(nested, ".venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "secret.py"), []byte("# nested_ignored_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(parent)

	// An explicitly selected ignored folder is searched as a plain folder
	// corpus, so its own content is found.
	out, err := captureStdout(t, func() error {
		return run(context.Background(), "scratch_plain_marker", []string{scratch}, 0, 0)
	})
	if err != nil {
		t.Fatalf("explicitly selected ignored folder should be searched, got err=%v", err)
	}
	if !strings.Contains(out, "scratch_plain_marker") {
		t.Fatalf("expected ignored folder content to be searched, got:\n%s", out)
	}

	// A nested Git repo under that folder is still discovered and indexed
	// as its own corpus, so committed nested content is found.
	out, err = captureStdout(t, func() error {
		return run(context.Background(), "nested_committed_marker", []string{scratch}, 0, 0)
	})
	if err != nil {
		t.Fatalf("nested Git under selected folder should be discovered, got err=%v out=%q", err, out)
	}
	if !strings.Contains(out, "nested_committed_marker") {
		t.Fatalf("expected nested Git content discovered under folder search, got:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "nested_committed_marker", []string{nested}, 0, 0)
	})
	if err != nil {
		t.Fatalf("explicit nested marker run: %v", err)
	}
	if !strings.Contains(out, "nested_committed_marker") {
		t.Fatalf("expected explicit nested git content, got:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "nested_ignored_marker", []string{nested}, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("nested git ignored file should not match, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "nested_ignored_marker") {
		t.Fatalf("nested ignored content leaked through folder search, got:\n%s", out)
	}
}

func TestRun_ParentFolderAndExplicitNestedGitRootDoNotDuplicateOrLeak(t *testing.T) {
	requireTools(t)

	parent := t.TempDir()
	nested := initGitRepoNoRemoteAt(t, filepath.Join(parent, "nested"), "src.go", "package nested\n// explicit_nested_marker\n")
	if err := os.WriteFile(filepath.Join(nested, ".gitignore"), []byte(".venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, nested, "add", ".gitignore")
	gitRunIn(t, nested, "commit", "-m", "add gitignore")
	venv := filepath.Join(nested, ".venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "secret.py"), []byte("# explicit_nested_ignored_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "explicit_nested_marker", []string{parent, nested}, 0, 0)
	})
	if err != nil {
		t.Fatalf("nested marker run: %v", err)
	}
	if count := strings.Count(out, "explicit_nested_marker"); count != 1 {
		t.Fatalf("expected explicit nested Git result once, got %d occurrences:\n%s", count, out)
	}

	out, err = captureStdout(t, func() error {
		return run(context.Background(), "explicit_nested_ignored_marker", []string{parent, nested}, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("nested ignored file should not match, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "explicit_nested_ignored_marker") {
		t.Fatalf("nested ignored content leaked through parent folder search, got:\n%s", out)
	}
}

func TestRun_ParentFolderAndNestedGitExactIgnoredFileKeepsFileOwner(t *testing.T) {
	requireTools(t)

	parent := t.TempDir()
	nested := initGitRepoNoRemoteAt(t, filepath.Join(parent, "nested"), "src.go", "package nested\n// visible\n")
	if err := os.WriteFile(filepath.Join(nested, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, nested, "add", ".gitignore")
	gitRunIn(t, nested, "commit", "-m", "add gitignore")
	ignoredDir := filepath.Join(nested, "ignored")
	if err := os.MkdirAll(ignoredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ignoredDir, "target.txt")
	if err := os.WriteFile(target, []byte("parent_nested_exact_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignoredDir, "sibling.txt"), []byte("parent_nested_exact_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "parent_nested_exact_marker", []string{parent, target}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "target.txt") {
		t.Fatalf("expected exact file owner to survive parent folder operand, got:\n%s", out)
	}
	if strings.Contains(out, "sibling.txt") {
		t.Fatalf("exact nested file search should not include sibling, got:\n%s", out)
	}
}

func TestRun_ParentFolderAndNestedGitExactTrackedFileKeepsFileOwner(t *testing.T) {
	requireTools(t)

	parent := t.TempDir()
	nested := initGitRepoNoRemoteAt(t, filepath.Join(parent, "nested"), "src.go", "package nested\n// parent_nested_tracked_exact_marker\n")
	target := filepath.Join(nested, "src.go")

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "parent_nested_tracked_exact_marker", []string{parent, target}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "src.go") {
		t.Fatalf("expected exact tracked file owner to survive parent folder operand, got:\n%s", out)
	}
	if count := strings.Count(out, "parent_nested_tracked_exact_marker"); count != 1 {
		t.Fatalf("expected exact tracked file result once, got %d occurrences:\n%s", count, out)
	}
}

func TestRun_ExternalExactFileDoesNotSearchSibling(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	target := filepath.Join(folder, "target.txt")
	if err := os.WriteFile(target, []byte("external_exact_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(folder, "sibling.txt"),
		[]byte("external_exact_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_exact_marker", []string{target}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## target.txt") {
		t.Fatalf("expected exact file result, got:\n%s", out)
	}
	if strings.Contains(out, "## sibling.txt") {
		t.Fatalf("exact file search should not include sibling, got:\n%s", out)
	}
}

func TestRun_SymlinkPathOperandFollowsTarget(t *testing.T) {
	requireTools(t)

	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("symlink_operand_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "symlink_operand_marker", []string{link}, 0, 0)
	})
	if err != nil {
		t.Fatalf("expected symlink path operand to follow target, got err=%v out=%q", err, out)
	}
	if !strings.Contains(out, "## target.txt") {
		t.Fatalf("expected resolved target file in output, got:\n%s", out)
	}
	if !strings.Contains(out, "symlink_operand_marker") {
		t.Fatalf("expected marker content from resolved target, got:\n%s", out)
	}
}

func TestRun_ExternalFolderSymlinkDedupesWithTarget(t *testing.T) {
	requireTools(t)

	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(targetDir, "marker.txt"),
		[]byte("symlink_dedup_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(targetDir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "symlink_dedup_marker", []string{link, targetDir}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if count := strings.Count(out, "## marker.txt"); count != 1 {
		t.Fatalf("expected exactly one result for symlink-vs-target operands, got count=%d:\n%s", count, out)
	}
}

func TestRun_ExternalFolderWalkSkipsDiscoveredSymlinks(t *testing.T) {
	requireTools(t)

	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("folder_walk_symlink_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "folder_walk_symlink_marker", []string{root}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## target.txt") {
		t.Fatalf("expected target file result, got:\n%s", out)
	}
	if strings.Contains(out, "## link.txt") {
		t.Fatalf("folder walk should skip discovered symlink, got:\n%s", out)
	}
}

func TestRun_SymlinkInsideWorktreeSearchesResolvedFile(t *testing.T) {
	requireTools(t)

	dir := initScopedGitRepo(t, "git_symlink_inside_marker")
	link := filepath.Join(dir, "a-link")
	if err := os.Symlink(filepath.Join(dir, "a", "app.go"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	setTestUserCache(t)
	t.Chdir(dir)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "git_symlink_inside_marker", []string{link}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## app.go") {
		t.Fatalf("expected scoped output to include resolved target file, got:\n%s", out)
	}
	if strings.Contains(out, "## b/app.go") || strings.Contains(out, "package b") {
		t.Fatalf("expected scoped output to exclude b/app.go, got:\n%s", out)
	}
}

func TestRun_GitScopeSymlinkOutsideWorktreeRoutedExternal(t *testing.T) {
	requireTools(t)

	dir := initScopedGitRepo(t, "git_symlink_outside_marker")

	external := t.TempDir()
	externalFile := filepath.Join(external, "outside.txt")
	if err := os.WriteFile(externalFile, []byte("git_symlink_outside_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(externalFile, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	setTestUserCache(t)
	t.Chdir(dir)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "git_symlink_outside_marker", []string{link}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## outside.txt") {
		t.Fatalf("expected external file header ## outside.txt, got:\n%s", out)
	}
	if !strings.Contains(out, "git_symlink_outside_marker") {
		t.Fatalf("expected output to contain external file marker, got:\n%s", out)
	}
}

func TestRun_BrokenSymlinkOperandErrors(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "broken-link")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	err := run(context.Background(), "broken_symlink_marker", []string{link}, 0, 0)
	if err == nil {
		t.Fatal("expected broken symlink operand to fail")
	}
	if !strings.Contains(err.Error(), "read path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_CurrentRepoExactFilePathOperandScopesSearch(t *testing.T) {
	requireTools(t)

	dir := initScopedGitRepo(t, "exact_scope_marker")
	setTestUserCache(t)
	t.Chdir(dir)

	assertScopedRunIncludesOnly(t, "exact_scope_marker", "a/app.go")
}

func TestRun_CurrentGitExactDirtyFileOperandUsesGitDirtyLayer(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// clean\n")
	target := filepath.Join(dir, "app.go")
	if err := os.WriteFile(target, []byte("package main\n// current_exact_dirty_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	t.Chdir(dir)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "current_exact_dirty_marker", []string{target}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run current exact dirty file: %v", err)
	}
	if !strings.Contains(out, "current_exact_dirty_marker") {
		t.Fatalf("expected dirty exact file content, got:\n%s", out)
	}
	if !strings.Contains(out, "[uncommitted]") {
		t.Fatalf("dirty tracked file should be searched via the Git dirty layer, got:\n%s", out)
	}
}

func TestRun_ExternalGitMetadataOnlyFolderReturnsNoMatch(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	metadataDir := filepath.Join(folder, ".git", "objects")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(metadataDir, "object.txt"),
		[]byte("metadata_only_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	_, err := captureStdout(t, func() error {
		return run(context.Background(), "metadata_only_marker", []string{folder}, 0, 0)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("expected no match, got %v", err)
	}
}

func TestRun_WarmFolderSearchDoesNotRequireCtags(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "note.txt"), []byte("warm_no_ctags_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	if _, err := captureStdout(t, func() error {
		return run(context.Background(), "warm_no_ctags_marker", []string{folder}, 0, 0)
	}); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	forceMissingCtags(t)
	out, err := captureStdout(t, func() error {
		return run(context.Background(), "warm_no_ctags_marker", []string{folder}, 0, 0)
	})
	if err != nil {
		t.Fatalf("warm run should not require ctags: %v", err)
	}
	if !strings.Contains(out, "warm_no_ctags_marker") {
		t.Fatalf("expected warm folder result, got:\n%s", out)
	}
}

func TestRun_WarmGitSearchDoesNotRequireCtags(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// warm_git_no_ctags_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)

	if _, err := captureStdout(t, func() error {
		return run(context.Background(), "warm_git_no_ctags_marker", nil, 0, 0)
	}); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	forceMissingCtags(t)
	out, err := captureStdout(t, func() error {
		return run(context.Background(), "warm_git_no_ctags_marker", nil, 0, 0)
	})
	if err != nil {
		t.Fatalf("warm Git run should not require ctags: %v", err)
	}
	if !strings.Contains(out, "warm_git_no_ctags_marker") {
		t.Fatalf("expected warm Git result, got:\n%s", out)
	}
}

func TestRun_GitRepoWithoutRemoteIndexesCommittedContent(t *testing.T) {
	requireTools(t)

	dir := initGitRepoNoRemote(t, "app.go", "package main\n// no_remote_committed_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "no_remote_committed_marker", nil, 0, 0)
	})
	if err != nil {
		t.Fatalf("run without remote: %v", err)
	}
	if !strings.Contains(out, "no_remote_committed_marker") {
		t.Fatalf("expected committed content from no-remote repo, got:\n%s", out)
	}
}

func TestRun_ExternalGitExactFileWithoutRemoteUsesGitDirtyLayer(t *testing.T) {
	requireTools(t)

	dir := initGitRepoNoRemote(t, "app.go", "package main\n// external_no_remote_marker\n")
	dirtyPath := filepath.Join(dir, "dirty.go")
	if err := os.WriteFile(dirtyPath, []byte("package main\n// external_no_remote_dirty_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "external_no_remote_marker", []string{filepath.Join(dir, "app.go")}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run external no-remote file: %v", err)
	}
	if !strings.Contains(out, "external_no_remote_marker") {
		t.Fatalf("expected committed content from external no-remote repo, got:\n%s", out)
	}

	dirtyOut, err := captureStdout(t, func() error {
		return run(context.Background(), "external_no_remote_dirty_marker", []string{dirtyPath}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run external no-remote dirty file: %v", err)
	}
	if !strings.Contains(dirtyOut, "external_no_remote_dirty_marker") {
		t.Fatalf("expected dirty content from external no-remote repo, got:\n%s", dirtyOut)
	}
	if !strings.Contains(dirtyOut, "[uncommitted]") {
		t.Fatalf("dirty tracked file should be searched via the Git dirty layer, got:\n%s", dirtyOut)
	}
}

func TestRun_GitRepoNamedUncommittedWithoutRemoteLabelsOnlyDirtyResultUncommitted(t *testing.T) {
	requireTools(t)

	dir := initGitRepoNoRemoteAt(t, filepath.Join(t.TempDir(), repoUncommitted), "clean.go", "package main\n// repo_named_uncommitted_clean\n")
	if err := os.WriteFile(filepath.Join(dir, "dirty.go"), []byte("package main\n// repo_named_uncommitted_dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	t.Chdir(dir)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "repo_named_uncommitted", nil, 0, 0)
	})
	if err != nil {
		t.Fatalf("run repo named %q without remote: %v", repoUncommitted, err)
	}
	for _, want := range []string{
		"repo_named_uncommitted_clean",
		"repo_named_uncommitted_dirty",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in output, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "## clean.go (Go) [uncommitted]") {
		t.Fatalf("committed file should not be tagged uncommitted, got:\n%s", out)
	}
	if !strings.Contains(out, "## dirty.go (Go) [uncommitted]") {
		t.Fatalf("dirty file should be tagged uncommitted, got:\n%s", out)
	}
}

func TestRun_GitColdCacheNoShardFailsWhenIndexingFails(t *testing.T) {
	dir := initGitRepo(t, "app.go", "package main\n// git_no_usable_shard_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)
	forceMissingCtags(t)

	_, err := captureStdout(t, func() error {
		return run(context.Background(), "git_no_usable_shard_marker", nil, 0, 0)
	})
	if err == nil {
		t.Fatal("expected Git indexing failure without shards")
	}
	if errors.Is(err, errNoMatch) {
		t.Fatalf("expected hard error, got no-match: %v", err)
	}
	if strings.Contains(err.Error(), "open search lock") ||
		strings.Contains(err.Error(), "no index shards") {
		t.Fatalf("expected indexing failure, got search fallback error: %v", err)
	}
	for _, want := range []string{
		"git corpus root=" + strconv.Quote(dir),
		"index=",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("git indexing error missing %q in: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "missing-ctags") {
		t.Fatalf("expected ctags indexing failure, got: %v", err)
	}
}

func TestRun_GitColdCacheNoShardSurfacesCommittedIndexerFailure(t *testing.T) {
	dir := initGitRepo(t, "app.go", "package main\n// git_no_usable_shard_committed_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)
	forceFailingCtags(t)

	_, err := captureStdout(t, func() error {
		return run(context.Background(), "git_no_usable_shard_committed_marker", nil, 0, 0)
	})
	if err == nil {
		t.Fatal("expected committed indexing failure without shards")
	}
	if errors.Is(err, errNoMatch) {
		t.Fatalf("expected hard error, got no-match: %v", err)
	}
	if strings.Contains(err.Error(), "no index shards") {
		t.Fatalf("expected committed indexing failure, got search fallback error: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("expected committed indexer failure cause, got: %v", err)
	}
}

func TestRun_MultiCorpusShowsContextEvenWhenOnlyOneCorpusMatches(t *testing.T) {
	requireTools(t)

	matching := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(matching, "note.txt"),
		[]byte("single_corpus_context_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "note.txt"), []byte("different content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "single_corpus_context_marker", []string{matching, other}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantRoot := canonicalCorpusPath(matching)
	if !strings.Contains(out, "## "+filepath.Join(wantRoot, "note.txt")) ||
		!strings.Contains(out, "[folder]") {
		t.Fatalf("expected corpus context for multi-corpus search, got:\n%s", out)
	}
}

// TestRun_PipedOutputHasNoANSI is the load-bearing guarantee for the agent-first
// design: captureStdout replaces os.Stdout with a pipe (not a TTY), so the color
// gate must yield plain, escape-free text end-to-end through run().
func TestRun_PipedOutputHasNoANSI(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(folder, "probe.go"),
		[]byte("package x\n// ansi_probe_marker here\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "ansi_probe_marker", []string{folder}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "ansi_probe_marker") {
		t.Fatalf("expected the match in output, got:\n%q", out)
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("piped output must be ANSI-free for agents/CI, got:\n%q", out)
	}
}

func TestRun_ExternalFolderFreshStateMissingShardsRebuilds(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(folder, "note.txt"),
		[]byte("missing_shard_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	if _, err := captureStdout(t, func() error {
		return run(context.Background(), "missing_shard_marker", []string{folder}, 0, 0)
	}); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	info, err := os.Lstat(folder)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(folder, info)
	if err != nil {
		t.Fatalf("planFolderCorpus: %v", err)
	}
	if readStateFile(plan.cacheDir) == "" {
		t.Fatalf("expected folder state at %q", plan.cacheDir)
	}
	if err := os.RemoveAll(plan.indexDir); err != nil {
		t.Fatalf("remove shards: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "missing_shard_marker", []string{folder}, 0, 0)
	})
	if err != nil {
		t.Fatalf("rerun after missing shards: %v", err)
	}
	if !strings.Contains(out, "missing_shard_marker") {
		t.Fatalf("expected rebuilt shard result, got:\n%s", out)
	}
}

func TestRun_ExternalFolderFallsBackToStaleV2Shards(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	path := filepath.Join(folder, "note.txt")
	if err := os.WriteFile(path, []byte("stale_cached_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	if _, err := captureStdout(t, func() error {
		return run(context.Background(), "stale_cached_marker", []string{folder}, 0, 0)
	}); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	if err := os.WriteFile(path, []byte("stale_reindex_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	forceMissingCtags(t)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "stale_cached_marker", []string{folder}, 0, 0)
	})
	if err != nil {
		t.Fatalf("stale fallback run: %v", err)
	}
	if !strings.Contains(out, "stale_cached_marker") {
		t.Fatalf("expected cached shard result, got:\n%s", out)
	}
}

func TestRun_ExternalFolderCapErrorDoesNotSearchStaleShards(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "note.txt"), []byte("cap_stale_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	if _, err := captureStdout(t, func() error {
		return run(context.Background(), "cap_stale_marker", []string{folder}, 0, 0)
	}); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	size := int64(maxIndexedDocumentBytes)
	count := maxFolderIndexedBytes/maxIndexedDocumentBytes + 1
	for i := range count {
		path := filepath.Join(folder, fmt.Sprintf("large_%03d.bin", i))
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "cap_stale_marker", []string{folder}, 0, 0)
	})
	if !errors.Is(err, errFolderCapExceeded) {
		t.Fatalf("expected folder cap error, got err=%v out=%q", err, out)
	}
	errText := err.Error()
	for _, want := range []string{
		"root=" + strconv.Quote(canonicalCorpusPath(folder)),
		"cache=",
		"index=",
		"indexed_bytes=",
		"limit=",
	} {
		if !strings.Contains(errText, want) {
			t.Fatalf("cap error missing %q in: %v", want, err)
		}
	}
	if strings.Contains(out, "cap_stale_marker") {
		t.Fatalf("must not search stale shards after cap error, got:\n%s", out)
	}
}

func TestRun_ExternalFolderNoUsableShardFailsWhenIndexingFails(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(folder, "note.txt"),
		[]byte("no_usable_shard_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())
	forceMissingCtags(t)

	_, err := captureStdout(t, func() error {
		return run(context.Background(), "no_usable_shard_marker", []string{folder}, 0, 0)
	})
	if err == nil {
		t.Fatal("expected indexing failure without usable shards")
	}
	if errors.Is(err, errNoMatch) {
		t.Fatalf("expected hard error, got no-match: %v", err)
	}
	if strings.Contains(err.Error(), "open search lock") ||
		strings.Contains(err.Error(), "no index shards") {
		t.Fatalf("expected indexing failure, got search fallback error: %v", err)
	}
	if !strings.Contains(err.Error(), "missing-ctags") {
		t.Fatalf("expected ctags indexing failure, got: %v", err)
	}
}

func forceMissingCtags(t *testing.T) {
	t.Helper()

	t.Setenv("CTAGS_COMMAND", filepath.Join(t.TempDir(), "missing-ctags"))
	ctagsOnce = sync.Once{}
	ctagsErr = nil
	t.Cleanup(func() {
		ctagsOnce = sync.Once{}
		ctagsErr = nil
	})
}

func forceFailingCtags(t *testing.T) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "failing-ctags")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CTAGS_COMMAND", path)
	ctagsOnce = sync.Once{}
	ctagsErr = nil
	t.Cleanup(func() {
		ctagsOnce = sync.Once{}
		ctagsErr = nil
	})
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = writer
	fnErr := fn()
	_ = writer.Close()
	os.Stdout = oldStdout

	data, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatalf("read captured stdout: %v", readErr)
	}
	return string(data), fnErr
}

func TestIntegration_EditThenSearch_SeesFreshContent(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", `package main

func original() {
	// original_marker_abc
}
`)

	// Verify original content is searchable
	files, err := runSeekInRepo(t, dir, "original_marker_abc")
	if err != nil {
		t.Fatalf("search for original failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for original committed content")
	}

	// Edit the file (uncommitted change)
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte(`package main

func updated() {
	// updated_marker_xyz
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Search for NEW content — this is the core guarantee
	files, err = runSeekInRepo(t, dir, "updated_marker_xyz")
	if err != nil {
		t.Fatalf("search for updated content failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("FRESHNESS VIOLATION: search after edit did not find updated content")
	}
}

func TestIntegration_NewUntrackedFile(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "existing.go", `package main
// existing_content
`)

	// Add a new untracked file
	if err := os.WriteFile(filepath.Join(dir, "new_file.go"), []byte(`package main
// untracked_marker_456
`), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "untracked_marker_456")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for untracked file content")
	}
}

func TestIntegration_DeletedFileNotSearchable(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "doomed.go", `package main
// doomed_marker_789
`)

	// Verify it's searchable first
	files, err := runSeekInRepo(t, dir, "doomed_marker_789")
	if err != nil {
		t.Fatalf("initial search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match before deletion")
	}

	// Delete the file (uncommitted deletion)
	if err := os.Remove(filepath.Join(dir, "doomed.go")); err != nil {
		t.Fatal(err)
	}

	// After deletion + reindex, the committed version may still appear
	// in the committed shard (git still has it in HEAD). That's expected behavior.
	// The key is that this doesn't crash.
	_, err = runSeekInRepo(t, dir, "doomed_marker_789")
	if err != nil {
		t.Fatalf("search after deletion should not error: %v", err)
	}
}

func TestIntegration_SecondSearchAfterEdit_AlwaysFresh(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "counter.go", `package main
// version_one_aaa
`)

	// First search — indexes the committed state
	_, err := runSeekInRepo(t, dir, "version_one_aaa")
	if err != nil {
		t.Fatal(err)
	}

	// Edit
	if err := os.WriteFile(filepath.Join(dir, "counter.go"), []byte(`package main
// version_two_bbb
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second search — must see version_two_bbb
	files, err := runSeekInRepo(t, dir, "version_two_bbb")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("FRESHNESS VIOLATION: second search after edit did not find updated content")
	}

	// Third edit
	if err := os.WriteFile(filepath.Join(dir, "counter.go"), []byte(`package main
// version_three_ccc
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Third search — must see version_three_ccc
	files, err = runSeekInRepo(t, dir, "version_three_ccc")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("FRESHNESS VIOLATION: third search after edit did not find updated content")
	}

	// Verify old content is gone from uncommitted results
	files, err = runSeekInRepo(t, dir, "version_two_bbb")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Error("expected version_two_bbb to no longer match after overwrite")
	}
}

func TestIntegration_MultipleFiles_EditOne(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "stable.go", `package main
// stable_content_111
`)

	// Add second file
	if err := os.WriteFile(filepath.Join(dir, "changing.go"), []byte(`package main
// changing_content_222
`), 0o644); err != nil {
		t.Fatal(err)
	}

	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "add changing")

	// Edit only changing.go
	if err := os.WriteFile(filepath.Join(dir, "changing.go"), []byte(`package main
// changed_content_333
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stable content should still be findable
	files, err := runSeekInRepo(t, dir, "stable_content_111")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("expected stable file to remain searchable")
	}

	// Changed content should be findable
	files, err = runSeekInRepo(t, dir, "changed_content_333")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("expected changed file to be searchable")
	}
}

// verifies the full seek pipeline inside a git worktree
func TestIntegration_Worktree_CommittedContent(t *testing.T) {
	requireTools(t)

	_, worktreeDir := initGitWorktree(t, "wt.go", `package main
// worktree_committed_marker_e2e
`)

	files, err := runSeekInRepo(t, worktreeDir, "worktree_committed_marker_e2e")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected committed marker to be found inside worktree")
	}
}

// verifies that an uncommitted edit inside a git worktree is visible without committing
func TestIntegration_Worktree_DirtyFile(t *testing.T) {
	requireTools(t)

	_, worktreeDir := initGitWorktree(t, "wt_dirty.go", `package main
// worktree_clean_marker_fff
`)

	// Dirty the file without committing
	if err := os.WriteFile(filepath.Join(worktreeDir, "wt_dirty.go"), []byte(`package main
// worktree_dirty_marker_fff
`), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, worktreeDir, "worktree_dirty_marker_fff")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("FRESHNESS VIOLATION: uncommitted edit inside worktree not found")
	}
}

func TestIntegration_Worktree_NestedUntrackedFileWithStatusConfig(t *testing.T) {
	requireTools(t)

	_, worktreeDir := initGitWorktree(t, "wt.go", `package main
// worktree_base_marker
`)
	gitRunIn(t, worktreeDir, "config", "status.showUntrackedFiles", "no")

	nested := filepath.Join(worktreeDir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "new.go"), []byte("package main\n// worktree_nested_untracked_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, worktreeDir, "worktree_nested_untracked_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("status.showUntrackedFiles=no should not hide nested untracked files in linked worktrees")
	}
}

// TestRun_NestedRepoVenvNotLeakedToParent — end-to-end regression
// guard for the .venv leak bug (95156aa). User searches a PARENT
// folder containing a nested git repo whose .gitignore excludes
// `.venv/`. The walker must discover the nested repo as a boundary
// and carve out its subtree from the parent folder corpus; the
// nested git corpus must respect .gitignore (gitindex.IndexGitRepo
// only indexes tracked files). Net: search must NOT match anything
// inside .venv.
//
// Pre-fix bug: walker descended into the nested repo on the second
// pass (state pass, after fingerprint pass had already enqueued the
// boundary), and the parent folder corpus ate the whole working tree
// including .venv content. Today's TestDedupHitMustSuppressDescent
// catches it at the mid-layer (newTestPool + scanFolderCorpus); this
// test exercises the same contract through the production run() path
// so a regression in main.go's pool wiring or in the walker's
// fingerprint-vs-state-pass interplay surfaces here.
func TestRun_NestedRepoVenvNotLeakedToParent(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested-repo")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real git init + commit so the boundary detector confirms.
	gitRunIn(t, nested, "init", "-q")
	gitRunIn(t, nested, "config", "user.email", "test@example.com")
	gitRunIn(t, nested, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(nested, ".gitignore"), []byte(".venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "src.go"), []byte("package main\n// regular src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, nested, "add", ".")
	gitRunIn(t, nested, "commit", "-q", "-m", "initial")

	// Plant gitignored content with a unique marker that we will
	// search for. If the walker descends into nested-repo under the
	// PARENT corpus, this file gets indexed and the search will hit.
	venvDir := filepath.Join(nested, ".venv")
	if err := os.MkdirAll(venvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = "zzz_venv_leak_marker_zzz"
	if err := os.WriteFile(filepath.Join(venvDir, "leaked.py"), []byte("# "+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Parent-level file so the parent folder corpus has at least one
	// selected entry after the carve-out — otherwise it's classified
	// corpusKnownEmpty and the cached state file would short-circuit
	// the second run's walker before the bug can fire.
	if err := os.WriteFile(filepath.Join(parent, "parent-marker.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(parent)

	// First run: cold cache. Walker discovers nested boundary,
	// enqueues git corpus, parent folder corpus carves out the subtree.
	if _, err := captureStdout(t, func() error {
		return run(context.Background(), marker, []string{parent}, 0, 0)
	}); err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("first run: %v", err)
	}

	// Mutate parent-level content so the second run's fingerprint
	// pass MUST diverge from the cached value, forcing
	// ensureFolderCorpusFresh past its line-62 cache-hit short-circuit
	// into the state pass where the dedup-rejection-descent bug
	// manifests.
	if err := os.WriteFile(filepath.Join(parent, "parent-marker.txt"), []byte("seed\nedit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second run: warm cache + mismatch. ensureFolderCorpusFresh runs
	// walker twice (fingerprint pass + state pass). PRE-fix the second
	// pass hit pool dedup → returned false → walker descended → .venv
	// indexed → search returned matches. POST-fix dedup returns true
	// → descent suppressed → no match.
	out, err := captureStdout(t, func() error {
		return run(context.Background(), marker, []string{parent}, 0, 0)
	})
	if err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("second run: %v", err)
	}
	if strings.Contains(out, marker) {
		t.Fatalf("nested .venv content leaked into parent folder corpus on second search; got output containing %q:\n%s", marker, out)
	}
}

// I6 (split case) — a tracked file and a gitignored file of the SAME repo,
// passed together, are searched by exactly one corpus each: the tracked file
// via the git index, the ignored file via a folder fallback. Both contents
// are found and neither file is duplicated.
func TestRun_TrackedAndIgnoredFileOperandsBothSearchedOnce(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "tracked.go", "package main\n// splitneedle tracked\n")
	writeIgnoredFile(t, repo, "secret.txt", "splitneedle ignored\n")
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "splitneedle", []string{
			filepath.Join(repo, "tracked.go"), filepath.Join(repo, "secret.txt"),
		}, 0, 0)
	})
	if err != nil {
		t.Fatalf("run tracked+ignored split: %v", err)
	}
	if !strings.Contains(out, "splitneedle tracked") {
		t.Fatalf("tracked file content missing (git index), got:\n%s", out)
	}
	if !strings.Contains(out, "splitneedle ignored") {
		t.Fatalf("ignored file content missing (folder fallback), got:\n%s", out)
	}
	// Multi-corpus mode renders absolute headers; the file names appear only
	// in headers (not in the content), so a single occurrence each proves
	// neither file is searched by two corpora.
	if n := strings.Count(out, "tracked.go"); n != 1 {
		t.Fatalf("tracked.go should appear once, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "secret.txt"); n != 1 {
		t.Fatalf("secret.txt should appear once, got %d:\n%s", n, out)
	}
}

// On a case-insensitive filesystem, a file operand typed with different case
// than git stored must still be found (routed to the git index with the scope
// corrected to the real on-disk byte name), not silently missed.
func TestRun_CaseMismatchedFileOperandStillFound(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "readme.md", "MARKER_CASE_FIX\n")
	// The bug only exists on case/normalization-insensitive filesystems, so
	// this regression coverage rides on the CI macOS (APFS) legs; on a
	// case-sensitive FS the mistyped name is a genuinely different file.
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Skip("case-sensitive filesystem; mistyped operand cannot resolve")
	}
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "MARKER_CASE_FIX", []string{filepath.Join(repo, "README.md")}, 0, 0)
	})
	if err != nil {
		t.Fatalf("case-mismatched file operand should be found via the git index: %v", err)
	}
	if !strings.Contains(out, "MARKER_CASE_FIX") {
		t.Fatalf("expected content via case-corrected git scope, got:\n%s", out)
	}
}

// Run-level twin for the untracked-NEW (`??`) visibility branch: a never-
// committed file passed as an operand must be searched via the git dirty
// layer (distinct from the modified-tracked ` M` branch covered elsewhere).
func TestRun_CurrentGitUntrackedFileOperandSearchesContent(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "committed.go", "package main\n")
	fresh := writeUntrackedFile(t, repo, "fresh.go", "package main\n// UNTRACKED_NEW_MARKER\n")
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "UNTRACKED_NEW_MARKER", []string{fresh}, 0, 0)
	})
	if err != nil {
		t.Fatalf("untracked-new file operand should be searched via the git dirty layer: %v", err)
	}
	if !strings.Contains(out, "UNTRACKED_NEW_MARKER") {
		t.Fatalf("expected untracked-new file content via the dirty layer, got:\n%s", out)
	}
	if !strings.Contains(out, "[uncommitted]") {
		t.Fatalf("untracked-new file should be tagged [uncommitted], got:\n%s", out)
	}
}

// Run-level symptom for the leading-colon bug: an ignored file whose name
// starts with ':' must fall back to a folder corpus and have its content
// searched. Pre-fix it was misparsed as a pathspec, classified visGit, and
// silently missed (routed to the git index that excludes it).
func TestRun_LeadingColonIgnoredFileOperandSearchesContent(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "a.go", "package main\n")
	colon := writeIgnoredFile(t, repo, ":weird", "COLON_IGNORED_MARKER\n")
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "COLON_IGNORED_MARKER", []string{colon}, 0, 0)
	})
	if err != nil {
		t.Fatalf("leading-colon ignored file operand should be searched via folder fallback: %v", err)
	}
	if !strings.Contains(out, "COLON_IGNORED_MARKER") {
		t.Fatalf("expected colon-named ignored file content via folder fallback, got:\n%s", out)
	}
}

// Run-level twin for the composed symlink→ignored path: a symlink resolving
// to a gitignored in-worktree file must fall back to a folder corpus and
// still surface the target's content.
func TestRun_SymlinkToIgnoredFileSearchesContent(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "a.go", "package main\n")
	target := writeIgnoredFile(t, repo, "secret.txt", "SYMLINK_IGNORED_MARKER\n")
	link := filepath.Join(repo, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := captureStdout(t, func() error {
		return run(context.Background(), "SYMLINK_IGNORED_MARKER", []string{link}, 0, 0)
	})
	if err != nil {
		t.Fatalf("symlink to an ignored file should search content via folder fallback: %v", err)
	}
	if !strings.Contains(out, "SYMLINK_IGNORED_MARKER") {
		t.Fatalf("expected resolved ignored-target content via folder fallback, got:\n%s", out)
	}
}
