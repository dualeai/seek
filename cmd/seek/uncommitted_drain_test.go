package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestUncommittedDeltaPossible_StatSizesAtTheWindowGiveNoDelta checks the
// check before the reads: when the stat sizes of the files to read reach the
// window, uncommittedDeltaPossible gives no delta, so no reader starts.
func TestUncommittedDeltaPossible_StatSizesAtTheWindowGiveNoDelta(t *testing.T) {
	repoDir, cacheDir, indexDir := t.TempDir(), t.TempDir(), t.TempDir()
	// A live family of one shard. Only its name counts here.
	if err := os.WriteFile(filepath.Join(indexDir, repoUncommitted+"_v16.00000.zoekt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeUncommittedManifest(cacheDir, "cached", nil, 0, 1); err != nil {
		t.Fatal(err)
	}
	const fileSize = 512 * 1024
	names := []string{"a.go", "b.go"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(repoDir, name), bytes.Repeat([]byte("a"), fileSize), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := corpusStore{cacheDir: cacheDir, indexDir: indexDir}
	for _, tc := range []struct {
		window int64
		delta  bool
	}{
		{window: 2*fileSize + 1, delta: true},
		{window: 2 * fileSize, delta: false},
	} {
		ctx := context.WithValue(t.Context(), indexWindowKey{}, tc.window)
		if got := uncommittedDeltaPossible(ctx, repoDir, store, "cached", true, names); (got != nil) != tc.delta {
			t.Fatalf("window %d: delta=%v, want %v", tc.window, got != nil, tc.delta)
		}
	}
}

// TestReadUncommittedCandidates_BoundedDrainOnOversize checks that
// readUncommittedCandidates returns errDeltaPayloadExceedsWindow and releases
// every acquired weight when the reads reach the window. Each candidate states
// 1 byte, so the stat sizes pass and the reads reach the window. The drain
// must then release the weight of every read.
func TestReadUncommittedCandidates_BoundedDrainOnOversize(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

	// 12 files of 512 KiB hold 6 MiB, above the 2 MiB window of the 4 MiB
	// budget.
	const fileCount = 12
	const fileSize = 512 * 1024
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	repoDir := t.TempDir()
	candidates := make([]uncommittedCandidate, fileCount)
	for i := range fileCount {
		name := testFileName("dirty", i)
		candidates[i] = uncommittedCandidate{uncommittedManifestEntry: uncommittedManifestEntry{Name: name, Size: 1}}
		path := filepath.Join(repoDir, name)
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		_, candidates[i].Ino, candidates[i].Mtime = fileInfoIdentity(info)
	}

	docs, _, err := readUncommittedCandidates(t.Context(), repoDir, candidates)
	if !errors.Is(err, errDeltaPayloadExceedsWindow) {
		t.Fatalf("expected errDeltaPayloadExceedsWindow, got err=%v docs=%d", err, len(docs))
	}
	if docs != nil {
		t.Fatalf("expected nil docs on bounded-drain abort, got %d", len(docs))
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after bounded drain: got=%d want=%d", got, testBudget)
	}
}

// TestReadUncommittedCandidates_SmallPayloadFitsBudget checks that a payload
// below the window gives every document and no error: the guards do not stop
// a small delta.
func TestReadUncommittedCandidates_SmallPayloadFitsBudget(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 8 * 1024 * 1024 // 8 MiB, so the window is 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

	repoDir := t.TempDir()
	const fileCount = 4
	const fileSize = 256 * 1024 // 1 MiB total < 4 MiB window
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte('z')
	}
	names := make([]string, fileCount)
	for i := 0; i < fileCount; i++ {
		names[i] = fmt.Sprintf("tiny%02d.go", i)
		if err := os.WriteFile(filepath.Join(repoDir, names[i]), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The uncommitted build stats the dirty files with
	// statUncommittedCandidates before it reads them.
	candidates := statUncommittedCandidates(repoDir, names)

	docs, _, err := readUncommittedCandidates(t.Context(), repoDir, candidates)
	if err != nil {
		t.Fatalf("readUncommittedCandidates: %v", err)
	}
	if len(docs) != fileCount {
		t.Fatalf("expected %d docs, got %d", fileCount, len(docs))
	}
	// Release the weights as the caller does (tryUncommittedDelta passes
	// them to indexDeltaDocuments), so the semaphore check below passes.
	releaseFileContentWeights(docs)
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak: got=%d want=%d", got, testBudget)
	}
}
