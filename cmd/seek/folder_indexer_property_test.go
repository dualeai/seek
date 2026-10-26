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

	"github.com/sourcegraph/zoekt/index"
)

// TestEnsureFolderCorpusFresh_RandomDistribution checks with testing/quick that
// a build of random file sizes (each at most a quarter of the test budget)
// completes and gives back all readSemaphore weight.
//
// Sparse files limit fixture I/O. A small test-local readSemaphore budget lets
// small payloads put back pressure on the readers.
func TestEnsureFolderCorpusFresh_RandomDistribution(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	// Keep each file within testBudget so semaphore.Acquire can succeed. A
	// quarter-budget limit lets up to four readers hold weight at the same
	// time.
	const perFileCap = testBudget / 4
	// Total corpus cap = a multiple of testBudget so the reads pass the budget
	// several times across the run. Kept modest (8×) because each iteration
	// does a real zoekt index build; under -race + coverage this test is the
	// suite's slowest, so larger volumes risk the package test timeout.
	const totalCap = 8 * testBudget

	check := func(seed uint64, n uint8, maxSize uint32) bool {
		if n == 0 || maxSize == 0 {
			return true
		}
		defer goroutineLeakGuard(t)()
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

		if _, err := ensureFolderCorpusFresh(t.Context(), plan); err != nil {
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
// readOneFolderFileStreaming. The fuzzer's size folds into
// [0, testDocumentContentLimit+1], so every input tests a size. For each size,
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
		const sizes = testDocumentContentLimit + 2
		size = (size%sizes + sizes) % sizes
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
		// The walk makes candidates with newFolderCandidate, and the build
		// gives the reader a failure function.
		c := newFolderCandidate("fuzz.bin", p, info)
		out := make(chan fileContent, 1)
		ctx, fail := context.WithCancelCause(t.Context())
		defer fail(nil)
		readOneFolderFileStreaming(ctx, c, out, fail)
		close(out)
		if cause := context.Cause(ctx); cause != nil {
			t.Fatalf("the read reported a failure: %v", cause)
		}
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

// TestEnsureFolderCorpusFresh_SingleFileAroundReadWeightCap builds a folder
// with one file of maxReadWeightBytes, and then of one byte more, next to a
// 16-byte file. The production semaphore (not swapped) must admit the first
// size and clamp the weight of the second, with no hang and no leaked weight.
// A hang runs until the go test timeout, which prints each goroutine.
func TestEnsureFolderCorpusFresh_SingleFileAroundReadWeightCap(t *testing.T) {
	requireTools(t)
	for _, size := range []int64{maxReadWeightBytes, maxReadWeightBytes + 1} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			testReadSemMu.Lock()
			defer testReadSemMu.Unlock()
			defer goroutineLeakGuard(t)()

			root := t.TempDir()
			writeSparseFile(t, filepath.Join(root, "huge.bin"), size)
			writeSparseFile(t, filepath.Join(root, "tiny.bin"), 16)

			plan := planSynthCorpus(t, root)

			before := availableWeight(readSemaphore)
			if _, err := ensureFolderCorpusFresh(t.Context(), plan); err != nil {
				t.Fatalf("ensureFolderCorpusFresh: %v", err)
			}
			if got := availableWeight(readSemaphore); got != before {
				t.Fatalf("semaphore leak: before=%d after=%d", before, got)
			}
		})
	}
}
