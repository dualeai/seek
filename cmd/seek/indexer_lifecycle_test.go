package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
)

// makeFileContent fabricates a fileContent that mimics a reader-side
// Acquire: it Acquires `size` weight against the shared readSemaphore
// so subsequent indexDocuments / indexDeltaDocuments accounting can
// validate that exactly that weight gets Released. Callers MUST hold
// testReadSemMu for the duration of any accounting comparison.
func makeFileContent(t *testing.T, name string, payload []byte) fileContent {
	t.Helper()
	weight := int64(len(payload))
	if err := readSemaphore.Acquire(t.Context(), weight); err != nil {
		t.Fatalf("Acquire(%d) failed: %v", weight, err)
	}
	return fileContent{name: name, content: payload, weight: weight}
}

// withReadSemLock serializes accounting tests that observe the shared
// readSemaphore via availableWeight. Returns a captured baseline and
// a finalize() that re-checks and reports any delta. Centralizes the
// before/after pattern so individual tests can't forget to compare.
func withReadSemLock(t *testing.T) (before int64, finalize func()) {
	t.Helper()
	testReadSemMu.Lock()
	before = availableWeight(readSemaphore)
	finalize = func() {
		t.Helper()
		defer testReadSemMu.Unlock()
		after := availableWeight(readSemaphore)
		if after != before {
			t.Fatalf("semaphore leak: before=%d after=%d (delta=%d)",
				before, after, before-after)
		}
	}
	return before, finalize
}

// TestIndexDocuments_ReleaseAccounting_Success — happy path: every
// Acquired byte is Released after Finish.
func TestIndexDocuments_ReleaseAccounting_Success(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	_, done := withReadSemLock(t)
	defer done()

	indexDir := t.TempDir()
	source := t.TempDir()
	ch := make(chan fileContent, 3)
	for _, d := range []fileContent{
		makeFileContent(t, "a.go", []byte("package a\nfunc A() {}\n")),
		makeFileContent(t, "b.go", []byte("package b\nfunc B() {}\n")),
		makeFileContent(t, "c.go", []byte("package c\nfunc C() {}\n")),
	} {
		ch <- d
	}
	close(ch)

	indexed, err := indexDocuments(t.Context(), indexDir, "test_repo", source, ch, 1)
	if err != nil {
		t.Fatalf("indexDocuments: %v", err)
	}
	if !indexed {
		t.Fatal("expected indexed=true")
	}
}

// TestIndexDocuments_EmptyChannel_NoLeak — zero docs received → no
// Acquire happened on this path → no Release should fire.
func TestIndexDocuments_EmptyChannel_NoLeak(t *testing.T) {
	_, done := withReadSemLock(t)
	defer done()

	indexDir := t.TempDir()
	source := t.TempDir()
	ch := make(chan fileContent)
	close(ch)

	if _, err := indexDocuments(t.Context(), indexDir, "test_repo", source, ch, 1); err != nil {
		t.Fatalf("indexDocuments: %v", err)
	}
}

// TestIndexDocuments_BuilderInitFail_ReleasesWeight — when NewBuilder
// fails (indexDir is a regular file), every already-queued doc must
// still have its weight Released.
//
// Fatal (not Skip) if NewBuilder unexpectedly succeeds: that would
// mean Zoekt got more permissive and we lost the precondition that
// makes this test meaningful.
func TestIndexDocuments_BuilderInitFail_ReleasesWeight(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	_, done := withReadSemLock(t)
	defer done()

	bogusFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bogusFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ch := make(chan fileContent, 2)
	ch <- makeFileContent(t, "first.go", []byte("package first\n"))
	ch <- makeFileContent(t, "second.go", []byte("package second\n"))
	close(ch)

	_, err := indexDocuments(t.Context(), bogusFile, "test_repo", t.TempDir(), ch, 1)
	if err == nil {
		t.Fatal("expected NewBuilder failure with regular-file indexDir; precondition lost")
	}
}

// TestIndexDeltaDocuments_ReleaseAccounting_Success — same accounting
// check for the delta entry point.
func TestIndexDeltaDocuments_ReleaseAccounting_Success(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	indexDir := t.TempDir()
	source := t.TempDir()

	// Seed a prior shard (delta builder requires one).
	{
		testReadSemMu.Lock()
		seedCh := make(chan fileContent, 1)
		seedCh <- makeFileContent(t, "seed.go", []byte("package seed\nfunc Seed() {}\n"))
		close(seedCh)
		if _, err := indexDocuments(t.Context(), indexDir, "delta_repo", source, seedCh, 1); err != nil {
			testReadSemMu.Unlock()
			t.Fatalf("seed indexDocuments: %v", err)
		}
		testReadSemMu.Unlock()
	}

	_, done := withReadSemLock(t)
	defer done()

	docs := []fileContent{
		makeFileContent(t, "delta_a.go", []byte("package a\nfunc A() {}\n")),
		makeFileContent(t, "delta_b.go", []byte("package b\nfunc B() {}\n")),
	}
	if _, err := indexDeltaDocuments(indexDir, "delta_repo", source, docs, shardMax, []string{"delta_a.go", "delta_b.go"}); err != nil {
		t.Fatalf("indexDeltaDocuments: %v", err)
	}
}

// TestIndexDeltaDocuments_BuilderInitFail_ReleasesWeight — delta-side
// counterpart. Fatal if NewBuilder doesn't fail.
func TestIndexDeltaDocuments_BuilderInitFail_ReleasesWeight(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	_, done := withReadSemLock(t)
	defer done()

	bogusFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bogusFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	docs := []fileContent{
		makeFileContent(t, "x.go", []byte("package x\n")),
	}
	_, err := indexDeltaDocuments(bogusFile, "delta_repo", t.TempDir(), docs, shardMax, []string{"x.go"})
	if err == nil {
		t.Fatal("expected NewBuilder failure with regular-file indexDir; precondition lost")
	}
}

// TestIndexDocuments_StressNoLeakUnderRace drives the reader and consumer with
// cumulative content above a test-local semaphore budget.
//
// Uses swapReadSemaphoreForTest so the test stays cheap (~12 MiB of
// payload) while still driving Acquire queueing through the real
// production consumer (indexDocuments).
func TestIndexDocuments_StressNoLeakUnderRace(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short")
	}
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	const fileSize = 256 * 1024              // 256 KiB → 12 MiB total > budget
	const numFiles = 48
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 60*time.Second)()

	dir := t.TempDir()
	files := make([]string, numFiles)
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	for i := range numFiles {
		name := testFileName("f", i)
		files[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fileCh := streamFiles(t.Context(), dir, files, 8)
	indexDir := t.TempDir()
	if _, err := indexDocuments(t.Context(), indexDir, "stress_repo", dir, fileCh, 8); err != nil {
		t.Fatalf("indexDocuments: %v", err)
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak: got=%d want=%d", got, testBudget)
	}
}

// TestReader_ContextCancelled_NoLeak — cancel before any worker starts.
// Workers must observe ctx.Err() from Acquire and return without
// touching the semaphore.
func TestReader_ContextCancelled_NoLeak(t *testing.T) {
	_, done := withReadSemLock(t)
	defer done()

	dir := t.TempDir()
	const numFiles = 16
	files := make([]string, numFiles)
	for i := range numFiles {
		name := testFileName("g", i)
		files[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before any worker starts

	out := make(chan fileContent)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		readFilesToChannel(ctx, dir, files, 4, out)
	}()
	// Drain whatever races through (a few may have Acquired before
	// cancel propagated). Release those weights so the accounting
	// balances. By the time the outer wg.Wait returns, all
	// readFilesToChannel inner workers have exited and their deferred
	// Releases have fired — no settle delay needed.
	for fc := range out {
		readSemaphore.Release(fc.weight)
	}
	wg.Wait()
}

// TestReader_OpenFails_NoLeak — a file that passes Lstat but fails
// Open must Release its Acquired weight via the worker sentinel.
// Reliable trigger: a regular file chmod-ed to 0 (unreadable for
// non-root processes on Linux and macOS).
func TestReader_OpenFails_NoLeak(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0 cannot block root; skipping unreadable-file test")
	}
	_, done := withReadSemLock(t)
	defer done()

	dir := t.TempDir()
	name := "unreadable.go"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	out := make(chan fileContent, 1)
	// readFilesToChannel closes out before returning.
	readFilesToChannel(t.Context(), dir, []string{name}, 1, out)
	for fc := range out {
		t.Fatalf("expected no fileContent for unreadable file, got %+v", fc)
	}
	// withReadSemLock's done() asserts the semaphore returned to baseline.
}

// TestReader_SymlinkRejected_NoLeak — symlinks must be skipped before
// Acquire (they fail the IsRegular check in readOneDirtyFile). No
// weight should be Acquired and no release needed.
func TestReader_SymlinkRejected_NoLeak(t *testing.T) {
	_, done := withReadSemLock(t)
	defer done()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	link := filepath.Join(dir, "link.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}

	out := make(chan fileContent, 1)
	// readFilesToChannel closes out before returning.
	readFilesToChannel(t.Context(), dir, []string{"link.go"}, 1, out)
	for fc := range out {
		t.Fatalf("expected symlink to be skipped, got %+v", fc)
	}
}

// TestReader_BackPressureQueuesAcquires — focused unit on
// readFilesToChannel's contract: when concurrent in-flight bytes
// exceed the budget, the reader pool queues Acquires correctly and
// every file eventually delivered through the channel pipeline. The
// CONSUMER here is a fake fan-out pool that Releases per-doc — NOT
// the production indexDocuments consumer (which Releases per shard
// window). Don't confuse this for an end-to-end semaphore test.
//
// For full reader → production consumer coverage under back-pressure,
// see TestEnsureFolderCorpusFresh_ManySmallFilesOverBudget (E2E) and
// TestIndexDocuments_StallsHoldingMoreThanBudget (consumer-only).
//
// Uses a TEST-LOCAL semaphore (swapReadSemaphoreForTest) with a
// small budget so we can force queueing without writing hundreds of
// MiB to disk. Spawns parallel consumers — a single sleeping
// consumer would serialize the downstream and make the peak counter
// meaningless (workers all block on unbuffered send, peak=1 always).
//
// All bounds derive from `testBudget` and `fileSize` so the
// assertions remain self-consistent if these constants are tuned.
func TestReader_BackPressureQueuesAcquires(t *testing.T) {
	if testing.Short() {
		t.Skip("saturation test writes ~64 MiB to disk; skipped in -short")
	}
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget = 16 * 1024 * 1024 // 16 MiB
	const fileSize = 4 * 1024 * 1024    // 4 MiB → budget/file = 4
	const expectedPeakLimit = testBudget / fileSize
	const parallelism = 2 * expectedPeakLimit // 8 → forces queueing
	const numConsumers = parallelism          // match readers
	const numFiles = 4 * expectedPeakLimit    // 16 → multiple rounds
	const consumerHold = 3 * time.Millisecond

	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	dir := t.TempDir()
	files := make([]string, numFiles)
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	for i := range numFiles {
		name := testFileName("big", i)
		files[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := make(chan fileContent)
	go readFilesToChannel(t.Context(), dir, files, parallelism, out)

	var indexed, concurrent, maxConcurrent int32
	var wg sync.WaitGroup
	for range numConsumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fc := range out {
				cur := atomic.AddInt32(&concurrent, 1)
				for {
					prev := atomic.LoadInt32(&maxConcurrent)
					if cur <= prev || atomic.CompareAndSwapInt32(&maxConcurrent, prev, cur) {
						break
					}
				}
				time.Sleep(consumerHold)
				atomic.AddInt32(&concurrent, -1)
				atomic.AddInt32(&indexed, 1)
				readSemaphore.Release(fc.weight)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&indexed); got != numFiles {
		t.Fatalf("saturation: expected %d files indexed, got %d", numFiles, got)
	}
	peak := atomic.LoadInt32(&maxConcurrent)
	if peak > int32(expectedPeakLimit) {
		t.Fatalf("semaphore violated: peak in-flight=%d > expected %d (budget=%d B / file=%d B)",
			peak, expectedPeakLimit, testBudget, fileSize)
	}
	// Sanity: under genuine saturation, peak should sit between 2 and
	// expectedPeakLimit. peak < 2 means the workload never queued any
	// concurrent Acquires — the test ran too fast or the scheduler
	// serialized everything — and proves nothing about back-pressure.
	if peak < 2 {
		t.Fatalf("no queueing observed: peak=%d — test failed to saturate the semaphore", peak)
	}
}

// swapReadSemaphoreForTest replaces the package-level readSemaphore
// with a fresh one of the given budget for the test's lifetime.
// Caller MUST hold testReadSemMu — both for accounting comparisons
// AND for the swap itself: production goroutines reading readSemaphore
// concurrently with this reassignment would be a data race. Tests
// that use this helper are serialized by the lock by convention.
//
// Also shrinks indexWindowBytes proportionally so shardWindow rotation
// in indexDocuments triggers at the new (smaller) budget. Without
// this, rotation would not fire until defaultIndexWindowBytes of weight
// accumulated and tests using small synthesised budgets would still
// deadlock.
func swapReadSemaphoreForTest(budget int64) func() {
	mustHoldTestReadSemMu()
	old := readSemaphore
	readSemaphore = semaphore.NewWeighted(budget)
	restoreWindow := swapIndexWindowBytesForTest(budget / 2)
	return func() {
		restoreWindow()
		readSemaphore = old
	}
}

// mustHoldTestReadSemMu panics if testReadSemMu is not held by some
// goroutine. Catches the "forgot the lock" bug that would silently
// race readSemaphore swaps with concurrent production reads if a
// future test added t.Parallel(). TryLock is reverse-logic: success
// means nobody held the lock when we tried.
func mustHoldTestReadSemMu() {
	if testReadSemMu.TryLock() {
		testReadSemMu.Unlock()
		panic("swap helpers require testReadSemMu held by caller")
	}
}

// swapIndexWindowBytesForTest sets the windowed-indexer rotation
// threshold for the duration of a test. Pair with swapReadSemaphoreForTest
// so the invariant indexWindowBytes + maxIndexedDocumentBytes <=
// semaphore budget still holds in the test scope.
//
// Caller MUST hold testReadSemMu for the entire swap → action → restore
// sequence.
func swapIndexWindowBytesForTest(window int64) func() {
	mustHoldTestReadSemMu()
	old := indexWindowBytes
	indexWindowBytes = window
	return func() { indexWindowBytes = old }
}

// setupPressureTest bundles the boilerplate every pressure test repeats:
// require ctags, hold testReadSemMu, swap the readSemaphore + window
// budget, arm the goroutine-leak watchdog. Returns the restore func to
// defer. Use:
//
//	defer setupPressureTest(t, testBudget, 60*time.Second)()
func setupPressureTest(t *testing.T, budget int64, leakDeadline time.Duration) func() {
	t.Helper()
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	restoreSem := swapReadSemaphoreForTest(budget)
	guardCleanup := goroutineLeakGuard(t, leakDeadline)
	return func() {
		guardCleanup()
		restoreSem()
		testReadSemMu.Unlock()
	}
}

// TestFolderReader_FileGrew_NoLeak — when readFolderFile detects the
// file grew past its cap mid-read, it returns an error. The
// readOneFolderFileStreaming caller must release the semaphore weight
// it Acquired against the pre-read size.
func TestFolderReader_FileGrew_NoLeak(t *testing.T) {
	_, done := withReadSemLock(t)
	defer done()

	dir := t.TempDir()
	path := filepath.Join(dir, "grew.go")
	// Write more bytes than the candidate claims, so readFolderFile's
	// post-read extra-byte check trips the "file grew beyond max size"
	// branch.
	const claimedSize = 16
	if err := os.WriteFile(path, []byte("package grew_more_than_claimed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino, mtime := fileInfoIdentity(info)
	c := folderCandidate{
		name:  "grew.go",
		path:  path,
		mode:  info.Mode(),
		size:  claimedSize, // lies about size to trigger the grew check
		mtime: mtime,
		dev:   dev,
		ino:   ino,
	}

	out := make(chan fileContent, 1)
	readOneFolderFileStreaming(t.Context(), c, out)
	close(out)
	for fc := range out {
		t.Fatalf("expected file-grew rejection, got %+v", fc)
	}
}

// testFileName returns a short per-test filename. Kept local to this
// file to avoid leaking helpers into the wider test surface.
func testFileName(prefix string, i int) string {
	return prefix + "_" + strconv.Itoa(i) + ".go"
}
