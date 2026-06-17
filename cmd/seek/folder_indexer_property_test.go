package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"testing/quick"
	"time"
)

// TestEnsureFolderCorpusFresh_RandomDistribution — quick-check that
// arbitrary file-size distributions (within caps) drive the streaming
// pipeline to completion without leaking weight.
//
// Sparse files via Truncate keep each iteration cheap. We use a small
// test-local readSemaphore budget so even modest payloads exercise
// back-pressure.
func TestEnsureFolderCorpusFresh_RandomDistribution(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	if testing.Short() {
		t.Skip("property test skipped in -short")
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	// Per-file size cap MUST be <= testBudget so semaphore.Acquire can
	// be satisfied. With testBudget=4 MiB, we cap files at ~1 MiB so
	// up to 4 readers can hold weight concurrently — exercising real
	// back-pressure without unsatisfiable Acquires.
	const perFileCap = testBudget / 4
	// Total corpus cap = a multiple of testBudget so rotation fires
	// multiple times across the run. Kept modest (8×) because each iteration
	// does a real zoekt index build; under -race + coverage this test is the
	// suite's slowest, so larger volumes risk the package test timeout.
	const totalCap = 8 * testBudget

	check := func(seed uint64, n uint8, maxSize uint32) bool {
		if n == 0 || maxSize == 0 {
			return true
		}
		defer goroutineLeakGuard(t, 30*time.Second)()
		rng := rand.New(rand.NewPCG(seed|1, seed^0xDEADBEEF))
		root := t.TempDir()
		var total int64
		for i := 0; i < int(n) && total < totalCap; i++ {
			sz := int64(rng.Uint32() % maxSize)
			if sz > perFileCap {
				sz = perFileCap
			}
			path := filepath.Join(root, fmt.Sprintf("f%04d.bin", i))
			writeSparseFile(t, path, sz)
			total += sz
		}
		plan := planSynthCorpus(t, root)

		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		defer cancel()
		if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			t.Logf("ensureFolderCorpusFresh seed=%d n=%d maxSize=%d: %v", seed, n, maxSize, err)
			return false
		}
		return availableWeight(readSemaphore) == testBudget
	}

	// 10 random distributions exercise the back-pressure property without
	// making this real-indexing test a multi-minute outlier under -race.
	cfg := &quick.Config{MaxCount: 10}
	if err := quick.Check(check, cfg); err != nil {
		t.Fatal(err)
	}
}

// FuzzReadOneFolderFileStreaming_Sizes — fuzz the per-file size
// boundary of readOneFolderFileStreaming (folder_indexer.go:997). For
// any size in [0, maxIndexedDocumentBytes+1] the function must either
// (a) send exactly one fileContent with weight=size and never leak,
// or (b) skip silently without touching the semaphore.
//
// Uses a sparse tempfile so each fuzz input runs cheaply regardless
// of size. The shared readSemaphore is left at production budget —
// a single Acquire of <=maxIndexedDocumentBytes always fits.
func FuzzReadOneFolderFileStreaming_Sizes(f *testing.F) {
	for _, s := range []int64{0, 1, 1024, 64 * 1024, int64(maxIndexedDocumentBytes) - 1, int64(maxIndexedDocumentBytes), int64(maxIndexedDocumentBytes) + 1} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, size int64) {
		if size < 0 || size > int64(maxIndexedDocumentBytes)+1 {
			t.Skip()
		}
		testReadSemMu.Lock()
		defer testReadSemMu.Unlock()
		before := availableWeight(readSemaphore)

		dir := t.TempDir()
		p := filepath.Join(dir, "fuzz.bin")
		writeSparseFile(t, p, size)
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		dev, ino, mtime := fileInfoIdentity(info)
		c := folderCandidate{
			name:  "fuzz.bin",
			path:  p,
			mode:  info.Mode(),
			size:  size,
			mtime: mtime,
			dev:   dev,
			ino:   ino,
		}
		out := make(chan fileContent, 1)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		readOneFolderFileStreaming(ctx, c, out)
		close(out)
		sent := 0
		for fc := range out {
			sent++
			if fc.weight != size {
				t.Fatalf("weight mismatch: got=%d want=%d", fc.weight, size)
			}
			readSemaphore.Release(fc.weight)
		}
		switch {
		case size > int64(maxIndexedDocumentBytes):
			if sent != 0 {
				t.Fatalf("oversize candidate must be skipped, got sent=%d", sent)
			}
		default:
			if sent != 1 {
				t.Fatalf("expected exactly one delivery, got sent=%d", sent)
			}
		}
		if got := availableWeight(readSemaphore); got != before {
			t.Fatalf("semaphore leak: before=%d after=%d", before, got)
		}
	})
}

// TestEnsureFolderCorpusFresh_SingleFileAtPerDocCap — exact-boundary
// case: one file at maxIndexedDocumentBytes, surrounded by near-empty
// files. Verifies the per-doc cap is honoured under the production
// semaphore (NOT swapped, since we need to fit a 100 MiB Acquire).
func TestEnsureFolderCorpusFresh_SingleFileAtPerDocCap(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	if testing.Short() {
		t.Skip("creates a 100 MiB sparse file")
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()
	defer goroutineLeakGuard(t, 120*time.Second)()

	root := t.TempDir()
	writeSparseFile(t, filepath.Join(root, "huge.bin"), maxIndexedDocumentBytes)
	writeSparseFile(t, filepath.Join(root, "tiny.bin"), 16)

	plan := planSynthCorpus(t, root)

	before := availableWeight(readSemaphore)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("ensureFolderCorpusFresh: %v", err)
	}
	if got := availableWeight(readSemaphore); got != before {
		t.Fatalf("semaphore leak: before=%d after=%d", before, got)
	}
}
