package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCommittedShardHead_ReportsThePublishedCommit checks that the helper reads
// the commit from the shards themselves, for a full build and after a later
// commit, and that it reports "unknown" rather than a wrong answer when no
// committed shard exists.
func TestCommittedShardHead_ReportsThePublishedCommit(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// SHARDHEAD_MARKER\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	if files, err := runSeekInPlannedGitCorpus(ctx, "SHARDHEAD_MARKER", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("initial build: files=%v err=%v", files, err)
	}

	state := mustGitRepoStateIn(t, ctx, dir)
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatalf("scanFamily: %v", err)
	}
	got, ok := committedShardHead(scan)
	if !ok {
		t.Fatal("committedShardHead reported no commit after a full build")
	}
	if got != state.HeadSHA {
		t.Fatalf("shard records %q, HEAD is %q", got, state.HeadSHA)
	}

	// A second commit must move the recorded commit with the published shards.
	if err := os.WriteFile(
		filepath.Join(dir, "next.go"),
		[]byte("package main\n// SHARDHEAD_MARKER second\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", ".")
	gitRunIn(t, dir, "commit", "-m", "second")

	if files, err := runSeekInPlannedGitCorpus(ctx, "SHARDHEAD_MARKER", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("rebuild after second commit: files=%v err=%v", files, err)
	}
	moved := mustGitRepoStateIn(t, ctx, dir)
	if moved.HeadSHA == state.HeadSHA {
		t.Fatal("second commit did not move HEAD")
	}
	scan, err = scanFamily(plan.indexDir)
	if err != nil {
		t.Fatalf("scanFamily after second commit: %v", err)
	}
	got, ok = committedShardHead(scan)
	if !ok {
		t.Fatal("committedShardHead reported no commit after the second build")
	}
	if got != moved.HeadSHA {
		t.Fatalf("shard records %q, HEAD is %q", got, moved.HeadSHA)
	}
}

// TestCommittedShardHead_UnknownWithoutACommittedShard checks the "unknown"
// answer. A caller must keep the answer it has; it must never read this as
// stale.
func TestCommittedShardHead_UnknownWithoutACommittedShard(t *testing.T) {
	if _, ok := committedShardHead(familyScan{}); ok {
		t.Fatal("an empty scan reported a commit")
	}

	// An unnumbered name is not a family member, so the path lookup rejects it
	// before any read.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "not-a-shard.zoekt"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	scan, err := scanFamily(dir)
	if err != nil {
		t.Fatalf("scanFamily: %v", err)
	}
	if _, ok := committedShardZeroPath(scan); ok {
		t.Fatal("an unnumbered file was taken for shard 0")
	}
	if _, ok := committedShardHead(scan); ok {
		t.Fatal("an unnumbered file reported a commit")
	}

	// A real shard 0 whose bytes stop being a zoekt index must reach
	// index.ReadMetadataPathAlive and still report "unknown". This is the case
	// committedSnapshotReady relies on to keep today's answer.
	//
	// Build a real corpus and damage its shard rather than writing a file with
	// a hand-written name: the name then comes from the indexer, so this case
	// keeps testing the metadata read even if the naming ever changes.
	requireTools(t)
	repo := initGitRepo(t, "app.go", "package main\n// DAMAGED_SHARD_MARKER\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, repo)
	if files, err := runSeekInPlannedGitCorpus(ctx, "DAMAGED_SHARD_MARKER", paths, plan); err != nil || len(files) == 0 {
		t.Fatalf("build: files=%v err=%v", files, err)
	}
	built, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatalf("scanFamily: %v", err)
	}
	shardPath, ok := committedShardZeroPath(built)
	if !ok {
		t.Fatal("the build published no committed shard 0")
	}
	if _, ok := committedShardHead(built); !ok {
		t.Fatal("an intact shard reported no commit")
	}
	if err := os.WriteFile(shardPath, []byte("not a zoekt shard"), 0o644); err != nil {
		t.Fatal(err)
	}
	damagedScan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatalf("scanFamily after damage: %v", err)
	}
	if _, ok := committedShardZeroPath(damagedScan); !ok {
		t.Fatal("the damaged shard is no longer recognised as shard 0")
	}
	if _, ok := committedShardHead(damagedScan); ok {
		t.Fatal("unreadable shard metadata reported a commit")
	}
}
