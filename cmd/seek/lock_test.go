package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquireSearchLock_StaleShardFallback verifies the new fallback
// branch in lock.go:115-118. Pre-existing shards in indexDir +
// LOCK_EX held by another goroutine → acquireSearchLock returns nil
// without polling, so search proceeds against stale shards instead of
// blocking 60s. Mirrors acquireLock's stale-shard fallback.
func TestAcquireSearchLock_StaleShardFallback(t *testing.T) {
	dir := t.TempDir()
	indexDir := dir
	// Create a fake shard file so shardsExist returns true.
	if err := os.WriteFile(filepath.Join(indexDir, "fakerepo_v17.00000.zoekt"), []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(dir, ".lock")
	// Holder: take LOCK_EX and keep it for the duration of the test.
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockFile(holder); _ = holder.Close() })
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	// Search side: open a fresh fd on the same lockfile, attempt
	// acquireSearchLock. Must return nil within ms (no poll).
	searchFd, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = searchFd.Close() })

	start := time.Now()
	if err := acquireSearchLock(context.Background(), indexDir, searchFd); err != nil {
		t.Fatalf("acquireSearchLock: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("acquireSearchLock took %v; expected <200ms (stale-fallback should not poll)", elapsed)
	}
}

// TestAcquireSearchLock_NoShardsPolls verifies the OTHER branch — no
// shards exist + LOCK_EX held → poll the lock with 60s timeout. Uses
// a holder that releases after 100ms so the poll succeeds before the
// timeout.
func TestAcquireSearchLock_NoShardsPolls(t *testing.T) {
	dir := t.TempDir()
	indexDir := dir // empty

	lockPath := filepath.Join(dir, ".lock")
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(holder); err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	// Unlocker goroutine — synchronized via channel so Cleanup runs
	// only after the goroutine exits (avoids -race fd-state race).
	unlockerDone := make(chan struct{})
	go func() {
		defer close(unlockerDone)
		time.Sleep(100 * time.Millisecond)
		unlockFile(holder)
	}()
	t.Cleanup(func() { <-unlockerDone; _ = holder.Close() })

	searchFd, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = searchFd.Close() })

	start := time.Now()
	if err := acquireSearchLock(context.Background(), indexDir, searchFd); err != nil {
		t.Fatalf("acquireSearchLock: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("acquireSearchLock returned in %v; expected to have polled the lock", elapsed)
	}
}

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

func TestAcquireLock_Immediate(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")

	fd, acquired, err := acquireLock(context.Background(), dir, lockPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired")
	}
	if fd == nil {
		t.Fatal("expected non-nil fd")
	}
	releaseLock(fd)
}

func TestAcquireLock_StaleFallback(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")

	// Create a shard so the stale fallback path is taken
	_ = os.WriteFile(filepath.Join(dir, "repo_v16.00000.zoekt"), []byte{}, 0o644)

	// Hold the lock from another fd
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if err := lockFileExclusive(holder); err != nil {
		t.Fatal(err)
	}

	// acquireLock should return (nil, false, nil) — stale fallback
	fd, acquired, err := acquireLock(context.Background(), dir, lockPath)
	if err != nil {
		t.Fatalf("expected no error on stale fallback, got %v", err)
	}
	if acquired {
		t.Fatal("expected lock NOT to be acquired (stale fallback)")
	}
	if fd != nil {
		t.Fatal("expected nil fd on stale fallback")
	}

	unlockFile(holder)
}

func TestAcquireLock_PollAndAcquire(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")
	// No shards — this forces the poll path

	// Hold the lock from another fd
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	if err := lockFileExclusive(holder); err != nil {
		t.Fatal(err)
	}

	// Release after 300ms
	go func() {
		time.Sleep(300 * time.Millisecond)
		unlockFile(holder)
		_ = holder.Close()
	}()

	start := time.Now()
	fd, acquired, err := acquireLock(context.Background(), dir, lockPath)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired after polling")
	}
	if fd == nil {
		t.Fatal("expected non-nil fd")
	}
	releaseLock(fd)

	if elapsed < 200*time.Millisecond {
		t.Errorf("expected polling delay, but acquired in %v", elapsed)
	}
}

func TestAcquireLock_Timeout(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")
	// No shards — forces poll path

	// Hold the lock forever
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unlockFile(holder)
		_ = holder.Close()
	})
	if err := lockFileExclusive(holder); err != nil {
		t.Fatal(err)
	}

	// Use a short timeout via context
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, acquired, err := acquireLock(ctx, dir, lockPath)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if acquired {
		t.Fatal("should not acquire lock on timeout")
	}
}
