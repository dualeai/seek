package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sync/errgroup"
)

// newTestPool constructs a corpusPool ready for direct Enqueue /
// discoverNestedGit testing. Caller supplies the worker (production
// callers use prepareAndSearchCorpus via a closure; tests typically
// pass a no-op or assertion-emitting body). bufferSize sizes the
// resultsCh. Direct pool tests should size it for the plans they enqueue;
// production runCorpusPool drains concurrently while workers run.
func newTestPool(t *testing.T, worker corpusWorkerFunc, bufferSize int) (*corpusPool, *errgroup.Group) {
	t.Helper()
	g, gctx := errgroup.WithContext(t.Context())
	pool := &corpusPool{
		g:           g,
		gctx:        gctx,
		resultsCh:   make(chan corpusPoolResult, bufferSize),
		worker:      worker,
		workerSlots: make(chan struct{}, corpusWorkerCap),
	}
	return pool, g
}

// noopPoolWorker lets tests exercise enqueue and deduplication without indexing.
func noopPoolWorker(_ context.Context, _ corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
	return nil, nil, nil
}

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

// writeMinimalGitRepo creates a working-tree directory containing a
// valid `.git/` triad. Equivalent to writeGitTriadAt(t, filepath.Join(
// repoDir, ".git")) but takes the working-tree root, which is what
// callers asserting walker discovery typically have on hand.
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
