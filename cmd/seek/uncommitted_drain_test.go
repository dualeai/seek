package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadUncommittedCandidates_BoundedDrainOnOversize checks that
// readUncommittedCandidates returns errDeltaPayloadExceedsWindow and releases
// every pre-acquired weight when the candidate set reaches the window. It
// covers both guards:
//   - pre-flight: no file exists, so only the lstat-sum short-circuit
//     can return the error; it reads no file.
//   - mid-drain: each candidate states 1 byte, so the pre-flight sum
//     passes and the real reads cross the window; the drain must then
//     release the weight of every read.
func TestReadUncommittedCandidates_BoundedDrainOnOversize(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	// Fixture: 12 files × 512 KiB = 6 MiB cumulative > window (= 2 MiB).
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
			defer goroutineLeakGuard(t, 30*time.Second)()
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

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			docs, _, err := readUncommittedCandidates(ctx, repoDir, candidates)
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

// TestReadUncommittedCandidates_SmallPayloadFitsBudget — control: a
// payload that fits within the window threshold returns docs and nil
// error. Confirms the bounded-drain guard does NOT trip on benign
// inputs.
func TestReadUncommittedCandidates_SmallPayloadFitsBudget(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 8 * 1024 * 1024 // 8 MiB → window = 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 15*time.Second)()

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
