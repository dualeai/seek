package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIndexFolderDocumentsDelta_FallsBackOnOversizePayload checks the payload
// guard of a folder delta from end to end. The guard is in
// changedFolderDocumentsFromManifest (folder_indexer.go): it returns
// errDeltaPayloadExceedsWindow when the changed size reaches the window.
//
// Sequence:
//  1. Index 8 files of 64 KiB. This writes the shards and the manifest.
//  2. Grow every file to 384 KiB, so the delta payload (3 MiB) passes the
//     2 MiB test window (swapReadSemaphoreForTest sets half the budget).
//  3. Index again. indexFolderDocumentsDelta returns
//     errDeltaPayloadExceedsWindow, and indexFolderDocuments then does a full
//     rebuild through streamFolderFiles and indexDocuments.
//  4. Check that the base shard file is replaced (a delta keeps it and adds
//     shards; a full rebuild writes a new one), that a search finds the new
//     content, and that the read semaphore has its full budget again.
//
// The test uses the real folder pipeline with no mocked changedDocs, through
// changedFolderDocumentsSinceCachedState.
func TestIndexFolderDocumentsDelta_FallsBackOnOversizePayload(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
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
	// full rebuild via streamFolderFiles → windowed indexDocuments.
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
