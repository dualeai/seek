package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeGitTriadAt creates the structural minimum a git repo needs to
// satisfy hasGitTriad: HEAD (regular file) + objects/ (dir) + refs/
// (dir). Use this directly when you need a `.git/` directory that
// looks like a repo to the detector without spawning `git init`.
//
// Hermetic by design — no calls into the git binary, no temp files
// outside `gitDir`. The caller decides whether `gitDir` is a top-level
// path (bare-repo shape) or `<somewhere>/.git` (regular-repo shape).
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

// writeMinimalGitRepo creates a working-tree directory containing a
// valid `.git/` triad. Equivalent to writeGitTriadAt(t, filepath.Join(
// repoDir, ".git")) but takes the working-tree root.
func writeMinimalGitRepo(t *testing.T, repoDir string) {
	t.Helper()
	writeGitTriadAt(t, filepath.Join(repoDir, ".git"))
}

// canonTempDir returns a symlink-resolved temp directory.
//
// macOS's t.TempDir lives under /var which is a symlink to /private/var.
// Tests whose assertions traverse code that canonicalizes paths (e.g.
// planFolderCorpus → canonicalCorpusPath inside the folder-walker
// path) must compare against the resolved form, hence this helper.
//
// Tests that exercise code which DOES NOT canonicalize (e.g.
// detectGitBoundary returns the absDir argument verbatim as
// RepoDir) can use t.TempDir directly. git_detect_test.go is the
// canonical example: every test passes raw t.TempDir and compares
// the detector's returned RepoDir against the same raw root, so the
// /private/var resolution is irrelevant and adding canonTempDir
// there would only obscure the test intent.
func canonTempDir(t *testing.T) string {
	t.Helper()
	return canonicalCorpusPath(t.TempDir())
}
