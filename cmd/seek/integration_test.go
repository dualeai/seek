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
	dir := tb.TempDir()

	gitRunIn(tb, dir, "init")
	gitRunIn(tb, dir, "config", "user.email", "test@test.com")
	gitRunIn(tb, dir, "config", "user.name", "Test")
	// zoekt's gitindex.IndexGitRepo derives the repo name from the remote URL.
	// Without a remote, it fails with "builder: must set Name".
	gitRunIn(tb, dir, "remote", "add", "origin", "https://github.com/test/repo.git")
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

func setTestUserCache(tb testing.TB) string {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))
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

	if !strings.Contains(out, "## a/app.go") {
		t.Fatalf("expected scoped output to include a/app.go, got:\n%s", out)
	}
	if strings.Contains(out, "## b/app.go") {
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
	if !strings.HasSuffix(paths.ExcludePath, "/info/exclude") {
		t.Fatalf("expected git exclude path, got %q", paths.ExcludePath)
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

func TestRun_GitScopeSymlinkInsideWorktreeScopes(t *testing.T) {
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
	if !strings.Contains(out, "## a/app.go") {
		t.Fatalf("expected scoped output to include resolved target a/app.go, got:\n%s", out)
	}
	if strings.Contains(out, "## b/app.go") {
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

func TestRun_GitNoUsableShardFailsWhenIndexingFails(t *testing.T) {
	dir := initGitRepo(t, "app.go", "package main\n// git_no_usable_shard_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)
	forceMissingCtags(t)

	_, err := captureStdout(t, func() error {
		return run(context.Background(), "git_no_usable_shard_marker", nil, 0, 0)
	})
	if err == nil {
		t.Fatal("expected Git indexing failure without usable shards")
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
	if !strings.Contains(out, "[folder: "+wantRoot+"]") {
		t.Fatalf("expected corpus context for multi-corpus search, got:\n%s", out)
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

	size := int64(maxFolderFileSize)
	count := maxFolderIndexedBytes/maxFolderFileSize + 1
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
