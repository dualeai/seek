package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"syscall"
	"time"
)

// lockFileExclusive attempts a non-blocking exclusive lock on f.
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// lockFileSharedNB attempts a non-blocking shared lock on f.
func lockFileSharedNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
}

// unlockFile releases the lock on f.
func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// pollLock retries lockFn with exponential backoff and jitter until it
// succeeds or the timeout expires. Jitter (up to 50% of backoff) prevents
// thundering herd when multiple processes poll the same lock.
func pollLock(ctx context.Context, lockFn func() error, initialBackoff, maxBackoff, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	backoff := initialBackoff
	for {
		if err := lockFn(); err == nil {
			return nil
		}

		jitter := time.Duration(rand.Int64N(int64(backoff) / 2))
		timer := time.NewTimer(backoff + jitter)
		select {
		case <-timeoutCtx.Done():
			timer.Stop()
			return fmt.Errorf("lock poll timeout (%v)", timeout)
		case <-timer.C:
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// acquireLock tries to acquire an exclusive flock on the lock file.
// Returns (fd, acquired, error).
func acquireLock(ctx context.Context, indexDir, lockPath string) (*os.File, bool, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}

	// Try non-blocking lock first
	if err := lockFileExclusive(f); err == nil {
		return f, true, nil
	}

	// Lock held by another process — use stale shards if available
	if shardsExist(indexDir) {
		_ = f.Close()
		return nil, false, nil
	}

	// No shards exist (first run) — poll until lock is acquired.
	// Uses non-blocking attempts to prevent fd-reuse races when the
	// timeout fires and closes the file.
	if err := pollLock(ctx, func() error { return lockFileExclusive(f) }, 100*time.Millisecond, 2*time.Second, 60*time.Second); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("indexer lock held >60s")
	}
	return f, true, nil
}

// releaseLock releases the flock and closes the file.
func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	unlockFile(f)
	_ = f.Close()
}

// acquireSearchLock acquires a shared lock with a timeout, polling with
// exponential backoff plus jitter. Uses non-blocking LOCK_SH to avoid
// goroutine/fd-reuse races.
func acquireSearchLock(ctx context.Context, f *os.File) error {
	if err := lockFileSharedNB(f); err == nil {
		return nil
	}
	if err := pollLock(ctx, func() error { return lockFileSharedNB(f) }, 50*time.Millisecond, 500*time.Millisecond, 60*time.Second); err != nil {
		return fmt.Errorf("timeout waiting for indexer to finish (60s)")
	}
	return nil
}
