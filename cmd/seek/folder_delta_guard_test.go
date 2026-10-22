package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIndexFolderDocumentsDelta_FallsBackOnOversizePayload checks the payload
// guard of a folder delta from end to end, with the manifest of the first
// pass (changedFolderDocumentsFromManifest) and with no manifest
// (changedFolderDocuments, which reads every file): each returns
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
//  4. Check that the base shard file is replaced (a delta keeps it and adds
//     shards; a full rebuild writes a new one), that a search finds the new
//     content, and that the read semaphore has its full budget again.
//
// The test uses the real folder pipeline and mocks no changed documents.
func TestIndexFolderDocumentsDelta_FallsBackOnOversizePayload(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	for _, dropManifest := range []bool{false, true} {
		t.Run(fmt.Sprintf("drop manifest %t", dropManifest), func(t *testing.T) {
			checkOversizePayloadFallsBack(t, dropManifest)
		})
	}
}

func checkOversizePayloadFallsBack(t *testing.T, dropManifest bool) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB → window = 2 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 60*time.Second)()

	root := t.TempDir()
	const fileCount = 8
	const initialSize = 64 * 1024 // small initial — fits delta path
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

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// First pass — establishes initial shards + manifest.
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

	// Mutate every file with content size > window/fileCount each. Total
	// = fileCount * 384 KiB = 3 MiB > 2 MiB window → guard trips.
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

	// Second pass — delta would exceed window; guard trips; fallback to
	// full rebuild via streamFolderFilesWithMarkers → windowed indexDocuments.
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

	// Verify the post-mutation content is actually in the new shards.
	// searchPlannedCorpusForTest uses the production search path
	// (searchPlannedCorpusParsed).
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
// that went away), the delta does not run and the build directory is empty
// again, so the full build starts from no seed.
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
