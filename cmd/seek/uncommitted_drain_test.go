package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestReadUncommittedCandidates_BoundedDrainOnOversize checks that
// readUncommittedCandidates returns errDeltaPayloadExceedsWindow and releases
// every acquired weight when the candidates reach the window. It covers both
// guards:
//   - pre-flight: no file exists, so only the check of the stat sizes can
//     return the error. It reads no file.
//   - mid-drain: each candidate states 1 byte, so the stat sizes pass and the
//     reads reach the window. The drain must then release the weight of every
//     read.
func TestReadUncommittedCandidates_BoundedDrainOnOversize(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	// 12 files of 512 KiB hold 6 MiB, above the 2 MiB window of the 4 MiB
	// budget.
	const fileCount = 12
	const fileSize = 512 * 1024
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	for _, tc := range []struct {
		name          string
		writeFiles    bool
		candidateSize int64
	}{
		{name: "pre-flight", writeFiles: false, candidateSize: fileSize},
		{name: "mid-drain", writeFiles: true, candidateSize: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer goroutineLeakGuard(t)()
			repoDir := t.TempDir()
			candidates := make([]uncommittedCandidate, fileCount)
			for i := 0; i < fileCount; i++ {
				name := testFileName("dirty", i)
				candidates[i] = uncommittedCandidate{name: name, size: tc.candidateSize}
				if !tc.writeFiles {
					continue
				}
				path := filepath.Join(repoDir, name)
				if err := os.WriteFile(path, payload, 0o644); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				_, candidates[i].ino, candidates[i].mtime = fileInfoIdentity(info)
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
		})
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
