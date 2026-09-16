package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedShards_FromAnotherCommitAreNotServed reproduces the state a
// crash leaves between the shard swap and the receipt writes: the index holds
// one commit's shards while every receipt still names the previous commit.
//
// Nothing in the cache is invented here. The shards and the manifest come from
// a real build at the second commit; the receipts come from a real build at the
// first. Seek must notice that the shards hold another commit and rebuild,
// rather than answer from them.
func TestCommittedShards_FromAnotherCommitAreNotServed(t *testing.T) {
	requireTools(t)

	const file = "app.go"
	dir := initGitRepo(t, file, "package main\n// STALE_MARKER alpha\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	if files, err := runSeekInPlannedGitCorpus(ctx, "alpha", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("build at the first commit: files=%v err=%v", files, err)
	}
	first := mustGitRepoStateIn(t, ctx, dir)
	receipts := saveReceipts(t, plan.cacheDir)

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
	if files, err := runSeekInPlannedGitCorpus(ctx, "gamma", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("build at the second commit: files=%v err=%v", files, err)
	}

	// Put the working tree back on the first commit and restore its receipts,
	// leaving the second commit's shards and manifest in place.
	gitRunIn(t, dir, "reset", "--hard", first.HeadSHA)
	restoreReceipts(t, plan.cacheDir, receipts)

	onDisk, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "gamma") {
		t.Fatal("the working tree still holds the second commit")
	}

	files, err := runSeekInPlannedGitCorpus(ctx, "gamma", paths, plan)
	if err != nil {
		t.Fatalf("search after the restored receipts: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("served content from another commit: %v", files)
	}
	// The corpus must still answer for the commit that is checked out.
	if files, err := runSeekInPlannedGitCorpus(ctx, "alpha", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("search for the checked-out commit: files=%v err=%v", files, err)
	}
}

// saveReceipts copies every receipt file at the top level of a corpus
// directory. It enumerates what the build wrote rather than naming the files,
// so this test keeps describing a whole crash state when the set of receipts
// changes. The index directory is a subdirectory and is not touched.
func saveReceipts(tb testing.TB, cacheDir string) map[string][]byte {
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
		tb.Fatal("the first build wrote no receipts")
	}
	return saved
}

// restoreReceipts puts the saved receipts back and removes any that appeared
// after the snapshot, so the corpus directory matches the moment it was taken.
func restoreReceipts(tb testing.TB, cacheDir string, saved map[string][]byte) {
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
