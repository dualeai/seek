package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testUserCacheMarkerEnv = "SEEK_TEST_USER_CACHE_CONFIGURED"

// requireTools fails tb when git is not on PATH or checkCtags finds no
// Universal Ctags. checkCtags accepts the CTAGS_COMMAND command, a
// "universal-ctags" binary, or a "ctags" binary that reports Universal Ctags.
func requireTools(tb testing.TB) {
	tb.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		tb.Fatal("requires git on PATH")
	}
	if err := checkCtags(); err != nil {
		tb.Fatalf("requires universal-ctags: %v", err)
	}
}

// requireMissingOperand searches operand, which names no file, and requires
// exit 2 with the missing-path error. On a case-sensitive file system, an
// operand typed in another letter case names no file, and Seek never maps it
// to the file on disk.
func requireMissingOperand(t *testing.T, operand string) {
	t.Helper()
	err := seekRoot(t.Context(), t, nil, "--lexical-only", "zqmissingoperand", operand)
	want := "seek: path does not exist: " + strconv.Quote(operand)
	if code, got := exitCodeForError(err), formatCLIError(err); code != 2 || got != want {
		t.Fatalf("operand %q: exit=%d message=%q, want exit 2 and %q", operand, code, got, want)
	}
}

// initEmptyGitRepo makes a Git repository with no commit in a temporary
// directory, with a user identity and an origin remote, and returns its path.
func initEmptyGitRepo(tb testing.TB) string {
	tb.Helper()
	dir := initEmptyGitRepoNoRemote(tb)
	// The origin remote gives the setup of a common clone. The committed
	// indexer reads no remote (nativeGitRepository), so the remote does not
	// change the index.
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

// initGitRepo is initEmptyGitRepo with one commit that holds fileName. It
// returns the repository directory and does not change the working directory.
func initGitRepo(tb testing.TB, fileName, content string) string {
	tb.Helper()
	dir := initEmptyGitRepo(tb)

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

// setTestUserCache sets HOME to a new temporary directory and XDG_CACHE_HOME
// to its xdg-cache folder for the rest of tb. It clears SEEK_CACHE_DIR, sets
// SEEK_CACHE_BUDGET to 1P, marks the test cache for ensureTestUserCache, and
// returns that home. The search then uses the default user cache below it, as
// the README gives.
func setTestUserCache(tb testing.TB) string {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))
	tb.Setenv("SEEK_CACHE_DIR", "")
	tb.Setenv(testUserCacheMarkerEnv, "1")
	// Build tests do not depend on the free space of the host; the
	// budget tests set their own.
	tb.Setenv(envCacheBudget, "1P")
	return home
}

// documentedUserCacheRoot returns the default cache root that the README gives
// for the HOME and XDG_CACHE_HOME that setTestUserCache sets.
func documentedUserCacheRoot(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "seek")
	}
	return filepath.Join(home, "xdg-cache", "seek")
}

// ensureTestUserCache calls setTestUserCache unless tb already has a test
// cache, so a second call keeps the indexes of the first.
func ensureTestUserCache(tb testing.TB) {
	tb.Helper()
	if os.Getenv(testUserCacheMarkerEnv) != "" {
		return
	}
	setTestUserCache(tb)
}

func mustGitRepoStateIn(tb testing.TB, ctx context.Context, dir string) repoState {
	tb.Helper()
	state, err := gitRepoStateIn(ctx, dir)
	if err != nil {
		tb.Fatalf("read Git repository state: %v", err)
	}
	return state
}

// planGitTestCorpus sets a new test cache (setTestUserCache), plans the Git
// node of repoDir with no operands, and makes its cache and index directories.
func planGitTestCorpus(tb testing.TB, repoDir string) (gitPaths, node) {
	tb.Helper()
	setTestUserCache(tb)

	paths, err := resolveGitPaths(context.Background(), repoDir)
	if err != nil {
		tb.Fatalf("resolveGitPaths: %v", err)
	}
	plan, err := planGitCorpus(paths, nil)
	if err != nil {
		tb.Fatalf("planGitCorpus: %v", err)
	}
	if err := os.MkdirAll(plan.cacheDir, 0o755); err != nil {
		tb.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
		tb.Fatalf("mkdir index dir: %v", err)
	}
	return paths, plan
}

// planFolderTestCorpus is planGitTestCorpus for the folder node of root.
func planFolderTestCorpus(tb testing.TB, root string) node {
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

// runSeekInRepo runs a lexical search of pattern in repoDir through the root
// command and returns the path of each printed result. It changes the working
// directory of t to repoDir. No match gives no paths and no error.
func runSeekInRepo(t *testing.T, repoDir, pattern string) ([]string, error) {
	t.Helper()

	ensureTestUserCache(t)
	t.Chdir(repoDir)
	out, err := seek(t, "--lexical-only", pattern)
	if errors.Is(err, errNoMatch) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var fileNames []string
	for _, result := range printedResults(out) {
		fileNames = append(fileNames, result.path)
	}
	return fileNames, nil
}

// runSeekInPlannedGitCorpus refreshes and searches plan for pattern with no
// CLI, and returns the file names of the results. Tests and benchmarks use it
// to search a node that they planned before. A test of what a user sees uses
// runSeekInRepo or seek, which also run the CLI planning and the output format.
func runSeekInPlannedGitCorpus(
	ctx context.Context,
	pattern string,
	paths gitPaths,
	plan node,
) ([]string, error) {
	query, err := parseSearchQuery(pattern)
	if err != nil {
		return nil, err
	}
	results, _, err := prepareAndSearchCorpus(ctx, plan, &paths, query, defaultSearchConfig())
	if err != nil {
		return nil, err
	}

	var fileNames []string
	for _, result := range results {
		fileNames = append(fileNames, result.file.FileName)
	}
	return fileNames, nil
}

// initScopedGitRepo is initGitRepo with seed.go and a committed a/app.go and
// b/app.go. Both app.go files hold marker, so a scope leak prints b/app.go.
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

// assertScopedRunIncludesOnly searches query in operand, a path of a repository
// of initScopedGitRepo, and requires assertScopedOutputIncludesOnlyA.
func assertScopedRunIncludesOnly(t *testing.T, query, operand string) {
	t.Helper()

	out, err := seek(t, "--lexical-only", query, operand)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertScopedOutputIncludesOnlyA(t, out)
}

// assertScopedOutputIncludesOnlyA requires one printed result: a/app.go of a
// repository of initScopedGitRepo. A header starts with the operand as typed,
// so the path is "a/app.go" or ends with "/a/app.go".
func assertScopedOutputIncludesOnlyA(t *testing.T, out string) {
	t.Helper()

	paths := headerPaths(out)
	if len(paths) != 1 || paths[0] != "a/app.go" && !strings.HasSuffix(paths[0], "/a/app.go") {
		t.Fatalf("result paths=%q, want a/app.go only, got:\n%s", paths, out)
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

// gitOutputIn runs a git command in dir and returns its stdout with no
// trailing newline. A Git error fails t.
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

// initGitWorktree makes a repository with initGitRepo and a linked worktree of
// it on the branch worktree-branch. It returns the repository directory, then
// the worktree directory.
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
	if paths.CommonDir != filepath.Join(resolvedRepoDir, ".git") {
		t.Fatalf("expected common git dir %q, got %q", filepath.Join(resolvedRepoDir, ".git"), paths.CommonDir)
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
	out, err := seek(t, "--lexical-only", "findme_marker_123")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(out, "## hello.go") || !strings.Contains(out, "findme_marker_123") {
		t.Fatalf("expected committed content in output, got:\n%s", out)
	}

	// Search for non-existent content
	out, err = seek(t, "--lexical-only", "nothere_xyz_999")
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("expected no-match, got err=%v out=%q", err, out)
	}
}

func TestRun_UnbornRepositorySearchesUntrackedFile(t *testing.T) {
	requireTools(t)
	dir := initEmptyGitRepo(t)
	if err := os.WriteFile(
		filepath.Join(dir, "untracked.go"),
		[]byte("package unborn\n// unborn_untracked_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	t.Chdir(dir)

	out, err := seek(t, "--lexical-only", "unborn_untracked_marker")
	if err != nil {
		t.Fatalf("search unborn repository: %v", err)
	}
	if !strings.Contains(out, "## untracked.go") || !strings.Contains(out, "unborn_untracked_marker") {
		t.Fatalf("expected untracked file from unborn repository, got:\n%s", out)
	}
}

func TestRun_EmptyUnbornRepositoryReturnsNoMatch(t *testing.T) {
	requireTools(t)
	dir := initEmptyGitRepo(t)
	setTestUserCache(t)
	t.Chdir(dir)

	out, err := seek(t, "--lexical-only", "absent_unborn_marker")
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("empty unborn repository: error=%v output=%q, want no match", err, out)
	}
}

func TestEnsureCombinedGitCorpusPublishesEmptyNativeSnapshot(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "old.go", "package old\n// old_empty_transition_marker\n")
	paths, plan := planGitTestCorpus(t, dir)
	state := mustGitRepoStateIn(t, context.Background(), dir)
	if got, err := ensureCombinedGitCorpus(context.Background(), plan, paths, state); err != nil || got != corpusSearchable {
		t.Fatalf("warm native corpus: state=%v error=%v", got, err)
	}

	if err := os.WriteFile(
		filepath.Join(plan.cacheDir, uncommittedManifestFileName),
		[]byte("stale"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	// A missing manifest rejects the prior family as a delta base. The common
	// native full path must replace that family with an empty snapshot.
	removeFamilyManifest(plan.indexDir)

	gitRun(t, dir, "switch", "--orphan", "empty")
	gitRun(t, dir, "rm", "-rf", "--ignore-unmatch", ".")
	gitRun(t, dir, "commit", "--allow-empty", "-m", "empty")
	state = mustGitRepoStateIn(t, context.Background(), dir)
	// The corpus is known empty only while its empty marker holds the current
	// state, so this result and the reuse below check the marker.
	if got, err := ensureCombinedGitCorpus(context.Background(), plan, paths, state); err != nil || got != corpusKnownEmpty {
		t.Fatalf("publish empty native corpus: state=%v error=%v marker=%q shards=%v", got, err,
			readEmptyStateFile(plan.cacheDir), familyShardFilesForTest(t, plan.indexDir, familyAll))
	}
	if artifacts := familyShardFilesForTest(t, plan.indexDir, familyAll); len(artifacts) != 0 {
		t.Fatalf("empty native corpus retained artifacts: %v", artifacts)
	}
	if _, err := os.Stat(filepath.Join(plan.cacheDir, uncommittedManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("empty native corpus retained manifest: %v", err)
	}
	if got := readHeadFile(plan.cacheDir); got != state.HeadSHA {
		t.Fatalf("empty HEAD=%q, want %q", got, state.HeadSHA)
	}
	if got, err := ensureCombinedGitCorpus(context.Background(), plan, paths, state); err != nil || got != corpusKnownEmpty {
		t.Fatalf("reuse empty native corpus: state=%v error=%v", got, err)
	}
}

// publishedGitGenerationBytes maps the path of each published shard of plan,
// and of its family manifest, to the file content. Two equal maps show that
// no published file changed.
func publishedGitGenerationBytes(t *testing.T, plan node) map[string]string {
	t.Helper()
	paths := familyShardFilesForTest(t, plan.indexDir, familyAll)
	paths = append(paths, filepath.Join(plan.indexDir, familyManifestFile))
	result := make(map[string]string, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read published artifact %s: %v", path, err)
		}
		result[path] = string(raw)
	}
	return result
}

// hideLooseGitObject moves the loose object oid out of commonDir, so Git
// reads it as missing. It returns a function that puts the object back. The
// cleanup of t calls that function too.
func hideLooseGitObject(t *testing.T, commonDir, oid string) func() {
	t.Helper()
	objectPath := filepath.Join(commonDir, "objects", oid[:2], oid[2:])
	backupPath := filepath.Join(t.TempDir(), "object")
	if err := os.Rename(objectPath, backupPath); err != nil {
		t.Fatalf("hide Git object %s: %v", oid, err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if err := os.Rename(backupPath, objectPath); err != nil {
			t.Fatalf("restore Git object %s: %v", oid, err)
		}
		restored = true
	}
	t.Cleanup(restore)
	return restore
}

func TestNativeGitMissingObjectKeepsPublishedFamily(t *testing.T) {
	for _, tc := range []struct {
		name       string
		objectSpec string
	}{
		{name: "tree", objectSpec: "HEAD:z-missing"},
		{name: "blob", objectSpec: "HEAD:z-missing/object.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireTools(t)
			repoDir := initGitRepo(t, "old.go", "package old\n// PUBLISHED_OBJECT_BASE\n")
			paths, plan := planGitTestCorpus(t, repoDir)
			reindexGit(t, t.Context(), paths, plan)
			before := publishedGitGenerationBytes(t, plan)

			if err := os.WriteFile(filepath.Join(repoDir, "old.go"), []byte("package current\n// PUBLISHED_OBJECT_TARGET\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(repoDir, "z-missing"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repoDir, "z-missing", "object.go"), []byte("package missing\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRunIn(t, repoDir, "add", ".")
			gitRunIn(t, repoDir, "commit", "-m", "target with required object")
			// With no committed state, admission selects native full before
			// it reads the target tree. The tree case also fails after Git
			// emits and Seek stages the valid root file.
			if err := os.Remove(filepath.Join(plan.cacheDir, committedGitStateFile)); err != nil {
				t.Fatal(err)
			}
			state := mustGitRepoStateIn(t, t.Context(), repoDir)
			target := captureHeadForTest(t, paths.RepoDir)
			oid := gitOutputIn(t, repoDir, "rev-parse", tc.objectSpec)
			restore := hideLooseGitObject(t, paths.CommonDir, oid)
			if tc.name == "tree" {
				stagedDir := t.TempDir()
				_, stagedErr := indexNativeGitFull(t.Context(), paths.RepoDir, stagedDir, target, 1, nil)
				if stagedErr == nil || !shardsExist(stagedDir) {
					t.Fatalf("late tree failure: error=%v shards=%t", stagedErr, shardsExist(stagedDir))
				}
			}

			err := runIndexingWithCache(t.Context(), paths, plan.cacheDir, plan.indexDir, state, gitCorpusStateHash(paths, state))
			if err == nil {
				t.Fatal("indexing succeeded with a required Git object missing")
			}
			if after := publishedGitGenerationBytes(t, plan); !maps.Equal(after, before) {
				var changed []string
				for path, want := range before {
					if got, ok := after[path]; !ok {
						changed = append(changed, "removed:"+filepath.Base(path))
					} else if got != want {
						changed = append(changed, "changed:"+filepath.Base(path))
					}
				}
				for path := range after {
					if _, ok := before[path]; !ok {
						changed = append(changed, "added:"+filepath.Base(path))
					}
				}
				sort.Strings(changed)
				t.Fatalf("failed native build changed published artifacts: %v; error=%v; shards=%t", changed, err, shardsExist(plan.indexDir))
			}
			entries, err := os.ReadDir(plan.indexDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), buildDirPrefix) {
					t.Fatalf("failed native build left staging directory %q", entry.Name())
				}
			}
			if matches, err := searchPlannedCorpusForTest(t.Context(), plan, "PUBLISHED_OBJECT_BASE"); err != nil || len(matches) != 1 {
				t.Fatalf("search prior generation: matches=%v error=%v", matches, err)
			}

			restore()
			reindexGit(t, t.Context(), paths, plan)
			if matches, err := searchPlannedCorpusForTest(t.Context(), plan, "PUBLISHED_OBJECT_TARGET"); err != nil || len(matches) != 1 {
				t.Fatalf("search restored target: matches=%v error=%v", matches, err)
			}
		})
	}
}

func TestRun_KnownEmptyRemovesCommittedSidecarBeforeShardNameReuse(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "same.go", "package same\n// sidecar_first_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)

	if _, err := seek(t, "--lexical-only", "sidecar_first_marker"); err != nil {
		t.Fatalf("warm first commit: %v", err)
	}
	paths, err := resolveGitPaths(context.Background(), dir)
	if err != nil {
		t.Fatalf("resolve Git paths: %v", err)
	}
	plan, err := planGitCorpus(paths, nil)
	if err != nil {
		t.Fatalf("plan Git corpus: %v", err)
	}

	if err := os.WriteFile(
		filepath.Join(dir, "same.go"),
		[]byte("package same\n// sidecar_second_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "same.go")
	gitRun(t, dir, "commit", "-m", "second")
	if _, err := seek(t, "--lexical-only", "sidecar_second_marker"); err != nil {
		t.Fatalf("index second commit: %v", err)
	}
	foundMeta := false
	for _, artifact := range familyShardFilesForTest(t, plan.indexDir, familyCommitted) {
		foundMeta = foundMeta || strings.HasSuffix(artifact, ".zoekt.meta")
	}
	if !foundMeta {
		t.Fatal("second commit did not create a committed sidecar")
	}

	gitRun(t, dir, "switch", "--orphan", "unborn")
	if _, err := seek(t, "--lexical-only", "sidecar_absent_marker"); !errors.Is(err, errNoMatch) {
		t.Fatalf("clear empty unborn repository: %v", err)
	}
	if artifacts := familyShardFilesForTest(t, plan.indexDir, familyAll); len(artifacts) != 0 {
		t.Fatalf("empty unborn repository retained artifacts: %v", artifacts)
	}

	if err := os.WriteFile(
		filepath.Join(dir, "same.go"),
		[]byte("package same\n// sidecar_third_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "same.go")
	gitRun(t, dir, "commit", "-m", "third")
	out, err := seek(t, "--lexical-only", "sidecar_third_marker")
	if err != nil || !strings.Contains(out, "sidecar_third_marker") {
		t.Fatalf("search recreated path: error=%v output=%q", err, out)
	}
}

func TestRun_UnbornRepositoryClearsWarmCommittedIndex(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "old.go", "package old\n// unborn_old_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)

	if _, err := seek(t, "--lexical-only", "unborn_old_marker"); err != nil {
		t.Fatalf("warm committed index: %v", err)
	}
	paths, err := resolveGitPaths(context.Background(), dir)
	if err != nil {
		t.Fatalf("resolve Git paths: %v", err)
	}
	plan, err := planGitCorpus(paths, nil)
	if err != nil {
		t.Fatalf("plan Git corpus: %v", err)
	}

	gitRun(t, dir, "switch", "--orphan", "unborn")
	if err := os.WriteFile(
		filepath.Join(dir, "new.go"),
		[]byte("package unborn\n// unborn_new_marker\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		out, err := seek(t, "--lexical-only", "unborn_new_marker")
		if err != nil || !strings.Contains(out, "unborn_new_marker") {
			t.Fatalf("search new marker, run %d: error=%v output=%q", i+1, err, out)
		}

		out, err = seek(t, "--lexical-only", "unborn_old_marker")
		if !errors.Is(err, errNoMatch) || out != "" {
			t.Fatalf("search old marker, run %d: error=%v output=%q", i+1, err, out)
		}
	}

	if artifacts := familyShardFilesForTest(t, plan.indexDir, familyCommitted); len(artifacts) != 0 {
		t.Fatalf("unborn repository retained committed artifacts: %v", artifacts)
	}
}

func TestRun_UnbornRepositoryRepairsMatchingStateCommittedArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metaOnly bool
	}{
		{name: "base"},
		{name: "meta_only", metaOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireTools(t)
			dir := initGitRepo(t, "old.go", "package old\n// repair_old_marker\n")
			setTestUserCache(t)
			t.Chdir(dir)

			if _, err := seek(t, "--lexical-only", "repair_old_marker"); err != nil {
				t.Fatalf("warm committed index: %v", err)
			}
			paths, err := resolveGitPaths(context.Background(), dir)
			if err != nil {
				t.Fatalf("resolve Git paths: %v", err)
			}
			plan, err := planGitCorpus(paths, nil)
			if err != nil {
				t.Fatalf("plan Git corpus: %v", err)
			}

			gitRun(t, dir, "switch", "--orphan", "unborn")
			if err := os.WriteFile(
				filepath.Join(dir, "new.go"),
				[]byte("package unborn\n// repair_new_marker\n"),
				0o644,
			); err != nil {
				t.Fatal(err)
			}
			state := mustGitRepoStateIn(t, context.Background(), dir)
			if state.HeadSHA != "no-head" {
				t.Fatalf("HEAD=%q, want no-head", state.HeadSHA)
			}
			if err := writeStateFile(plan.cacheDir, gitCorpusStateHash(paths, state)); err != nil {
				t.Fatalf("seed matching state: %v", err)
			}
			if err := writeHeadFile(plan.cacheDir, state.HeadSHA); err != nil {
				t.Fatalf("seed no-head file: %v", err)
			}

			artifacts := familyShardFilesForTest(t, plan.indexDir, familyCommitted)
			if len(artifacts) == 0 {
				t.Fatal("warm cache has no committed artifact")
			}
			if tc.metaOnly {
				for _, artifact := range artifacts {
					if strings.HasSuffix(artifact, ".zoekt") {
						if err := os.Rename(artifact, artifact+".meta"); err != nil {
							t.Fatalf("seed meta-only artifact: %v", err)
						}
					}
				}
			}

			out, err := seek(t, "--lexical-only", "repair_new_marker")
			if err != nil || !strings.Contains(out, "repair_new_marker") {
				t.Fatalf("search repaired new marker: error=%v output=%q", err, out)
			}
			out, err = seek(t, "--lexical-only", "repair_old_marker")
			if !errors.Is(err, errNoMatch) || out != "" {
				t.Fatalf("search repaired old marker: error=%v output=%q", err, out)
			}
			if artifacts := familyShardFilesForTest(t, plan.indexDir, familyCommitted); len(artifacts) != 0 {
				t.Fatalf("repair retained committed artifacts: %v", artifacts)
			}
		})
	}
}

// TestRun_UsesUserCache checks the cache location that the README gives, not
// the location that the corpus plan computes.
func TestRun_UsesUserCache(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// unique_marker_user_cache\n")
	home := setTestUserCache(t)
	t.Chdir(dir)

	if _, err := seek(t, "--lexical-only", "unique_marker_user_cache"); err != nil {
		t.Fatalf("run: %v", err)
	}

	countIndexFiles := func(root string) (states, shards int) {
		t.Helper()
		err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			switch {
			case entry.Name() == stateFile:
				states++
			case strings.HasSuffix(entry.Name(), ".zoekt"):
				shards++
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
		return states, shards
	}
	cacheRoot := documentedUserCacheRoot(home)
	if states, shards := countIndexFiles(filepath.Join(cacheRoot, "corpora")); states == 0 || shards == 0 {
		t.Fatalf("user cache %q has %d state files and %d shards, want both", cacheRoot, states, shards)
	}
	if states, shards := countIndexFiles(dir); states != 0 || shards != 0 {
		t.Fatalf("searched repository has %d state files and %d shards, want none", states, shards)
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

	out, err := seek(t, "--lexical-only", "root_scope_marker", ".")
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

			out, err := seek(t, "--lexical-only", marker, dir, tc.operand(dir, childDir))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if count := strings.Count(out, marker); count != 1 {
				t.Fatalf("expected child result once, got %d occurrences:\n%s", count, out)
			}
		})
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

	out, err := seek(t, "--lexical-only", "external_folder_marker", folder)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## "+filepath.Join(folder, "note.txt")) {
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

	out, err := seek(t, "--lexical-only", "dedup_operand_marker", root, root, nested, filepath.Join(nested, "note.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The deepest operand, the file itself, gives the header.
	if strings.Count(out, "## "+filepath.Join(nested, "note.txt")) != 1 || strings.Count(out, "## ") != 1 {
		t.Fatalf("expected one visible result for collapsed operands, got:\n%s", out)
	}
}

func TestRun_InvalidQueryDoesNotCreateCache(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "note.txt"), []byte("invalid_query_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	home := setTestUserCache(t)
	t.Chdir(t.TempDir())

	_, err := seek(t, "--lexical-only", "(", folder)
	if err == nil {
		t.Fatal("expected invalid query to fail")
	}
	queryErr, ok := errors.AsType[*querySyntaxError](err)
	if !ok {
		t.Fatalf("error=%v, want querySyntaxError", err)
	}
	if queryErr.query != "(" || errors.Unwrap(queryErr) == nil {
		t.Fatalf("query error=%+v, want query and wrapped parser cause", queryErr)
	}

	cacheRoot := documentedUserCacheRoot(home)
	if _, err := os.Stat(cacheRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid query should not create the cache at %q; stat err=%v", cacheRoot, err)
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

	out, err := seek(t, "--lexical-only", "external_git_folder_marker", externalRepo)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## "+filepath.Join(externalRepo, "app.go")) {
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

	out, err := seek(t, "--lexical-only", "external_gitignored_marker", externalRepo)
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("external Git repo should honor .gitignore, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "external_gitignored_marker") {
		t.Fatalf("ignored external Git file must not be searchable, got:\n%s", out)
	}
}

func TestRun_ExternalGitSubdirPathScopesSearch(t *testing.T) {
	requireTools(t)

	externalRepo := initScopedGitRepo(t, "external_git_scope_marker")
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := seek(t, "--lexical-only", "external_git_scope_marker", filepath.Join(externalRepo, "a"))
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

	out, err := seek(t, "--lexical-only", "external_git_exact_scope_marker", filepath.Join(externalRepo, "a", "app.go"))
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

	out, err := seek(t, "--lexical-only", "literal_ignored_file_marker", target)
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

	out, err := seek(t, "--lexical-only", "external_literal_ignored_file_marker", target)
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

	out, err := seek(t, "--lexical-only", "literal_ignored_dir_marker", scratch)
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

	out, err := seek(t, "--lexical-only", "external_literal_ignored_dir_marker", scratch)
	if err != nil {
		t.Fatalf("explicitly selected ignored directory should be searched as a folder, got err=%v", err)
	}
	if !strings.Contains(out, "external_literal_ignored_dir_marker") {
		t.Fatalf("explicitly selected ignored directory content should be searched, got:\n%s", out)
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

	out, err := seek(t, "--lexical-only", "literal_pathspec_committed_marker", scope)
	if err != nil {
		t.Fatalf("run committed literal pathspec marker: %v", err)
	}
	if !strings.Contains(out, "scope[1]/committed.go") {
		t.Fatalf("expected literal pathspec committed result, got:\n%s", out)
	}

	out, err = seek(t, "--lexical-only", "literal_pathspec_dirty_marker", scope)
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

	initGitRepoNoRemoteAt(t, filepath.Join(parent, "outside", "nested"), "src.go", "package outside\n// visible_nested_git_marker\n")

	setTestUserCache(t)
	t.Chdir(parent)

	out, err := seek(t, "--lexical-only", "visible_nested_git_marker", scope)
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

	out, err = seek(t, "--lexical-only", "visible_nested_ignored_marker", scope)
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

	out, err := seek(t, "--lexical-only", "recursive_submodule_marker", scope)
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

	out, err := seek(t, "--lexical-only", "folder_recursive_submodule_marker", parent)
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

	out, err := seek(t, "--lexical-only", "folder_linked_worktree_marker", parent)
	if err != nil {
		t.Fatalf("run folder linked worktree marker: %v", err)
	}
	if !strings.Contains(out, "linked.go") {
		t.Fatalf("expected folder-discovered linked worktree content, got:\n%s", out)
	}
	if count := strings.Count(out, "folder_linked_worktree_marker"); count != 1 {
		t.Fatalf("expected linked worktree result once, got %d occurrences:\n%s", count, out)
	}

	out, err = seek(t, "--lexical-only", "folder_linked_worktree_ignored_marker", parent)
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

	out, err := seek(t, "--lexical-only", "git_scope_linked_worktree_marker", scope)
	if err != nil {
		t.Fatalf("run scoped linked worktree marker: %v", err)
	}
	if !strings.Contains(out, "scope/linked/linked.go") {
		t.Fatalf("expected linked worktree content under scoped directory, got:\n%s", out)
	}
	if count := strings.Count(out, "git_scope_linked_worktree_marker"); count != 1 {
		t.Fatalf("expected linked worktree result once, got %d occurrences:\n%s", count, out)
	}

	out, err = seek(t, "--lexical-only", "git_scope_linked_worktree_ignored_marker", scope)
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

	out, err := seek(t, "--lexical-only", "submodule_scope_marker", scope)
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

	out, err := seek(t, "--lexical-only", "parent_ignored_nested_marker", scope)
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("parent-ignored nested Git should not match through scoped parent, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "parent_ignored_nested_marker") {
		t.Fatalf("parent-ignored nested Git content leaked, got:\n%s", out)
	}

	out, err = seek(t, "--lexical-only", "parent_ignored_nested_marker", nested)
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

	out, err := seek(t, "--lexical-only", "git_dir_exact_marker", scope, target)
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

	out, err := seek(t, "--lexical-only", "default_visible_nested_marker")
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

	out, err := seek(t, "--lexical-only", "root_submodule_marker", parent)
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

	out, err := seek(t, "--lexical-only", "default_ignored_nested_marker")
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("parent-ignored nested Git should not match through default search, got err=%v out=%q", err, out)
	}
	if strings.Contains(out, "default_ignored_nested_marker") {
		t.Fatalf("parent-ignored nested Git content leaked, got:\n%s", out)
	}

	out, err = seek(t, "--lexical-only", "default_ignored_nested_marker", nested)
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

	// A folder corpus searches the ignored folder that the operand names, so
	// its own content has a result.
	out, err := seek(t, "--lexical-only", "scratch_plain_marker", scratch)
	if err != nil {
		t.Fatalf("explicitly selected ignored folder should be searched, got err=%v", err)
	}
	if !strings.Contains(out, "scratch_plain_marker") {
		t.Fatalf("expected ignored folder content to be searched, got:\n%s", out)
	}

	// Seek finds the nested Git repository under that folder and indexes it
	// as its own corpus, so its committed content has a result.
	out, err = seek(t, "--lexical-only", "nested_committed_marker", scratch)
	if err != nil {
		t.Fatalf("nested Git under selected folder should be discovered, got err=%v out=%q", err, out)
	}
	if !strings.Contains(out, "nested_committed_marker") {
		t.Fatalf("expected nested Git content discovered under folder search, got:\n%s", out)
	}

	out, err = seek(t, "--lexical-only", "nested_committed_marker", nested)
	if err != nil {
		t.Fatalf("explicit nested marker run: %v", err)
	}
	if !strings.Contains(out, "nested_committed_marker") {
		t.Fatalf("expected explicit nested git content, got:\n%s", out)
	}

	out, err = seek(t, "--lexical-only", "nested_ignored_marker", nested)
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

	out, err := seek(t, "--lexical-only", "explicit_nested_marker", parent, nested)
	if err != nil {
		t.Fatalf("nested marker run: %v", err)
	}
	if count := strings.Count(out, "explicit_nested_marker"); count != 1 {
		t.Fatalf("expected explicit nested Git result once, got %d occurrences:\n%s", count, out)
	}

	out, err = seek(t, "--lexical-only", "explicit_nested_ignored_marker", parent, nested)
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

	out, err := seek(t, "--lexical-only", "parent_nested_exact_marker", parent, target)
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

	out, err := seek(t, "--lexical-only", "parent_nested_tracked_exact_marker", parent, target)
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

	out, err := seek(t, "--lexical-only", "external_exact_marker", target)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// A file operand prints itself.
	if !strings.Contains(out, "## "+target) {
		t.Fatalf("expected exact file result, got:\n%s", out)
	}
	// Only a sibling result prints the sibling's name.
	if strings.Contains(out, "sibling.txt") {
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
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := seek(t, "--lexical-only", "symlink_operand_marker", link)
	if err != nil {
		t.Fatalf("expected symlink path operand to follow target, got err=%v out=%q", err, out)
	}
	// A file operand prints itself as typed.
	if !strings.Contains(out, "## "+link) {
		t.Fatalf("expected the header of the link as typed, got:\n%s", out)
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
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := seek(t, "--lexical-only", "symlink_dedup_marker", link, targetDir)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if count := strings.Count(out, "## "); count != 1 || !strings.Contains(out, "marker.txt (Text)") {
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
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	out, err := seek(t, "--lexical-only", "folder_walk_symlink_marker", root)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## "+filepath.Join(root, "target.txt")) {
		t.Fatalf("expected target file result, got:\n%s", out)
	}
	if strings.Contains(out, "## "+filepath.Join(root, "link.txt")) {
		t.Fatalf("folder walk should skip discovered symlink, got:\n%s", out)
	}
}

func TestRun_SymlinkInsideWorktreeSearchesResolvedFile(t *testing.T) {
	requireTools(t)

	dir := initScopedGitRepo(t, "git_symlink_inside_marker")
	link := filepath.Join(dir, "a-link")
	if err := os.Symlink(filepath.Join(dir, "a", "app.go"), link); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(dir)

	out, err := seek(t, "--lexical-only", "git_symlink_inside_marker", link)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## "+link) {
		t.Fatalf("expected the header of the link as typed, got:\n%s", out)
	}
	if strings.Contains(out, "b/app.go") || strings.Contains(out, "package b") {
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
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(dir)

	out, err := seek(t, "--lexical-only", "git_symlink_outside_marker", link)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "## "+link) {
		t.Fatalf("expected the header of the link as typed, got:\n%s", out)
	}
	if !strings.Contains(out, "git_symlink_outside_marker") {
		t.Fatalf("expected output to contain external file marker, got:\n%s", out)
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

	out, err := seek(t, "--lexical-only", "current_exact_dirty_marker", target)
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

	_, err := seek(t, "--lexical-only", "metadata_only_marker", folder)
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

	if _, err := seek(t, "--lexical-only", "warm_no_ctags_marker", folder); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	forceMissingCtags(t)
	out, err := seek(t, "--lexical-only", "warm_no_ctags_marker", folder)
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

	if _, err := seek(t, "--lexical-only", "warm_git_no_ctags_marker"); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	forceMissingCtags(t)
	out, err := seek(t, "--lexical-only", "warm_git_no_ctags_marker")
	if err != nil {
		t.Fatalf("warm Git run should not require ctags: %v", err)
	}
	if !strings.Contains(out, "warm_git_no_ctags_marker") {
		t.Fatalf("expected warm Git result, got:\n%s", out)
	}
}

func TestRun_EmptyNativeGitSnapshotsDoNotRequireCtags(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("requires git on PATH")
	}

	tests := []struct {
		name  string
		setup func(*testing.T) string
	}{
		{
			name: "empty_commit",
			setup: func(t *testing.T) string {
				dir := initEmptyGitRepo(t)
				gitRun(t, dir, "commit", "--allow-empty", "-m", "empty")
				return dir
			},
		},
		{
			name: "gitlink_only",
			setup: func(t *testing.T) string {
				dir := initEmptyGitRepo(t)
				gitRun(t, dir, "commit", "--allow-empty", "-m", "gitlink target")
				gitlinkOID := gitOutputIn(t, dir, "rev-parse", "HEAD")
				gitRun(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+gitlinkOID+",submodule")
				gitRun(t, dir, "commit", "-m", "gitlink only")
				return dir
			},
		},
		{
			name: "sha256_empty_commit",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				cmd := exec.Command("git", "init", "--object-format=sha256", dir)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("the installed Git cannot make a SHA-256 repository: %v: %s", err, out)
				}
				gitRun(t, dir, "config", "user.email", "test@test.com")
				gitRun(t, dir, "config", "user.name", "Test")
				gitRun(t, dir, "commit", "--allow-empty", "-m", "sha256 empty")
				return dir
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			setTestUserCache(t)
			t.Chdir(dir)
			forceMissingCtags(t)

			out, err := seek(t, "--lexical-only", "absent_empty_native_marker")
			if !errors.Is(err, errNoMatch) || out != "" {
				t.Fatalf("empty native search: error=%v output=%q", err, out)
			}
			paths, err := resolveGitPaths(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planGitCorpus(paths, nil)
			if err != nil {
				t.Fatal(err)
			}
			if shardsExist(plan.indexDir) || readEmptyStateFile(plan.cacheDir) == "" {
				t.Fatalf("empty native cache has shards=%t marker=%q", shardsExist(plan.indexDir), readEmptyStateFile(plan.cacheDir))
			}
		})
	}
}

func TestRun_EmptyCommittedFamilyWithDirtyShardUsesWarmPath(t *testing.T) {
	requireTools(t)
	for _, tc := range []struct {
		name  string
		setup func(*testing.T) string
	}{
		{
			name: "gitlink_only",
			setup: func(t *testing.T) string {
				dir := initEmptyGitRepo(t)
				gitRun(t, dir, "commit", "--allow-empty", "-m", "gitlink target")
				gitlinkOID := gitOutputIn(t, dir, "rev-parse", "HEAD")
				gitRun(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+gitlinkOID+",submodule")
				gitRun(t, dir, "commit", "-m", "gitlink only")
				return dir
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			if err := os.WriteFile(filepath.Join(dir, "dirty.go"), []byte("package dirty\n// empty_committed_dirty_marker\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			setTestUserCache(t)
			t.Chdir(dir)
			if out, err := seek(t, "--lexical-only", "empty_committed_dirty_marker"); err != nil || !strings.Contains(out, "empty_committed_dirty_marker") {
				t.Fatalf("cold dirty search: error=%v output=%q", err, out)
			}

			paths, err := resolveGitPaths(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planGitCorpus(paths, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(plan.cacheDir, uncommittedManifestFileName), []byte("corrupt"), 0o644); err != nil {
				t.Fatal(err)
			}
			forceMissingCtags(t)
			if out, err := seek(t, "--lexical-only", "empty_committed_dirty_marker"); err != nil || !strings.Contains(out, "empty_committed_dirty_marker") {
				t.Fatalf("warm dirty search: error=%v output=%q", err, out)
			}
		})
	}
}

func TestRun_GitRepoWithoutRemoteIndexesCommittedContent(t *testing.T) {
	requireTools(t)

	dir := initGitRepoNoRemote(t, "app.go", "package main\n// no_remote_committed_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)

	out, err := seek(t, "--lexical-only", "no_remote_committed_marker")
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

	out, err := seek(t, "--lexical-only", "external_no_remote_marker", filepath.Join(dir, "app.go"))
	if err != nil {
		t.Fatalf("run external no-remote file: %v", err)
	}
	if !strings.Contains(out, "external_no_remote_marker") {
		t.Fatalf("expected committed content from external no-remote repo, got:\n%s", out)
	}

	dirtyOut, err := seek(t, "--lexical-only", "external_no_remote_dirty_marker", dirtyPath)
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

	out, err := seek(t, "--lexical-only", "repo_named_uncommitted")
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

// TestRun_ReservedZoektNameKeepsCommittedFamily sets zoekt.name to the dirty
// repository name. Seek reads no zoekt.* config, so the committed file keeps
// its committed label.
func TestRun_ReservedZoektNameKeepsCommittedFamily(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "committed.go", "package committed\n// reserved_name_committed_marker\n")
	gitRun(t, dir, "config", "zoekt.name", repoUncommitted)
	setTestUserCache(t)
	t.Chdir(dir)

	// The committed file keeps its committed label on a clean tree and after a
	// dirty file rebuilds the uncommitted family.
	for _, step := range []string{"clean", "dirty"} {
		if step == "dirty" {
			if err := os.WriteFile(filepath.Join(dir, "dirty.go"), []byte("package dirty\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, err := seek(t, "--lexical-only", "reserved_name_committed_marker")
		if err != nil || !strings.Contains(out, "## committed.go (Go)\n") {
			t.Fatalf("%s search of committed file with reserved name: error=%v output=%q", step, err, out)
		}
	}
}

func TestRun_FirstNativeFailureDoesNotOpenLegacyGeneration(t *testing.T) {
	requireTools(t)
	repoDir := initGitRepo(t, "legacy.go", "package legacy\n// LEGACY_GENERATION_DECOY\n")
	paths, currentPlan := planGitTestCorpus(t, repoDir)
	root := canonicalCorpusPath(paths.RepoDir)
	commonDir := canonicalCorpusPath(paths.CommonDir)
	legacyPlan, err := newCorpusPlan(
		corpusKindGit,
		rootTypeWorktree,
		root,
		"git",
		"git_worktree", root,
		"git_common_dir", commonDir,
	)
	if err != nil {
		t.Fatal(err)
	}
	if legacyPlan.cacheDir == currentPlan.cacheDir {
		t.Fatal("native Git identity did not rotate the cache")
	}
	snapshot := captureHeadForTest(t, paths.RepoDir)
	if _, err := indexNativeGitFull(t.Context(), paths.RepoDir, legacyPlan.indexDir, snapshot, 1, nil); err != nil {
		t.Fatalf("build legacy-generation decoy: %v", err)
	}
	if matches, err := searchPlannedCorpusForTest(t.Context(), legacyPlan, "LEGACY_GENERATION_DECOY"); err != nil || len(matches) != 1 {
		t.Fatalf("legacy-generation decoy is not searchable: matches=%v error=%v", matches, err)
	}

	forceMissingCtags(t)
	t.Chdir(repoDir)
	out, err := seek(t, "--lexical-only", "LEGACY_GENERATION_DECOY")
	if err == nil || errors.Is(err, errNoMatch) {
		t.Fatalf("first native failure returned error=%v output=%q", err, out)
	}
	if out != "" || shardsExist(currentPlan.indexDir) {
		t.Fatalf("first native failure used another generation: output=%q current_shards=%t", out, shardsExist(currentPlan.indexDir))
	}
}

func TestRun_GitColdCacheNoShardFailsWhenIndexingFails(t *testing.T) {
	dir := initGitRepo(t, "app.go", "package main\n// git_no_usable_shard_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)
	forceMissingCtags(t)

	_, err := seek(t, "--lexical-only", "git_no_usable_shard_marker")
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
	// No path means ".": the error names the root with its symlinks resolved.
	root, evalErr := filepath.EvalSymlinks(dir)
	if evalErr != nil {
		t.Fatal(evalErr)
	}
	for _, want := range []string{
		"git corpus root=" + strconv.Quote(root),
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
	logs := captureTestLogs(t, slog.LevelWarn)

	_, err := seek(t, "--lexical-only", "git_no_usable_shard_committed_marker")
	// The failed build skips the node with a warning that names the
	// indexer's cause; the search has no result.
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("err=%v, want no match", err)
	}
	if !logsMention(logs.Records(), "exit status 7") || logsMention(logs.Records(), "no index shards") {
		t.Fatalf("warnings=%v, want the committed indexer failure cause", logs.Records())
	}
}

func TestRun_GitWarmCachePreservesIndexWhenStatusFails(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "app.go", "package main\n// git_status_failure_marker\n")
	setTestUserCache(t)
	t.Chdir(dir)

	if _, err := seek(t, "--lexical-only", "git_status_failure_marker"); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find Git executable: %v", err)
	}
	shimDir := t.TempDir()
	shim := "#!/bin/sh\n" + gitShimSubcommand +
		"if [ \"$subcommand\" = status ]; then\n" +
		"  echo forced-status-failure >&2\n" +
		"  exit 42\n" +
		"fi\n" +
		"exec \"$SEEK_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write Git shim: %v", err)
	}
	t.Setenv("SEEK_TEST_REAL_GIT", realGit)
	pathWithGit := os.Getenv("PATH")
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+pathWithGit)

	logs := captureTestLogs(t, slog.LevelWarn)
	_, err = seek(t, "--lexical-only", "git_status_failure_marker")
	// The node fails with a warning that gives Git's text.
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("err=%v, want no match", err)
	}
	for _, want := range []string{"git status", "forced-status-failure"} {
		if !logsMention(logs.Records(), want) {
			t.Fatalf("warnings=%v, want %q", logs.Records(), want)
		}
	}

	// The next search uses the warm index. Without ctags, a search that must
	// build the index again fails or warns. A result with no warning shows that
	// the failure kept the index.
	t.Setenv("PATH", pathWithGit)
	forceMissingCtags(t)
	logs = captureTestLogs(t, slog.LevelWarn)
	out, err := seek(t, "--lexical-only", "git_status_failure_marker")
	if err != nil || !strings.Contains(out, "git_status_failure_marker") {
		t.Fatalf("search after the Git status failure: error=%v output=%q", err, out)
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("search after the Git status failure rebuilt the index: warnings=%v", records)
	}
}

func TestRun_GitWarmCacheReportsIndexerFailureOnce(t *testing.T) {
	requireTools(t)
	for _, tc := range []struct {
		name           string
		commit         bool
		wantStaleMatch bool
	}{
		{name: "committed", commit: true, wantStaleMatch: true},
		{name: "uncommitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := "stale_" + tc.name + "_marker"
			dir := initGitRepo(t, "app.go", "package main\n// "+marker+"\n")
			setTestUserCache(t)
			t.Chdir(dir)

			if _, err := seek(t, "--lexical-only", marker); err != nil {
				t.Fatalf("initial run: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// replacement_marker\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.commit {
				gitRunIn(t, dir, "add", "app.go")
				gitRunIn(t, dir, "commit", "-m", "change marker")
			}
			forceFailingCtags(t)
			logs := captureTestLogs(t, slog.LevelWarn)

			out, err := seek(t, "--lexical-only", marker)
			if tc.wantStaleMatch {
				if err != nil || !strings.Contains(out, marker) {
					t.Fatalf("stale run output=%q error=%v, want cached match", out, err)
				}
			} else if !errors.Is(err, errNoMatch) || out != "" {
				t.Fatalf("dirty stale run output=%q error=%v, want no stale committed match", out, err)
			}

			warningCount := 0
			for _, record := range logs.Records() {
				if record.Level == slog.LevelWarn && record.Message == "Index update failed; using the existing index" {
					warningCount++
				}
			}
			if warningCount != 1 {
				t.Fatalf("stale-index warning count=%d, want 1", warningCount)
			}
		})
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

	out, err := seek(t, "--lexical-only", "single_corpus_context_marker", matching, other)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The header keeps the operand as typed.
	if !strings.Contains(out, "## "+filepath.Join(matching, "note.txt")) ||
		!strings.Contains(out, "[folder]") {
		t.Fatalf("expected corpus context for multi-corpus search, got:\n%s", out)
	}
}

// TestRun_PipedOutputHasNoANSI checks that output to a pipe has no ANSI
// escape.
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

	out, err := seek(t, "--lexical-only", "ansi_probe_marker", folder)
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

	if _, err := seek(t, "--lexical-only", "missing_shard_marker", folder); err != nil {
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

	out, err := seek(t, "--lexical-only", "missing_shard_marker", folder)
	if err != nil {
		t.Fatalf("rerun after missing shards: %v", err)
	}
	if !strings.Contains(out, "missing_shard_marker") {
		t.Fatalf("expected rebuilt shard result, got:\n%s", out)
	}
}

func TestRun_ExternalFolderFallsBackToStaleShards(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	path := filepath.Join(folder, "note.txt")
	if err := os.WriteFile(path, []byte("stale_cached_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(t.TempDir())

	if _, err := seek(t, "--lexical-only", "stale_cached_marker", folder); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	if err := os.WriteFile(path, []byte("stale_reindex_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	forceMissingCtags(t)

	out, err := seek(t, "--lexical-only", "stale_cached_marker", folder)
	if err != nil {
		t.Fatalf("stale fallback run: %v", err)
	}
	if !strings.Contains(out, "stale_cached_marker") {
		t.Fatalf("expected cached shard result, got:\n%s", out)
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

	_, err := seek(t, "--lexical-only", "no_usable_shard_marker", folder)
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

// forceMissingCtags sets CTAGS_COMMAND to a path that does not exist and
// clears the cached result of checkCtagsCached, so the next build finds no
// ctags.
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

// forceFailingCtags is forceMissingCtags with a ctags script that exits with
// status 7, so a build that runs ctags fails with "exit status 7".
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

// captureStdout runs fn with os.Stdout on a pipe and returns what fn wrote
// and the error of fn. It changes the process stdout, so two calls must not
// run at the same time.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	// Read while fn runs: output larger than the pipe buffer must not block
	// the command.
	type readResult struct {
		data []byte
		err  error
	}
	read := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(reader)
		read <- readResult{data, err}
	}()
	os.Stdout = writer
	fnErr := fn()
	_ = writer.Close()
	os.Stdout = oldStdout

	result := <-read
	data, readErr := result.data, result.err
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

	// The committed content has a result.
	files, err := runSeekInRepo(t, dir, "original_marker_abc")
	if err != nil {
		t.Fatalf("search for original failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for original committed content")
	}

	// Change the file with no commit.
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte(`package main

func updated() {
	// updated_marker_xyz
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The search after the edit must find the new content.
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

	// The committed file has a result before the deletion.
	files, err := runSeekInRepo(t, dir, "doomed_marker_789")
	if err != nil {
		t.Fatalf("initial search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match before deletion")
	}

	// Delete the file with no commit.
	if err := os.Remove(filepath.Join(dir, "doomed.go")); err != nil {
		t.Fatal(err)
	}

	// HEAD still holds the file, but the search shows the working tree: the
	// deleted file has no result.
	files, err = runSeekInRepo(t, dir, "doomed_marker_789")
	if err != nil {
		t.Fatalf("search after deletion should not error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("deleted file is still searchable: %v", files)
	}
}

func TestIntegration_SecondSearchAfterEdit_AlwaysFresh(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "counter.go", `package main
// version_one_aaa
`)

	// The first search indexes the committed state.
	_, err := runSeekInRepo(t, dir, "version_one_aaa")
	if err != nil {
		t.Fatal(err)
	}

	// The first edit, with no commit.
	if err := os.WriteFile(filepath.Join(dir, "counter.go"), []byte(`package main
// version_two_bbb
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The second search must find version_two_bbb.
	files, err := runSeekInRepo(t, dir, "version_two_bbb")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("FRESHNESS VIOLATION: second search after edit did not find updated content")
	}

	// The second edit, with no commit.
	if err := os.WriteFile(filepath.Join(dir, "counter.go"), []byte(`package main
// version_three_ccc
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The third search must find version_three_ccc.
	files, err = runSeekInRepo(t, dir, "version_three_ccc")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("FRESHNESS VIOLATION: third search after edit did not find updated content")
	}

	// The overwritten content has no result.
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

	// The unchanged file still has a result.
	files, err := runSeekInRepo(t, dir, "stable_content_111")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("expected stable file to remain searchable")
	}

	// The changed file has a result for its new content.
	files, err = runSeekInRepo(t, dir, "changed_content_333")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("expected changed file to be searchable")
	}
}

// TestIntegration_Worktree_CommittedContent searches a linked worktree through
// the root command and checks that its committed content has a result.
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

// TestIntegration_Worktree_DirtyFile changes a committed file in a linked
// worktree and checks that a search finds the uncommitted content.
func TestIntegration_Worktree_DirtyFile(t *testing.T) {
	requireTools(t)

	_, worktreeDir := initGitWorktree(t, "wt_dirty.go", `package main
// worktree_clean_marker_fff
`)

	// Change the file with no commit.
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

// TestRun_NestedRepoVenvNotLeakedToParent searches a parent folder twice. The
// folder holds a nested Git repository whose .gitignore ignores .venv. The
// first search builds the cache; the parent then changes, so the second search
// scans the folder again. The second search must keep the repository boundary:
// the .venv file of the nested repository has no result.
func TestRun_NestedRepoVenvNotLeakedToParent(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested-repo")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// A real Git repository, so that detectGitBoundary confirms the boundary.
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

	// Add ignored content that would match if the parent walker entered the
	// nested repository.
	venvDir := filepath.Join(nested, ".venv")
	if err := os.MkdirAll(venvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = "zzz_venv_leak_marker_zzz"
	if err := os.WriteFile(filepath.Join(venvDir, "leaked.py"), []byte("# "+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Keep one selected file in the parent corpus so the second search scans it
	// again instead of using an empty-corpus marker.
	if err := os.WriteFile(filepath.Join(parent, "parent-marker.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setTestUserCache(t)
	t.Chdir(parent)

	// First run: cold cache. The graph expansion finds the nested repository,
	// and the parent folder walk skips its subtree.
	if _, err := seek(t, "--lexical-only", marker, parent); err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("first run: %v", err)
	}

	// Change parent content so the second fingerprint differs from the cached
	// value and the search performs a new state scan.
	if err := os.WriteFile(filepath.Join(parent, "parent-marker.txt"), []byte("seed\nedit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The second scan, with a warm cache, must keep the nested repository
	// boundary. It must not index .venv as parent-folder content.
	out, err := seek(t, "--lexical-only", marker, parent)
	if err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("second run: %v", err)
	}
	if strings.Contains(out, marker) {
		t.Fatalf("nested .venv content leaked into parent folder corpus on second search; got output containing %q:\n%s", marker, out)
	}
}

// TestRun_TrackedAndIgnoredFileOperandsBothSearchedOnce passes a tracked file
// and an ignored file of one repository together. The Git corpus searches the
// tracked file and a folder corpus searches the ignored file. Each file prints
// once.
func TestRun_TrackedAndIgnoredFileOperandsBothSearchedOnce(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "tracked.go", "package main\n// splitneedle tracked\n")
	writeIgnoredFile(t, repo, "secret.txt", "splitneedle ignored\n")
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := seek(t, "--lexical-only", "splitneedle", filepath.Join(repo, "tracked.go"), filepath.Join(repo, "secret.txt"))
	if err != nil {
		t.Fatalf("run tracked+ignored split: %v", err)
	}
	if !strings.Contains(out, "splitneedle tracked") {
		t.Fatalf("tracked file content missing (Git corpus), got:\n%s", out)
	}
	if !strings.Contains(out, "splitneedle ignored") {
		t.Fatalf("ignored file content missing (folder fallback), got:\n%s", out)
	}
	// Each header is the file operand as typed. The file names appear only in
	// headers, not in the content, so one occurrence each shows that no two
	// corpora search one file.
	if n := strings.Count(out, "tracked.go"); n != 1 {
		t.Fatalf("tracked.go should appear once, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "secret.txt"); n != 1 {
		t.Fatalf("secret.txt should appear once, got %d:\n%s", n, out)
	}
}

// TestRun_CaseMismatchedFileOperandStillFound passes a file operand in another
// letter case. On a case-insensitive file system, the Git corpus scope uses
// the name on disk and finds the file. On a case-sensitive file system, the
// operand names no file.
func TestRun_CaseMismatchedFileOperandStillFound(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "readme.md", "MARKER_CASE_FIX\n")
	setTestUserCache(t)
	t.Chdir(repo)
	mistyped := filepath.Join(repo, "README.md")
	if _, err := os.Stat(mistyped); err != nil {
		requireMissingOperand(t, mistyped)
		return
	}

	out, err := seek(t, "--lexical-only", "MARKER_CASE_FIX", mistyped)
	if err != nil {
		t.Fatalf("case-mismatched file operand should be found through the Git corpus: %v", err)
	}
	if !strings.Contains(out, "MARKER_CASE_FIX") {
		t.Fatalf("expected content via case-corrected git scope, got:\n%s", out)
	}
}

// TestRun_CurrentGitUntrackedFileOperandSearchesContent passes an untracked
// file (Git status `??`) as an operand: Seek searches it through the Git dirty
// layer and tags it [uncommitted]. A modified tracked file (` M`) is the case
// of TestRun_CurrentGitExactDirtyFileOperandUsesGitDirtyLayer.
func TestRun_CurrentGitUntrackedFileOperandSearchesContent(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "committed.go", "package main\n")
	fresh := writeUntrackedFile(t, repo, "fresh.go", "package main\n// UNTRACKED_NEW_MARKER\n")
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := seek(t, "--lexical-only", "UNTRACKED_NEW_MARKER", fresh)
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

// TestRun_LeadingColonIgnoredFileOperandSearchesContent passes an ignored file
// whose name starts with ':'. A folder corpus searches it and finds its
// content.
func TestRun_LeadingColonIgnoredFileOperandSearchesContent(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "a.go", "package main\n")
	colon := writeIgnoredFile(t, repo, ":weird", "COLON_IGNORED_MARKER\n")
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := seek(t, "--lexical-only", "COLON_IGNORED_MARKER", colon)
	if err != nil {
		t.Fatalf("leading-colon ignored file operand should be searched via folder fallback: %v", err)
	}
	if !strings.Contains(out, "COLON_IGNORED_MARKER") {
		t.Fatalf("expected colon-named ignored file content via folder fallback, got:\n%s", out)
	}
}

// TestRun_SymlinkToIgnoredFileSearchesContent passes a symlink to an ignored
// file of the worktree as an operand. Seek searches the target through a
// folder corpus and finds its content.
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

	out, err := seek(t, "--lexical-only", "SYMLINK_IGNORED_MARKER", link)
	if err != nil {
		t.Fatalf("symlink to an ignored file should search content via folder fallback: %v", err)
	}
	if !strings.Contains(out, "SYMLINK_IGNORED_MARKER") {
		t.Fatalf("expected resolved ignored-target content via folder fallback, got:\n%s", out)
	}
}

// TestRun_CleanScopedSearchUsesTheCombinedIndex checks that a scoped search of
// a clean tree makes one corpus directory, the index of the whole repository.
// The result prints with no [uncommitted] tag.
func TestRun_CleanScopedSearchUsesTheCombinedIndex(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "committed.go", "package main\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "x.go"), "package sub\n// CLEANSCOPE_MARKER\n")
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := seek(t, "--lexical-only", "CLEANSCOPE_MARKER", filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatalf("clean scoped search: %v", err)
	}
	if !strings.Contains(out, "CLEANSCOPE_MARKER") {
		t.Fatalf("expected committed content, got:\n%s", out)
	}
	if strings.Contains(out, "[uncommitted]") {
		t.Fatalf("clean scope must not be tagged uncommitted, got:\n%s", out)
	}
	if n := countCorpora(t); n != 1 {
		t.Fatalf("clean scoped search must make 1 corpus (the combined whole-repo index), got %d", n)
	}
}

// TestRun_DirtyScopedSearchReusesCombinedIndex checks that a scoped search
// with a dirty file in the scope uses one corpus directory, the index of the
// whole repository. The result prints with the [uncommitted] tag.
func TestRun_DirtyScopedSearchReusesCombinedIndex(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "committed.go", "package main\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "x.go"), "package sub\n")
	if err := os.WriteFile(filepath.Join(repo, "sub", "x.go"), []byte("package sub\n// DIRTYSCOPE_MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := seek(t, "--lexical-only", "DIRTYSCOPE_MARKER", filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatalf("dirty scoped search: %v", err)
	}
	if !strings.Contains(out, "[uncommitted]") {
		t.Fatalf("dirty scope must tag uncommitted, got:\n%s", out)
	}
	if n := countCorpora(t); n != 1 {
		t.Fatalf("dirty scoped search must reuse 1 combined corpus, got %d", n)
	}
}

// TestRun_ScopedSearchesShareCommittedIndex runs clean scoped searches of two
// subtrees of one repository. Both use one corpus directory, the index of the
// whole repository, and neither prints a file of the other subtree.
func TestRun_ScopedSearchesShareCommittedIndex(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "root.go", "package main\n")
	writeTrackedFile(t, repo, filepath.Join("aaa", "x.go"), "package aaa\n// SHARED_MARKER\n")
	writeTrackedFile(t, repo, filepath.Join("bbb", "y.go"), "package bbb\n// SHARED_MARKER\n")
	setTestUserCache(t)
	t.Chdir(repo)

	sibling := map[string]string{"aaa": "bbb", "bbb": "aaa"}
	for _, dir := range []string{"aaa", "bbb"} {
		out, err := seek(t, "--lexical-only", "SHARED_MARKER", filepath.Join(repo, dir))
		if err != nil {
			t.Fatalf("scoped search %s: %v", dir, err)
		}
		if !strings.Contains(out, dir+"/") {
			t.Fatalf("expected in-scope %s match, got:\n%s", dir, out)
		}
		if strings.Contains(out, sibling[dir]+"/") {
			t.Fatalf("sibling %s leaked into %s scope:\n%s", sibling[dir], dir, out)
		}
	}
	if n := countCorpora(t); n != 1 {
		t.Fatalf("two clean scoped searches of one repo must share 1 corpus, got %d", n)
	}
}

// TestScoped_EqualsUnscopedIntersectScope checks that a scoped result set is
// the unscoped result set limited to that scope. For six query forms (a word,
// content:, lang:, file:, type:file, and sym:), it compares the scoped and the
// unscoped result files with expected lists. The fixture holds committed,
// modified, deleted, untracked, and renamed files, plus a matching sibling
// outside the scope. A scoped header starts with the operand as typed, and an
// unscoped header is repo-relative. So the test compares basenames, which are
// unique in the fixture.
func TestScoped_EqualsUnscopedIntersectScope(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "root.go", "package main\n")
	// committed, in-scope
	writeTrackedFile(t, repo, filepath.Join("sub", "keep.go"), "package sub\nfunc Foo() {} // QTERM\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "also.go"), "package sub\n// QTERM also\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "gone.go"), "package sub\n// QTERM gone\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "old.go"), "package sub\n// QTERM old\n")
	// committed, out-of-scope sibling that also matches: it shows a scope leak
	writeTrackedFile(t, repo, filepath.Join("other", "sib.go"), "package other\n// QTERM sibling\n")

	// Working-tree mutations (uncommitted): modify keep.go (still matches),
	// delete gone.go, rename old.go->renamed.go, add untracked new.go.
	if err := os.WriteFile(filepath.Join(repo, "sub", "keep.go"),
		[]byte("package sub\nfunc Foo() {} // QTERM modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "rm", filepath.Join("sub", "gone.go"))
	gitRunIn(t, repo, "mv", filepath.Join("sub", "old.go"), filepath.Join("sub", "renamed.go"))
	writeUntrackedFile(t, repo, filepath.Join("sub", "new.go"), "package sub\n// QTERM new\n")

	setTestUserCache(t)
	t.Chdir(repo)
	basenames := func(out string) map[string]bool {
		set := map[string]bool{}
		for _, path := range headerPaths(out) {
			set[filepath.Base(path)] = true
		}
		return set
	}

	cases := []struct {
		name           string
		query          string
		scopedHave     []string
		scopedAbsent   []string
		unscopedHave   []string
		unscopedAbsent []string
	}{
		{
			name:           "plain",
			query:          "QTERM",
			scopedHave:     []string{"keep.go", "also.go", "renamed.go", "new.go"},
			scopedAbsent:   []string{"gone.go", "old.go", "sib.go", "root.go"},
			unscopedHave:   []string{"keep.go", "also.go", "renamed.go", "new.go", "sib.go"},
			unscopedAbsent: []string{"gone.go", "old.go"},
		},
		{
			name:           "regex",
			query:          "content:Q.*M",
			scopedHave:     []string{"keep.go", "also.go", "renamed.go", "new.go"},
			scopedAbsent:   []string{"gone.go", "old.go", "sib.go", "root.go"},
			unscopedHave:   []string{"keep.go", "also.go", "renamed.go", "new.go", "sib.go"},
			unscopedAbsent: []string{"gone.go", "old.go"},
		},
		{
			name:           "lang",
			query:          "QTERM lang:go",
			scopedHave:     []string{"keep.go", "also.go", "renamed.go", "new.go"},
			scopedAbsent:   []string{"gone.go", "old.go", "sib.go", "root.go"},
			unscopedHave:   []string{"keep.go", "also.go", "renamed.go", "new.go", "sib.go"},
			unscopedAbsent: []string{"gone.go", "old.go"},
		},
		{
			name:           "file",
			query:          `QTERM file:keep\.go`,
			scopedHave:     []string{"keep.go"},
			scopedAbsent:   []string{"also.go", "gone.go", "old.go", "renamed.go", "new.go", "sib.go", "root.go"},
			unscopedHave:   []string{"keep.go"},
			unscopedAbsent: []string{"also.go", "gone.go", "old.go", "renamed.go", "new.go", "sib.go", "root.go"},
		},
		{
			name:           "type-file",
			query:          "type:file keep",
			scopedHave:     []string{"keep.go"},
			scopedAbsent:   []string{"also.go", "gone.go", "old.go", "renamed.go", "new.go", "sib.go", "root.go"},
			unscopedHave:   []string{"keep.go"},
			unscopedAbsent: []string{"also.go", "gone.go", "old.go", "renamed.go", "new.go", "sib.go", "root.go"},
		},
		{
			name:           "symbol",
			query:          "sym:Foo",
			scopedHave:     []string{"keep.go"},
			scopedAbsent:   []string{"also.go", "gone.go", "old.go", "renamed.go", "new.go", "sib.go"},
			unscopedHave:   []string{"keep.go"},
			unscopedAbsent: []string{"also.go", "sib.go"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scopedOut, err := seek(t, "--lexical-only", tc.query, filepath.Join(repo, "sub"))
			if err != nil {
				t.Fatalf("scoped search %q: %v", tc.query, err)
			}
			unscopedOut, err := seek(t, "--lexical-only", tc.query)
			if err != nil {
				t.Fatalf("unscoped search %q: %v", tc.query, err)
			}
			scoped := basenames(scopedOut)
			unscoped := basenames(unscopedOut)
			for _, f := range tc.scopedHave {
				if !scoped[f] {
					t.Errorf("query %q: scoped missing in-scope %q\n%s", tc.query, f, scopedOut)
				}
			}
			for _, f := range tc.scopedAbsent {
				if scoped[f] {
					t.Errorf("query %q: scoped leaked %q\n%s", tc.query, f, scopedOut)
				}
			}
			for _, f := range tc.unscopedHave {
				if !unscoped[f] {
					t.Errorf("query %q: unscoped missing %q\n%s", tc.query, f, unscopedOut)
				}
			}
			for _, f := range tc.unscopedAbsent {
				if unscoped[f] {
					t.Errorf("query %q: unscoped showed shadowed/deleted %q\n%s", tc.query, f, unscopedOut)
				}
			}
		})
	}
}

// TestScoped_RankingEqualsUnscopedIntersectScope checks that the scoped order
// equals the unscoped order limited to the scope: both searches read the same
// shards. It compares the base names of the results in print order.
func TestScoped_RankingEqualsUnscopedIntersectScope(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "root.go", "package main\n")
	// The files in the scope differ in term count and length, so their BM25
	// scores differ.
	writeTrackedFile(t, repo, filepath.Join("sub", "many.go"), "package sub\n// RANKQ RANKQ RANKQ RANKQ\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "few.go"), "package sub\n"+strings.Repeat("// filler line\n", 40)+"// RANKQ\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "mid.go"), "package sub\n// RANKQ RANKQ\n")
	// A file out of the scope that also matches: a scope leak adds it to the
	// scoped order.
	writeTrackedFile(t, repo, filepath.Join("other", "sib.go"), "package other\n// RANKQ RANKQ RANKQ\n")

	setTestUserCache(t)
	t.Chdir(repo)

	scopedOut, err := seek(t, "--lexical-only", "RANKQ", filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	unscopedOut, err := seek(t, "--lexical-only", "RANKQ")
	if err != nil {
		t.Fatalf("unscoped search: %v", err)
	}

	resultOrder := func(out string) []string {
		var order []string
		for _, result := range printedResults(out) {
			order = append(order, filepath.Base(result.path))
		}
		return order
	}
	scopedOrder := resultOrder(scopedOut)
	inScope := map[string]bool{"many.go": true, "few.go": true, "mid.go": true}
	var unscopedInScope []string
	for _, f := range resultOrder(unscopedOut) {
		if inScope[f] {
			unscopedInScope = append(unscopedInScope, f)
		}
	}
	if len(scopedOrder) < 3 {
		t.Fatalf("expected >=3 in-scope ranked hits, got %v\n%s", scopedOrder, scopedOut)
	}
	if strings.Join(scopedOrder, ",") != strings.Join(unscopedInScope, ",") {
		t.Fatalf("scoped ranking %v != unscoped∩scope ranking %v", scopedOrder, unscopedInScope)
	}
}

// TestScoped_FileOperandRegexpEqualsFileNameSet checks that both scope filters
// of gitScopeSpec.query keep the file operands and drop their siblings. Up to
// maxRegexpFileOperands file operands become anchored file-name regexps. One
// operand more makes the scope a FileNameSet.
func TestScoped_FileOperandRegexpEqualsFileNameSet(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "root.go", "package main\n")
	writeFileAt(t, repo, filepath.Join("sub", "keep.go"), "package sub\n// FILEQ keep\n")
	writeFileAt(t, repo, filepath.Join("sub", "other.go"), "package sub\n// FILEQ other\n")
	// The pad files do not hold FILEQ: as operands, they only raise the count.
	pads := make([]string, maxRegexpFileOperands)
	for i := range pads {
		name := filepath.Join("sub", "pad"+strconv.Itoa(i)+".go")
		writeFileAt(t, repo, name, "package sub\n")
		pads[i] = filepath.Join(repo, name)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-m", "add scoped files")
	setTestUserCache(t)
	t.Chdir(repo)

	target := filepath.Join(repo, "sub", "keep.go")
	for _, tc := range []struct {
		name     string
		operands []string
	}{
		{name: "regexp", operands: []string{target}},
		{name: "file_name_set", operands: append([]string{target}, pads...)},
	} {
		out, err := seek(t, slices.Concat([]string{"--lexical-only", "FILEQ"}, tc.operands)...)
		if err != nil {
			t.Fatalf("%s: search %d file operands: %v", tc.name, len(tc.operands), err)
		}
		if paths := headerPaths(out); !slices.Equal(paths, []string{target}) {
			t.Fatalf("%s: result paths=%q, want only %q:\n%s", tc.name, paths, target, out)
		}
	}
}

// TestRace_ConcurrentScopedUnscopedSameRepo runs four scoped and four unscoped
// searches of one repository at the same time from a cold cache. Each search
// must succeed, the cache must hold one corpus (the combined index), and a
// later search must find the marker in it.
func TestRace_ConcurrentScopedUnscopedSameRepo(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "root.go", "package main\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "x.go"), "package sub\n// CONCURRENT_MARKER\n")
	setTestUserCache(t)
	t.Chdir(repo)

	// The searches write to os.Stdout. captureStdout changes the process
	// stdout, so concurrent searches cannot use it: the test sends stdout to
	// the null device once.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = devnull.Close()
	})

	var wg sync.WaitGroup
	type concurrentRunResult struct {
		scoped bool
		err    error
	}
	runResults := make(chan concurrentRunResult, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		scoped := i%2 == 0
		go func(scoped bool) {
			defer wg.Done()
			var operands []string
			if scoped {
				operands = []string{filepath.Join(repo, "sub")}
			}
			runResults <- concurrentRunResult{
				scoped: scoped,
				err:    seekRoot(context.Background(), t, nil, slices.Concat([]string{"--lexical-only", "CONCURRENT_MARKER"}, operands)...),
			}
		}(scoped)
	}
	wg.Wait()
	close(runResults)
	for result := range runResults {
		if result.err != nil {
			t.Errorf("scoped=%v concurrent search: %v", result.scoped, result.err)
		}
	}
	if t.Failed() {
		return
	}

	if n := countCorpora(t); n != 1 {
		t.Fatalf("concurrent scoped+unscoped on one repo must share 1 combined corpus, got %d", n)
	}
	files, err := runSeekInRepo(t, repo, "CONCURRENT_MARKER")
	if err != nil || len(files) == 0 {
		t.Fatalf("search published combined index: files=%v error=%v", files, err)
	}
}

// TestRun_SharedCommittedIndexUpdatesOnHeadChange checks that a new commit
// updates the index of a scoped search, with a delta or a full build. An index
// that is not updated does not hold the new content.
func TestRun_SharedCommittedIndexUpdatesOnHeadChange(t *testing.T) {
	requireTools(t)

	repo := initGitRepo(t, "root.go", "package main\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "x.go"), "package sub\n// SHARED_V1MARKER\n")
	setTestUserCache(t)
	t.Chdir(repo)

	out, err := seek(t, "--lexical-only", "SHARED_V1MARKER", filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatalf("v1 scoped search: %v", err)
	}
	if !strings.Contains(out, "SHARED_V1MARKER") {
		t.Fatalf("expected v1 committed content, got:\n%s", out)
	}

	// Replace the in-scope content in a new commit (new HEAD).
	if err := os.WriteFile(filepath.Join(repo, "sub", "x.go"), []byte("package sub\n// SHARED_V2MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "sub/x.go")
	gitRunIn(t, repo, "commit", "-m", "v2")

	out2, err := seek(t, "--lexical-only", "SHARED_V2MARKER", filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatalf("v2 scoped search: %v", err)
	}
	if !strings.Contains(out2, "SHARED_V2MARKER") {
		t.Fatalf("the index did not update on the HEAD change, got:\n%s", out2)
	}
}

// countCorpora returns the number of corpus directories in the user cache.
func countCorpora(tb testing.TB) int {
	tb.Helper()
	root, err := seekUserCacheRoot()
	if err != nil {
		tb.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, corporaDir))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		tb.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}
