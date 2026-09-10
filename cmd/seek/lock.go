package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// errCorpusEvicted is returned by acquirePublishLock when the corpus directory
// no longer exists. GC can remove it during a build outside the publish lock.
// The caller must discard its temporary build and let the next search rebuild
// it. The caller must not recreate the lock file and its missing corpus.
var errCorpusEvicted = errors.New("corpus evicted during build")

// The build and publish locks have separate scopes:
//
//   - acquireBuildLock holds an exclusive cacheDir/.build.lock for the complete
//     build. Readers do not take this lock.
//   - acquirePublishLock holds an exclusive cacheDir/.lock while data is
//     published. It opens an existing file so an evicted corpus returns
//     errCorpusEvicted.
//   - acquireReadLock: shared lock on cacheDir/.lock, held while the reader
//     lists, opens, and searches shards. It waits for a bounded time. If the
//     wait expires and shards remain, it permits an unlocked read of them.

// acquireBuildLock holds an exclusive cacheDir/.build.lock for the complete
// build. If another builder holds it and any shard exists, it returns
// (nil, false, nil), and the caller uses the current shards. A corpus with no
// shards waits for the lock so its first build is not skipped.
func acquireBuildLock(ctx context.Context, cacheDir, indexDir string) (*os.File, bool, error) {
	lockPath := filepath.Join(cacheDir, buildLockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open build lock file: %w", err)
	}
	if err := lockFileExclusive(f); err != nil {
		if shardsExist(indexDir) {
			_ = f.Close()
			return nil, false, nil
		}
		if perr := pollLock(ctx, func() error { return lockFileExclusive(f) }, 100*time.Millisecond, 2*time.Second, lockPollTimeout); perr != nil {
			_ = f.Close()
			return nil, false, fmt.Errorf("build lock held >%v: %w", lockPollTimeout, perr)
		}
	}
	// Ensure the publish lock file exists so acquirePublishLock (which opens it
	// without O_CREATE) can distinguish a first build (file present, lockable)
	// from a gc-evicted corpus (file gone → errCorpusEvicted).
	if lf, lerr := os.OpenFile(filepath.Join(cacheDir, lockFile), os.O_CREATE|os.O_RDWR, 0o644); lerr == nil {
		_ = lf.Close()
	}
	// Mark the corpus in-use at build START so a concurrent gc does not evict it
	// in the window between the build releasing this lock and the search
	// re-opening the corpus's publish lock (which would ENOENT on the just-
	// trashed dir). gc's readUsedAt > cutoff check then keeps it (gc.go).
	touchUsed(cacheDir)
	return f, true, nil
}

// tryBuildLock attempts a NON-blocking LOCK_EX on cacheDir/.build.lock. Returns
// (fd, true, nil) when acquired, (nil, false, nil) when a builder holds it. Used
// by gc to skip a corpus whose build is in progress.
func tryBuildLock(cacheDir string) (*os.File, bool, error) {
	lockPath := filepath.Join(cacheDir, buildLockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open build lock file: %w", err)
	}
	if err := lockFileExclusive(f); err != nil {
		_ = f.Close()
		return nil, false, nil
	}
	return f, true, nil
}

// acquirePublishLock takes the brief publish lock (LOCK_EX on cacheDir/.lock).
// O_RDWR without O_CREATE: a missing lock file means the corpus was evicted, so
// it returns errCorpusEvicted rather than resurrecting a phantom corpus.
func acquirePublishLock(ctx context.Context, cacheDir string) (*os.File, error) {
	lockPath := filepath.Join(cacheDir, lockFile)
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errCorpusEvicted
		}
		return nil, fmt.Errorf("open publish lock file: %w", err)
	}
	if err := lockFileExclusive(f); err == nil {
		return f, nil
	}
	if err := pollLock(ctx, func() error { return lockFileExclusive(f) }, 20*time.Millisecond, 500*time.Millisecond, lockPollTimeout); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("publish lock held >%v: %w", lockPollTimeout, err)
	}
	return f, nil
}

// acquireReadLock takes the shared lock on the open cacheDir/.lock file. The
// caller holds it while it lists, opens, and searches shards. If the bounded
// wait expires and shards remain, the caller can read them without the lock.
func acquireReadLock(ctx context.Context, indexDir string, f *os.File) error {
	if err := lockFileSharedNB(f); err == nil {
		return nil
	}
	// A publish swap is ms-scale; poll briefly for it to finish.
	if err := pollLock(ctx, func() error { return lockFileSharedNB(f) }, 20*time.Millisecond, 500*time.Millisecond, readLockTimeout); err != nil {
		// Wedged swap: use the shards that remain rather than hang. This is the
		// sole unlocked read path and fires only when the publish lock stays held
		// past the timeout.
		if shardsExist(indexDir) {
			slog.Warn("Publish lock contended past timeout; searching remaining shards without the lock", "index_dir", indexDir)
			return nil
		}
		return fmt.Errorf("timeout waiting for indexer to finish (%v)", readLockTimeout)
	}
	return nil
}

// readLockTimeout bounds how long a search waits for a publish swap before it
// can read remaining shards without the lock. The swap is usually ms-scale.
// This is a variable so tests can shorten the wait.
var readLockTimeout = 10 * time.Second

// lockPollTimeout bounds how long the build/publish lock acquisitions poll for a
// held lock before giving up. Named so the value and the ">Ns" error messages
// stay in sync.
const lockPollTimeout = 60 * time.Second

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
			// Distinguish parent-context cancellation (e.g. Ctrl-C) from the
			// poll deadline so callers can errors.Is(err, context.Canceled).
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("lock poll timeout (%v)", timeout)
		case <-timer.C:
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// releaseLock releases the flock and closes the file.
func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	unlockFile(f)
	_ = f.Close()
}
