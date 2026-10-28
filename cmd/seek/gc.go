package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Files and folders of the cache root. gcTrashDir is in the corpora
	// folder.
	gcStampFile = ".last-gc"
	gcLockFile  = "gc.lock"
	corporaDir  = "corpora"
	rerankerDir = "reranker"
	gcTrashDir  = ".trash"
	// corpusIndexDir is the folder of a corpus that holds its shards and
	// semantic generations (node.indexDir).
	corpusIndexDir = "index"

	// usedFile is the marker of one corpus. Its mtime records the last
	// access, because most file systems mount with noatime or relatime, which
	// keep no current atime.
	usedFile          = ".used"
	defaultGCInterval = 24 * time.Hour
	// gcRunTimeout bounds the automatic GC after a command, so the command
	// ends soon after its output (fireOpportunisticGC).
	gcRunTimeout = 5 * time.Second
	// providerArtifactMaxAge is the age at which GC removes a compiled
	// accelerator model that no search used, and the runtime directory of
	// another Seek version. The cache budget does not cover them.
	providerArtifactMaxAge = 14 * 24 * time.Hour
	// providerArtifactUsedFile records the last time a process wanted a
	// compiled accelerator model. The collector measures the age of the model
	// from it, as it measures the age of a corpus from usedFile. The
	// accelerated provider writes it (touchProviderArtifactUse). The name is
	// here because the collector compiles on every target, and the provider
	// code only where an accelerator exists.
	providerArtifactUsedFile = ".used"

	envGCInterval = "SEEK_GC_INTERVAL"

	gcLocalCacheHint = "Set SEEK_CACHE_DIR to a local directory. " +
		"On Linux, you can instead unset SEEK_CACHE_DIR and set XDG_CACHE_HOME."
)

// corpusDirEntry is one eviction candidate: a corpus directory
// (enumerateCorpusDirs) or a unit-store segment (semanticUnitsSegments).
// usedAt is its last use, which orders the eviction.
type corpusDirEntry struct {
	name   corpusID
	path   string
	usedAt time.Time
}

// gcAction is the outcome of one corpus eviction. It fills the ACTION column
// of the manual `seek gc` table. The words are in the past tense: a live row
// tells what happened, and a dry-run row tells what would happen.
type gcAction uint8

const (
	actionKept    gcAction = iota // not planned, or used again since the plan
	actionEvicted                 // the rename to .trash and RemoveAll worked
	actionLocked                  // a search or build has .lock or .build.lock
	actionGone                    // the corpus directory went away
	actionTrashed                 // renamed; drainTrash retries the RemoveAll
	actionFailed                  // a lock open, lock check, or rename failed
)

// gcRowResult is what evictCorpus returns. action fills the ACTION column. err
// is set only for trashed and failed; evictIfPlanned logs it with slog.Warn.
type gcRowResult struct {
	action gcAction
	err    error
}

// String renders the ACTION cell. trashed and failed carry a short error, so
// the table alone shows the cause.
func (r gcRowResult) String() string {
	switch r.action {
	case actionKept:
		return "kept"
	case actionEvicted:
		return "evicted"
	case actionLocked:
		return "locked"
	case actionGone:
		return "gone"
	case actionTrashed:
		return "trashed: " + shortErr(r.err)
	default: // actionFailed
		return "failed: " + shortErr(r.err)
	}
}

// shortErr renders an error for the ACTION cell. It keeps the first line and
// at most 60 bytes, cut on a rune boundary (utf8Prefix). So a
// "trashed: <reason>" or "failed: <reason>" row stays on one terminal line. A
// nil error gives "unknown", so the table never fails.
func shortErr(err error) string {
	if err == nil {
		return "unknown"
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 60
	if len(s) > max {
		s = utf8Prefix(s, max) + "..."
	}
	return s
}

// gcOptions sets one runGC run.
//   - skipThrottle skips the .last-gc mtime gate (`seek gc --force` or
//     `--all`).
//   - evictAll evicts every corpus that no search holds (`seek gc --all`).
//     Else GC evicts the least recently used corpora only while the cache is
//     over its budget. Age alone never evicts a corpus.
//   - writer, when not nil, makes runGC stream a table of the corpora to it
//     (the manual path). Nil is silent (the opportunistic path after a run).
//   - sortKey orders the streamed table (`seek gc --sort`). runGC reads it
//     only when writer is not nil. The zero value keeps the hash order.
type gcOptions struct {
	skipThrottle bool
	evictAll     bool
	writer       io.Writer
	sortKey      gcSortKey
}

// fireOpportunisticGC runs runFn in a goroutine with a timeout. The caller
// waits until the goroutine ends or until the timeout fires. It is a function
// of its own, not inline in main(). So unit tests can check the goroutine, the
// timeout, and the context cleanup in the test process. main() can call
// os.Exit, so a test runs main() only in a child process
// (TestCLIProcessHelper). TestGC_Integration_RunFlowEvictsStale runs a search
// in such a process and checks the GC after it.
func fireOpportunisticGC(runFn func(context.Context), timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFn(ctx)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// runOpportunisticGC is the automatic GC that main() runs after each search
// command, successful or not (shouldRunOpportunisticGC). It logs every failure
// and a panic, and returns no error, so it never changes the exit code. ctx
// bounds it (gcRunTimeout).
func runOpportunisticGC(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("gc panic", "error", r)
		}
	}()
	runGC(ctx, gcOptions{})
}

// runGC is the GC body that two callers share:
//   - runOpportunisticGC: writer nil, a silent eviction sweep after a run.
//   - runGCCommandCmd (`seek gc`, `--force`, `--all`): writer os.Stdout. It
//     streams the same banner, table, and summary as `--dry-run`, and the
//     ACTION column shows the real outcome of each corpus.
//
// Order of operations:
//  1. the throttle gate (`.last-gc` mtime), unless skipThrottle
//  2. the NFS check: GC does not run on a network file system
//  3. the global gc.lock: when another GC holds it, skip (and tell a
//     streaming caller)
//  4. empty the trash that an earlier run left
//  5. list the corpora in hash order, so the sweep order is fixed
//  6. plan the eviction (planCorpusEviction): every corpus with --all, else
//     the least recently used corpora while the cache is over its budget
//  7. streaming with --sort=age|size: sweep, measure all rows, sort, then
//     evict and render in table order (see the comment of that branch for a
//     cancel). The default name order and the silent path make one pass per
//     entry: sweep, measure (streaming only), and evict. Rows stream as they
//     come.
//  8. evict the unit-store segments of the plan, then sweep aged compiled
//     accelerator models and the old runtimes of other versions
//  9. stamp .last-gc to throttle the next opportunistic run
func runGC(ctx context.Context, opts gcOptions) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		slog.Debug("gc skipped: cannot resolve cache root", "error", err)
		return
	}

	// The throttle gate comes first. On the warm path it returns after one
	// stat() of .last-gc, with no MkdirAll and no Statfs on each run of Seek.
	if !opts.skipThrottle && !shouldRunGC(cacheRoot, gcIntervalFromEnv()) {
		// The opportunistic path (writer nil) does nothing and stays silent.
		// A manual `seek gc` (writer not nil) gets a one-line hint: nothing
		// ran, and --force runs it.
		if opts.writer != nil {
			_, _ = fmt.Fprintln(opts.writer, "seek gc: throttled — pass --force to run anyway")
		}
		return
	}

	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		slog.Debug("gc skipped: cannot create cache root", "error", err)
		return
	}

	if isOnNFS(cacheRoot) {
		warnNFSOnce(cacheRoot)
		return
	}

	gcLockPath := filepath.Join(cacheRoot, gcLockFile)
	lockFd, err := os.OpenFile(gcLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		slog.Debug("gc skipped: cannot open gc lock", "error", err)
		return
	}
	defer releaseLock(lockFd)
	if err := lockFileExclusive(lockFd); err != nil {
		// Tell the manual caller about the other GC. Else `seek gc --force`
		// would exit with no message and no sign that another GC took the lock.
		// The opportunistic path (writer nil) logs at Debug level only.
		if opts.writer != nil {
			_, _ = fmt.Fprintf(opts.writer, "seek gc: another gc is already running on %s; skipping\n", cacheRoot)
		}
		slog.Debug("gc skipped: another gc is running")
		return
	}

	corporaPath := filepath.Join(cacheRoot, corporaDir)
	trashPath := filepath.Join(corporaPath, gcTrashDir)
	if err := os.MkdirAll(trashPath, 0o755); err != nil {
		slog.Warn("gc cannot create trash dir", "error", err)
		return
	}

	// Every run empties the trash, so an entry that an earlier run could not
	// remove goes now.
	drainTrash(trashPath)

	entries, err := enumerateCorpusDirs(corporaPath)
	if err != nil {
		slog.Warn("gc cannot enumerate corpora", "error", err)
		// Update the stamp all the same, to throttle the next run.
		touchStamp(cacheRoot)
		return
	}

	now := time.Now()
	evict, err := planCorpusEviction(cacheRoot, entries, opts.evictAll, nil, true)
	if err != nil {
		slog.Warn("gc cannot compute the cache budget", "error", err)
	}
	streaming := opts.writer != nil
	if streaming {
		printGCTableBanner(opts.writer, cacheRoot, len(entries))
	}
	stats := &gcTableStats{}
	if streaming && opts.sortKey != sortByName {
		// Sorted table (--sort=age|size): sweep, measure every corpus, sort,
		// then evict in table order. The sort needs all rows first, and the
		// size and display info must come before the eviction. This order has
		// two results, which a manual command accepts. The eviction follows
		// the sort, so a canceled run evicts a part that depends on the sort.
		// A cancel during the measurement evicts nothing (run again with
		// --force). With --sort=age, a corpus with no .used marker takes the
		// time of its directory, read before the sweep. The sweep of temporary
		// files moves that time when it removes a file, so a later run can
		// show such a corpus as young.
		for _, e := range entries {
			if ctx.Err() != nil {
				break
			}
			sweepCorpusOrphans(e, now)
		}
		rows := buildGCRows(ctx, entries)
		sortGCRows(rows, opts.sortKey)
		for _, r := range rows {
			if ctx.Err() != nil {
				break
			}
			emitGCRow(opts.writer, stats, r, now, evictIfPlanned(r.entry, trashPath, evict))
		}
	} else {
		// The default name order and the silent opportunistic path make one
		// pass per entry: sweep, measure (streaming only), evict, and render.
		// Rows stream as they come, and a Ctrl-C keeps the evictions done so
		// far.
		for _, e := range entries {
			if ctx.Err() != nil {
				break
			}
			sweepCorpusOrphans(e, now)
			if streaming {
				r := measureGCRow(e)
				emitGCRow(opts.writer, stats, r, now, evictIfPlanned(e, trashPath, evict))
			} else {
				evictIfPlanned(e, trashPath, evict)
			}
		}
	}
	if ctx.Err() == nil {
		evictPlannedUnits(cacheRoot, evict)
	}
	if streaming && stats.rendered > 0 {
		// The summary counts the rendered rows, not len(entries). A Ctrl-C or
		// a cancel can stop the loop part way, and the summary tells what the
		// run saw.
		stats.summary(opts.writer, "evicted")
	}

	// Compiled accelerator models and old runtimes come last. Corpus eviction
	// reclaims more and is the collection's main job, so it takes the time
	// budget first.
	before := time.Now().Add(-providerArtifactAge(opts.evictAll))
	sweepAgedProviderArtifacts(ctx, opts.writer, cacheRoot, before)
	sweepRetiredRuntimes(ctx, opts.writer, cacheRoot, before)

	// Update the stamp last, also after a partial failure. Else a lasting
	// error would make GC run again on each run of Seek.
	touchStamp(cacheRoot)
}

// sweepRetiredRuntimes removes the native runtimes of other Seek versions.
// These are ONNX Runtime under reranker/<version>/<sha256>, USearch under
// semantic/usearch/<key>, and NumKong under semantic/numkong/<key>. The
// runtimes of this binary stay. A runtime goes when its directory is older
// than the age bound (retiredRuntimes), and its next use installs it again.
// Each runtime gets a new name first, so no process finds it half removed.
func sweepRetiredRuntimes(ctx context.Context, writer io.Writer, cacheRoot string, before time.Time) {
	retireRuntimes(ctx, writer, cacheRoot, before, retiredRuntimes(cacheRoot, before))
}

// retireRuntimes removes the listed runtimes and prints a count. It reads each
// age again, because a process can use a runtime after the list. A version
// directory goes with its last runtime, and an old empty one goes too.
func retireRuntimes(ctx context.Context, writer io.Writer, cacheRoot string, before time.Time, due []string) {
	rerankerRoot := filepath.Join(cacheRoot, rerankerDir)
	aged := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.ModTime().Before(before)
	}
	// The removal gives the version directory a new time, so a later age test
	// would keep it: remove it now. Remove fails on a version directory that
	// still holds a runtime.
	removeVersionDir := func(path string) {
		if parent := filepath.Dir(path); filepath.Dir(parent) == rerankerRoot {
			_ = os.Remove(parent)
		}
	}
	if !discardAgedDirs(ctx, writer, "retired runtimes", due, aged, removeVersionDir) {
		return
	}
	// An old empty version directory is the leftover of a cut sweep.
	entries, _ := os.ReadDir(rerankerRoot)
	for _, entry := range entries {
		if isRuntimeVersionDir(entry) && runtimeAged(entry, before) {
			_ = os.Remove(filepath.Join(rerankerRoot, entry.Name()))
		}
	}
}

// retiredRuntimes lists the runtime directories of other Seek versions that
// are older than the age bound, and the leftovers of a cut sweep. The age of
// a directory is its modification time: its install, or for versions that
// touch it (touchRuntimeUse), its last use. With no runtime bundle of its
// own, it lists no ONNX Runtime.
func retiredRuntimes(cacheRoot string, before time.Time) []string {
	usearchRoot := filepath.Join(cacheRoot, "semantic", "usearch")
	due := agedRuntimeDirs(usearchRoot, filepath.Join(usearchRoot, semanticUSearchAssetKey()), before)
	numkongRoot := filepath.Join(cacheRoot, "semantic", "numkong")
	due = append(due, agedRuntimeDirs(numkongRoot, filepath.Join(numkongRoot, semanticNumKongAssetKey()), before)...)
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		return due
	}
	rerankerRoot := filepath.Join(cacheRoot, rerankerDir)
	current := lateOnRuntimeDir(cacheRoot, bundle)
	entries, _ := os.ReadDir(rerankerRoot)
	for _, entry := range entries {
		// Take only the reranker/<version> directories, and each runtime in
		// one by its own age: reranker/coreml and other provider caches keep
		// their own rules.
		if isRuntimeVersionDir(entry) {
			due = append(due, agedRuntimeDirs(filepath.Join(rerankerRoot, entry.Name()), current, before)...)
		}
	}
	return due
}

// agedRuntimeDirs lists the directories in root, other than keep, that are
// older than the age bound or a cut sweep's leftover.
func agedRuntimeDirs(root, keep string, before time.Time) []string {
	entries, _ := os.ReadDir(root)
	var due []string
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() && path != keep && runtimeAged(entry, before) {
			due = append(due, path)
		}
	}
	return due
}

// isRuntimeVersionDir reports a reranker/<version> directory: its name starts
// with a digit.
func isRuntimeVersionDir(entry os.DirEntry) bool {
	return entry.IsDir() && entry.Name()[0] >= '0' && entry.Name()[0] <= '9'
}

// runtimeAged reports a runtime directory older than the age bound (before),
// or one that a cut sweep renamed aside.
func runtimeAged(entry os.DirEntry, before time.Time) bool {
	if strings.HasPrefix(entry.Name(), providerArtifactDiscardPrefix) {
		return true
	}
	info, err := entry.Info()
	return err == nil && info.ModTime().Before(before)
}

// touchRuntimeUse marks a runtime directory as used now, the age that
// sweepRetiredRuntimes reads.
func touchRuntimeUse(dir string) {
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
}

// touchStamp updates the .last-gc modification time (touchCacheFile).
func touchStamp(cacheRoot string) {
	_ = touchCacheFile(cacheRoot, gcStampFile)
}

// touchCacheFile sets the modification time of the marker cacheDir/name to
// now. When that fails (the file is missing), it makes the marker with a
// temporary file and a rename (writeCacheFile). Two touches can share the
// temporary file, so one rename can fail; the other touch makes the marker.
func touchCacheFile(cacheDir, name string) error {
	now := time.Now()
	if err := os.Chtimes(filepath.Join(cacheDir, name), now, now); err == nil {
		return nil
	}
	return writeCacheFile(cacheDir, name, "")
}

// warnNFSOnce logs a warning the first time GC finds the cache on a network
// file system. It then writes .last-gc, and while that file exists, later
// runs log at Debug level only.
func warnNFSOnce(cacheRoot string) {
	stampPath := filepath.Join(cacheRoot, gcStampFile)
	if _, err := os.Stat(stampPath); errors.Is(err, fs.ErrNotExist) {
		slog.Warn(
			"seek cache on network filesystem; auto-GC disabled. "+gcLocalCacheHint,
			"cache_root", cacheRoot,
		)
		touchStamp(cacheRoot)
		return
	}
	slog.Debug("gc skipped: cache on network filesystem", "cache_root", cacheRoot)
}

// providerArtifactAge is the age after which GC collects a compiled model or
// another version's runtime: zero with --all.
func providerArtifactAge(all bool) time.Duration {
	if all {
		return 0
	}
	return providerArtifactMaxAge
}

// gcIntervalFromEnv returns the interval of SEEK_GC_INTERVAL, or
// defaultGCInterval when the variable is not set or not valid.
func gcIntervalFromEnv() time.Duration {
	if v := os.Getenv(envGCInterval); v != "" {
		if d, err := parseGCDuration(v); err == nil && d >= 0 {
			return d
		}
		slog.Debug("invalid SEEK_GC_INTERVAL, using default", "value", v)
	}
	return defaultGCInterval
}

// parseGCDuration adds a `d` (days) suffix to time.ParseDuration. It reads
// `14d` or `7d` as multiples of 24h. Any other value goes to time.ParseDuration
// (ns, us, ms, s, m, h, and compounds such as `1h30m`). Seek does not support a
// compound with `d`. `1d2h` is an error, and `2h1d` gives a wrong value. Give
// such a value in hours.
func parseGCDuration(s string) (time.Duration, error) {
	if n := len(s); n > 1 && s[n-1] == 'd' {
		days, err := time.ParseDuration(s[:n-1] + "h")
		if err != nil {
			return 0, err
		}
		return days * 24, nil
	}
	return time.ParseDuration(s)
}

// shouldRunGC reports whether interval has passed since the .last-gc stamp,
// or whether no stamp exists.
func shouldRunGC(cacheRoot string, interval time.Duration) bool {
	st, err := os.Stat(filepath.Join(cacheRoot, gcStampFile))
	if err != nil {
		return true
	}
	return time.Since(st.ModTime()) >= interval
}

// buildTmpMaxAge is the age after which a temporary build directory is a
// leftover of a builder that crashed. A live builder writes shards all the
// time, so the mtime of its directory stays well within this bound, also for
// a large cold build.
const buildTmpMaxAge = time.Hour

// providerArtifactDiscardPrefix marks a compiled model or runtime that a sweep
// is removing. A removal in place is not atomic. A cut removal changes the
// directory's modification time, so the age test sees the directory as new.
// It also leaves a broken compiled model or runtime where Seek expects a whole
// one. A rename first marks the leftover, so a later sweep removes it at any
// age, as drainTrash retries a partly evicted corpus.
const providerArtifactDiscardPrefix = ".discarding-"

// sweepAgedProviderArtifacts removes compiled accelerator models that no search
// used recently. Their cache key holds the operating system build. So each
// system update leaves the previous directory unused, and each one holds tens
// of megabytes.
//
// The removal of a model that is still in use costs a new compile. It also
// costs a new provider check, because the verdict file is in the same
// directory. So the age comes from the access marker that a search updates. The
// directory's own time does not change after the compile.
//
// It reports what it reclaimed to writer, so a live collection says the same
// thing its plan did. A nil writer stays silent, which is the opportunistic
// path.
//
// Callers reach this only from runGC. The dry-run path returns before runGC, so
// this never runs for a plan.
func sweepAgedProviderArtifacts(ctx context.Context, writer io.Writer, cacheRoot string, before time.Time) {
	// Read the access marker again before the rename. The list and the removal
	// are not one step, and a search that started in between uses the model.
	unused := func(path string) bool {
		info, err := os.Stat(filepath.Join(path, providerArtifactUsedFile))
		return err != nil || info.ModTime().Before(before)
	}
	discardAgedDirs(ctx, writer, "compiled model files", agedProviderArtifacts(ctx, cacheRoot, before), unused, nil)
}

// providerArtifactRoot is the cache folder of the compiled accelerator models:
// reranker/coreml/<format>/<key>. It is here because the collector compiles on
// every target, and the provider code only where an accelerator exists.
func providerArtifactRoot(cacheRoot string) string {
	return filepath.Join(cacheRoot, rerankerDir, "coreml")
}

// agedProviderArtifacts lists the compiled models a collection would remove,
// so the live sweep and `--dry-run` agree on what is due. An entry a previous
// sweep left marked is always due, whatever its age.
func agedProviderArtifacts(ctx context.Context, cacheRoot string, before time.Time) []string {
	root := providerArtifactRoot(cacheRoot)
	formats, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var due []string
	for _, format := range formats {
		if ctx.Err() != nil {
			return due
		}
		if !format.IsDir() {
			continue
		}
		formatPath := filepath.Join(root, format.Name())
		keys, err := os.ReadDir(formatPath)
		if err != nil {
			continue
		}
		for _, key := range keys {
			if ctx.Err() != nil {
				return due
			}
			path := filepath.Join(formatPath, key.Name())
			if key.IsDir() && (strings.HasPrefix(key.Name(), providerArtifactDiscardPrefix) ||
				providerArtifactIsAged(path, key, before)) {
				due = append(due, path)
			}
		}
	}
	return due
}

// providerArtifactIsAged reports whether no search used a compiled model since
// before. It reads the access marker first. Nothing updates the directory's
// own modification time after the compile. So without the marker, a model in
// daily use would age as an unused one does.
func providerArtifactIsAged(path string, key os.DirEntry, before time.Time) bool {
	if info, err := os.Stat(filepath.Join(path, providerArtifactUsedFile)); err == nil {
		return info.ModTime().Before(before)
	}
	info, err := key.Info()
	return err == nil && info.ModTime().Before(before)
}

// discardAgedDirs removes the due directories and prints their count under
// label to writer; a nil writer stays silent. It renames each directory aside
// first, so no process finds it half removed. A directory that a cut sweep
// renamed aside goes whatever its age. Any other directory goes only when aged
// still reports it, because a process can use it after the list. A removal
// that fails leaves the marked directory for the next sweep. When after is not
// nil, it runs on each path after the removal attempt. discardAgedDirs returns
// false when ctx ends first, and then prints nothing.
func discardAgedDirs(
	ctx context.Context,
	writer io.Writer,
	label string,
	due []string,
	aged func(path string) bool,
	after func(path string),
) bool {
	removed := 0
	var reclaimed int64
	for _, path := range due {
		if ctx.Err() != nil {
			return false
		}
		discarded := path
		if name := filepath.Base(path); !strings.HasPrefix(name, providerArtifactDiscardPrefix) {
			if !aged(path) {
				continue
			}
			discarded = filepath.Join(filepath.Dir(path), providerArtifactDiscardPrefix+name)
			if err := os.Rename(path, discarded); err != nil {
				slog.Debug("gc cannot retire a directory", "path", path, "error", err)
				continue
			}
		}
		// The rename moves the directory and keeps its size.
		size := corpusDirSize(discarded)
		if err := os.RemoveAll(discarded); err != nil {
			slog.Debug("gc cannot remove a retired directory", "path", path, "error", err)
		} else {
			slog.Debug("gc removed a retired directory", "path", path)
			removed++
			reclaimed += size
		}
		if after != nil {
			after(path)
		}
	}
	if removed > 0 && writer != nil {
		_, _ = fmt.Fprintf(writer, "%s removed: %d (%s)\n", label, removed, humanBytes(reclaimed))
	}
	return true
}

// sweepOrphanBuildDirs removes the .build-* temporary index directories that
// a crashed or killed builder left, when their mtime is older than `before`.
// The directory of a running build has a recent mtime, so it stays.
func sweepOrphanBuildDirs(indexDir string, before time.Time) {
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if !ent.IsDir() || !strings.HasPrefix(ent.Name(), buildDirPrefix) {
			continue
		}
		info, err := ent.Info()
		if err != nil || !info.ModTime().Before(before) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(indexDir, ent.Name()))
	}
}

// sweepOrphanManifestTmps removes old temporary files from a corpus cache or
// index directory: the .tmp files of the manifests and the temp files of
// writeSharedCacheFile. Each writer writes a temporary file and renames it. A
// forced stop can leave the temporary file. Readers use only the canonical
// names.
//
// Restrict cleanup to known names (isOrphanManifestTmpName) so it cannot
// remove an unrelated file.
func sweepOrphanManifestTmps(cacheDir string, before time.Time) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !isOrphanManifestTmpName(name) {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(before) {
			continue
		}
		_ = os.Remove(filepath.Join(cacheDir, name))
	}
}

// isOrphanManifestTmpName reports a temporary file name of a manifest or of
// writeSharedCacheFile.
func isOrphanManifestTmpName(name string) bool {
	switch name {
	case folderManifestFileName + cacheTmpSuffix,
		uncommittedManifestFileName + cacheTmpSuffix,
		familyManifestFile + cacheTmpSuffix:
		return true
	}
	for _, record := range []string{bm25StatsFile, corpusSizeFile} {
		if strings.HasPrefix(name, record+sharedTmpInfix) {
			return true
		}
	}
	return false
}

// enumerateCorpusDirs returns the cache's corpora in hash order, because
// os.ReadDir sorts by name. So the live table has the order of the dry-run
// table, and the opportunistic sweep has a fixed order.
func enumerateCorpusDirs(corporaPath string) ([]corpusDirEntry, error) {
	entries, err := os.ReadDir(corporaPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]corpusDirEntry, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !isCorpusHashName(name) {
			continue
		}
		path := filepath.Join(corporaPath, name)
		used, ok := readUsedAt(path)
		if !ok {
			// A corpus with no .used marker takes the mtime of its
			// directory.
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			used = st.ModTime()
		}
		out = append(out, corpusDirEntry{name: corpusID(name), path: path, usedAt: used})
	}
	return out, nil
}

// isCorpusHashName checks for the exact hex form that newCorpusID makes:
// corpusHashHexLen lowercase hex characters. A looser check could take a
// directory of the user that is in corpora/ too.
func isCorpusHashName(name string) bool {
	return len(name) == corpusHashHexLen && strings.Trim(name, "0123456789abcdef") == ""
}

func readUsedAt(cacheDir string) (time.Time, bool) {
	st, err := os.Stat(filepath.Join(cacheDir, usedFile))
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), true
}

// sweepCorpusOrphans removes the leftovers of a crash in one corpus
// directory:
//   - .tmp manifest files (folder, uncommitted, family) and the temporary
//     files of writeSharedCacheFile (.bm25-v2, .size) older than 1 hour. A
//     SIGKILL between the write and the rename can leave them, and no live
//     process reads them again. Errors stay silent, because the user has
//     nothing to do.
//   - old .build-* temporary index directories, only when no build holds the
//     build lock. So a slow live builder (mtime older than the bound) keeps
//     its directory. evictCorpus uses the same guard.
func sweepCorpusOrphans(e corpusDirEntry, now time.Time) {
	sweepOrphanManifestTmps(e.path, now.Add(-1*time.Hour))
	// The publish puts .family-v1 beside the shards, not in cacheDir, so the
	// sweep also reads the index directory for its .tmp file.
	sweepOrphanManifestTmps(filepath.Join(e.path, corpusIndexDir), now.Add(-1*time.Hour))
	if bf, ok, _ := tryBuildLock(e.path); ok {
		releaseLock(bf)
		sweepOrphanBuildDirs(filepath.Join(e.path, corpusIndexDir), now.Add(-buildTmpMaxAge))
	}
}

// evictIfPlanned applies the eviction plan to one corpus and logs a failed or
// partial eviction. The loops of runGC and enforceCacheBudget share it.
func evictIfPlanned(e corpusDirEntry, trashDir string, evict map[corpusID]bool) gcRowResult {
	if !evict[e.name] {
		return gcRowResult{action: actionKept}
	}
	res := evictCorpus(e, trashDir)
	switch res.action {
	case actionFailed:
		slog.Warn("gc eviction failed", "corpus", e.name, "error", res.err)
	case actionTrashed:
		slog.Warn("gc partial eviction", "corpus", e.name, "error", res.err)
	}
	return res
}

// evictCorpus deletes a corpus in two steps (rename, then RemoveAll) under a
// non-blocking lock of the corpus. Its gcRowResult tells the outcome:
//   - actionLocked: another Seek holds the corpus .lock or .build.lock.
//   - actionKept: .used is newer than e.usedAt, the time of the list (a new
//     use since the list).
//   - actionGone: the corpus directory went away (another process).
//   - actionEvicted: the rename and RemoveAll worked.
//   - actionTrashed: the rename worked and RemoveAll failed; drainTrash
//     finishes it.
//   - actionFailed: the lock open, build-lock check, or rename gave an error
//     other than ENOENT.
func evictCorpus(e corpusDirEntry, trashDir string) gcRowResult {
	lockPath := filepath.Join(e.path, lockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return gcRowResult{action: actionGone}
		}
		return gcRowResult{action: actionFailed, err: fmt.Errorf("open corpus lock: %w", err)}
	}
	// releaseLock accepts nil, so after the rename clears f below, the
	// deferred call does nothing.
	defer func() { releaseLock(f) }()
	if err := lockFileExclusive(f); err != nil {
		return gcRowResult{action: actionLocked}
	}

	// A temp-swap build holds the build lock, not the publish/read lock above.
	// Skip eviction while a build is in progress, so the builder keeps its
	// corpus directory.
	bf, ok, bErr := tryBuildLock(e.path)
	if bErr != nil {
		return gcRowResult{action: actionFailed, err: fmt.Errorf("try build lock: %w", bErr)}
	}
	if !ok {
		return gcRowResult{action: actionLocked}
	}
	releaseLock(bf)

	if used, ok := readUsedAt(e.path); ok && used.After(e.usedAt) {
		return gcRowResult{action: actionKept}
	}

	trashName := fmt.Sprintf("%s-%d", e.name, time.Now().UnixNano())
	target := filepath.Join(trashDir, trashName)
	if err := os.Rename(e.path, target); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return gcRowResult{action: actionGone}
		}
		return gcRowResult{action: actionFailed, err: fmt.Errorf("rename to trash: %w", err)}
	}

	// Release the corpus lock before RemoveAll. The old path is gone, so no
	// other Seek can lock the corpus under that name.
	releaseLock(f)
	// Keep the deferred release off the closed fd.
	f = nil

	if err := os.RemoveAll(target); err != nil {
		// A part stays; drainTrash finishes it on the next run.
		return gcRowResult{action: actionTrashed, err: fmt.Errorf("remove trash entry: %w", err)}
	}
	return gcRowResult{action: actionEvicted}
}

// pickDisplayShard chooses the shard whose metadata the table reads. It takes
// a committed shard first, so Repository.Source comes from the committed
// corpus when there is one. Repository.Name is an opaque name, so the table
// reads Source. When all shards are uncommitted, it returns shards[0]: their
// Source is the repository path too.
func pickDisplayShard(shards []string) string {
	for _, s := range shards {
		if !isUncommittedShard(filepath.Base(s)) {
			return s
		}
	}
	return shards[0]
}

// corpusDisplayInfo is a corpus summary for the ROOT column of the seek gc
// table. `--dry-run` (reportGCPlan) and a live `seek gc` (the streaming path
// of runGC) use it. source is the absolute root on disk (Zoekt
// Repository.Source). gone is true when the source path is not on disk.
type corpusDisplayInfo struct {
	source string
	gone   bool
}

// readCorpusDisplayInfo reads Repository.Source from a committed shard when one
// exists. It uses an uncommitted shard when that is the only available shard.
//
// It returns an empty value when no readable shard exists. The table then
// shows "[empty]" in the ROOT column (formatRoot).
func readCorpusDisplayInfo(corpusDir string) corpusDisplayInfo {
	indexDir := filepath.Join(corpusDir, corpusIndexDir)
	shards, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil || len(shards) == 0 {
		return corpusDisplayInfo{}
	}
	pick := pickDisplayShard(shards)
	repos, err := zoektReadMetadata(pick)
	if err != nil || len(repos) == 0 {
		return corpusDisplayInfo{}
	}
	source := strings.TrimSpace(repos[0].Source)
	if source == "" {
		return corpusDisplayInfo{}
	}
	info := corpusDisplayInfo{source: source}
	if _, err := os.Stat(source); errors.Is(err, fs.ErrNotExist) {
		info.gone = true
	}
	return info
}

// drainTrash removes all entries under trashDir. It finishes a RemoveAll that
// a kill -9 stopped, and a second run does no harm.
func drainTrash(trashDir string) {
	entries, err := os.ReadDir(trashDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if err := os.RemoveAll(filepath.Join(trashDir, ent.Name())); err != nil {
			slog.Warn("gc drain trash entry", "entry", ent.Name(), "error", err)
		}
	}
}

// cacheRootOnNFS reports whether the cache root is on a network file system.
// touchUsed checks it once per process; tests replace it.
var cacheRootOnNFS = sync.OnceValue(checkCacheRootOnNFS)

// checkCacheRootOnNFS reports whether the cache root is on a network file
// system. A root that it cannot find counts as local.
func checkCacheRootOnNFS() bool {
	root, err := seekUserCacheRoot()
	return err == nil && isOnNFS(root)
}

// touchUsed updates <cacheDir>/.used. It logs errors at Debug level and skips
// caches on network filesystems. Scoped and unscoped searches of one
// repository read one index, so both keep it recent for LRU eviction.
func touchUsed(cacheDir string) {
	if cacheRootOnNFS() {
		return
	}
	if err := touchCacheFile(cacheDir, usedFile); err != nil {
		slog.Debug("touch used failed", "cache_dir", cacheDir, "error", err)
	}
}
