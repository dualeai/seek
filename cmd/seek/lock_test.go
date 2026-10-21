package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestLockFileExclusive_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if err := lockFileExclusive(f); err != nil {
		t.Fatalf("expected lock to succeed, got %v", err)
	}
	unlockFile(f)
}

func TestLockFileExclusive_Contention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lock")

	f1, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f1.Close() })

	f2, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })

	// Acquire on fd1
	if err := lockFileExclusive(f1); err != nil {
		t.Fatalf("first lock should succeed: %v", err)
	}

	// Non-blocking attempt on fd2 should fail
	err = lockFileExclusive(f2)
	if err == nil {
		t.Fatal("expected second lock to fail while first is held")
	}

	// Release fd1, now fd2 should succeed
	unlockFile(f1)
	if err := lockFileExclusive(f2); err != nil {
		t.Fatalf("second lock should succeed after release: %v", err)
	}
	unlockFile(f2)
}

func TestLockFileExclusive_ReleaseAndReacquire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if err := lockFileExclusive(f); err != nil {
		t.Fatal(err)
	}
	unlockFile(f)

	// Re-acquire on same fd
	if err := lockFileExclusive(f); err != nil {
		t.Fatalf("re-acquire should succeed: %v", err)
	}
	unlockFile(f)
}

func TestAcquireBuildLock_SingleFlight(t *testing.T) {
	dir := t.TempDir()
	indexDir := filepath.Join(dir, "index")
	first, acquired, err := corpusStore{cacheDir: dir, indexDir: indexDir}.acquireBuildLock(context.Background())
	if err != nil || !acquired {
		t.Fatalf("first acquireBuildLock: acquired=%v err=%v", acquired, err)
	}
	// A second builder must NOT acquire while the first holds it.
	f2, ok, err := tryBuildLock(dir)
	if err != nil {
		t.Fatalf("tryBuildLock: %v", err)
	}
	if ok {
		releaseLock(f2)
		t.Fatal("tryBuildLock acquired while build lock held by first")
	}
	// With shards present, a second builder SKIPS (acquired=false) instead of
	// blocking on the peer's build.
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, "x_v16.00000.zoekt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f4, ok4, err := corpusStore{cacheDir: dir, indexDir: indexDir}.acquireBuildLock(context.Background())
	if err != nil {
		t.Fatalf("second acquireBuildLock: %v", err)
	}
	if ok4 {
		releaseLock(f4)
		t.Fatal("second acquireBuildLock should skip (false) when held + shards exist")
	}
	releaseLock(first)
	// After release, the slot is free again.
	f3, ok, err := tryBuildLock(dir)
	if err != nil || !ok {
		t.Fatalf("tryBuildLock after release: ok=%v err=%v", ok, err)
	}
	releaseLock(f3)
}

func TestAcquirePublishLock_EvictedReturnsErrCorpusEvicted(t *testing.T) {
	dir := t.TempDir()
	// No .lock file exists (simulating an evicted corpus dir).
	_, err := acquirePublishLock(context.Background(), dir)
	if err != errCorpusEvicted {
		t.Fatalf("expected errCorpusEvicted for missing lock file, got %v", err)
	}
	// With the lock file present, it acquires.
	if err := os.WriteFile(filepath.Join(dir, lockFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := acquirePublishLock(context.Background(), dir)
	if err != nil {
		t.Fatalf("acquirePublishLock with lock file present: %v", err)
	}
	releaseLock(f)
}

func TestAcquireReadLock_SharedConcurrent(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFile)
	indexDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	open := func() *os.File {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	a, b := open(), open()
	defer releaseLock(a)
	defer releaseLock(b)
	if err := acquireReadLock(context.Background(), indexDir, a); err != nil {
		t.Fatalf("first read lock: %v", err)
	}
	// A second shared read lock must also succeed concurrently.
	if err := acquireReadLock(context.Background(), indexDir, b); err != nil {
		t.Fatalf("second concurrent read lock: %v", err)
	}
}

// TestAcquireReadLock_WaitsForPublishUntilCancel holds the publish lock as a
// wedged publish would: the read waits until its context ends, with no
// unlocked read of the remaining shards.
func TestAcquireReadLock_WaitsForPublishUntilCancel(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFile)
	indexDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, "x_v16.00000.zoekt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("hold exclusive lock: %v", err)
	}
	reader, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := acquireReadLock(ctx, indexDir, reader); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read under a wedged publish: %v, want the context's end", err)
	}
	// Once the publish ends, the same wait gets the lock, however long the
	// publish took: a wait deadline would fail here.
	done := make(chan error, 1)
	go func() { done <- acquireReadLock(t.Context(), indexDir, reader) }()
	time.Sleep(lockWaitProof)
	releaseLock(holder)
	if err := <-done; err != nil {
		t.Fatalf("read after the publish: %v", err)
	}
	unlockFile(reader)
}

// lockWaitProof is how long the lock tests hold a lock that another wait
// needs, so that a lock wait with a deadline fails them.
const lockWaitProof = 3 * time.Second

// TestLockOrderNoReadLockWhileWaitingForPublish proves that a build or
// publish lock wait under a read lock fails as a Seek defect, so any route
// that takes that order fails the suite.
func TestLockOrderNoReadLockWhileWaitingForPublish(t *testing.T) {
	dir := t.TempDir()
	indexDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	held := withReadLockHeld(t.Context())
	if _, err := acquirePublishLock(held, dir); !errors.Is(err, errLockOrder) || !errors.Is(err, errInvariant) {
		t.Fatalf("publish wait under a read lock: %v", err)
	}
	if _, _, err := (corpusStore{cacheDir: dir, indexDir: indexDir}).acquireBuildLock(held); !errors.Is(err, errLockOrder) {
		t.Fatalf("build wait under a read lock: %v", err)
	}
	if f, err := acquirePublishLock(t.Context(), dir); err != nil {
		t.Fatalf("publish lock with no read lock: %v", err)
	} else {
		releaseLock(f)
	}
}

// TestTwoColdSearchesSameNode runs two searches of one cold node at the same
// time: one builds, the other waits for that build with no deadline, and both
// answer.
func TestTwoColdSearchesSameNode(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	for index := range 50 {
		writeFileAt(t, folder, fmt.Sprintf("f%02d.txt", index), fmt.Sprintf("zqtwocold %d\n", index))
	}
	setTestUserCache(t)
	// run writes to os.Stdout, which one swap covers for both searches.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() {
		os.Stdout = stdout
		_ = devnull.Close()
	}()
	var group errgroup.Group
	for range 2 {
		group.Go(func() error {
			return run(context.Background(), "zqtwocold", []string{folder})
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatalf("a search failed: %v", err)
	}
}

// TestColdSearchWaitsForAnotherProcessBuild holds the build lock of a cold
// node for lockWaitProof while a search process reads the node: the process
// waits on the lock with no deadline, then builds and answers. In one process,
// the buildWindows semaphore orders two builds before the build lock does, so
// the test uses a second process.
func TestColdSearchWaitsForAnotherProcessBuild(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.txt", "zqprocesswait\n")
	cacheDir := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cacheDir)
	plans, err := planCorpora(context.Background(), []string{folder})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	if err := os.MkdirAll(plans[0].cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(filepath.Join(plans[0].cacheDir, buildLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("hold the build lock: %v", err)
	}
	done := make(chan cliProcessResult, 1)
	go func() {
		done <- runCLIProcessWithCache(t, cacheDir, folder, []string{"--lexical-only", "zqprocesswait"}, nil)
	}()
	time.Sleep(lockWaitProof)
	releaseLock(holder)
	result := <-done
	if result.code != 0 || !strings.Contains(result.stdout, "a.txt") {
		t.Fatalf("code=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
	}
}

// TestAcquireBuildLock_TouchesUsedAtStart proves that a build bumps the
// corpus .used at start so a concurrent gc cannot evict it in the window between
// the build releasing the lock and the search re-opening the corpus.
func TestAcquireBuildLock_TouchesUsedAtStart(t *testing.T) {
	dir := t.TempDir()
	indexDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, ok, err := corpusStore{cacheDir: dir, indexDir: indexDir}.acquireBuildLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("acquireBuildLock: ok=%v err=%v", ok, err)
	}
	defer releaseLock(f)

	info, err := os.Stat(filepath.Join(dir, usedFile))
	if err != nil {
		t.Fatalf(".used must be created at build start: %v", err)
	}
	if d := time.Since(info.ModTime()); d > 30*time.Second {
		t.Fatalf(".used should be fresh after build-start touch, age=%v", d)
	}
}

// TestPollLockRetriesOnlyAHeldLock retries EAGAIN and EINTR until the lock is
// free, and returns any other flock error at once with the lock path.
func TestPollLockRetriesOnlyAHeldLock(t *testing.T) {
	for _, held := range []error{syscall.EWOULDBLOCK, syscall.EAGAIN, syscall.EINTR} {
		calls := 0
		err := pollLock(t.Context(), "/cache/.lock", func() error {
			if calls++; calls == 1 {
				return held
			}
			return nil
		}, time.Millisecond, time.Millisecond)
		if err != nil || calls != 2 {
			t.Fatalf("%v: err=%v calls=%d, want a retry then success", held, err, calls)
		}
	}
	for _, lasting := range []error{syscall.ENOLCK, syscall.EIO} {
		calls := 0
		err := pollLock(t.Context(), "/cache/.lock", func() error {
			calls++
			return lasting
		}, time.Millisecond, time.Millisecond)
		if !errors.Is(err, lasting) || calls != 1 || !strings.Contains(err.Error(), "/cache/.lock") {
			t.Fatalf("%v: err=%v calls=%d, want one call and the lock path", lasting, err, calls)
		}
	}
}

// TestReaderDoesNotWaitForAFullRebuild holds a full uncommitted rebuild for
// lockWaitProof on one file read, in this process: the family builds before
// the publish lock, so a search in another process reads the live index at
// once.
func TestReaderDoesNotWaitForAFullRebuild(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "committed\n")
	for _, name := range []string{"a.txt", "b.txt"} {
		writeFixtureFile(t, filepath.Join(repo, name), "zqreader "+name+"\n")
	}
	cacheDir := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cacheDir)
	if _, err := captureStdout(t, func() error { return run(context.Background(), "zqreader", []string{repo}) }); err != nil {
		t.Fatal(err)
	}
	plans, err := planCorpora(context.Background(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(plans[0].cacheDir, stateFile)); err != nil {
		t.Fatal(err)
	}
	realOpen := dirtyOpenFile
	t.Cleanup(func() { dirtyOpenFile = realOpen })
	var slow sync.Once
	dirtyOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "b.txt" {
			slow.Do(func() { time.Sleep(lockWaitProof) })
		}
		return realOpen(path)
	}
	built := make(chan error, 1)
	go func() {
		_, err := captureStdout(t, func() error { return run(context.Background(), "zqreader", []string{repo}) })
		built <- err
	}()
	time.Sleep(500 * time.Millisecond)
	started := time.Now()
	result := runCLIProcessWithCache(t, cacheDir, repo, []string{"--lexical-only", "zqreader"}, nil)
	elapsed := time.Since(started)
	if err := <-built; err != nil {
		t.Fatal(err)
	}
	if result.code != 0 || !strings.Contains(result.stdout, "b.txt") || elapsed > lockWaitProof-time.Second {
		t.Fatalf("reader: code=%d after %v, want the live index at once:\n%s%s", result.code, elapsed, result.stdout, result.stderr)
	}
}

// TestOpenNodeReadLock opens a read on a cache with no .lock: the read makes
// the file and marks only its own context. Its lock is shared: an exclusive
// lock fails, and a second read opens. Close releases the lock, and a second
// close does nothing.
func TestOpenNodeReadLock(t *testing.T) {
	cacheDir := t.TempDir()
	plan := node{corpusStore: corpusStore{cacheDir: cacheDir, indexDir: filepath.Join(cacheDir, "index")}}
	read, readCtx, err := openNodeRead(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(checkLockOrder(readCtx), errLockOrder) || checkLockOrder(t.Context()) != nil {
		t.Fatal("only the read's context must be marked")
	}
	other, err := os.OpenFile(filepath.Join(cacheDir, lockFile), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("the read must make .lock: %v", err)
	}
	defer func() { _ = other.Close() }()
	if err := lockFileExclusive(other); !lockHeld(err) {
		t.Fatalf("exclusive lock during the read: err=%v, want held", err)
	}
	// The lock is shared: a second read opens while the first one holds it.
	secondCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	second, _, err := openNodeRead(secondCtx, plan)
	if err != nil {
		t.Fatalf("a second read during the first: %v", err)
	}
	second.close()
	read.close()
	read.close()
	if err := lockFileExclusive(other); err != nil {
		t.Fatalf("exclusive lock after close: %v", err)
	}
}
