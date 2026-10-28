package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/sourcegraph/zoekt"
	"golang.org/x/sync/semaphore"
)

// makeFileContent returns a fileContent of payload that holds its read
// weight, as a reader sends it. It takes len(payload) weight from the shared
// readSemaphore, so a test can check that indexDocuments or
// indexDeltaDocuments releases exactly that weight. The caller must hold
// testReadSemMu while it compares the weights.
func makeFileContent(t *testing.T, name string, payload []byte) fileContent {
	t.Helper()
	weight := int64(len(payload))
	if err := readSemaphore.Acquire(t.Context(), weight); err != nil {
		t.Fatalf("Acquire(%d) failed: %v", weight, err)
	}
	return fileContent{name: name, content: payload, weight: weight}
}

// withReadSemLock locks testReadSemMu for a test that checks the weight of
// the shared readSemaphore (availableWeight). It returns a function that fails
// the test when the available weight differs from its start value, and then
// unlocks.
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
// no error: indexDocuments releases every byte of their weight before it
// returns.
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

// TestIndexDocuments_EmptyChannel_NoLeak gives indexDocuments no document:
// it releases no weight.
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

// TestIndexDocuments_BuilderInitFail_ReleasesWeight makes NewBuilder fail,
// because indexDir is a regular file. indexDocuments must still release the
// weight of each queued document. The test fails when NewBuilder succeeds,
// because it then checks no error path.
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

// TestIndexDocuments_AddErrorOutlivesProducerCancel uses a ctags that fails
// each parse. A shardMax of 60 bytes keeps each document below one shard, so
// the Add of the second document flushes the first shard and returns the
// ctags failure. The consumer then cancels its producer. The Git full build passes
// a producer cancel that also ends the consumer's context, as this test does.
// The third document must not replace the add error with that cancellation.
// The documents carry their read weight, and the deferred check finds it
// released.
func TestIndexDocuments_AddErrorOutlivesProducerCancel(t *testing.T) {
	done := withReadSemLock(t)
	defer done()
	// The ctags passes Zoekt's feature check and fails each parse.
	ctags := filepath.Join(t.TempDir(), "parse-failing-ctags")
	writeExecutable(t, ctags, "#!/bin/sh\nif [ \"$1\" = --help ]; then echo +interactive; exit 0; fi\nexit 7\n")
	forceCtagsCommand(t, ctags)
	swapForTest(t, &shardMax, 60)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch := make(chan fileContent, 3)
	for _, name := range []string{"first", "second", "third"} {
		ch <- makeFileContent(t, name+".go", []byte("package "+name+"\nfunc F() {}\n"))
	}
	close(ch)

	_, err := indexDocumentsWithRepository(ctx, t.TempDir(), zoekt.Repository{Name: "test_repo"}, ch, 1, cancel)
	if err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "add document second.go") {
		t.Fatalf("err=%v, want the add error of the failing ctags", err)
	}
}

// TestIndexDeltaDocuments_ReleaseAccounting_Success indexes two documents in
// a delta with no error: indexDeltaDocuments releases every byte of their
// weight before it returns.
func TestIndexDeltaDocuments_ReleaseAccounting_Success(t *testing.T) {
	requireTools(t)
	indexDir := t.TempDir()
	source := t.TempDir()

	// Seed a prior shard for the delta to build on.
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

// TestIndexDeltaDocuments_BuilderInitFail_ReleasesWeight makes the NewBuilder
// of indexDeltaDocuments fail, because indexDir is a regular file. The weight
// of each document must return. The test fails when NewBuilder succeeds.
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

// TestIndexDocuments_StressNoLeakUnderRace reads 12 MiB of files with the
// dirty reader into indexDocuments, under a read budget of 4 MiB. The readers
// queue their Acquires, and the whole budget must return at the end.
// swapReadSemaphoreForTest sets the small budget, so 12 MiB of files fill it
// 3 times.
func TestIndexDocuments_StressNoLeakUnderRace(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	const fileSize = 256 * 1024              // 48 files of 256 KiB: 12 MiB
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

// TestReader_ContextCancelled_NoLeak cancels the context before the dirty
// reader starts. The Acquire of each worker then fails, and the worker returns
// with no weight.
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
	cancel()

	// A document that arrives holds its weight. The loop releases it, so
	// done() checks only the weight that the workers keep. The channel closes
	// after every worker exits, so their deferred releases have run when the
	// loop ends.
	documents, check := streamDirtyFilesAsTextIndex(t, ctx, dir, files, 4)
	for fc := range documents {
		readSemaphore.Release(fc.weight)
	}
	check()
}

// TestReader_OpenFails_NoLeak reads a file whose Lstat passes and whose open
// fails: a regular file with mode 000. The worker must release the weight that
// it took for the file. The test fails when the test user can open the file,
// as root can.
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
	// done() checks that the readSemaphore weight returned to its start value.
}

// TestReader_SymlinkRejected_NoLeak reads a symlink. readOneDirtyFile skips
// it before Acquire, because it is not a regular file. So the reader sends no
// document and takes no weight.
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

// TestReader_BackPressureQueuesAcquires tests the contract of streamDirtyFiles.
// When the weight that the documents hold reaches the budget, the readers queue
// their Acquires, and every file still reaches the channel. The test is the
// consumer: it keeps each document with its weight, and releases the weight of
// a full round at once. The production consumer indexDocuments releases the
// weight of a normal document when Builder.Add returns. So this is no
// end-to-end semaphore test.
//
// TestEnsureFolderCorpusFresh_ManySmallFilesOverBudget (end to end) and
// TestIndexDocuments_ReadsBeyondTheBudget (consumer only) test the
// production reader and consumer under back-pressure.
//
// A test semaphore with a small budget (swapReadSemaphoreForTest) makes the
// readers queue with only 64 MiB of files. The read runs in a synctest
// bubble, where synctest.Wait returns only when each reader waits on its send
// or its Acquire. So when no document is ready after synctest.Wait, the
// readers wait for weight, and the test holds the documents that fill the
// budget.
//
// All bounds come from testBudget and fileSize, so the checks stay true when
// these constants change.
func TestReader_BackPressureQueuesAcquires(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget = 16 * 1024 * 1024 // 16 MiB
	const fileSize = 4 * 1024 * 1024    // 4 MiB: 4 files fill the budget
	const expectedPeakLimit = testBudget / fileSize
	// 8 readers: twice what the budget holds.
	const parallelism = 2 * expectedPeakLimit
	const numFiles = 4 * expectedPeakLimit // 16 files: 4 rounds

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

// swapReadSemaphoreForTest replaces readSemaphore with a new semaphore of
// budget until the returned function restores it. The caller must hold
// testReadSemMu for the swap and for each weight check. A production goroutine
// that reads readSemaphore during the swap would race with it.
//
// It also sets the build slots to budget and the index window
// (indexWindowBytes) to half of budget. So the large-document windows, the
// delta payload limits, and the build slots fit the small budget.
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

// mustHoldTestReadSemMu panics when no goroutine holds testReadSemMu. A swap
// without the lock would race with the production reads of a parallel test.
// A TryLock that succeeds proves that no goroutine held the lock.
func mustHoldTestReadSemMu() {
	if testReadSemMu.TryLock() {
		testReadSemMu.Unlock()
		panic("swap helpers require testReadSemMu held by caller")
	}
}

// swapIndexWindowBytesForTest sets the index window (indexWindowBytes) until
// the returned function restores it. swapReadSemaphoreForTest calls it with
// half of the test budget. The caller must hold testReadSemMu from the swap to
// the restore.
func swapIndexWindowBytesForTest(window int64) func() {
	mustHoldTestReadSemMu()
	old := indexWindowBytes
	indexWindowBytes = window
	return func() { indexWindowBytes = old }
}

// setupPressureTest prepares a pressure test. It requires the tools
// (requireTools), locks testReadSemMu, sets a read budget of budget
// (swapReadSemaphoreForTest), and starts goroutineLeakGuard. It returns the
// function that undoes these steps:
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

// TestFolderReader_FileGrew_NoLeak reads a file that is larger than its walk
// size, so readFolderFile returns errFolderFileGrew.
// streamFolderFilesWithMarkers must release the weight that it took for the
// walk size (readWeight). The error is lasting (folderReadIsLasting). So the
// read continues, and the file stays searchable by name only.
func TestFolderReader_FileGrew_NoLeak(t *testing.T) {
	done := withReadSemLock(t)
	defer done()

	dir := t.TempDir()
	path := filepath.Join(dir, "grew.go")
	// Write more bytes than the candidate states, so the extra-byte check of
	// readFolderFile finds the growth.
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
		size:  claimedSize, // smaller than the file
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
	for fc := range streamFolderFilesWithMarkers(readCtx, []folderCandidate{c}, 1, cancel, make([]int64, 1)) {
		docs = append(docs, fc)
	}
	if cause := context.Cause(readCtx); cause != nil {
		t.Fatalf("the read stopped: %v", cause)
	}
	if len(docs) != 1 || docs[0].name != "grew.go" || len(docs[0].content) != 0 || docs[0].weight != 0 {
		t.Fatalf("docs=%+v, want one name-only document with no weight", docs)
	}
}

// testFileName returns the file name <prefix>_<i>.go.
func testFileName(prefix string, i int) string {
	return prefix + "_" + strconv.Itoa(i) + ".go"
}
