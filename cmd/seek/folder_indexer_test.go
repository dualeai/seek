package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFolderCorpusState_DoesNotReadContents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.txt")
	if err := os.WriteFile(path, []byte("folder_state_marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0o600) }()

	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("planFolderCorpus: %v", err)
	}

	state, selected, err := folderCorpusState(context.Background(), plan)
	if err != nil {
		t.Fatalf("folderCorpusState: %v", err)
	}
	if state == "" {
		t.Fatal("expected folder state hash")
	}
	if len(selected) != 1 {
		t.Fatalf("expected unreadable file to remain selected by metadata, got %d", len(selected))
	}
}

func TestFolderCorpusState_UnreadableDirectoryRootFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read chmod 000 directories")
	}

	root := t.TempDir()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("planFolderCorpus: %v", err)
	}

	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(root, 0o700) }()

	_, _, err = folderCorpusState(context.Background(), plan)
	if err == nil {
		t.Fatal("expected unreadable root to fail")
	}
}

func TestFolderCorpusState_OversizeNameOnlyFileChangesFingerprint(t *testing.T) {
	swapDocumentContentLimitForTest(t, testDocumentContentLimit)
	root := t.TempDir()
	path := filepath.Join(root, "large.bin")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, testDocumentContentLimit+1); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("planFolderCorpus: %v", err)
	}

	first, selected, err := folderCorpusState(context.Background(), plan)
	if err != nil {
		t.Fatalf("folderCorpusState first: %v", err)
	}
	if len(selected) != 1 || selected[0].name != "large.bin" {
		t.Fatalf("expected the oversize file as one name-only candidate, got %+v", selected)
	}

	if err := os.Truncate(path, testDocumentContentLimit+2); err != nil {
		t.Fatal(err)
	}
	second, selected, err := folderCorpusState(context.Background(), plan)
	if err != nil {
		t.Fatalf("folderCorpusState second: %v", err)
	}
	if len(selected) != 1 {
		t.Fatalf("expected the oversize file to stay one name-only candidate, got %d selected", len(selected))
	}
	if first == second {
		t.Fatal("expected oversize file metadata change to change fingerprint")
	}
}

func TestStreamFolderFiles_OutputChannelUnbuffered(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("folder stream marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	// The test is the consumer: it gives back the weight of each document,
	// and done checks that no weight leaks.
	done := withReadSemLock(t)
	defer done()
	// The build gives the stream a failure function; it cancels the read.
	ctx, fail := context.WithCancelCause(t.Context())
	defer fail(nil)
	ch := streamFolderFilesWithMarkers(
		ctx,
		[]folderCandidate{newFolderCandidate("note.txt", path, info)},
		4,
		fail,
		nil,
	)
	if cap(ch) != 0 {
		t.Fatalf("streamFolderFilesWithMarkers output must be unbuffered, got cap=%d", cap(ch))
	}
	var docs []fileContent
	for doc := range ch {
		docs = append(docs, doc)
		readSemaphore.Release(doc.weight)
	}
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("the stream reported a failure: %v", cause)
	}
	if len(docs) != 1 || docs[0].name != "note.txt" || string(docs[0].content) != "folder stream marker\n" {
		t.Fatalf("docs=%+v, want note.txt with its content", docs)
	}
}

func TestFolderCorpusState_SkipsGitMetadataOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.go"), []byte("package keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "node_modules", "dep.go"),
		filepath.Join(root, ".git", "objects", "metadata.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("planFolderCorpus: %v", err)
	}

	_, selected, err := folderCorpusState(context.Background(), plan)
	if err != nil {
		t.Fatalf("folderCorpusState: %v", err)
	}
	got := make(map[string]struct{}, len(selected))
	for _, candidate := range selected {
		got[candidate.name] = struct{}{}
	}
	for _, want := range []string{"keep.go", "node_modules/dep.go"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("expected %s to be selected, got %#v", want, selected)
		}
	}
	if _, ok := got[".git/objects/metadata.txt"]; ok {
		t.Fatalf("expected .git metadata to be skipped, got %#v", selected)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly regular folder files selected, got %#v", selected)
	}
}

func TestFolderCorpusFingerprintMatchesFullState(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "root.go"),
		filepath.Join(root, "pkg", "a.go"),
		filepath.Join(root, "pkg", "nested", "b.go"),
		filepath.Join(root, "zz", "c.go"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	plan := planFolderTestCorpus(t, root)
	fullState, selected, err := folderCorpusState(context.Background(), plan)
	if err != nil {
		t.Fatalf("folderCorpusState: %v", err)
	}
	fingerprint, selectedCount, err := folderCorpusFingerprint(context.Background(), plan)
	if err != nil {
		t.Fatalf("folderCorpusFingerprint: %v", err)
	}
	if fingerprint != fullState {
		t.Fatalf("fingerprint hash differs from full state: %s != %s", fingerprint, fullState)
	}
	if selectedCount != len(selected) {
		t.Fatalf("selected count differs: %d != %d", selectedCount, len(selected))
	}
}

func TestFstatatRegularFileModeMatchesFileInfo(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mode.go")
	if err := os.WriteFile(path, []byte("package keep\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o4755); err != nil {
		t.Fatal(err)
	}

	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	stat, ok := fstatatRegularFile(int(dir.Fd()), "mode.go")
	if !ok {
		t.Fatal("expected regular file stat")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.mode != info.Mode() {
		t.Fatalf("mode mismatch: fstatat=%v lstat=%v", stat.mode, info.Mode())
	}
}

func TestFolderCorpusRefreshUpdatesChangedFile(t *testing.T) {
	requireTools(t)

	ctx := context.Background()
	root := t.TempDir()
	changedPath := filepath.Join(root, "changed.go")
	unchangedPath := filepath.Join(root, "unchanged.go")
	if err := os.WriteFile(changedPath, []byte("package main\n\nfunc Changed() string { return \"folder_delta_old\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unchangedPath, []byte("package main\n\nfunc Unchanged() string { return \"folder_delta_keep\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planFolderTestCorpus(t, root)

	if state, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("initial ensureFolderCorpusFresh: %v", err)
	} else if state == corpusKnownEmpty {
		t.Fatal("folder should not be empty")
	}
	if err := os.WriteFile(changedPath, []byte("package main\n\nfunc Changed() string { return \"folder_delta_new\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("delta ensureFolderCorpusFresh: %v", err)
	} else if state == corpusKnownEmpty {
		t.Fatal("folder should not be empty after delta")
	}

	oldResults, err := searchPlannedCorpusForTest(ctx, plan, "folder_delta_old")
	if err != nil {
		t.Fatalf("search old marker: %v", err)
	}
	if len(oldResults) != 0 {
		t.Fatalf("old marker should be tombstoned, got %d results", len(oldResults))
	}
	for _, pattern := range []string{"folder_delta_new", "folder_delta_keep"} {
		results, err := searchPlannedCorpusForTest(ctx, plan, pattern)
		if err != nil {
			t.Fatalf("search %s: %v", pattern, err)
		}
		if len(results) == 0 {
			t.Fatalf("expected result for %s", pattern)
		}
	}
}

func TestFolderCorpusRepeatedRefreshMaintainsSearchability(t *testing.T) {
	requireTools(t)

	ctx := context.Background()
	root := t.TempDir()
	changedPath := filepath.Join(root, "changed.go")
	unchangedPath := filepath.Join(root, "unchanged.go")
	if err := os.WriteFile(changedPath, []byte("package main\n\nfunc Changed() string { return \"folder_delta_cleanup_0\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unchangedPath, []byte("package main\n\nfunc Unchanged() string { return \"folder_delta_cleanup_keep\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planFolderTestCorpus(t, root)
	if state, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("initial ensureFolderCorpusFresh: %v", err)
	} else if state == corpusKnownEmpty {
		t.Fatal("folder should not be empty")
	}

	for i := 1; i <= 5; i++ {
		content := fmt.Sprintf("package main\n\nfunc Changed() string { return \"folder_delta_cleanup_%d\" }\n", i)
		if err := os.WriteFile(changedPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if state, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			t.Fatalf("delta %d ensureFolderCorpusFresh: %v", i, err)
		} else if state == corpusKnownEmpty {
			t.Fatalf("folder should not be empty after delta %d", i)
		}
	}

	latestResults, err := searchPlannedCorpusForTest(ctx, plan, "folder_delta_cleanup_5")
	if err != nil {
		t.Fatalf("search latest marker: %v", err)
	}
	if len(latestResults) == 0 {
		t.Fatal("expected latest changed marker to be searchable")
	}
	oldResults, err := searchPlannedCorpusForTest(ctx, plan, "folder_delta_cleanup_1")
	if err != nil {
		t.Fatalf("search old marker: %v", err)
	}
	if len(oldResults) != 0 {
		t.Fatalf("old marker should be tombstoned, got %d results", len(oldResults))
	}
	keptResults, err := searchPlannedCorpusForTest(ctx, plan, "folder_delta_cleanup_keep")
	if err != nil {
		t.Fatalf("search kept marker: %v", err)
	}
	if len(keptResults) == 0 {
		t.Fatal("expected unchanged marker to remain searchable")
	}
}

func TestFolderCorpusReindexRemovesDeletedFile(t *testing.T) {
	requireTools(t)

	ctx := context.Background()
	root := t.TempDir()
	deletedPath := filepath.Join(root, "deleted.go")
	keptPath := filepath.Join(root, "kept.go")
	if err := os.WriteFile(deletedPath, []byte("package main\n\nfunc Deleted() string { return \"folder_delta_deleted\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keptPath, []byte("package main\n\nfunc Kept() string { return \"folder_delta_survives\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planFolderTestCorpus(t, root)

	if state, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("initial ensureFolderCorpusFresh: %v", err)
	} else if state == corpusKnownEmpty {
		t.Fatal("folder should not be empty")
	}
	if err := os.Remove(deletedPath); err != nil {
		t.Fatal(err)
	}
	if state, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("delete ensureFolderCorpusFresh: %v", err)
	} else if state == corpusKnownEmpty {
		t.Fatal("folder should not be empty after delete")
	}

	deletedResults, err := searchPlannedCorpusForTest(ctx, plan, "folder_delta_deleted")
	if err != nil {
		t.Fatalf("search deleted marker: %v", err)
	}
	if len(deletedResults) != 0 {
		t.Fatalf("deleted marker should be tombstoned, got %d results", len(deletedResults))
	}
	keptResults, err := searchPlannedCorpusForTest(ctx, plan, "folder_delta_survives")
	if err != nil {
		t.Fatalf("search kept marker: %v", err)
	}
	if len(keptResults) == 0 {
		t.Fatal("expected kept marker to remain searchable")
	}
}

// TestFolderCorpusRemovalOnlyDeltaKeepsShards removes one small file of a
// folder. The delta writes tombstones only: every prior shard keeps its inode,
// the shard count stays, and only the .meta sidecars change.
func TestFolderCorpusRemovalOnlyDeltaKeepsShards(t *testing.T) {
	requireTools(t)
	ctx := context.Background()
	root := t.TempDir()
	for name, content := range map[string]string{
		"keep.go": "package main\n// folder_keep_marker\n" + strings.Repeat("// filler line\n", 40),
		"gone.go": "package main\n// folder_gone_marker\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFolderTestCorpus(t, root)
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	before := readShardFileState(t, plan.indexDir, folderRepoName(plan))
	if len(before.shards) == 0 {
		t.Fatal("no folder shard")
	}

	if err := os.Remove(filepath.Join(root, "gone.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	requireTombstoneOnlyDelta(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))
	if results, err := searchPlannedCorpusForTest(ctx, plan, "folder_gone_marker"); err != nil || len(results) != 0 {
		t.Fatalf("folder_gone_marker matches %d files, want none; error=%v", len(results), err)
	}
}

// TestFolderCorpusTombstonedBytesCompactTheFamily removes 3 of 10 files of
// one size, then 2 more. The first delta tombstones 3 files against 7 live
// ones and stays a delta. With the second, deltas tombstoned 5 files against 5
// live ones, so the build replaces the family (deltaNeedsCompaction).
func TestFolderCorpusTombstonedBytesCompactTheFamily(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	ctx := context.Background()
	root := t.TempDir()
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFolderTestCorpus(t, root)
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	remove := func(files ...int) {
		t.Helper()
		for _, i := range files {
			if err := os.Remove(filepath.Join(root, fmt.Sprintf("f%02d.go", i))); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}

	before := readShardFileState(t, plan.indexDir, folderRepoName(plan))
	remove(0, 1, 2)
	requireTombstoneOnlyDelta(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))

	before = readShardFileState(t, plan.indexDir, folderRepoName(plan))
	remove(3, 4)
	requireFullRebuild(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))
	for i := range 10 {
		results, err := searchPlannedCorpusForTest(ctx, plan, fmt.Sprintf("compact_marker_%02d", i))
		if want := min(max(i-4, 0), 1); err != nil || len(results) != want {
			t.Fatalf("compact_marker_%02d matches %d files, want %d; error=%v", i, len(results), want, err)
		}
	}
}

// TestFolderCorpusWithNoManifestCountsDeadBytesFromShards removes the folder
// manifest before each delta. Removing 2 of 10 files of one size stays a delta
// and records the dead bytes that the shards list. Removing 3 more builds the
// folder in full, because the shards then list 5 dead files against 5 live
// ones (deltaNeedsCompaction), and the full build records zero.
func TestFolderCorpusWithNoManifestCountsDeadBytesFromShards(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	ctx := context.Background()
	root := t.TempDir()
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFolderTestCorpus(t, root)
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	removeWithNoManifest := func(files ...int) folderManifest {
		t.Helper()
		if err := os.Remove(filepath.Join(plan.cacheDir, folderManifestFileName)); err != nil {
			t.Fatal(err)
		}
		for _, i := range files {
			if err := os.Remove(filepath.Join(root, fmt.Sprintf("f%02d.go", i))); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			t.Fatal(err)
		}
		manifest, ok := readFolderManifest(plan.cacheDir, readStateFile(plan.cacheDir))
		if !ok {
			t.Fatal("the build wrote no manifest")
		}
		return manifest
	}

	before := readShardFileState(t, plan.indexDir, folderRepoName(plan))
	if manifest := removeWithNoManifest(0, 1); manifest.DeadBytes < 2*int64(len(compactionFixtureFile(0))) {
		t.Fatalf("dead_bytes=%d after removing 2 files, want at least their bytes", manifest.DeadBytes)
	}
	requireTombstoneOnlyDelta(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))

	before = readShardFileState(t, plan.indexDir, folderRepoName(plan))
	if manifest := removeWithNoManifest(2, 3, 4); manifest.DeadBytes != 0 {
		t.Fatalf("dead_bytes=%d after the full build, want 0", manifest.DeadBytes)
	}
	requireFullRebuild(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))
	for i := range 10 {
		results, err := searchPlannedCorpusForTest(ctx, plan, fmt.Sprintf("compact_marker_%02d", i))
		if want := min(max(i-4, 0), 1); err != nil || len(results) != want {
			t.Fatalf("compact_marker_%02d matches %d files, want %d; error=%v", i, len(results), want, err)
		}
	}
}

// TestFolderCorpusBinaryFilesCountTheirMarker removes 8 of 10 text files
// beside a binary file of 8,000 bytes, with the folder manifest and without
// it. The binary file counts as its marker on both paths, so 8 dead text files
// against 2 live ones and the marker build the folder in full, and the full
// build records no dead bytes (deltaNeedsCompaction).
func TestFolderCorpusBinaryFilesCountTheirMarker(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	ctx := context.Background()
	for _, dropManifest := range []bool{false, true} {
		t.Run(fmt.Sprintf("drop manifest %t", dropManifest), func(t *testing.T) {
			root := t.TempDir()
			for i := range 10 {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "blob.bin"), binaryFixtureFile, 0o644); err != nil {
				t.Fatal(err)
			}
			plan := planFolderTestCorpus(t, root)
			if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
				t.Fatal(err)
			}
			if dropManifest {
				if err := os.Remove(filepath.Join(plan.cacheDir, folderManifestFileName)); err != nil {
					t.Fatal(err)
				}
			}
			before := readShardFileState(t, plan.indexDir, folderRepoName(plan))
			for i := range 8 {
				if err := os.Remove(filepath.Join(root, fmt.Sprintf("f%02d.go", i))); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
				t.Fatal(err)
			}
			requireFullRebuild(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))
			manifest, ok := readFolderManifest(plan.cacheDir, readStateFile(plan.cacheDir))
			if !ok || manifest.DeadBytes != 0 {
				t.Fatalf("manifest ok=%t dead_bytes=%d after the full build, want 0", ok, manifest.DeadBytes)
			}
		})
	}
}

// TestFolderCorpusTextFileThatBecomesBinaryCompacts removes 2 of 10 text files
// beside a text file of 8,000 bytes, then makes that file binary, with the
// folder manifest. Before the reads, the file counts at its size, so the rule
// keeps a delta. After the reads, it counts as its 36-byte marker: 2 dead text
// files and its old 8,000 bytes then reach the 8 live text files and the
// marker, so the folder builds in full (deltaNeedsCompaction).
func TestFolderCorpusTextFileThatBecomesBinaryCompacts(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	ctx := context.Background()
	root := t.TempDir()
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), textFixture8000, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := planFolderTestCorpus(t, root)
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := os.Remove(filepath.Join(root, fmt.Sprintf("f%02d.go", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	before := readShardFileState(t, plan.indexDir, folderRepoName(plan))
	if err := os.WriteFile(filepath.Join(root, "big.txt"), binaryFixtureFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	requireFullRebuild(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))
}

// TestFolderCorpusBinaryFileThatBecomesTextStaysADelta removes 4 of 10 text
// files beside a binary file, then 2 more while the binary file becomes 20,280
// bytes of text, with the folder manifest. Before the reads, the changed file
// counts at an upper bound of its shard bytes, not at its old marker, so the
// rule keeps the delta that the read bytes keep (deltaNeedsCompaction).
func TestFolderCorpusBinaryFileThatBecomesTextStaysADelta(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	ctx := context.Background()
	root := t.TempDir()
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), binaryFixtureFile, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := planFolderTestCorpus(t, root)
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	remove := func(files ...int) {
		t.Helper()
		for _, i := range files {
			if err := os.Remove(filepath.Join(root, fmt.Sprintf("f%02d.go", i))); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	remove(0, 1, 2, 3)
	before := readShardFileState(t, plan.indexDir, folderRepoName(plan))
	text := bytes.Repeat([]byte("// text line of the former binary file\n"), 520)
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), text, 0o644); err != nil {
		t.Fatal(err)
	}
	remove(4, 5)
	requireTombstoneOnlyDeltaOrNewShard(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))
}

// TestFolderCorpusWithNoManifestTinyFilesStayADelta adds one file to a folder
// of 2 text files and 100 one-byte files, with no manifest. Before the reads,
// each file counts at an upper bound of its shard bytes, so the 38-byte marker
// of a one-byte file does not count as dead content, and the folder stays a
// delta (deltaNeedsCompaction).
func TestFolderCorpusWithNoManifestTinyFilesStayADelta(t *testing.T) {
	requireTools(t)
	// A small shardMax puts the floor of deltaNeedsCompaction below the
	// byte counts of these files (634 bytes each).
	swapShardMaxForTest(t, 1024)
	ctx := context.Background()
	root := t.TempDir()
	for i := range 2 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte(compactionFixtureFile(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 100 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("t%03d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFolderTestCorpus(t, root)
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(plan.cacheDir, folderManifestFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f02.go"), []byte(compactionFixtureFile(2)), 0o644); err != nil {
		t.Fatal(err)
	}
	before := readShardFileState(t, plan.indexDir, folderRepoName(plan))
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	requireTombstoneOnlyDeltaOrNewShard(t, before, readShardFileState(t, plan.indexDir, folderRepoName(plan)))
}

func TestFolderCorpusRefreshHandlesAddedChangedDeletedFiles(t *testing.T) {
	requireTools(t)

	ctx := context.Background()
	root := t.TempDir()
	deletedPath := filepath.Join(root, "deleted.go")
	changedPath := filepath.Join(root, "changed.go")
	unchangedPath := filepath.Join(root, "unchanged.go")
	addedPath := filepath.Join(root, "added.go")
	if err := os.WriteFile(deletedPath, []byte("package main\n\nfunc Deleted() string { return \"folder_delta_combo_deleted\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedPath, []byte("package main\n\nfunc Changed() string { return \"folder_delta_combo_old\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unchangedPath, []byte("package main\n\nfunc Unchanged() string { return \"folder_delta_combo_keep\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planFolderTestCorpus(t, root)
	if state, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("initial ensureFolderCorpusFresh: %v", err)
	} else if state == corpusKnownEmpty {
		t.Fatal("folder should not be empty")
	}

	if err := os.Remove(deletedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedPath, []byte("package main\n\nfunc Changed() string { return \"folder_delta_combo_new\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(addedPath, []byte("package main\n\nfunc Added() string { return \"folder_delta_combo_added\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("delta ensureFolderCorpusFresh: %v", err)
	} else if state == corpusKnownEmpty {
		t.Fatal("folder should not be empty after delta")
	}

	for _, pattern := range []string{"folder_delta_combo_deleted", "folder_delta_combo_old"} {
		results, err := searchPlannedCorpusForTest(ctx, plan, pattern)
		if err != nil {
			t.Fatalf("search %s: %v", pattern, err)
		}
		if len(results) != 0 {
			t.Fatalf("%s should be tombstoned, got %d results", pattern, len(results))
		}
	}

	for _, pattern := range []string{"folder_delta_combo_new", "folder_delta_combo_added", "folder_delta_combo_keep"} {
		results, err := searchPlannedCorpusForTest(ctx, plan, pattern)
		if err != nil {
			t.Fatalf("search %s: %v", pattern, err)
		}
		if len(results) == 0 {
			t.Fatalf("expected result for %s", pattern)
		}
	}
}

// TestFolderFamilyUsable checks the post-lock family scan. A peer can publish
// after the pre-lock scan, but one surviving shard does not make a partial
// family usable.
func TestFolderFamilyUsable(t *testing.T) {
	dir := t.TempDir()

	if folderFamilyUsable(dir) {
		t.Error("empty directory reported usable")
	}

	shardA := "folder_abc_v16.00000.zoekt"
	shardB := "folder_abc_v16.00001.zoekt"
	for _, name := range []string{shardA, shardB} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Shards present but never published: no manifest, so not usable.
	if folderFamilyUsable(dir) {
		t.Error("unpublished family reported usable")
	}

	if err := writeFamilyManifest(dir, []string{shardA, shardB}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if !folderFamilyUsable(dir) {
		t.Error("published family reported unusable")
	}

	// One survivor of a torn family: presence still says yes, the manifest says
	// no. This is the case the manifest check exists for.
	if err := os.Remove(filepath.Join(dir, shardB)); err != nil {
		t.Fatal(err)
	}
	if shardsExist(dir) != true {
		t.Fatal("precondition: a shard should still be present")
	}
	if folderFamilyUsable(dir) {
		t.Error("torn family reported usable")
	}
}
