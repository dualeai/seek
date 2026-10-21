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
//     lists, opens, and searches shards.
//
// Every wait lasts until the lock is free or the user cancels: a search waits
// for another process's build of the same node. This lock order keeps every
// wait finite: a goroutine that holds a read lock never waits for a build or
// a publish lock (withReadLockHeld marks its context, and such a wait fails as
// a Seek defect). The pipeline prepares a node, which builds and publishes,
// before its read takes the read lock, and a retry prepares again only after
// the read returned it (openNode). So each read lock is released after a
// search of bounded time, and each build wait ends when the build before it
// publishes or fails, in this process and across processes. flock keeps no
// queue: a publish waits while readers overlap. That delays a new index only:
// a search of a node that has shards reads them and never waits for its build.

type readLockHeldKey struct{}

// withReadLockHeld marks a context whose goroutine holds a read lock.
func withReadLockHeld(ctx context.Context) context.Context {
	return context.WithValue(ctx, readLockHeldKey{}, true)
}

// errLockOrder reports a build or publish wait under a read lock: the one
// order that could wait forever.
var errLockOrder = fmt.Errorf("%w: build or publish lock wait while a read lock is held", errInvariant)

// checkLockOrder returns errLockOrder when ctx is marked by withReadLockHeld.
func checkLockOrder(ctx context.Context) error {
	if held, _ := ctx.Value(readLockHeldKey{}).(bool); held {
		return errLockOrder
	}
	return nil
}

// acquireBuildLock holds an exclusive cacheDir/.build.lock for the complete
// build. If another builder holds it and any shard exists, it returns
// (nil, false, nil), and the caller uses the current shards. A corpus with no
// shards waits for the lock so its first build is not skipped.
func acquireBuildLock(ctx context.Context, cacheDir, indexDir string) (*os.File, bool, error) {
	if err := checkLockOrder(ctx); err != nil {
		return nil, false, err
	}
	lockPath := filepath.Join(cacheDir, buildLockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open build lock file: %w", err)
	}
	if err := lockFileExclusive(f); err != nil {
		if !lockHeld(err) {
			// Only a held lock means another builder.
			_ = f.Close()
			return nil, false, fmt.Errorf("lock %s: %w", lockPath, err)
		}
		if shardsExist(indexDir) {
			_ = f.Close()
			return nil, false, nil
		}
		slog.Debug("Waiting for another build of the node", "cache_dir", cacheDir)
		if perr := pollLock(ctx, lockPath, func() error { return lockFileExclusive(f) }, 100*time.Millisecond, 2*time.Second); perr != nil {
			_ = f.Close()
			return nil, false, perr
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
// (fd, true, nil) when acquired, and (nil, false, nil) when the lock is not
// taken: a builder holds it, or flock fails. Used by gc to skip a corpus whose
// build is in progress.
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
	if err := checkLockOrder(ctx); err != nil {
		return nil, err
	}
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
	slog.Debug("Waiting for the readers of the node", "cache_dir", cacheDir)
	if err := pollLock(ctx, lockPath, func() error { return lockFileExclusive(f) }, 20*time.Millisecond, 500*time.Millisecond); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// acquireReadLock takes the shared lock on the open cacheDir/.lock file. The
// caller holds it while it lists, opens, and searches shards, and marks its
// context with withReadLockHeld. A publish in progress makes it wait until the
// publish ends or the user cancels; the wait writes one debug line.
func acquireReadLock(ctx context.Context, indexDir string, f *os.File) error {
	if err := lockFileSharedNB(f); err == nil {
		return nil
	}
	slog.Debug("Waiting for a publish of the node", "index_dir", indexDir)
	return pollLock(ctx, f.Name(), func() error { return lockFileSharedNB(f) }, 20*time.Millisecond, 500*time.Millisecond)
}

// lockFileExclusive attempts a non-blocking exclusive lock on f.
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// lockFileSharedNB attempts a non-blocking shared lock on f.
func lockFileSharedNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
}

// lockHeld reports a flock error that means another holder, or a signal: the
// only errors that a wait retries. Any other error (ENOLCK on a mount with no
// lock support, EIO) returns with the lock path.
func lockHeld(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR)
}

// unlockFile releases the lock on f.
func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// pollLock retries lockFn with exponential backoff and jitter until it
// succeeds or ctx ends. Jitter (up to 50% of backoff) keeps processes that poll
// the same lock from retrying at the same time. Only a held lock is retried:
// any other flock error never clears, so it returns at once.
func pollLock(ctx context.Context, path string, lockFn func() error, initialBackoff, maxBackoff time.Duration) error {
	backoff := initialBackoff
	for {
		err := lockFn()
		if err == nil {
			return nil
		}
		if !lockHeld(err) {
			return fmt.Errorf("lock %s: %w", path, err)
		}

		jitter := time.Duration(rand.Int64N(int64(backoff) / 2))
		timer := time.NewTimer(backoff + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
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
