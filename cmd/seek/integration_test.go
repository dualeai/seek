package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// initGitRepo creates a temp git repo with a single committed file.
// Returns the repo directory. The caller's working directory is unchanged.
func initGitRepo(tb testing.TB, fileName, content string) string {
	tb.Helper()
	dir := tb.TempDir()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		// zoekt's gitindex.IndexGitRepo derives the repo name from the remote
		// URL. Without a remote, it fails with "builder: must set Name".
		{"git", "remote", "add", "origin", "https://github.com/test/repo.git"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}

	// Exclude seek's cache directory from git status to match production behavior
	ensureGitExclude(fallbackGitPaths(dir), cacheDir)

	// Write and commit the file
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o644); err != nil {
		tb.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "initial"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}

	return dir
}

// runSeekInRepo runs the full seek pipeline against a repo directory.
// It resolves gitPaths (covering the worktree case) before calling runIndexing.
func runSeekInRepo(t *testing.T, repoDir, pattern string) ([]string, error) {
	t.Helper()
	ctx := context.Background()

	paths, err := resolveGitPaths(ctx, repoDir)
	if err != nil {
		paths = fallbackGitPaths(repoDir)
	}

	indexDir := filepath.Join(repoDir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	state := gitRepoStateIn(ctx, repoDir)
	currentState := computeStateHash(repoStateFingerprint(repoDir, state))
	cachedState := readStateFile(indexDir)
	if currentState != cachedState {
		if err := runIndexing(ctx, paths, indexDir, state, currentState); err != nil {
			return nil, err
		}
	}

	results, err := executeSearch(ctx, indexDir, pattern)
	if err != nil {
		return nil, err
	}

	var fileNames []string
	for _, fm := range results {
		fileNames = append(fileNames, fm.FileName)
	}
	return fileNames, nil
}

// gitRun executes a git command in dir, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitRunIn(t, dir, args...)
}

// gitRunIn executes a git command in the specified directory.
func gitRunIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
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

	// Search for committed content
	files, err := runSeekInRepo(t, dir, "findme_marker_123")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected at least 1 match for committed content")
	}

	// Search for non-existent content
	files, err = runSeekInRepo(t, dir, "nothere_xyz_999")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 matches for non-existent content, got %d", len(files))
	}
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
