package main

import (
	"os"
	"path/filepath"
	"testing"
)

// scanFolderForTest runs the graph expansion's walk of root and returns the
// state hash and the Git roots that the walk recorded.
func scanFolderForTest(t *testing.T, root string) (string, []gitPaths) {
	t.Helper()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatal(err)
	}
	capture, gitRoots, err := captureFolderNode(t.Context(), plan)
	if err != nil {
		t.Fatalf("captureFolderNode: %v", err)
	}
	return capture.stateHash, gitRoots
}

// TestFolderWalkerDiscoversNestedGit checks that the walk of a folder with one
// nested Git repo records exactly one Git root, at the nested repo path.
func TestFolderWalkerDiscoversNestedGit(t *testing.T) {
	root := canonTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "sub", "repo")
	writeMinimalGitRepo(t, nested)
	if err := os.WriteFile(filepath.Join(nested, "src.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, discovered := scanFolderForTest(t, root)
	if len(discovered) != 1 {
		t.Fatalf("discovered=%d, want 1", len(discovered))
	}
	if discovered[0].RepoDir != nested {
		t.Fatalf("RepoDir=%q, want %q", discovered[0].RepoDir, nested)
	}
}

// TestFolderWalkerDepth3 checks that the walk records a Git root below three
// folders (a/b/c/repo).
func TestFolderWalkerDepth3(t *testing.T) {
	root := canonTempDir(t)
	deep := filepath.Join(root, "a", "b", "c", "repo")
	writeMinimalGitRepo(t, deep)
	_, discovered := scanFolderForTest(t, root)
	if len(discovered) != 1 {
		t.Fatalf("discovered=%d, want 1 (depth-3 boundary)", len(discovered))
	}
	if discovered[0].RepoDir != deep {
		t.Fatalf("RepoDir=%q, want %q", discovered[0].RepoDir, deep)
	}
}

// TestFolderFingerprintStableUnderNestedCommit checks that a file edit and a
// new file inside a nested repo do not change the parent's state hash. The
// nested repo is its own node. The test makes no commit.
func TestFolderFingerprintStableUnderNestedCommit(t *testing.T) {
	root := canonTempDir(t)
	nested := filepath.Join(root, "repo")
	writeMinimalGitRepo(t, nested)
	if err := os.WriteFile(filepath.Join(nested, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := scanFolderForTest(t, root)

	// Change the work tree of the nested repo.
	if err := os.WriteFile(filepath.Join(nested, "a.go"), []byte("package main\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "b.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := scanFolderForTest(t, root)

	if before != after {
		t.Fatalf("parent fingerprint changed under nested working-tree edit: before=%s after=%s", before, after)
	}
}

// TestFolderFingerprintStableUnderNestedGitLockFile checks that a Git lock
// file that comes and goes in a nested repo does not change the parent's
// state hash.
func TestFolderFingerprintStableUnderNestedGitLockFile(t *testing.T) {
	root := canonTempDir(t)
	nested := filepath.Join(root, "repo")
	writeMinimalGitRepo(t, nested)
	before, _ := scanFolderForTest(t, root)

	lockPath := filepath.Join(nested, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	after, _ := scanFolderForTest(t, root)

	if before != after {
		t.Fatalf("nested Git lock changed parent fingerprint: before=%s after=%s", before, after)
	}
}

// TestFolderWalkerSkipsSymlinkedWorktree verifies that the walk records the
// canonical worktree and does not follow a symlink to it.
func TestFolderWalkerSkipsSymlinkedWorktree(t *testing.T) {
	root := canonTempDir(t)
	canonical := filepath.Join(root, "real")
	writeMinimalGitRepo(t, canonical)
	if err := os.Symlink(canonical, filepath.Join(root, "alias")); err != nil {
		t.Fatalf("make the symlink to the worktree: %v", err)
	}
	// The symlink points to the working tree, not to `.git`. The walk
	// skips every symlink entry (folderWalker.visit), so it never reaches the
	// alias.
	_, discovered := scanFolderForTest(t, root)
	if len(discovered) != 1 {
		t.Fatalf("discovered=%d, want 1 (symlinked alias should be skipped by walker)", len(discovered))
	}
}

// TestBrokenSubmoduleGracefulSkip checks that a .git file that points to a
// missing directory makes a failed node. The walk records no Git root and
// returns no error.
func TestBrokenSubmoduleGracefulSkip(t *testing.T) {
	root := canonTempDir(t)
	bad := filepath.Join(root, "bad-sub")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	// gitdir: points to a non-existent path.
	if err := os.WriteFile(filepath.Join(bad, ".git"), []byte("gitdir: "+filepath.Join(root, "missing")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, discovered := scanFolderForTest(t, root)
	if len(discovered) != 0 {
		t.Fatalf("discovered=%d, want 0 (broken submodule must be skipped)", len(discovered))
	}
}
