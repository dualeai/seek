package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIndexFolderDocumentsDelta_FallsBackOnOversizePayload — end-to-end
// verification of the contentBytes guard added to
// indexFolderDocumentsDelta (folder_indexer.go).
//
// Sequence:
//  1. Build initial corpus (small fixture → cachedState1, manifest1).
//  2. Mutate every file such that the delta payload would exceed
//     indexWindowBytes (with the test-local window override).
//  3. Re-index. Expectation: indexFolderDocumentsDelta returns the
//     "exceeds window threshold" error → indexFolderDocuments
//     falls through to streamFolderFiles → indexDocuments full
//     rebuild → searchable result.
//  4. Assert: searches still hit the post-mutation content (full
//     rebuild succeeded), and the semaphore returned to baseline.
//
// Tests the guard at the layer it lives in: real folder corpus
// pipeline, no mocked changedDocs. Drives the real
// changedFolderDocumentsSinceCachedState path.
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

	// Verify the post-mutation content is actually in the new shards.
	// searchPlannedCorpusParsed uses the production search path.
	results, err := searchPlannedCorpusForTest(ctx, plan, "MUTATED_BEACON_BAFFA")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("post-mutation marker not found; full-rebuild fallback did not produce searchable shards")
	}
}
