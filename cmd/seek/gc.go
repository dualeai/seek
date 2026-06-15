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
	"sort"
	"strings"
	"sync"
	"time"

	zoektindex "github.com/sourcegraph/zoekt/index"
)

const (
	// Cache-root level files.
	gcStampFile = ".last-gc"
	gcLockFile  = "gc.lock"
	corporaDir  = "corpora"
	gcTrashDir  = ".trash"

	// Per-corpus marker. mtime = "last access" — explicit, since atime is
	// disabled (noatime/relatime) on most modern filesystems.
	usedFile = ".used"

	defaultGCMaxAge   = 14 * 24 * time.Hour
	defaultGCInterval = 24 * time.Hour
	gcRunTimeout      = 5 * time.Second

	envGCMaxAge   = "SEEK_GC_MAX_AGE"
	envGCInterval = "SEEK_GC_INTERVAL"
)

type gcConfig struct {
	maxAge   time.Duration
	interval time.Duration
}

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
	actionKept    gcAction = iota // .used within TTL, or TOCTOU re-bump under lock
	actionEvicted                 // rename to .trash + RemoveAll succeeded
	actionLocked                  // corpus .lock held by active indexer/searcher
	actionGone                    // corpus dir vanished mid-flight
	actionTrashed                 // renamed to .trash but RemoveAll failed; drainTrash will finish
	actionFailed                  // open-lock or rename error
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
//   - skipThrottle bypasses the .last-gc mtime gate (manual `seek gc --force`).
//   - maxAge is the per-corpus TTL — entries with .used older than now-maxAge
//     are evicted. `seek gc --all` collapses this to 0.
//   - writer, when non-nil, makes runGC stream a per-corpus table to it
//     (manual path). Nil = silent (opportunistic path after every search).
type gcOptions struct {
	skipThrottle bool
	maxAge       time.Duration
	writer       io.Writer
}

// fireOpportunisticGC runs runFn in a goroutine bounded by timeout, blocking
// the caller only until the goroutine finishes or the deadline fires —
// whichever comes first. Extracted from main() so the orchestration
// (goroutine spawn, select-on-timeout, context cleanup) is unit-testable
// instead of buried behind os.Exit.
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

// runOpportunisticGC is the entry point fired from main() after a successful
// search. Every failure is logged and swallowed; never returns an error and
// never affects exit code. Bounded by ctx (gcRunTimeout).
func runOpportunisticGC(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("gc panic", "error", r)
		}
	}()
	cfg := gcConfigFromEnv()
	runGC(ctx, gcOptions{maxAge: cfg.maxAge, skipThrottle: false}, cfg.interval)
}

// runGC is the shared GC body. Two callers:
//   - runOpportunisticGC: writer nil → silent eviction sweep, post-search.
//   - runGCCommand (manual `seek gc --force` / `--all`): writer os.Stdout →
//     streams the same banner + table + summary as `--dry-run`, with the
//     ACTION column reflecting the real per-corpus outcome.
//
// Order of operations:
//  1. throttle gate (`.last-gc` mtime) unless skipThrottle
//  2. NFS detection (auto-GC disabled on NFS)
//  3. acquire global gc.lock (other gc → skip, surfaced when streaming)
//  4. drain any leftover trash (idempotent crash recovery)
//  5. enumerate + sort corpora by hash (deterministic table order)
//  6. per-entry: capture size+display BEFORE eviction (live row needs it),
//     then either skip (within TTL) or evictCorpus, then render row +
//     accumulate counters
//  7. stamp .last-gc to throttle the next opportunistic run
func runGC(ctx context.Context, opts gcOptions, interval time.Duration) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		slog.Debug("gc skipped: cannot resolve cache root", "error", err)
		return
	}

	// Throttle gate FIRST — on the warm path this exits after a single
	// stat() on .last-gc and skips the MkdirAll + Statfs that would
	// otherwise fire on every seek invocation.
	if !opts.skipThrottle && !shouldRunGC(cacheRoot, interval) {
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
	defer func() {
		unlockFile(lockFd)
		_ = lockFd.Close()
	}()
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

	// TODO(gc-undo): if/when we expose `seek gc undo`, trashed entries
	// should outlive a single GC run (TTL-gated drain). Today they are
	// drained eagerly, matching pre-table behavior.
	drainTrash(trashPath)

	entries, err := enumerateCorpusDirs(corporaPath)
	if err != nil {
		slog.Warn("gc cannot enumerate corpora", "error", err)
		// Still update stamp to throttle next attempt.
		touchStamp(cacheRoot)
		return
	}
	// Sort by corpus hash so the streamed table matches dry-run ordering;
	// also gives opportunistic-path eviction a deterministic sweep order.
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	cutoff := time.Now().Add(-opts.maxAge)
	now := time.Now()
	streaming := opts.writer != nil
	var totalBytes, evictBytes int64
	var evictCount, processedCount int
	if streaming {
		_, _ = fmt.Fprintf(opts.writer, "seek gc: cache root %s\n", cacheRoot)
		if len(entries) == 0 {
			_, _ = fmt.Fprintln(opts.writer, "  no corpora")
		} else {
			_, _ = fmt.Fprintln(opts.writer, gcTableHeader)
		}
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		// Sweep orphan tmp manifest files (folder + uncommitted) older
		// than 1 hour. SIGKILL between os.WriteFile and os.Rename in
		// writeFolderManifest / writeUncommittedManifest can leave these
		// behind; no live process ever reads them again. Best-effort —
		// errors are silent because the user does not need to act.
		sweepOrphanManifestTmps(e.path, now.Add(-1*time.Hour))

		// Capture size + display info BEFORE eviction — once the dir is
		// renamed to .trash the original path is gone and corpusDirSize
		// would return 0, corpusDisplayName would return [empty].
		var size int64
		var info corpusDisplayInfo
		if streaming {
			size = corpusDirSize(e.path)
			info = corpusDisplayName(e.path)
		}
		var res gcRowResult
		if e.usedAt.After(cutoff) {
			res = gcRowResult{action: actionKept}
		} else {
			res = evictCorpus(e, trashPath, cutoff)
		}
		switch res.action {
		case actionFailed:
			slog.Warn("gc eviction failed", "corpus", e.name, "error", res.err)
		case actionTrashed:
			slog.Warn("gc partial eviction", "corpus", e.name, "error", res.err)
		}
		if streaming {
			renderGCRow(opts.writer, e, info, size, now, res.String())
			totalBytes += size
			if res.action == actionEvicted || res.action == actionTrashed {
				evictBytes += size
				evictCount++
			}
		}
		processedCount++
	}
	if streaming && processedCount > 0 {
		// processedCount (not len(entries)) — Ctrl-C / ctx cancel may break
		// the loop mid-sweep; summary must describe what we actually saw.
		_, _ = fmt.Fprintf(opts.writer, "%d corpora, %s total, %d evicted (%s).\n",
			processedCount, humanBytes(totalBytes), evictCount, humanBytes(evictBytes))
	}

	// Stamp updated last and even on partial failures — prevents retry
	// storms when persistent errors would otherwise re-run GC every
	// invocation.
	touchStamp(cacheRoot)
}

// touchStamp bumps the .last-gc mtime to now. Steady-state cost: one
// utimensat (~µs). On first ever run the file doesn't exist yet, so we fall
// back to writeCacheFile (tmp + rename, ~3 syscalls). Subsequent calls take
// the fast path.
func touchStamp(cacheRoot string) {
	stampPath := filepath.Join(cacheRoot, gcStampFile)
	now := time.Now()
	if err := os.Chtimes(stampPath, now, now); err == nil {
		return
	}
	_ = writeCacheFile(cacheRoot, gcStampFile, "")
}

// warnNFSOnce logs a Warn the first ever time GC detects a network FS, then
// debounces future warnings via .last-gc presence.
func warnNFSOnce(cacheRoot string) {
	stampPath := filepath.Join(cacheRoot, gcStampFile)
	if _, err := os.Stat(stampPath); errors.Is(err, fs.ErrNotExist) {
		slog.Warn(
			"seek cache on network filesystem; auto-GC disabled. "+
				"Set XDG_CACHE_HOME to a local directory to enable.",
			"cache_root", cacheRoot,
		)
		touchStamp(cacheRoot)
		return
	}
	slog.Debug("gc skipped: cache on network filesystem", "cache_root", cacheRoot)
}

func gcConfigFromEnv() gcConfig {
	cfg := gcConfig{maxAge: defaultGCMaxAge, interval: defaultGCInterval}
	if v := os.Getenv(envGCMaxAge); v != "" {
		if d, err := parseGCDuration(v); err == nil && d > 0 {
			cfg.maxAge = d
		} else {
			slog.Debug("invalid SEEK_GC_MAX_AGE, using default", "value", v)
		}
	}
	if v := os.Getenv(envGCInterval); v != "" {
		if d, err := parseGCDuration(v); err == nil && d >= 0 {
			cfg.interval = d
		} else {
			slog.Debug("invalid SEEK_GC_INTERVAL, using default", "value", v)
		}
	}
	return cfg
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

// sweepOrphanManifestTmps removes leftover .tmp manifest files in a
// corpus cache directory whose mtime is older than threshold. Both the
// folder manifest writer (folder_indexer.go:771-797) and the
// uncommitted manifest writer (uncommitted_manifest.go) write to a
// tmp path and then Rename — on SIGKILL between the WriteFile and the
// Rename, the tmp persists forever. They are not picked up by any
// reader (manifest readers open the canonical filename), but they
// accumulate disk usage and confuse manual cache inspection.
//
// Conservatively only sweeps known suffixes so we never collide with a
// legitimate in-flight write.
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
		uncommittedManifestFileName + ".tmp":
		return true
	}
	return false
}

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
	if len(name) != corpusHashHexLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func readUsedAt(cacheDir string) (time.Time, bool) {
	st, err := os.Stat(filepath.Join(cacheDir, usedFile))
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), true
}

// evictCorpus performs a two-phase delete under a non-blocking per-corpus
// lock. Returns a gcRowResult describing the outcome:
//   - actionLocked: corpus .lock held by another seek
//   - actionKept:   .used was bumped between enumeration and lock (TOCTOU close)
//   - actionGone:   corpus dir vanished mid-flight (concurrent process)
//   - actionEvicted: rename + RemoveAll both succeeded
//   - actionTrashed: rename succeeded, RemoveAll failed; drainTrash finishes
//   - actionFailed:  open-lock or rename returned a non-ENOENT error
func evictCorpus(e corpusDirEntry, trashDir string, cutoff time.Time) gcRowResult {
	lockPath := filepath.Join(e.path, lockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return gcRowResult{action: actionGone}
		}
		return gcRowResult{action: actionFailed, err: fmt.Errorf("open corpus lock: %w", err)}
	}
	defer func() {
		if f != nil {
			unlockFile(f)
			_ = f.Close()
		}
	}()
	if err := lockFileExclusive(f); err != nil {
		return gcRowResult{action: actionLocked}
	}

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
	unlockFile(f)
	_ = f.Close()
	// Prevent the deferred close from operating on a stale fd.
	f = nil

	if err := os.RemoveAll(target); err != nil {
		// Partial cleanup; drainTrash will finish next run.
		return gcRowResult{action: actionTrashed, err: fmt.Errorf("remove trash entry: %w", err)}
	}
	return gcRowResult{action: actionEvicted}
}

// pickDisplayShard chooses which shard's metadata to read for display.
// Prefers a non-uncommitted shard so Repository.Name carries the real repo
// identity when callers want it. Zoekt's shard filename pattern is
// `<urlencoded-name>_v<format>.<seq>.zoekt`, so an exact prefix match against
// `uncommitted_` is reliable; a substring match on `uncommitted` would
// false-positive on real repo names like `github.com/foo/uncommitted-tool`.
// Returns shards[0] when all shards are uncommitted (their Source is still
// the repo path).
func pickDisplayShard(shards []string) string {
	uncommittedPrefix := repoUncommitted + "_"
	for _, s := range shards {
		if !strings.HasPrefix(filepath.Base(s), uncommittedPrefix) {
			return s
		}
	}
	return shards[0]
}

// corpusDisplayInfo summarizes a corpus for the ROOT column of the seek gc
// table — used by both `--dry-run` (reportGCPlan) and live `--force` (runGC
// streaming path). Source is the absolute on-disk root (from zoekt
// Repository.Source). When the source path no longer exists on the
// filesystem, gone is true.
type corpusDisplayInfo struct {
	source string
	gone   bool
}

// corpusDisplayName reads the first non-uncommitted shard's Repository.Source
// to recover the original root path. Zero new schema — the data was already
// persisted by every index cycle (indexer.go:653, folder_indexer.go:627/649,
// zoekt gitindex sets Source = repoDir).
//
// Returns zero corpusDisplayInfo when no shards exist (empty corpus, crashed
// indexer) — callers should fall back to the hash.
func corpusDisplayName(corpusDir string) corpusDisplayInfo {
	indexDir := filepath.Join(corpusDir, "index")
	shards, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil || len(shards) == 0 {
		return corpusDisplayInfo{}
	}
	pick := pickDisplayShard(shards)
	repos, _, err := zoektindex.ReadMetadataPath(pick)
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

// touchUsed bumps the mtime of <cacheDir>/.used to time.Now(). Best-effort:
// errors are logged at Debug. Hot-path; sub-millisecond per call. Skips when
// the cache lives on a network filesystem (gated once per process).
var (
	nfsCheckOnce sync.Once
	nfsCached    bool
)

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
	// File missing (or chtimes failed): create via atomic tmp+rename so
	// concurrent touches don't trample each other.
	if err := writeCacheFile(cacheDir, usedFile, ""); err != nil {
		slog.Debug("touch used failed", "cache_dir", cacheDir, "error", err)
	}
}
