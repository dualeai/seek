package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/quick"
	"time"

	"github.com/sourcegraph/zoekt/index"
)

// TestEnsureFolderCorpusFresh_RandomDistribution — quick-check that
// arbitrary file-size distributions (within caps) drive the streaming
// pipeline to completion without leaking weight.
//
// Sparse files limit fixture I/O. A small test-local readSemaphore budget lets
// modest payloads exercise back pressure.
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

	// Keep each file within testBudget so semaphore.Acquire can succeed. A
	// quarter-budget limit lets up to four readers hold weight at the same time.
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

	// Ten random distributions exercise back pressure while keeping the test
	// bounded under the race detector.
	cfg := &quick.Config{MaxCount: 10}
	if err := quick.Check(check, cfg); err != nil {
		t.Fatal(err)
	}
}

// FuzzReadOneFolderFileStreaming_Sizes checks the per-file size boundary of
// readOneFolderFileStreaming. For any size in [0, testDocumentContentLimit+1],
// the function must either
// (a) send exactly one fileContent with weight=size and never leak,
// or (b) send one name-only document without touching the semaphore.
//
// The test uses writeSparseFile to limit fixture I/O. It keeps the shared
// readSemaphore at the production budget and swaps in a small content limit.
func FuzzReadOneFolderFileStreaming_Sizes(f *testing.F) {
	for _, s := range []int64{0, 1, 1024, 64 * 1024, testDocumentContentLimit - 1, testDocumentContentLimit, testDocumentContentLimit + 1} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, size int64) {
		swapDocumentContentLimitForTest(t, testDocumentContentLimit)
		if size < 0 || size > testDocumentContentLimit+1 {
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
		// Every fuzzed size is far below the 100 MiB read-weight clamp, so a
		// document that keeps its content weighs its size.
		oversize := size > testDocumentContentLimit
		for fc := range out {
			sent++
			wantWeight := size
			if oversize {
				wantWeight = 0
			}
			if fc.weight != wantWeight {
				t.Fatalf("weight mismatch: got=%d want=%d", fc.weight, wantWeight)
			}
			if oversize && (fc.skipReason != index.SkipReasonTooLarge || fc.content != nil) {
				t.Fatalf("oversize candidate = skip %v with %d content bytes, want name-only", fc.skipReason, len(fc.content))
			}
			readSemaphore.Release(fc.weight)
		}
		switch {
		case oversize:
			if sent != 1 {
				t.Fatalf("oversize candidate must send one name-only document, got sent=%d", sent)
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

// TestEnsureFolderCorpusFresh_SingleFileAroundReadWeightCap — boundary
// cases: one file at maxReadWeightBytes and one byte above it, next to a
// near-empty file. The production semaphore (NOT swapped) must admit the
// first and clamp the second, with no hang and no leaked weight.
func TestEnsureFolderCorpusFresh_SingleFileAroundReadWeightCap(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	if testing.Short() {
		t.Skip("creates 100 MiB sparse files")
	}
	for _, size := range []int64{maxReadWeightBytes, maxReadWeightBytes + 1} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			testReadSemMu.Lock()
			defer testReadSemMu.Unlock()
			defer goroutineLeakGuard(t, 120*time.Second)()

			root := t.TempDir()
			writeSparseFile(t, filepath.Join(root, "huge.bin"), size)
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
		})
	}
}
