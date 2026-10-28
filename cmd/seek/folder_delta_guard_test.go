package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestIndexFolderDocumentsDelta_FallsBackOnOversizePayload checks the payload
// guard of a folder delta from end to end. It runs with the manifest of the
// first pass (changedFolderDocumentsFromManifest) and with no manifest
// (changedFolderDocuments, which reads every file). Each returns
// errDeltaPayloadExceedsWindow when the changed size reaches the window.
//
// Sequence:
//  1. Index 8 files of 64 KiB. This writes the shards and the manifest; the
//     second case then removes the manifest.
//  2. Grow every file to 384 KiB, so the delta payload (3 MiB) passes the
//     2 MiB test window (swapReadSemaphoreForTest sets half the budget).
//  3. Index again. indexFolderDocumentsDelta returns
//     errDeltaPayloadExceedsWindow, and indexFolderDocuments then does a full
//     rebuild through streamFolderFilesWithMarkers and indexDocuments.
//  4. Check that the build replaced the base shard file. A delta keeps it and
//     adds shards; a full rebuild writes a new one. Check also that a search
//     finds the new content, and that the read semaphore has its full budget
//     again.
//
// The test uses the real folder pipeline and mocks no changed documents.
func TestIndexFolderDocumentsDelta_FallsBackOnOversizePayload(t *testing.T) {
	requireTools(t)
	for _, dropManifest := range []bool{false, true} {
		t.Run(fmt.Sprintf("drop manifest %t", dropManifest), func(t *testing.T) {
			checkOversizePayloadFallsBack(t, dropManifest)
		})
	}
}

func checkOversizePayloadFallsBack(t *testing.T, dropManifest bool) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB: a 2 MiB window
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

	root := t.TempDir()
	const fileCount = 8
	const initialSize = 64 * 1024
	files := make([]string, fileCount)
	for i := 0; i < fileCount; i++ {
		name := testFileName("doc", i) + ".txt"
		files[i] = filepath.Join(root, name)
		initial := make([]byte, initialSize)
		for j := range initial {
			initial[j] = byte('a' + i%26)
		}
		if err := os.WriteFile(files[i], initial, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	plan := planSynthCorpus(t, root)
	ctx := t.Context()

	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("initial ensureFolderCorpusFresh: %v", err)
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after initial index: got=%d want=%d", got, testBudget)
	}
	baseBefore := firstShardInfo(t, plan.indexDir)
	if dropManifest {
		if err := os.Remove(filepath.Join(plan.cacheDir, folderManifestFileName)); err != nil {
			t.Fatal(err)
		}
	}

	// 8 files of 384 KiB: a 3 MiB change, above the 2 MiB window.
	const grownSize = 384 * 1024
	marker := []byte("MUTATED_BEACON_BAFFA\n")
	for i, p := range files {
		grown := make([]byte, grownSize)
		copy(grown, marker)
		for j := len(marker); j < len(grown); j++ {
			grown[j] = byte('A' + i%26)
		}
		if err := os.WriteFile(p, grown, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("post-mutation ensureFolderCorpusFresh: %v", err)
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after fallback rebuild: got=%d want=%d", got, testBudget)
	}
	// A delta keeps the base shard file and adds shards; only a full
	// rebuild replaces it.
	if os.SameFile(baseBefore, firstShardInfo(t, plan.indexDir)) {
		t.Fatal("the base shard was kept after an oversize change: a delta ran, not the full rebuild")
	}

	results, err := searchPlannedCorpusForTest(ctx, plan, "MUTATED_BEACON_BAFFA")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("post-mutation marker not found; full-rebuild fallback did not produce searchable shards")
	}
}

// TestSeedFolderDeltaLinksOrFallsBack seeds a delta from a live family. When
// every file links, the delta runs on the links. When one link fails (a shard
// that went away), the delta does not run. The build directory is then empty
// again, so the full build starts with no seed.
func TestSeedFolderDeltaLinksOrFallsBack(t *testing.T) {
	live := t.TempDir()
	shard := filepath.Join(live, "folder_x_v16.00000.zoekt")
	meta := shard + ".meta"
	for _, name := range []string{shard, meta} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listed := func(dir string) []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	linked := t.TempDir()
	if !seedFolderDelta(linked, "folder_x", []string{shard, meta}) {
		t.Fatal("the delta did not run with every seed linked")
	}
	if got := listed(linked); len(got) != 2 {
		t.Fatalf("seeds %v, want the shard and its record", got)
	}
	failed := t.TempDir()
	missing := filepath.Join(live, "folder_x_v16.00001.zoekt")
	if seedFolderDelta(failed, "folder_x", []string{shard, meta, missing}) {
		t.Fatal("the delta ran with a seed that did not link")
	}
	if got := listed(failed); len(got) != 0 {
		t.Fatalf("the failed seed left %v", got)
	}
}

// TestFolderDeltaAboveLargeFullBase gives a folder whose full build writes
// more shards than maxFolderDeltaShards. The cap counts only the shards that
// deltas add since the full build. So each change of one file takes a delta,
// which keeps the base shard file. The second change checks that the delta
// keeps the base count in the manifest.
func TestFolderDeltaAboveLargeFullBase(t *testing.T) {
	requireTools(t)
	// Zoekt ends a shard when its content passes shardMax. Each file holds
	// more than half of it, so each shard holds two files.
	const fileBytes = 3000
	swapForTest(t, &shardMax, 4096)
	root := t.TempDir()
	const fileCount = 2 * (maxFolderDeltaShards + 4)
	for i := range fileCount {
		content := bytes.Repeat([]byte{byte('a' + i%26)}, fileBytes)
		if err := os.WriteFile(filepath.Join(root, testFileName("doc", i)+".txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := planSynthCorpus(t, root)
	if _, err := ensureFolderCorpusFresh(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	repoName := folderRepoName(plan)
	base := repositoryShardCount(plan.indexDir, repoName)
	if base <= maxFolderDeltaShards {
		t.Fatalf("the full build wrote %d shards, want more than %d", base, maxFolderDeltaShards)
	}
	baseBefore := firstShardInfo(t, plan.indexDir)

	for change := 1; change <= 2; change++ {
		marker := fmt.Sprintf("LARGE_BASE_DELTA_BEACON_%d", change)
		if err := os.WriteFile(filepath.Join(root, testFileName("doc", change)+".txt"), []byte(marker+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ensureFolderCorpusFresh(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(baseBefore, firstShardInfo(t, plan.indexDir)) {
			t.Fatalf("change %d: the build replaced the base shard: a full rebuild ran, not a delta", change)
		}
		if got := repositoryShardCount(plan.indexDir, repoName); got != base+change {
			t.Fatalf("change %d: shards=%d, want the %d base shards and %d delta shards", change, got, base, change)
		}
		results, err := searchPlannedCorpusForTest(t.Context(), plan, marker)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 {
			t.Fatalf("change %d: got %d files for the changed content, want 1", change, len(results))
		}
	}
}

// TestFolderDeltaShardLimitIsRelativeToFullBase checks the bounds of the
// folder cap. With no recorded base, the cap counts every shard.
func TestFolderDeltaShardLimitIsRelativeToFullBase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current int
		base    int
		want    bool
	}{
		{name: "large-full-base", current: 153, base: 153},
		{name: "sixty-four-delta-shards", current: 217, base: 153},
		{name: "sixty-five-delta-shards", current: 218, base: 153, want: true},
		{name: "unknown-base-at-cap", current: 64, base: 0},
		{name: "unknown-base-above-cap", current: 65, base: 0, want: true},
		{name: "base-ahead-of-family", current: 10, base: 11, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := folderDeltaShardLimitExceeded(tc.current, tc.base); got != tc.want {
				t.Fatalf("limit exceeded=%t, want %t (current=%d base=%d)", got, tc.want, tc.current, tc.base)
			}
		})
	}
}
