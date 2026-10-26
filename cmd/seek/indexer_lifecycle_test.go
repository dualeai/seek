package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"

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
// readSemaphore via availableWeight. It returns a function that checks the
// final weight and unlocks the test mutex.
func withReadSemLock(t *testing.T) func() {
	t.Helper()
	testReadSemMu.Lock()
	before := availableWeight(readSemaphore)
	return func() {
		t.Helper()
		defer testReadSemMu.Unlock()
		after := availableWeight(readSemaphore)
		if after != before {
			t.Fatalf("semaphore leak: before=%d after=%d (delta=%d)",
				before, after, before-after)
		}
	}
}

// TestIndexDocuments_ReleaseAccounting_Success indexes three documents with
// no error: every acquired byte is released when indexDocuments returns.
func TestIndexDocuments_ReleaseAccounting_Success(t *testing.T) {
	requireTools(t)
	done := withReadSemLock(t)
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
	done := withReadSemLock(t)
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
	requireTools(t)
	done := withReadSemLock(t)
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
	requireTools(t)
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

	done := withReadSemLock(t)
	defer done()

	docs := []fileContent{
		makeFileContent(t, "delta_a.go", []byte("package a\nfunc A() {}\n")),
		makeFileContent(t, "delta_b.go", []byte("package b\nfunc B() {}\n")),
	}
	if _, err := indexDeltaDocuments(indexDir, "delta_repo", source, docs, indexParallelism(), []string{"delta_a.go", "delta_b.go"}); err != nil {
		t.Fatalf("indexDeltaDocuments: %v", err)
	}
}

// TestIndexDeltaDocuments_BuilderInitFail_ReleasesWeight — delta-side
// counterpart. Fatal if NewBuilder doesn't fail.
func TestIndexDeltaDocuments_BuilderInitFail_ReleasesWeight(t *testing.T) {
	requireTools(t)
	done := withReadSemLock(t)
	defer done()

	bogusFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bogusFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	docs := []fileContent{
		makeFileContent(t, "x.go", []byte("package x\n")),
	}
	_, err := indexDeltaDocuments(bogusFile, "delta_repo", t.TempDir(), docs, indexParallelism(), []string{"x.go"})
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
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	const fileSize = 256 * 1024              // 256 KiB → 12 MiB total > budget
	const numFiles = 48
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

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

	fileCh, check := streamDirtyFilesAsTextIndex(t, t.Context(), dir, files, 8)
	indexDir := t.TempDir()
	if _, err := indexDocuments(t.Context(), indexDir, "stress_repo", dir, fileCh, 8); err != nil {
		t.Fatalf("indexDocuments: %v", err)
	}
	check()
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak: got=%d want=%d", got, testBudget)
	}
}

// TestReader_ContextCancelled_NoLeak — cancel before any worker starts.
// Workers must observe ctx.Err() from Acquire and return without
// touching the semaphore.
func TestReader_ContextCancelled_NoLeak(t *testing.T) {
	done := withReadSemLock(t)
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

	// Drain whatever races through (a few may have Acquired before
	// cancel propagated). Release those weights so the accounting
	// balances. The channel closes after every worker exits, so their
	// deferred Releases have fired when the loop ends.
	documents, check := streamDirtyFilesAsTextIndex(t, ctx, dir, files, 4)
	for fc := range documents {
		readSemaphore.Release(fc.weight)
	}
	check()
}

// TestReader_OpenFails_NoLeak — a file that passes Lstat but fails
// Open must Release its Acquired weight via the worker sentinel.
// Reliable trigger: a regular file chmod-ed to 0 (unreadable for
// non-root processes on Linux and macOS).
func TestReader_OpenFails_NoLeak(t *testing.T) {
	done := withReadSemLock(t)
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
	if file, err := os.Open(path); err == nil {
		_ = file.Close()
		t.Fatalf("the test user opens the mode-000 file %s, so the open cannot fail", path)
	}

	documents, check := streamDirtyFilesAsTextIndex(t, t.Context(), dir, []string{name}, 1)
	var got []fileContent
	for fc := range documents {
		got = append(got, fc)
	}
	check()
	// The unreadable file stays searchable by name: one document with no
	// content and no weight.
	if len(got) != 1 || got[0].name != name || len(got[0].content) != 0 || got[0].weight != 0 ||
		got[0].skipReason != seekNoBytesReason {
		t.Fatalf("documents=%+v, want one name-only document for the unreadable file", got)
	}
	// withReadSemLock's done() asserts the semaphore returned to baseline.
}

// TestReader_SymlinkRejected_NoLeak — symlinks must be skipped before
// Acquire (they fail the IsRegular check in readOneDirtyFile). No
// weight should be Acquired and no release needed.
func TestReader_SymlinkRejected_NoLeak(t *testing.T) {
	done := withReadSemLock(t)
	defer done()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	link := filepath.Join(dir, "link.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	documents, check := streamDirtyFilesAsTextIndex(t, t.Context(), dir, []string{"link.go"}, 1)
	for fc := range documents {
		t.Errorf("expected symlink to be skipped, got %+v", fc)
		readSemaphore.Release(fc.weight)
	}
	check()
}

// TestReader_BackPressureQueuesAcquires tests the contract of
// streamDirtyFiles: when the bytes in flight reach the budget, the readers
// queue their Acquires, and every file still reaches the channel. The test is
// the consumer: it keeps each document with its weight, and releases the
// weight of a full round at once. It is not the production consumer
// indexDocuments, which releases the weight of a normal document when
// Builder.Add returns, so this is not an end-to-end semaphore test.
//
// TestEnsureFolderCorpusFresh_ManySmallFilesOverBudget (end to end) and
// TestIndexDocuments_ReadsBeyondTheBudget (consumer only) cover the
// production reader and consumer under back-pressure.
//
// A test semaphore with a small budget (swapReadSemaphoreForTest) forces
// the queue without writing hundreds of MiB to disk. The read runs in a
// synctest bubble, where synctest.Wait returns only when each reader waits on
// its send or its Acquire. So when no document is ready after synctest.Wait,
// the readers wait for weight, and the test holds the documents that fill the
// budget.
//
// All bounds derive from `testBudget` and `fileSize`, so the assertions
// stay consistent when these constants change.
func TestReader_BackPressureQueuesAcquires(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget = 16 * 1024 * 1024 // 16 MiB
	const fileSize = 4 * 1024 * 1024    // 4 MiB → budget/file = 4
	const expectedPeakLimit = testBudget / fileSize
	const parallelism = 2 * expectedPeakLimit // 8 → forces queueing
	const numFiles = 4 * expectedPeakLimit    // 16 → multiple rounds

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

	synctest.Test(t, func(t *testing.T) {
		out, check := streamDirtyFilesAsTextIndex(t, t.Context(), dir, files, parallelism)
		indexed := 0
		var held []fileContent
		// endRound requires a full budget of held documents, or all the
		// files that stay, and then releases their weight.
		endRound := func() {
			t.Helper()
			if want := min(expectedPeakLimit, numFiles-indexed); len(held) != want {
				t.Fatalf("the readers stopped with %d documents in flight, want %d (budget=%d B / file=%d B)",
					len(held), want, testBudget, fileSize)
			}
			indexed += len(held)
			releaseFileContentWeights(held)
			held = held[:0]
		}
	read:
		for {
			synctest.Wait()
			select {
			case fc, ok := <-out:
				if !ok {
					endRound()
					break read
				}
				if fc.weight != fileSize {
					t.Fatalf("document %s has weight %d, want %d", fc.name, fc.weight, fileSize)
				}
				held = append(held, fc)
				if len(held) > expectedPeakLimit {
					t.Fatalf("semaphore violated: %d documents in flight > expected %d (budget=%d B / file=%d B)",
						len(held), expectedPeakLimit, testBudget, fileSize)
				}
			default:
				// No reader can send: each one waits for weight.
				endRound()
			}
		}
		check()
		if indexed != numFiles {
			t.Fatalf("saturation: expected %d files indexed, got %d", numFiles, indexed)
		}
	})
}

// swapReadSemaphoreForTest replaces the package-level readSemaphore
// with a fresh one of the given budget for the test's lifetime.
// Caller MUST hold testReadSemMu — both for accounting comparisons
// AND for the swap itself: production goroutines reading readSemaphore
// concurrently with this reassignment would be a data race. Tests
// that use this helper are serialized by the lock by convention.
//
// Also shrinks indexWindowBytes proportionally, so the large-document
// windows, the delta payload limits, and the build slots fit the new
// (smaller) budget.
func swapReadSemaphoreForTest(budget int64) func() {
	mustHoldTestReadSemMu()
	old := readSemaphore
	oldWindows, oldWindowBytes := buildWindows, buildWindowBytes
	readSemaphore = semaphore.NewWeighted(budget)
	// The build slots share the test budget too: a build takes two windows
	// of it, so the slots limit the builds of a small budget.
	buildWindowBytes = budget
	buildWindows = semaphore.NewWeighted(budget)
	restoreWindow := swapIndexWindowBytesForTest(budget / 2)
	return func() {
		restoreWindow()
		readSemaphore = old
		buildWindows, buildWindowBytes = oldWindows, oldWindowBytes
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

// swapIndexWindowBytesForTest sets the index window (indexWindowBytes) for
// the duration of a test. swapReadSemaphoreForTest calls it with half of
// the test budget.
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
// budget, and check for leaked goroutines. It returns the restore function.
//
//	defer setupPressureTest(t, testBudget)()
func setupPressureTest(t *testing.T, budget int64) func() {
	t.Helper()
	requireTools(t)
	testReadSemMu.Lock()
	restoreSem := swapReadSemaphoreForTest(budget)
	guardCleanup := goroutineLeakGuard(t)
	return func() {
		guardCleanup()
		restoreSem()
		testReadSemMu.Unlock()
	}
}

// TestFolderReader_FileGrew_NoLeak — when readFolderFile detects that the
// file grew past its walk size during the read, it returns
// errFolderFileGrew. streamFolderFilesWithMarkers must release the semaphore
// weight it Acquired for the pre-read size (readWeight). The error is
// lasting (folderReadIsLasting), so it does not stop the read, and the file
// stays searchable by name only.
func TestFolderReader_FileGrew_NoLeak(t *testing.T) {
	done := withReadSemLock(t)
	defer done()

	dir := t.TempDir()
	path := filepath.Join(dir, "grew.go")
	// Write more bytes than the candidate claims, so readFolderFile's
	// post-read extra-byte check trips the "file grew beyond its stat size"
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

	// The folder build gives streamFolderFilesWithMarkers the cancel of its
	// read context.
	readCtx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	// The read fails, so the file stays searchable by name only, with no
	// content and no read weight.
	var docs []fileContent
	for fc := range streamFolderFilesWithMarkers(readCtx, []folderCandidate{c}, 1, cancel, nil) {
		docs = append(docs, fc)
	}
	if cause := context.Cause(readCtx); cause != nil {
		t.Fatalf("the read stopped: %v", cause)
	}
	if len(docs) != 1 || docs[0].name != "grew.go" || len(docs[0].content) != 0 || docs[0].weight != 0 {
		t.Fatalf("docs=%+v, want one name-only document with no weight", docs)
	}
}

// testFileName returns a short per-test filename.
func testFileName(prefix string, i int) string {
	return prefix + "_" + strconv.Itoa(i) + ".go"
}
