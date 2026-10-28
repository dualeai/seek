package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFolderCorpusState_DoesNotReadContents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.txt")
	if err := os.WriteFile(path, []byte("folder_state_marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A user who can read the file would pass with no check: the state reads
	// metadata only, so only an unreadable file proves it.
	chmodUnreadableOrFatal(t, path, 0o000)

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
	root := t.TempDir()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("planFolderCorpus: %v", err)
	}

	chmodUnreadableOrFatal(t, root, 0)

	_, _, err = folderCorpusState(context.Background(), plan)
	if err == nil {
		t.Fatal("expected unreadable root to fail")
	}
}

func TestFolderCorpusState_OversizeNameOnlyFileChangesFingerprint(t *testing.T) {
	swapForTest(t, &documentContentLimit, testDocumentContentLimit)
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

// TestStreamFolderFiles_OutputChannelUnbuffered checks that the stream sends
// each document on a channel with no buffer. A buffer would hold documents
// that no worker holds. Then the worker count would no longer limit the
// documents that are read and not yet consumed (streamFolderFilesWithMarkers).
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
		make([]int64, 1),
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

// TestFolderCorpusFingerprintMatchesFullState checks that the walk with no
// file list gives the state and file count of the full walk. The warm path
// compares the fingerprint with the state that the last build stored.
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

// folderSearchNames runs a lexical search of root through the root command.
// It returns the base name of each printed file, or none when the search ends
// with exit status 1.
func folderSearchNames(t *testing.T, root, pattern string) []string {
	t.Helper()
	output, err := seek(t, "--lexical-only", pattern, root)
	if errors.Is(err, errNoMatch) {
		return nil
	}
	if err != nil {
		t.Fatalf("search %s: %v\n%s", pattern, err, output)
	}
	var names []string
	for _, path := range headerPaths(output) {
		names = append(names, filepath.Base(path))
	}
	return names
}

// requireFolderSearchNames fails unless each pattern of want finds the files
// that want gives it, with a search of root through the root command.
func requireFolderSearchNames(t *testing.T, root string, want map[string][]string) {
	t.Helper()
	for pattern, names := range want {
		if got := folderSearchNames(t, root, pattern); !slices.Equal(got, names) {
			t.Fatalf("%s matches %v, want %v", pattern, got, names)
		}
	}
}

// TestFolderCorpusRefreshUpdatesChangedFile edits one file of a folder
// between two searches. The second search finds the new text and not the old
// text, and it still finds the file that did not change.
func TestFolderCorpusRefreshUpdatesChangedFile(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	writeFileAt(t, root, "changed.go", "package main\n\nfunc Changed() string { return \"folder_delta_old\" }\n")
	writeFileAt(t, root, "unchanged.go", "package main\n\nfunc Unchanged() string { return \"folder_delta_keep\" }\n")
	requireFolderSearchNames(t, root, map[string][]string{"folder_delta_old": {"changed.go"}})

	writeFileAt(t, root, "changed.go", "package main\n\nfunc Changed() string { return \"folder_delta_new\" }\n")
	requireFolderSearchNames(t, root, map[string][]string{
		"folder_delta_old":  nil,
		"folder_delta_new":  {"changed.go"},
		"folder_delta_keep": {"unchanged.go"},
	})
}

// TestFolderCorpusRepeatedRefreshMaintainsSearchability edits one file of a
// folder five times, with a search after each edit. Each search finds the
// last text. At the end, no older text matches, and the file that did not
// change still matches.
func TestFolderCorpusRepeatedRefreshMaintainsSearchability(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	marker := func(i int) string { return fmt.Sprintf("folder_delta_cleanup_%d", i) }
	write := func(i int) {
		writeFileAt(t, root, "changed.go", fmt.Sprintf("package main\n\nfunc Changed() string { return %q }\n", marker(i)))
	}
	write(0)
	writeFileAt(t, root, "unchanged.go", "package main\n\nfunc Unchanged() string { return \"folder_delta_cleanup_keep\" }\n")
	requireFolderSearchNames(t, root, map[string][]string{marker(0): {"changed.go"}})

	for i := 1; i <= 5; i++ {
		write(i)
		requireFolderSearchNames(t, root, map[string][]string{marker(i): {"changed.go"}})
	}
	want := map[string][]string{"folder_delta_cleanup_keep": {"unchanged.go"}}
	for i := range 5 {
		want[marker(i)] = nil
	}
	requireFolderSearchNames(t, root, want)
}

// TestFolderCorpusReindexRemovesDeletedFile deletes one file of a folder
// between two searches. The second search does not find its text, and it
// still finds the other file.
func TestFolderCorpusReindexRemovesDeletedFile(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	writeFileAt(t, root, "deleted.go", "package main\n\nfunc Deleted() string { return \"folder_delta_deleted\" }\n")
	writeFileAt(t, root, "kept.go", "package main\n\nfunc Kept() string { return \"folder_delta_survives\" }\n")
	requireFolderSearchNames(t, root, map[string][]string{"folder_delta_deleted": {"deleted.go"}})

	if err := os.Remove(filepath.Join(root, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	requireFolderSearchNames(t, root, map[string][]string{
		"folder_delta_deleted":  nil,
		"folder_delta_survives": {"kept.go"},
	})
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
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two fixture files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
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
// folder in full: the shards then list 5 dead files against 5 live ones
// (deltaNeedsCompaction). The full build records zero dead bytes.
func TestFolderCorpusWithNoManifestCountsDeadBytesFromShards(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two fixture files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
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
// it. The binary file counts as its marker on both paths. So 8 dead text files
// reach 2 live ones and the marker, and the folder builds in full
// (deltaNeedsCompaction). The full build records no dead bytes.
func TestFolderCorpusBinaryFilesCountTheirMarker(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two fixture files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
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
// keeps a delta. After the reads, it counts as its 36-byte marker. Then 2 dead
// text files and its old 8,000 bytes reach the 8 live text files and the
// marker. So the folder builds in full (deltaNeedsCompaction).
func TestFolderCorpusTextFileThatBecomesBinaryCompacts(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two fixture files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
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

// TestFolderCorpusBinaryFileThatBecomesTextStaysADelta keeps the folder
// manifest. It removes 4 of 10 text files beside a binary file. Then it
// removes 2 more, while the binary file becomes 20,280 bytes of text. Before
// the reads, the changed file counts at an upper bound of its shard bytes, not
// at its old marker. So the rule keeps the delta that the read bytes keep
// (deltaNeedsCompaction).
func TestFolderCorpusBinaryFileThatBecomesTextStaysADelta(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two fixture files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
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
// each file counts at an upper bound of its shard bytes. So the 38-byte marker
// of a one-byte file does not count as dead content, and the folder stays a
// delta (deltaNeedsCompaction).
func TestFolderCorpusWithNoManifestTinyFilesStayADelta(t *testing.T) {
	requireTools(t)
	// A shardMax of 1 KiB puts the floor of deltaNeedsCompaction (one full
	// shard) below the bytes of two fixture files of 634 bytes.
	swapForTest(t, &shardMax, 1024)
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

// TestFolderCorpusRefreshHandlesAddedChangedDeletedFiles adds, edits, and
// deletes one file each between two searches of a folder. The second search
// finds the new texts and the file that did not change. It does not find the
// old text or the text of the deleted file.
func TestFolderCorpusRefreshHandlesAddedChangedDeletedFiles(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	writeFileAt(t, root, "deleted.go", "package main\n\nfunc Deleted() string { return \"folder_delta_combo_deleted\" }\n")
	writeFileAt(t, root, "changed.go", "package main\n\nfunc Changed() string { return \"folder_delta_combo_old\" }\n")
	writeFileAt(t, root, "unchanged.go", "package main\n\nfunc Unchanged() string { return \"folder_delta_combo_keep\" }\n")
	requireFolderSearchNames(t, root, map[string][]string{"folder_delta_combo": {"changed.go", "deleted.go", "unchanged.go"}})

	if err := os.Remove(filepath.Join(root, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, root, "changed.go", "package main\n\nfunc Changed() string { return \"folder_delta_combo_new\" }\n")
	writeFileAt(t, root, "added.go", "package main\n\nfunc Added() string { return \"folder_delta_combo_added\" }\n")
	requireFolderSearchNames(t, root, map[string][]string{
		"folder_delta_combo_deleted": nil,
		"folder_delta_combo_old":     nil,
		"folder_delta_combo_new":     {"changed.go"},
		"folder_delta_combo_added":   {"added.go"},
		"folder_delta_combo_keep":    {"unchanged.go"},
	})
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
