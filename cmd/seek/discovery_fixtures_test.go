package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeGitTriadAt writes the least that hasGitTriad accepts as a Git
// directory: a HEAD file and the objects and refs directories. It does not run
// Git and writes nothing outside gitDir. The caller picks the shape: a bare
// repository at gitDir, or the Git directory of a work tree at <dir>/.git.
func writeGitTriadAt(t testing.TB, gitDir string) {
	t.Helper()
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir gitDir: %v", err)
	}
	for _, sub := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(gitDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
}

// writeSubmoduleGitDir creates a submodule Git directory whose core.worktree
// names worktree, the way Git writes it in a superproject.
func writeSubmoduleGitDir(t testing.TB, gitDir, worktree string) {
	t.Helper()
	writeGitTriadAt(t, gitDir)
	rel, err := filepath.Rel(gitDir, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n\tworktree = "+rel+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// writeMinimalGitRepo writes the Git triad of writeGitTriadAt in
// repoDir/.git.
func writeMinimalGitRepo(t *testing.T, repoDir string) {
	t.Helper()
	writeGitTriadAt(t, filepath.Join(repoDir, ".git"))
}

// canonTempDir returns t.TempDir with its symlinks resolved. On macOS,
// t.TempDir is under /var, a symlink to /private/var. Code that canonicalizes
// a path (canonicalCorpusPath, as planFolderCorpus does) returns the resolved
// form, so a test that compares with its paths needs this form too.
func canonTempDir(t *testing.T) string {
	t.Helper()
	return resolvedPath(t, t.TempDir())
}

// resolvedPath returns path with its symlinks resolved, with
// filepath.EvalSymlinks and not with canonicalCorpusPath. So a test that
// compares a node root with it does not take its expected value from the
// code under test.
func resolvedPath(tb testing.TB, path string) string {
	tb.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		tb.Fatal(err)
	}
	return resolved
}
