package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestFolderCorpusState_OversizeSkippedFileChangesFingerprint(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.bin")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxIndexedDocumentBytes+1); err != nil {
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
	if len(selected) != 0 {
		t.Fatalf("expected oversize file to be skipped, got %d selected", len(selected))
	}

	if err := os.Truncate(path, maxIndexedDocumentBytes+2); err != nil {
		t.Fatal(err)
	}
	second, selected, err := folderCorpusState(context.Background(), plan)
	if err != nil {
		t.Fatalf("folderCorpusState second: %v", err)
	}
	if len(selected) != 0 {
		t.Fatalf("expected oversize file to remain skipped, got %d selected", len(selected))
	}
	if first == second {
		t.Fatal("expected skipped oversize file metadata change to change fingerprint")
	}
}

func TestFolderCorpusState_IndexedByteCapFailsClosed(t *testing.T) {
	root := t.TempDir()
	size := int64(maxIndexedDocumentBytes)
	count := maxFolderIndexedBytes/maxIndexedDocumentBytes + 1
	for i := range count {
		path := filepath.Join(root, fmt.Sprintf("file_%03d.bin", i))
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
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

	_, _, err = folderCorpusState(context.Background(), plan)
	if !errors.Is(err, errFolderCapExceeded) {
		t.Fatalf("error=%v, want folder cap", err)
	}
	capErr, ok := errors.AsType[indexCapExceededError](err)
	if !ok {
		t.Fatalf("error=%v, want indexCapExceededError", err)
	}
	wantCurrent := int64(count) * size
	if capErr.metric != indexCapIndexedBytes || capErr.current != wantCurrent || capErr.limit != maxFolderIndexedBytes {
		t.Fatalf("cap error=%+v, want metric=%q current=%d limit=%d", capErr, indexCapIndexedBytes, wantCurrent, maxFolderIndexedBytes)
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

	ch := streamFolderFiles(
		context.Background(),
		[]folderCandidate{newFolderCandidate("note.txt", path, info)},
		4,
	)
	if cap(ch) != 0 {
		t.Fatalf("streamFolderFiles output must be unbuffered, got cap=%d", cap(ch))
	}
	for range ch {
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
