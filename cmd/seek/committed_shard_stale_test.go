package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedShards_FromAnotherCommitAreNotServed makes the state that a
// crash leaves between the shard swap and the writes of the state files. The
// index holds the shards of one commit. Every state file still names the
// commit before it.
//
// The test invents nothing in the cache. The shards and the manifest come
// from a real build at the second commit; the state files come from a real
// build at the first. Seek must see that the shards hold another commit and
// rebuild, and not answer from them.
func TestCommittedShards_FromAnotherCommitAreNotServed(t *testing.T) {
	requireTools(t)

	const file = "app.go"
	dir := initGitRepo(t, file, "package main\n// STALE_MARKER alpha\n")
	ctx := context.Background()
	_, plan := planGitTestCorpus(t, dir)

	if files, err := runSeekInPlannedGitCorpus(ctx, "alpha", plan); err != nil || len(files) == 0 {
		t.Fatalf("build at the first commit: files=%v err=%v", files, err)
	}
	first := mustGitRepoStateIn(t, ctx, dir)
	stateFiles := saveStateFiles(t, plan.cacheDir)

	// Second commit, and a real build of it.
	if err := os.WriteFile(
		filepath.Join(dir, file),
		[]byte("package main\n// STALE_MARKER gamma\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", ".")
	gitRunIn(t, dir, "commit", "-m", "second")
	if files, err := runSeekInPlannedGitCorpus(ctx, "gamma", plan); err != nil || len(files) == 0 {
		t.Fatalf("build at the second commit: files=%v err=%v", files, err)
	}

	// Put the working tree back on the first commit and restore its state
	// files. The shards and the manifest of the second commit stay.
	gitRunIn(t, dir, "reset", "--hard", first.HeadSHA)
	restoreStateFiles(t, plan.cacheDir, stateFiles)

	onDisk, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "gamma") {
		t.Fatal("the working tree still holds the second commit")
	}

	files, err := runSeekInPlannedGitCorpus(ctx, "gamma", plan)
	if err != nil {
		t.Fatalf("search after the restored state files: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("served content from another commit: %v", files)
	}
	// The corpus must still answer for the commit that is checked out.
	if files, err := runSeekInPlannedGitCorpus(ctx, "alpha", plan); err != nil || len(files) == 0 {
		t.Fatalf("search for the checked-out commit: files=%v err=%v", files, err)
	}
}

// saveStateFiles copies every file at the top level of a corpus directory. It
// lists what the build wrote and does not name the files. So the test keeps a
// whole crash state when the set of state files changes. It does not copy the
// index directory, a subdirectory.
func saveStateFiles(tb testing.TB, cacheDir string) map[string][]byte {
	tb.Helper()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		tb.Fatalf("read corpus dir: %v", err)
	}
	saved := make(map[string][]byte)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cacheDir, entry.Name()))
		if err != nil {
			tb.Fatalf("read %s: %v", entry.Name(), err)
		}
		saved[entry.Name()] = data
	}
	if len(saved) == 0 {
		tb.Fatal("the first build wrote no state files")
	}
	return saved
}

// restoreStateFiles puts the saved state files back. It removes each
// top-level file that the save did not hold, so the corpus directory has the
// state files of the save.
func restoreStateFiles(tb testing.TB, cacheDir string, saved map[string][]byte) {
	tb.Helper()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		tb.Fatalf("read corpus dir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, ok := saved[entry.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(cacheDir, entry.Name())); err != nil {
			tb.Fatalf("remove %s: %v", entry.Name(), err)
		}
	}
	for name, data := range saved {
		if err := os.WriteFile(filepath.Join(cacheDir, name), data, 0o644); err != nil {
			tb.Fatalf("write %s: %v", name, err)
		}
	}
}
