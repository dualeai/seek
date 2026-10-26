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
	"testing/synctest"

	"github.com/sourcegraph/zoekt/index"
)

// TestEnsureFolderCorpusFresh_RandomDistribution builds folders of random
// file sizes with testing/quick. Each file is at most a quarter of the test
// budget. Each build must complete and give back all readSemaphore weight.
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
	// Each folder holds at most 8 budgets, so its reads fill the budget
	// several times. Each iteration runs a real Zoekt build, so a larger
	// folder makes the test slow under -race and coverage.
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
// the function sends exactly one document and leaks no weight. A file within
// the limit gives a document with weight=size. A larger file gives a name-only
// document with no weight.
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
		if sent != 1 {
			t.Fatalf("size %d sent %d documents, want 1", size, sent)
		}
		if got := availableWeight(readSemaphore); got != before {
			t.Fatalf("semaphore leak: before=%d after=%d", before, got)
		}
	})
}

// TestEnsureFolderCorpusFresh_SingleFileAroundReadWeightCap reads a file of
// maxReadWeightBytes (100 MiB), and then of one byte more. The reader takes
// 100 MiB of weight for each size before it reads the file, because readWeight
// stops there. The read runs in a synctest bubble on a budget of 100 MiB that
// the test holds. The test changes the head of the file while the reader waits
// for its weight, and then frees the budget. So the sent document has the new
// head and 100 MiB of weight. A larger weight waits forever, and the bubble
// fails as a deadlock. Then the test builds a folder of that file and a
// 16-byte file with the production semaphore (not swapped): no hang and no
// leaked weight. A hang runs until the go test timeout, which prints each
// goroutine.
func TestEnsureFolderCorpusFresh_SingleFileAroundReadWeightCap(t *testing.T) {
	requireTools(t)
	for _, size := range []int64{maxReadWeightBytes, maxReadWeightBytes + 1} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			testReadSemMu.Lock()
			defer testReadSemMu.Unlock()
			defer goroutineLeakGuard(t)()

			root := t.TempDir()
			huge := filepath.Join(root, "huge.bin")
			writeSparseFile(t, huge, size)
			writeSparseFile(t, filepath.Join(root, "tiny.bin"), 16)
			before := availableWeight(readSemaphore)

			info, err := os.Lstat(huge)
			if err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				defer swapReadSemaphoreForTest(maxReadWeightBytes)()
				// Busy readers hold the whole budget, so the reader waits for
				// its weight.
				if !readSemaphore.TryAcquire(maxReadWeightBytes) {
					t.Fatal("cannot hold the test read budget")
				}
				out := make(chan fileContent, 1)
				ctx, fail := context.WithCancelCause(t.Context())
				defer fail(nil)
				go readOneFolderFileStreaming(ctx, newFolderCandidate("huge.bin", huge, info), out, fail)
				synctest.Wait()
				file, err := os.OpenFile(huge, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt([]byte("y"), 0)
				if closeErr := file.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatal(err)
				}
				readSemaphore.Release(maxReadWeightBytes)
				document := <-out
				held := maxReadWeightBytes - availableWeight(readSemaphore)
				readSemaphore.Release(document.weight)
				if document.weight != 100<<20 || held != 100<<20 {
					t.Fatalf("the read of %d bytes holds %d bytes of weight (document %d), want 100 MiB", size, held, document.weight)
				}
				if len(document.content) == 0 || document.content[0] != 'y' {
					t.Fatalf("the read of %d bytes ran before the reader took its weight: head %q", size, document.content[:min(len(document.content), 1)])
				}
			})

			plan := planSynthCorpus(t, root)
			if _, err := ensureFolderCorpusFresh(t.Context(), plan); err != nil {
				t.Fatalf("ensureFolderCorpusFresh: %v", err)
			}
			if got := availableWeight(readSemaphore); got != before {
				t.Fatalf("semaphore leak: before=%d after=%d", before, got)
			}
		})
	}
}
