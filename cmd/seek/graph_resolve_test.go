package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveOperands checks the seed that each operand form resolves to: a
// Git root, a folder root, or a Git directory error.
func TestResolveOperands(t *testing.T) {
	requireGit(t)
	f := newGraphFixture(t)

	// A symlinked path into another repository resolves to the target.
	repoY := f.GitRepo("repoY", "y.txt", "y\n")
	writeFixtureFile(t, filepath.Join(repoY, "sub", "y.txt"), "y\n")
	gitRunIn(t, repoY, "add", ".")
	gitRunIn(t, repoY, "commit", "-m", "add sub")
	repoX := f.GitRepo("repoX", "x.txt", "x\n")
	link := filepath.Join(repoX, "link")
	if err := os.Symlink(filepath.Join(repoY, "sub"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// A symlinked .git: Git follows it, so the repository owns its files.
	store := f.path("store")
	symgit := f.path("outer/symgit")
	for _, dir := range []string{store, filepath.Dir(symgit)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, f.root, "init", "--quiet", "--separate-git-dir="+filepath.Join(store, "x.git"), symgit)
	if err := os.Remove(filepath.Join(symgit, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, "x.git"), filepath.Join(symgit, ".git")); err != nil {
		t.Fatal(err)
	}

	// A separate Git directory outside the walk root follows Git rules.
	separate := f.path("separate")
	gitRunIn(t, f.root, "init", "--quiet", "--separate-git-dir="+f.path("sep.git"), separate)

	// A linked worktree inside a bare repository is a work tree.
	bareClone := f.path("clone.git")
	gitRunIn(t, f.root, "clone", "--quiet", "--bare", repoX, bareClone)
	inBare := filepath.Join(bareClone, "main")
	gitRunIn(t, bareClone, "worktree", "add", "--quiet", inBare)

	// A symlinked current directory resolves to its target.
	cwdLink := f.path("cwd-link")
	if err := os.Symlink(filepath.Join(repoY, "sub"), cwdLink); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		operand  string
		cwd      string
		wantRepo string
	}{
		{name: "symlinked path into another repo", operand: link, wantRepo: repoY},
		{name: "symlinked .git", operand: symgit, wantRepo: symgit},
		{name: "separate git dir", operand: separate, wantRepo: separate},
		{name: "worktree inside a bare repo", operand: inBare, wantRepo: inBare},
		{name: "file in a worktree inside a bare repo", operand: filepath.Join(inBare, "x.txt"), wantRepo: inBare},
		{name: "symlinked current directory", operand: ".", cwd: cwdLink, wantRepo: repoY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cwd != "" {
				t.Chdir(tc.cwd)
			}
			planned, err := resolveOperands(context.Background(), []string{tc.operand})
			if err != nil {
				t.Fatalf("resolveOperands: %v", err)
			}
			if len(planned.gitRoots) != 1 || len(planned.roots) != 0 {
				t.Fatalf("planned %d Git roots and %d folder roots, want one Git root", len(planned.gitRoots), len(planned.roots))
			}
			if got, want := canonicalCorpusPath(planned.gitRoots[0].paths.RepoDir), canonicalCorpusPath(tc.wantRepo); got != want {
				t.Fatalf("repo=%q, want %q", got, want)
			}
		})
	}

	// A folder that only looks like a Git directory is a folder.
	fake := f.path("fake")
	writeFixtureFile(t, filepath.Join(fake, "HEAD"), "not a ref\n")
	for _, dir := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(fake, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if planned, err := resolveOperands(context.Background(), []string{fake}); err != nil || len(planned.roots) != 1 {
		t.Fatalf("fake Git directory: planned=%+v err=%v, want one folder root", planned, err)
	}

	bare := f.path("bare.git")
	gitRunIn(t, f.root, "init", "--quiet", "--bare", bare)
	gitDirOperands := []string{
		filepath.Join(repoX, ".git"),
		filepath.Join(repoX, ".git", "refs"),
		bare,
		filepath.Join(bare, "refs"),
	}
	if _, err := os.Stat(filepath.Join(repoX, ".GIT")); err == nil {
		// A case-insensitive file system maps ".GIT" to the Git directory.
		gitDirOperands = append(gitDirOperands, filepath.Join(repoX, ".GIT"))
	}
	for _, operand := range gitDirOperands {
		if _, err := resolveOperands(context.Background(), []string{operand}); !errors.Is(err, errGitDirectoryOperand) {
			t.Fatalf("operand %q: error=%v, want errGitDirectoryOperand", operand, err)
		}
	}
}

// TestFailureClasses checks the error text and exit code of each operand
// failure class.
func TestFailureClasses(t *testing.T) {
	requireGit(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "a\n")
	missing := f.path("missing")
	gitDir := filepath.Join(repo, ".git")
	bare := f.path("bare.git")
	gitRunIn(t, f.root, "init", "--quiet", "--bare", bare)
	for _, tc := range []struct {
		name    string
		operand string
		want    string
	}{
		{name: "missing operand", operand: missing, want: `seek: path does not exist: "` + missing + `"`},
		{name: "Git directory operand", operand: gitDir, want: `seek: cannot search a Git directory: "` + gitDir + `"`},
		{name: "bare repository operand", operand: bare, want: `seek: cannot search a Git directory: "` + bare + `"`},
		{name: "bare repository operand, dubious owner", operand: bare, want: `seek: cannot search a Git directory: "` + bare + `"`},
		{name: "bare repository operand, unreadable objects", operand: bare, want: `seek: cannot search a Git directory: "` + bare + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.name, "unreadable objects") {
				// Git says "not a git repository"; the file-system rule still
				// rejects it, so its config is never shown.
				if os.Geteuid() == 0 {
					t.Skip("root reads a mode-000 directory")
				}
				objects := filepath.Join(bare, "objects")
				if err := os.Chmod(objects, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(objects, 0o755) })
			}
			if strings.Contains(tc.name, "dubious owner") {
				// Git refuses the repository; it stays a Git directory.
				t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
				if err := exec.Command("git", "-C", bare, "rev-parse").Run(); err == nil {
					t.Skip("the installed Git ignores GIT_TEST_ASSUME_DIFFERENT_OWNER")
				}
			}
			setTestUserCache(t)
			err := run(context.Background(), "a", []string{tc.operand})
			if err == nil {
				t.Fatal("search succeeded, want an error")
			}
			if got := formatCLIError(err); got != tc.want {
				t.Fatalf("message=%q, want %q", got, tc.want)
			}
			if code := exitCodeForError(err); code != 2 {
				t.Fatalf("exit code=%d, want 2", code)
			}
		})
	}

	// Node rows: a node that fails is skipped with one warning;
	// the other nodes answer; the exit is 0 on matches and 1 on none, never 2.
	const nodeMarker = "zqnodefailuremarker"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *graphFixture) (operands []string, failed string)
		found []string
		// cause, when set, is a part of the warning's cause.
		cause string
	}{
		{
			// The start operand exists but cannot be read.
			name: "cwd mode 000",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				if os.Geteuid() == 0 {
					t.Skip("root reads a mode-000 directory")
				}
				locked := f.path("locked")
				f.graphFile("locked/a.txt", nodeMarker)
				t.Chdir(locked)
				if err := os.Chmod(locked, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
				return []string{"."}, "."
			},
		},
		{
			// An absolute operand that cannot be read fails without a Git
			// process; the warning names the canonical path.
			name: "operand mode 000",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				if os.Geteuid() == 0 {
					t.Skip("root reads a mode-000 directory")
				}
				locked := f.path("locked")
				f.graphFile("locked/a.txt", nodeMarker)
				f.graphFile("plain/b.txt", nodeMarker)
				if err := os.Chmod(locked, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
				return []string{locked, f.path("plain")}, canonicalCorpusPath(locked)
			},
			found: []string{"plain/b.txt"},
			cause: "cannot read path",
		},
		{
			// A folder operand that cannot be listed fails its node.
			name: "folder operand mode 0311",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				if os.Geteuid() == 0 {
					t.Skip("root lists a mode-0311 directory")
				}
				noRead := f.path("noread")
				f.graphFile("noread/a.txt", nodeMarker)
				f.graphFile("plain/b.txt", nodeMarker)
				if err := os.Chmod(noRead, 0o311); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(noRead, 0o755) })
				return []string{noRead, f.path("plain")}, noRead
			},
			found: []string{"plain/b.txt"},
		},
		{
			// A file operand that cannot be read fails its node.
			name: "file operand mode 000",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				if os.Geteuid() == 0 {
					t.Skip("root reads a mode-000 file")
				}
				locked := f.path("d/locked.txt")
				f.graphFile("d/locked.txt", nodeMarker)
				f.graphFile("plain/b.txt", nodeMarker)
				if err := os.Chmod(locked, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
				return []string{locked, f.path("plain")}, locked
			},
			found: []string{"plain/b.txt"},
		},
		{
			// Git refuses the repository; no folder content shows.
			name: "dubious owner",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				repo := f.GitRepo("owned", ".gitignore", "secret.env\n")
				writeFixtureFile(t, filepath.Join(repo, "secret.env"), nodeMarker)
				t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
				if err := exec.Command("git", "-C", repo, "status").Run(); err == nil {
					t.Skip("the installed Git ignores GIT_TEST_ASSUME_DIFFERENT_OWNER")
				}
				return []string{repo}, repo
			},
		},
		{
			// Git is missing: each Git node fails; folder nodes still run.
			name: "Git missing with an explicit operand",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				repo := f.GitRepo("repo-no-git", "a.txt", nodeMarker)
				f.graphFile("plain/b.txt", nodeMarker)
				t.Setenv("PATH", t.TempDir())
				return []string{repo, f.path("plain")}, repo
			},
			found: []string{"plain/b.txt"},
		},
		{
			// A Git failure for a subfolder operand fails the node; it is
			// never a folder search that shows Git-ignored files.
			name: "subfolder operand, corrupt index",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				repo := f.GitRepo("broken", ".gitignore", "secret.env\n")
				writeFixtureFile(t, filepath.Join(repo, "sub", "secret.env"), nodeMarker)
				writeFixtureFile(t, filepath.Join(repo, ".git", "index"), "not an index\n")
				f.graphFile("plain/c.txt", nodeMarker)
				return []string{filepath.Join(repo, "sub"), f.path("plain")}, repo
			},
			found: []string{"plain/c.txt"},
		},
		{
			// An operand whose `.git` file points to another work tree's Git
			// directory fails the node; Git never refreshes that repository's
			// index.
			name: "operand with a .git file of another work tree",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				victim := f.GitRepo("victim", "a.txt", nodeMarker)
				index := filepath.Join(victim, ".git", "index")
				before, err := os.ReadFile(index)
				if err != nil {
					t.Fatal(err)
				}
				attacker := f.path("att/x")
				writeFixtureFile(t, filepath.Join(attacker, ".git"), "gitdir: "+filepath.Join(victim, ".git")+"\n")
				t.Cleanup(func() {
					if after, err := os.ReadFile(index); err != nil || !bytes.Equal(after, before) {
						t.Errorf("the search rewrote the other repository's index (err=%v)", err)
					}
				})
				return []string{attacker}, attacker
			},
		},
		{
			name: "subfolder operand, dubious owner",
			setup: func(t *testing.T, f *graphFixture) ([]string, string) {
				repo := f.GitRepo("owned", ".gitignore", "secret.env\n")
				writeFixtureFile(t, filepath.Join(repo, "sub", "secret.env"), nodeMarker)
				t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
				if err := exec.Command("git", "-C", repo, "status").Run(); err == nil {
					t.Skip("the installed Git ignores GIT_TEST_ASSUME_DIFFERENT_OWNER")
				}
				return []string{filepath.Join(repo, "sub")}, repo
			},
		},
	} {
		t.Run("node row, "+tc.name, func(t *testing.T) {
			requireTools(t)
			f := newGraphFixture(t)
			operands, failed := tc.setup(t, f)
			setTestUserCache(t)
			logs := captureTestLogs(t, slog.LevelWarn)
			out, err := captureStdout(t, func() error {
				return run(context.Background(), nodeMarker, operands)
			})
			wantCode := 1
			if len(tc.found) > 0 {
				wantCode = 0
			}
			if code := exitCodeForError(err); code != wantCode {
				t.Fatalf("exit=%d err=%v, want %d", code, err, wantCode)
			}
			for _, rel := range tc.found {
				if !strings.Contains(out, rel) {
					t.Fatalf("output lacks %s:\n%s", rel, out)
				}
			}
			if strings.Contains(out, "secret.env") || strings.Contains(out, "a.txt") || strings.Contains(out, "locked.txt") {
				t.Fatalf("the failed node showed content:\n%s", out)
			}
			records := logs.Records()
			if len(records) != 1 || !strings.Contains(records[0].Message, "Skipped a search node") || !logsMention(records, failed) {
				t.Fatalf("warnings=%v, want one naming %s", records, failed)
			}
			if tc.cause != "" && !logsMention(records, tc.cause) {
				t.Fatalf("warnings=%v, want the cause %q", records, tc.cause)
			}
		})
	}

	// Environment rows: an error that every node would meet ends the
	// command with exit 2 and one error, not one warning per node.
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *graphFixture)
		want  string
	}{
		{name: "cache directory cannot be written", setup: func(t *testing.T, f *graphFixture) {
			if os.Geteuid() == 0 {
				t.Skip("root writes a mode-555 directory")
			}
			cache := f.path("ro-cache")
			if err := os.MkdirAll(cache, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SEEK_CACHE_DIR", cache)
			if err := os.Chmod(cache, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(cache, 0o755) })
		}, want: "cache directory"},
		{name: "no cache location", setup: func(t *testing.T, f *graphFixture) {
			t.Setenv("SEEK_CACHE_DIR", "")
			t.Setenv("HOME", "")
			t.Setenv("XDG_CACHE_HOME", "")
		}, want: "resolve user cache directory"},
	} {
		t.Run("environment row, "+tc.name, func(t *testing.T) {
			requireTools(t)
			f := newGraphFixture(t)
			f.graphFile("one/a.txt", nodeMarker)
			repo := f.graphRepo("two", "b.txt", nodeMarker)
			setTestUserCache(t)
			tc.setup(t, f)
			logs := captureTestLogs(t, slog.LevelWarn)
			err := run(context.Background(), nodeMarker, []string{f.path("one"), repo})
			if exitCodeForError(err) != 2 || !strings.Contains(formatCLIError(err), tc.want) {
				t.Fatalf("err=%v, want exit 2 with %q", err, tc.want)
			}
			if records := logs.Records(); len(records) != 0 {
				t.Fatalf("warnings=%v, want none", records)
			}
		})
	}

	// Boundary rows: a `.git` entry below a folder root. Git cannot use it: one
	// warning, and the folder never indexes that directory. A stray `.git`
	// entry is no boundary: its siblings are found, the entry itself is not.
	const marker = "zqfailureboundarymarker"
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, f *graphFixture, project string)
		warn   bool
		absent string
	}{
		{
			name: "unreadable .git",
			setup: func(t *testing.T, f *graphFixture, project string) {
				if os.Geteuid() == 0 {
					t.Skip("root reads a mode-000 directory")
				}
				gitRunIn(t, f.root, "init", "-q", project)
				gitDir := filepath.Join(project, ".git")
				if err := os.Chmod(gitDir, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })
			},
			warn:   true,
			absent: "folder/project/node_modules/dep.js",
		},
		{
			name: "dubious owner",
			setup: func(t *testing.T, f *graphFixture, project string) {
				gitRunIn(t, f.root, "init", "-q", project)
				t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
				if err := exec.Command("git", "-C", project, "status").Run(); err == nil {
					t.Skip("the installed Git ignores GIT_TEST_ASSUME_DIFFERENT_OWNER")
				}
			},
			warn:   true,
			absent: "folder/project/node_modules/dep.js",
		},
		{
			name: "broken gitdir: line",
			setup: func(t *testing.T, f *graphFixture, project string) {
				writeFixtureFile(t, filepath.Join(project, ".git"), "gitdir: "+f.path("nonexistent/git")+"\n")
			},
			warn:   true,
			absent: "folder/project/node_modules/dep.js",
		},
		{
			name: "corrupt index",
			setup: func(t *testing.T, f *graphFixture, project string) {
				gitRunIn(t, f.root, "init", "-q", project)
				writeFixtureFile(t, filepath.Join(project, ".git", "index"), "not an index\n")
			},
			warn:   true,
			absent: "folder/project/node_modules/dep.js",
		},
		{
			name: "stray .git file",
			setup: func(t *testing.T, f *graphFixture, project string) {
				writeFixtureFile(t, filepath.Join(project, ".git"), marker+" folder/project/.git\n")
			},
			absent: "folder/project/.git",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireTools(t)
			f := newGraphFixture(t)
			project := f.path("folder/project")
			f.graphFile("folder/sibling.txt", marker)
			f.graphFile("folder/project/node_modules/dep.js", marker)
			tc.setup(t, f, project)
			setTestUserCache(t)
			logs := captureTestLogs(t, slog.LevelWarn)

			got := graphSearch(t, marker, f.path("folder"))
			if _, ok := got["folder/sibling.txt"]; !ok {
				t.Fatalf("sibling not found: %v", got)
			}
			if _, ok := got[tc.absent]; ok {
				t.Fatalf("%s found: %v", tc.absent, got)
			}
			warned := false
			for _, record := range logs.Records() {
				warned = warned || strings.Contains(record.Message, "its results are missing")
			}
			if warned != tc.warn {
				t.Fatalf("warned=%t, want %t", warned, tc.warn)
			}
		})
	}
}

// TestOperandBelowAStrayGitEntryStaysInItsRepository searches a subfolder
// whose `.git` entry names no repository (a file with no `gitdir:` line, or a
// directory with HEAD only): the operand belongs to the parent repository, so
// its Git-ignored file stays hidden and its tracked file is found.
func TestOperandBelowAStrayGitEntryStaysInItsRepository(t *testing.T) {
	requireTools(t)
	const marker = "zqstraysubmarker"
	for _, tc := range []struct {
		name  string
		stray func(t *testing.T, sub string)
	}{
		{name: "junk file", stray: func(t *testing.T, sub string) {
			writeFixtureFile(t, filepath.Join(sub, ".git"), "junk text\n")
		}},
		{name: "HEAD-only directory", stray: func(t *testing.T, sub string) {
			writeFixtureFile(t, filepath.Join(sub, ".git", "HEAD"), "ref: refs/heads/main\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGraphFixture(t)
			repo := f.GitRepo("repo", ".gitignore", "sub/secret.env\n")
			writeFixtureFile(t, filepath.Join(repo, "sub", "tracked.txt"), marker)
			gitRunIn(t, repo, "add", "sub/tracked.txt")
			gitRunIn(t, repo, "commit", "-q", "-m", "tracked")
			writeFixtureFile(t, filepath.Join(repo, "sub", "secret.env"), marker)
			tc.stray(t, filepath.Join(repo, "sub"))
			setTestUserCache(t)
			logs := captureTestLogs(t, slog.LevelWarn)
			out, err := captureStdout(t, func() error {
				return run(context.Background(), marker, []string{filepath.Join(repo, "sub")})
			})
			if err != nil || !strings.Contains(out, "tracked.txt") || strings.Contains(out, "secret.env") || len(logs.Records()) != 0 {
				t.Fatalf("err=%v warnings=%v, want the tracked file and no ignored file:\n%s", err, logs.Records(), out)
			}
		})
	}
}

// TestOperandsOfOneRepositoryReadItsDriversOnce resolves three operands of a
// repository whose `.git` file points to a separate Git directory (an
// ambiguous boundary): Seek reads the repository's driver list once.
func TestOperandsOfOneRepositoryReadItsDriversOnce(t *testing.T) {
	requireGit(t)
	f := newGraphFixture(t)
	project := f.path("proj")
	gitRunIn(t, f.root, "init", "-q", "--separate-git-dir="+f.path("store"), project)
	var operands []string
	for _, name := range []string{"a", "b", "c"} {
		writeFixtureFile(t, filepath.Join(project, name, "file.txt"), "x\n")
		operands = append(operands, filepath.Join(project, name))
	}
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	seeds, err := resolveOperands(t.Context(), operands)
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds.gitRoots) != 1 || len(seeds.gitRoots[0].operands) != 3 {
		t.Fatalf("seeds=%+v, want one repository with three operands", seeds)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if reads := strings.Count(string(data), "config --show-scope"); reads != 1 {
		t.Fatalf("driver list read %d times, want 1:\n%s", reads, data)
	}
}
