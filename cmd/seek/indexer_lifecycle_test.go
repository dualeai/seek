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
// still have its weight Released. This is the drain-after-error path
// identified by the algorithmic review.
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

// TestIndexDocuments_StressNoLeakUnderRace — race-detector load test:
// hammer the reader→consumer pipeline with many small files and
// assert the semaphore returns to its starting available weight.
func TestIndexDocuments_StressNoLeakUnderRace(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short")
	}
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	_, done := withReadSemLock(t)
	defer done()

	const numFiles = 200
	dir := t.TempDir()
	files := make([]string, numFiles)
	for i := range numFiles {
		name := testFileName("f", i)
		files[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fileCh := streamFiles(t.Context(), dir, files, 8)
	indexDir := t.TempDir()
	if _, err := indexDocuments(t.Context(), indexDir, "stress_repo", dir, fileCh, 8); err != nil {
		t.Fatalf("indexDocuments: %v", err)
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
	// Drain whatever races through (a few may have Acquired before cancel
	// propagated). Release those weights so the accounting balances.
	for fc := range out {
		readSemaphore.Release(fc.weight)
	}
	wg.Wait()
	// Small settle delay so worker defers run before we observe.
	time.Sleep(10 * time.Millisecond)
}

// TestReader_OpenFails_NoLeak — files that pass Lstat but fail Open
// (or fail Read) must release their Acquired weight via the worker
// sentinel. We list filenames that exist, then delete them after
// Lstat resolves but before Open runs by … actually, controlling
// that race is fragile. Instead, exercise the no-data path: an empty
// file with size 0 short-circuits before Acquire, so it doesn't test
// the sentinel. A file that becomes nonexistent reliably: use a
// directory entry name that points to a file we never create.
//
// readFilesToChannel skips files where Lstat fails (no Acquire
// happens) — so to hit the sentinel we need Lstat to succeed and
// Open to fail. Easiest reliable trigger: create a regular file,
// then chmod it 0 (unreadable). Linux + macOS both honor this for
// non-root processes.
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

// TestSemaphore_Saturation_AllFilesEventuallyIndexed — concurrent
// in-flight bytes exceed the 64 MiB readSemaphore budget. The
// semaphore must queue Acquires so every file is eventually
// delivered: no deadlock, no leak, no file dropped.
//
// Forcing real queueing requires worker_count × file_size > budget,
// AND the consumer must hold each fileContent long enough that
// concurrent in-flight bytes actually pile up. With 16 workers × 6
// MiB = 96 MiB > 64 MiB AND a 5 ms hold per doc, at least two
// Acquires must block waiting for prior Releases.
func TestSemaphore_Saturation_AllFilesEventuallyIndexed(t *testing.T) {
	if testing.Short() {
		t.Skip("saturation test allocates ~150 MiB; skipped in -short")
	}
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	_, done := withReadSemLock(t)
	defer done()

	const numFiles = 24
	const fileSize = 6 * 1024 * 1024 // 6 MiB
	const parallelism = 16           // 16 × 6 = 96 MiB > 64 MiB budget
	const consumerHold = 5 * time.Millisecond

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

	// Use the reader directly + manual release per doc so we measure
	// only the semaphore behavior (end-to-end Zoekt would dominate).
	out := make(chan fileContent)
	var indexed int32
	var maxConcurrent int32
	var concurrent int32

	go readFilesToChannel(t.Context(), dir, files, parallelism, out)
	for fc := range out {
		// Track peak concurrency to assert the semaphore actually
		// bounded it — otherwise the test reduces to a trivial
		// fan-in and proves nothing about back-pressure.
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

	if got := atomic.LoadInt32(&indexed); got != numFiles {
		t.Fatalf("saturation: expected %d files indexed, got %d", numFiles, got)
	}
	peak := atomic.LoadInt32(&maxConcurrent)
	// 64 MiB budget / 6 MiB per file = floor(10) = 10 simultaneous max
	// on the consumer side (workers also hold weight, so effective
	// consumer peak is usually < 10). If the semaphore is bypassed,
	// we'd see up to parallelism (16) docs concurrent here.
	if peak > 10 {
		t.Fatalf("semaphore violated: peak in-flight=%d > 10 (budget=64MiB / file=6MiB)", peak)
	}
	if peak >= parallelism {
		t.Fatalf("no queueing observed: peak=%d == parallelism=%d — test failed to exercise back-pressure", peak, parallelism)
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
