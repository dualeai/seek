package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	first, acquired, err := acquireBuildLock(context.Background(), dir, indexDir)
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
	f4, ok4, err := acquireBuildLock(context.Background(), dir, indexDir)
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

func TestAcquireReadLock_WedgedSwapStaleValve(t *testing.T) {
	old := readLockTimeout
	readLockTimeout = 150 * time.Millisecond
	defer func() { readLockTimeout = old }()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFile)
	indexDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Shards present so the stale valve is allowed.
	if err := os.WriteFile(filepath.Join(indexDir, "x_v16.00000.zoekt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Hold EX (a wedged swap) on a separate fd.
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("hold EX: %v", err)
	}
	defer releaseLock(holder)

	reader, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	// EX wedged + shards exist → after the bounded wait, degrade to stale read.
	if err := acquireReadLock(context.Background(), indexDir, reader); err != nil {
		t.Fatalf("wedged-swap stale valve should return nil, got %v", err)
	}
}

// TestAcquireBuildLock_TouchesUsedAtStart covers FIX B: a build bumps the
// corpus .used at start so a concurrent gc cannot evict it in the window between
// the build releasing the lock and the search re-opening the corpus.
func TestAcquireBuildLock_TouchesUsedAtStart(t *testing.T) {
	dir := t.TempDir()
	indexDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, ok, err := acquireBuildLock(context.Background(), dir, indexDir)
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
