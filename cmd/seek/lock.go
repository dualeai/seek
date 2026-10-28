package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// lockWaits lets the user end the lock waits of one command. A wait has no
// deadline: another process can hold a lock until the user stops it. The
// command prints after every node returns, so one wait would hide the results
// of every other node. So the first interrupt ends the waits that run at that
// time (watchInterrupts). The search skips each such node with one warning, as
// it skips a failed node, and the others print. A wait that starts later, such
// as the publish of a build that was running, waits as usual. ended records
// that a wait ended this way (noMatchOrInterrupted).
type lockWaits struct {
	mu      sync.Mutex
	end     chan struct{}
	running int
	ended   atomic.Bool
}

func newLockWaits() *lockWaits {
	return &lockWaits{end: make(chan struct{})}
}

// start counts a wait and returns the channel that ends it.
func (waits *lockWaits) start() <-chan struct{} {
	waits.mu.Lock()
	defer waits.mu.Unlock()
	waits.running++
	return waits.end
}

// stop ends the count of a wait that start counted.
func (waits *lockWaits) stop() {
	waits.mu.Lock()
	defer waits.mu.Unlock()
	waits.running--
}

// endRunning ends the waits that run now, and reports whether one ran. Later
// waits get a new channel.
func (waits *lockWaits) endRunning() bool {
	waits.mu.Lock()
	defer waits.mu.Unlock()
	if waits.running == 0 {
		return false
	}
	close(waits.end)
	waits.end = make(chan struct{})
	return true
}

// noMatchOrInterrupted is the error of a command that wrote no result: the
// user's interrupt when an interrupt ended one of its lock waits, else
// errNoMatch. With no result to print, the command ends as the interrupt
// asked.
func noMatchOrInterrupted(ctx context.Context) error {
	if waits := lockWaitsOf(ctx); waits != nil && waits.ended.Load() {
		return context.Canceled
	}
	return errNoMatch
}

// errLockWaitEnded is the error of a lock wait that the user ended. It fails
// the node only.
var errLockWaitEnded = errors.New("the user ended the wait for the lock")

type lockWaitsKey struct{}

// withLockWaits gives ctx the lock waits of the command.
func withLockWaits(ctx context.Context, waits *lockWaits) context.Context {
	return context.WithValue(ctx, lockWaitsKey{}, waits)
}

// lockWaitsOf returns the lock waits of ctx's command, or nil.
func lockWaitsOf(ctx context.Context) *lockWaits {
	waits, _ := ctx.Value(lockWaitsKey{}).(*lockWaits)
	return waits
}

// errCorpusEvicted is the error of acquirePublishLock and publishGeneration
// when the corpus directory no longer exists. GC can remove it during a build
// outside the publish lock. The caller discards its temporary build and does
// not make the lock file again. The read of a Git node opens .lock with
// O_CREATE, finds no shard, and repairs. So the same command builds the node
// again, with an "Index damage detected" warning. The search skips a folder
// node whose read cannot open .lock, with a warning, and the next search
// builds it (TestEvictionDuringBuildPerKind).
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
// wait finite. A goroutine that holds a read lock never waits for a build or a
// publish lock. withReadLockHeld marks its context, and such a wait fails as a
// Seek defect. The pipeline prepares a node, which builds and publishes, before
// its read takes the read lock. A retry prepares again only after the read
// returned the lock (openNode). So each read lock is free again after a search
// of bounded time. Each build wait ends when the build before it publishes or
// fails, in this process and across processes. flock keeps no queue: a publish
// waits while readers overlap. That delays a new index only: a search of a node
// that has shards reads them and never waits for its build.

// The reasons of the three lock waits. pollLock writes the reason as the
// message of the wait's debug line, with the lock path in its "lock"
// attribute. Tests find a wait by this message and path.
const (
	lockWaitBuild   = "Waiting for another build of the node"
	lockWaitReaders = "Waiting for the readers of the node"
	lockWaitPublish = "Waiting for a publish of the node"
)

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
func (store corpusStore) acquireBuildLock(ctx context.Context) (*os.File, bool, error) {
	if err := checkLockOrder(ctx); err != nil {
		return nil, false, err
	}
	lockPath := filepath.Join(store.cacheDir, buildLockFile)
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
		if shardsExist(store.indexDir) {
			_ = f.Close()
			return nil, false, nil
		}
		if perr := pollLock(ctx, lockPath, lockWaitBuild, func() error { return lockFileExclusive(f) }, 100*time.Millisecond, 2*time.Second); perr != nil {
			_ = f.Close()
			return nil, false, perr
		}
	}
	// Make sure that the publish lock file exists. acquirePublishLock opens it
	// without O_CREATE, so it can tell a first build (the file is present) from
	// a corpus that gc evicted (no file: errCorpusEvicted).
	if lf, lerr := os.OpenFile(filepath.Join(store.cacheDir, lockFile), os.O_CREATE|os.O_RDWR, 0o644); lerr == nil {
		_ = lf.Close()
	}
	// Mark the corpus as used when the build starts. Then a concurrent gc does
	// not evict it between the release of this lock and the search's open of
	// the publish lock. An open in the trashed directory fails with ENOENT. gc
	// keeps a corpus whose readUsedAt time is after the time of its list
	// (evictCorpus).
	touchUsed(store.cacheDir)
	return f, true, nil
}

// tryBuildLock tries a non-blocking LOCK_EX on cacheDir/.build.lock. It returns
// (fd, true, nil) when it gets the lock. It returns (nil, false, nil) when a
// builder holds it or flock fails, and the error when the lock file does not
// open. gc and removeDamagedVectors use it to skip a corpus whose build runs.
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

// lockBuild takes the build lock of the store and repairs a swap that a crash,
// or a failed move back, left (recoverIncompleteSwap). With acquired false, a
// peer builds and shards exist: the caller serves the current shards.
func (store corpusStore) lockBuild(ctx context.Context) (unlock func(), acquired bool, err error) {
	buildFd, acquired, err := store.acquireBuildLock(ctx)
	if err != nil || !acquired {
		if err == nil {
			slog.Debug("Another process is indexing; serving current index")
		}
		return nil, false, err
	}
	// Repair a half-published swap before the build trusts the state.
	store.recoverIncompleteSwap()
	return func() { releaseLock(buildFd) }, true, nil
}

// servesStale reports whether a node whose update failed with err serves its
// published shards. When shards exist, it writes the stale-index warning once.
// With no shard, the error fails the node.
func (store corpusStore) servesStale(err error) bool {
	if !shardsExist(store.indexDir) {
		return false
	}
	reportStaleIndexWarning(err)
	return true
}

// acquirePublishLock takes the brief publish lock (LOCK_EX on cacheDir/.lock).
// O_RDWR without O_CREATE: a missing lock file means the corpus was evicted, so
// it returns errCorpusEvicted and does not make the corpus again.
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
	if err := pollLock(ctx, lockPath, lockWaitReaders, func() error { return lockFileExclusive(f) }, 20*time.Millisecond, 500*time.Millisecond); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// acquireReadLock takes the shared lock on the open cacheDir/.lock file. The
// caller holds it while it lists, opens, and searches shards, and marks its
// context with withReadLockHeld. A publish in progress makes it wait until the
// publish ends or the user cancels; the wait writes one debug line.
func acquireReadLock(ctx context.Context, f *os.File) error {
	if err := lockFileSharedNB(f); err == nil {
		return nil
	}
	return pollLock(ctx, f.Name(), lockWaitPublish, func() error { return lockFileSharedNB(f) }, 20*time.Millisecond, 500*time.Millisecond)
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
// succeeds, ctx ends, or the user ends the command's lock waits (lockWaits).
// It counts the wait before it writes reason as one debug line, so an
// interrupt after that line ends this wait. Jitter (up to 50% of backoff)
// keeps processes that poll the same lock from retrying at the same time. It
// retries only a held lock: any other flock error does not clear, so it
// returns at once.
func pollLock(ctx context.Context, path, reason string, lockFn func() error, initialBackoff, maxBackoff time.Duration) error {
	var ended <-chan struct{}
	waits := lockWaitsOf(ctx)
	if waits != nil {
		ended = waits.start()
		defer waits.stop()
	}
	slog.Debug(reason, "lock", path)
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
		case <-ended:
			timer.Stop()
			waits.ended.Store(true)
			return fmt.Errorf("lock %s: %w", path, errLockWaitEnded)
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
