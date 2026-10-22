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
	// Cache-root level files.
	gcStampFile = ".last-gc"
	gcLockFile  = "gc.lock"
	corporaDir  = "corpora"
	rerankerDir = "reranker"
	gcTrashDir  = ".trash"

	// Per-corpus marker. Its mtime records the last access, because most
	// file systems mount with noatime or relatime, which keep no current atime.
	usedFile          = ".used"
	defaultGCInterval = 24 * time.Hour
	gcRunTimeout      = 5 * time.Second
	// providerArtifactMaxAge collects compiled accelerator models that no
	// search used for this long, and the runtimes of other Seek versions
	// whose directories are this old. The cache budget does not cover them.
	providerArtifactMaxAge = 14 * 24 * time.Hour

	envGCInterval = "SEEK_GC_INTERVAL"

	gcLocalCacheHint = "Set SEEK_CACHE_DIR to a local directory. " +
		"On Linux, you can instead unset SEEK_CACHE_DIR and set XDG_CACHE_HOME."
)

type corpusDirEntry struct {
	name   corpusID
	path   string
	usedAt time.Time
}

// gcAction enumerates the outcomes a per-corpus eviction attempt can produce.
// Drives the ACTION column in the manual `seek gc` table. Past tense by
// design: live rows describe what happened; dry-run rows describe what would
// happen if executed.
type gcAction uint8

const (
	actionKept    gcAction = iota // not planned, or used again since the plan
	actionEvicted                 // rename to .trash + RemoveAll succeeded
	actionLocked                  // a search or build holds .lock or .build.lock
	actionGone                    // corpus dir vanished mid-flight
	actionTrashed                 // renamed, RemoveAll failed; drainTrash retries
	actionFailed                  // open-lock, build-lock check, or rename error
)

// gcRowResult is what evictCorpus returns. action drives the ACTION column;
// err is populated only for trashed / failed (callers log it via slog.Warn).
type gcRowResult struct {
	action gcAction
	err    error
}

// String renders the ACTION cell. trashed / failed carry a short error tail
// so the table is enough to diagnose without scrolling stderr.
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
	case actionFailed:
		return "failed: " + shortErr(r.err)
	}
	return "?"
}

// shortErr renders an error for the ACTION cell: strips trailing newlines
// and clamps to 60 bytes so a "trashed: <reason>" / "failed: <reason>"
// row stays on a single terminal line. Defensive nil → "unknown" (callers
// only invoke this on populated err, but the table must never crash).
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
		s = s[:max] + "..."
	}
	return s
}

// gcOptions tunes a single runGC invocation.
//   - skipThrottle bypasses the .last-gc mtime gate (`seek gc --force` or
//     `--all`).
//   - evictAll evicts every corpus that no search holds (`seek gc --all`).
//     Otherwise GC evicts the least recently used corpora only while the
//     cache is over its budget; age alone never evicts a corpus.
//   - writer, when non-nil, makes runGC stream a per-corpus table to it
//     (manual path). Nil = silent (opportunistic path after every run).
//   - sortKey orders the streamed table (`seek gc --sort`). Only consulted
//     when writer is non-nil; the zero value keeps the hash order, so the
//     opportunistic path is unaffected.
type gcOptions struct {
	skipThrottle bool
	evictAll     bool
	writer       io.Writer
	sortKey      gcSortKey
}

// fireOpportunisticGC runs runFn in a goroutine bounded by timeout, blocking
// the caller only until the goroutine finishes or the deadline fires —
// whichever comes first. It is a function of its own, not inline in main(),
// so unit tests can check the goroutine, the timeout select, and the context
// cleanup: main() ends with os.Exit, which a test cannot run.
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

// runGC is the shared GC body. Two callers:
//   - runOpportunisticGC: writer nil → silent eviction sweep, post-run.
//   - runGCCommandCmd (`seek gc`, `--force`, `--all`): writer os.Stdout →
//     streams the same banner + table + summary as `--dry-run`, with the
//     ACTION column reflecting the real per-corpus outcome.
//
// Order of operations:
//  1. throttle gate (`.last-gc` mtime) unless skipThrottle
//  2. NFS detection (auto-GC disabled on NFS)
//  3. acquire global gc.lock (other gc → skip, surfaced when streaming)
//  4. drain any leftover trash (idempotent crash recovery)
//  5. enumerate + sort corpora by hash (deterministic sweep order)
//  6. plan the eviction (planCorpusEviction): every corpus with --all, else
//     the least recently used corpora while the cache is over its budget
//  7. streaming with --sort=age|size: sweep, measure all rows, sort, then
//     evict + render in table order (cancel semantics: see the sorted-
//     branch comment). Default name order and the silent path: one
//     interleaved sweep → measure (streaming only) → evict pass per
//     entry, rows streaming incrementally.
//  8. evict the unit-store segments of the planned corpora, then sweep aged
//     compiled accelerator models and the old runtimes of other versions
//  9. stamp .last-gc to throttle the next opportunistic run
func runGC(ctx context.Context, opts gcOptions) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		slog.Debug("gc skipped: cannot resolve cache root", "error", err)
		return
	}

	// Throttle gate FIRST — on the warm path this exits after a single
	// stat() on .last-gc and skips the MkdirAll + Statfs that would
	// otherwise fire on every seek invocation.
	if !opts.skipThrottle && !shouldRunGC(cacheRoot, gcIntervalFromEnv()) {
		// Opportunistic path (writer nil) silently no-ops; manual
		// `seek gc` callers (writer != nil) get a one-line hint so
		// they know that nothing ran and how to force a run.
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
		// Surface contention to the manual caller — otherwise `seek gc
		// --force` would exit silently with no indication another gc
		// took the lock. Opportunistic callers (writer nil) stay Debug.
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
		// Still update stamp to throttle next attempt.
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
		// Sorted table (--sort=age|size): sweep, then measure every corpus,
		// then sort, then evict in table order — ordering needs all rows up
		// front, and size + display info must be captured before eviction
		// anyway. Consequences, acceptable for a manual command: eviction
		// order follows the sort, so a ctx-truncated run evicts a
		// sort-dependent subset; and a cancel DURING the measurement phase
		// evicts nothing at all (rerun with --force). Note --sort=age can
		// show legacy corpora (no .used, dir mtime fallback) as perpetually
		// young: the manifest-tmp sweep bumps the dir mtime whenever it
		// removes something.
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
		// Default name order and the silent opportunistic path: one
		// interleaved pass per entry — sweep, measure (streaming only),
		// evict, render. Rows stream incrementally and a Ctrl-C keeps the
		// already-evicted prefix.
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
		// Rendered count (not len(entries)) — Ctrl-C / ctx cancel may break
		// the loop mid-sweep; summary must describe what we actually saw.
		stats.summary(opts.writer, "evicted")
	}

	// Compiled accelerator models and old runtimes come last. Corpus eviction
	// reclaims more and is the collection's main job, so it takes the time
	// budget first.
	sweepAgedProviderArtifacts(ctx, opts.writer, cacheRoot, time.Now().Add(-providerArtifactAge(opts.evictAll)))
	sweepRetiredRuntimes(ctx, opts.writer, cacheRoot, time.Now().Add(-providerArtifactAge(opts.evictAll)))

	// Stamp updated last and even on partial failures — prevents retry
	// storms when persistent errors would otherwise re-run GC every
	// invocation.
	touchStamp(cacheRoot)
}

// sweepRetiredRuntimes removes the native runtimes of other Seek versions:
// ONNX Runtime under reranker/<version>/<sha256> (or the older flat
// reranker/ort-<version>-<platform>-<hash> layout), USearch under
// semantic/usearch/<key>, and NumKong under semantic/numkong/<key>. The
// runtimes of this binary stay. A runtime goes
// when its directory is older than the age bound (retiredRuntimes); its next
// use installs it again. Each runtime is renamed aside first, so no process
// finds it half removed.
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
	current := filepath.Join(rerankerRoot, bundle.version, bundle.sha256)
	entries, _ := os.ReadDir(rerankerRoot)
	for _, entry := range entries {
		// Take only runtime names: reranker/coreml and other provider
		// caches keep their own rules.
		name := strings.TrimPrefix(entry.Name(), providerArtifactDiscardPrefix)
		path := filepath.Join(rerankerRoot, entry.Name())
		switch {
		case !entry.IsDir():
		case strings.HasPrefix(name, "ort-"):
			// The older flat layout holds the library itself.
			if runtimeAged(entry, before) {
				due = append(due, path)
			}
		case isRuntimeVersionDir(entry):
			// reranker/<version>/<sha256>: each runtime by its own age.
			due = append(due, agedRuntimeDirs(path, current, before)...)
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

// touchStamp updates the .last-gc modification time. If the file does not yet
// exist, it creates the file with an atomic rename.
func touchStamp(cacheRoot string) {
	stampPath := filepath.Join(cacheRoot, gcStampFile)
	now := time.Now()
	if err := os.Chtimes(stampPath, now, now); err == nil {
		return
	}
	_ = writeCacheFile(cacheRoot, gcStampFile, "")
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

// parseGCDuration extends time.ParseDuration with a `d` (days) suffix.
// Accepts `14d`, `7d`, etc. as 24h multiples. Falls through to the stdlib
// parser for everything else (ns/us/ms/s/m/h and compounds like `1h30m`).
// Compound forms with `d` (e.g. `1d2h`) are not supported; users should
// convert to hours.
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

func shouldRunGC(cacheRoot string, interval time.Duration) bool {
	st, err := os.Stat(filepath.Join(cacheRoot, gcStampFile))
	if err != nil {
		return true
	}
	return time.Since(st.ModTime()) >= interval
}

// buildTmpMaxAge is the age past which a leftover temp build dir is considered
// orphaned (its builder crashed). A live builder writes shards continuously, so
// its temp dir's mtime stays fresh well within this bound even for a large cold
// build.
const buildTmpMaxAge = time.Hour

// providerArtifactDiscardPrefix marks a compiled model or runtime that is being
// removed. A removal in place is not atomic. A cut removal changes the
// directory's modification time, so the age test sees the directory as new.
// It also leaves a broken compiled model or runtime where Seek expects a whole
// one. A rename first marks the leftover, so a later sweep removes it at any
// age, as drainTrash retries a partly evicted corpus.
const providerArtifactDiscardPrefix = ".discarding-"

// sweepAgedProviderArtifacts removes compiled accelerator models that no search
// used recently. Their cache key carries the operating system build, so every
// system update strands the previous directory, and each one holds tens of
// megabytes.
//
// Removing one that is still wanted costs a recompile AND a fresh provider
// check, because the verdict file lives in the same directory. It therefore
// ages on the access marker a search refreshes, not on the directory's own
// timestamp, which nothing moves after the compile.
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

// agedProviderArtifacts lists the compiled models a collection would remove,
// so the live sweep and `--dry-run` agree on what is due. An entry a previous
// sweep left marked is always due, whatever its age.
func agedProviderArtifacts(ctx context.Context, cacheRoot string, before time.Time) []string {
	root := filepath.Join(cacheRoot, rerankerDir, "coreml")
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

// providerArtifactIsAged reports whether a compiled model has gone unwanted for
// long enough. It prefers the access marker, because nothing refreshes the
// directory's own modification time after the compile, so a model in daily use
// would otherwise age exactly like an abandoned one.
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

// sweepOrphanBuildDirs removes leftover .build-* temp index dirs (from a
// crashed or killed builder) whose mtime is older than `before`. A
// still-running build's temp dir has a recent mtime and is skipped.
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

func isOrphanManifestTmpName(name string) bool {
	switch name {
	case folderManifestFileName + ".tmp",
		uncommittedManifestFileName + ".tmp",
		familyManifestFile + ".tmp":
		return true
	}
	for _, record := range []string{bm25StatsFile, corpusSizeFile} {
		if strings.HasPrefix(name, record+sharedTmpInfix) {
			return true
		}
	}
	return false
}

// enumerateCorpusDirs returns the cache's corpora hash-sorted (os.ReadDir
// sorts by name), so the streamed live table matches dry-run ordering and
// opportunistic-path eviction has a deterministic sweep order.
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
			// Backward compat: existing caches without .used use dir
			// mtime as a conservative fallback.
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

// isCorpusHashName checks for the exact hex shape produced by newCorpusID
// (corpusHashHexLen chars, lowercase hex). Looser checks would risk picking
// up arbitrary user dirs colocated under corpora/.
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

// sweepCorpusOrphans removes crash leftovers inside one corpus dir:
//   - orphan .tmp manifest files (folder, uncommitted, family) and the
//     temp files of writeSharedCacheFile (.bm25-v2, .size) older than 1
//     hour. SIGKILL between the write and the rename can leave these
//     behind; no live process ever reads them again. Best-effort — errors
//     are silent because the user does not need to act.
//   - stale .build-* temp index dirs, only when no build holds the build
//     lock, so a genuinely-stalled-but-live builder (mtime older than the
//     bound) is not pulled out from under itself. Same guard evictCorpus
//     uses.
func sweepCorpusOrphans(e corpusDirEntry, now time.Time) {
	sweepOrphanManifestTmps(e.path, now.Add(-1*time.Hour))
	// .family-v1 is published beside the shards, not in cacheDir, so its
	// interrupted tmp needs the index dir swept too.
	sweepOrphanManifestTmps(filepath.Join(e.path, "index"), now.Add(-1*time.Hour))
	if bf, ok, _ := tryBuildLock(e.path); ok {
		releaseLock(bf)
		sweepOrphanBuildDirs(filepath.Join(e.path, "index"), now.Add(-buildTmpMaxAge))
	}
}

// evictIfPlanned applies the eviction plan to one corpus. Shared by the
// streaming and silent runGC loops.
func evictIfPlanned(e corpusDirEntry, trashDir string, evict map[corpusID]bool) gcRowResult {
	if !evict[e.name] {
		return gcRowResult{action: actionKept}
	}
	return evictPlanned(e, trashDir)
}

// evictCorpus performs a two-phase delete under a non-blocking per-corpus
// lock. Returns a gcRowResult describing the outcome:
//   - actionLocked: corpus .lock or .build.lock held by another seek
//   - actionKept:   .used changed between enumeration and lock (a new use)
//   - actionGone:   corpus dir vanished mid-flight (concurrent process)
//   - actionEvicted: rename + RemoveAll both succeeded
//   - actionTrashed: rename succeeded, RemoveAll failed; drainTrash finishes
//   - actionFailed:  open-lock, build-lock check, or rename returned a
//     non-ENOENT error
func evictCorpus(e corpusDirEntry, trashDir string, cutoff time.Time) gcRowResult {
	lockPath := filepath.Join(e.path, lockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return gcRowResult{action: actionGone}
		}
		return gcRowResult{action: actionFailed, err: fmt.Errorf("open corpus lock: %w", err)}
	}
	// releaseLock is nil-safe, so once f is cleared below (post-rename) the
	// deferred call is a no-op.
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

	if used, ok := readUsedAt(e.path); ok && used.After(cutoff) {
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

	// Release per-corpus lock before RemoveAll. Original path no longer
	// exists, so no other seek can re-acquire under the old name.
	releaseLock(f)
	// Prevent the deferred release from operating on a stale fd.
	f = nil

	if err := os.RemoveAll(target); err != nil {
		// Partial cleanup; drainTrash will finish next run.
		return gcRowResult{action: actionTrashed, err: fmt.Errorf("remove trash entry: %w", err)}
	}
	return gcRowResult{action: actionEvicted}
}

// pickDisplayShard chooses which shard's metadata to read for display.
// Prefers a non-uncommitted shard so Repository.Source comes from the committed
// corpus when available. Repository.Name is the opaque name of the
// repository. Returns shards[0] when all shards are uncommitted; their Source
// is still the repository path.
func pickDisplayShard(shards []string) string {
	for _, s := range shards {
		if !isUncommittedShard(filepath.Base(s)) {
			return s
		}
	}
	return shards[0]
}

// corpusDisplayInfo summarizes a corpus for the ROOT column of the seek gc
// table — used by both `--dry-run` (reportGCPlan) and a live `seek gc` (runGC
// streaming path). Source is the absolute on-disk root (from zoekt
// Repository.Source). When the source path no longer exists on the
// filesystem, gone is true.
type corpusDisplayInfo struct {
	source string
	gone   bool
}

// readCorpusDisplayInfo reads Repository.Source from a committed shard when one
// exists. It uses an uncommitted shard when that is the only available shard.
//
// It returns an empty value when no readable shard exists. Callers then show
// the corpus hash.
func readCorpusDisplayInfo(corpusDir string) corpusDisplayInfo {
	indexDir := filepath.Join(corpusDir, "index")
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

// drainTrash removes all entries under trashDir. Idempotent crash-recovery:
// finishes any RemoveAll interrupted by a previous kill -9.
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

// nfsCheckOnce and nfsCached hold whether the cache root is on a network
// filesystem. touchUsed checks it once per process.
var (
	nfsCheckOnce sync.Once
	nfsCached    bool
)

// touchUsed updates <cacheDir>/.used. It logs errors at Debug level and skips
// caches on network filesystems.
func touchUsed(cacheDir string) {
	nfsCheckOnce.Do(func() {
		if root, err := seekUserCacheRoot(); err == nil {
			nfsCached = isOnNFS(root)
		}
	})
	if nfsCached {
		return
	}
	p := filepath.Join(cacheDir, usedFile)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err == nil {
		return
	}
	// File missing (or chtimes failed): create it via tmp+rename. Two
	// touches can share the tmp file, so one rename can fail; the other
	// touch makes the marker.
	if err := writeCacheFile(cacheDir, usedFile, ""); err != nil {
		slog.Debug("touch used failed", "cache_dir", cacheDir, "error", err)
	}
}
