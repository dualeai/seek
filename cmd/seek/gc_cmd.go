package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type gcCmdOptions struct {
	force  bool
	dryRun bool
	all    bool
	sort   string
}

// runGCCommand is the test-facing entry point that mirrors the
// pre-Cobra signature. It instantiates the gc cobra command, feeds it
// raw args, and runs it. Production callers go through newGCCmd /
// rootCmd.Execute; this wrapper exists so the existing gc_test.go
// table doesn't need a full rewrite of every `runGCCommand(ctx, []string{...})`
// call site.
func runGCCommand(ctx context.Context, args []string) error {
	cmd := newGCCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(io.Discard)
	return cmd.ExecuteContext(ctx)
}

// runGCCommandCmd implements `seek gc`. Flags are parsed by Cobra and
// passed in via opts. Flag validation and setup failures (cache root,
// enumeration) return an error and exit non-zero; failures inside the
// live sweep itself are logged + swallowed by runGC.
func runGCCommandCmd(ctx context.Context, opts gcCmdOptions) error {
	// Validate flags before the throttle / NFS / enumeration work so an
	// invalid --sort errors loudly even on runs that would otherwise
	// silently no-op.
	sortKey, err := parseGCSortKey(opts.sort)
	if err != nil {
		return err
	}

	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return fmt.Errorf("resolve cache root: %w", err)
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return fmt.Errorf("create cache root: %w", err)
	}

	if isOnNFS(cacheRoot) {
		slog.Warn(
			"seek cache on network filesystem; gc disabled. "+
				"Set XDG_CACHE_HOME to a local directory to enable.",
			"cache_root", cacheRoot,
		)
		return nil
	}

	cfg := gcConfigFromEnv()
	maxAge := cfg.maxAge
	if opts.all {
		maxAge = 0
	}

	corporaPath := filepath.Join(cacheRoot, corporaDir)
	entries, err := enumerateCorpusDirs(corporaPath)
	if err != nil {
		return fmt.Errorf("enumerate corpora: %w", err)
	}

	if opts.dryRun {
		return reportGCPlan(ctx, os.Stdout, cacheRoot, entries, time.Now().Add(-maxAge), sortKey)
	}

	// Live eviction: reuse runGC for trash drain + lock + per-corpus
	// rename/RemoveAll. `--force` and `--all` both bypass the throttle
	// gate — `--all` is an explicit user wipe, not subject to .last-gc.
	// writer is non-nil → runGC streams the per-corpus table to stdout
	// (same shape as `--dry-run`, ACTION column shows real outcome).
	runGC(ctx, gcOptions{
		maxAge:       maxAge,
		skipThrottle: opts.force || opts.all,
		writer:       os.Stdout,
		sortKey:      sortKey,
	}, cfg.interval)
	return nil
}

// gcTableHeader is the column row shared by `seek gc --dry-run` and live
// `seek gc --force`. Widths align with the format string in renderGCRow.
const gcTableHeader = "  CORPUS      ROOT                                                  AGE     SIZE    ACTION"

// gcTableStats accumulates the counters behind the table summary line,
// shared by the dry-run and live paths so the two summaries cannot drift —
// they deliberately differ only in the verb ("evictable" vs "evicted").
type gcTableStats struct {
	totalBytes int64
	evictBytes int64
	evictCount int
	rendered   int
}

func (s *gcTableStats) summary(w io.Writer, verb string) {
	_, _ = fmt.Fprintf(w, "%d corpora, %s total, %d %s (%s).\n",
		s.rendered, humanBytes(s.totalBytes), s.evictCount, verb, humanBytes(s.evictBytes))
}

// gcSortKey selects the row order of the `seek gc` table. The zero value
// (sortByName) preserves the historical hash order shared by the dry-run
// and live paths — and is what the opportunistic path implicitly uses.
type gcSortKey uint8

const (
	sortByName gcSortKey = iota // corpus hash — historical, deterministic
	sortByAge                   // oldest .used first — matches eviction priority
	sortBySize                  // largest first — "what is eating my disk"
)

// gcSortValues is the single source for the --sort accepted values: the
// parse error message and the shell-completion list both derive from it, so
// adding a key can't leave completions or the error text stale.
var gcSortValues = []string{"name", "age", "size"}

func parseGCSortKey(s string) (gcSortKey, error) {
	switch s {
	case "name":
		return sortByName, nil
	case "age":
		return sortByAge, nil
	case "size":
		return sortBySize, nil
	}
	return 0, fmt.Errorf("--sort must be one of %s, got %q", strings.Join(gcSortValues, "|"), s)
}

// gcRow is one materialized table row: the corpus entry plus the size and
// display info renderGCRow needs. Sorted views (--sort=age|size) build all
// rows up front so they can be ordered before rendering; the default name
// order builds one transient row per entry.
type gcRow struct {
	entry corpusDirEntry
	size  int64
	info  corpusDisplayInfo
}

// measureGCRow captures a corpus's size + display info for one table row.
// Both are read before eviction — once the dir is renamed to .trash the
// original path is gone (corpusDirSize would return 0, readCorpusDisplayInfo
// [empty]).
func measureGCRow(e corpusDirEntry) gcRow {
	return gcRow{
		entry: e,
		size:  corpusDirSize(e.path),
		info:  readCorpusDisplayInfo(e.path),
	}
}

// buildGCRows materializes a row for every entry. ctx is honored between
// entries only — corpusDirSize walks are uncancelable — and on cancellation
// the rows built so far are returned.
func buildGCRows(ctx context.Context, entries []corpusDirEntry) []gcRow {
	rows := make([]gcRow, 0, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		rows = append(rows, measureGCRow(e))
	}
	return rows
}

// sortGCRows orders rows for the table. Callers pass rows already in hash
// order (enumeration output is hash-sorted), so the stable sort gives
// equal-size / equal-age rows a deterministic hash tiebreak.
func sortGCRows(rows []gcRow, key gcSortKey) {
	switch key {
	case sortBySize:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].size > rows[j].size })
	case sortByAge:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].entry.usedAt.Before(rows[j].entry.usedAt) })
	case sortByName:
		// Input is already hash-sorted.
	}
}

// renderGCRow prints one corpus row using the column layout in gcTableHeader.
// Display info is passed in (not computed here) so live callers can capture
// it before eviction.
func renderGCRow(w io.Writer, e corpusDirEntry, info corpusDisplayInfo, size int64, now time.Time, action string) {
	age := now.Sub(e.usedAt)
	_, _ = fmt.Fprintf(w, "  %-10s  %-52s  %-6s  %-6s  %s\n",
		truncateHash(e.name),
		formatRoot(info, 52),
		humanDuration(age),
		humanBytes(size),
		action,
	)
}

// printGCTableBanner prints the banner shared by the dry-run and live tables,
// then either the "no corpora" note (empty cache) or the column header.
func printGCTableBanner(w io.Writer, cacheRoot string, n int) {
	_, _ = fmt.Fprintf(w, "seek gc: cache root %s\n", cacheRoot)
	if n == 0 {
		_, _ = fmt.Fprintln(w, "  no corpora")
		return
	}
	_, _ = fmt.Fprintln(w, gcTableHeader)
}

// predictGCAction is the dry-run counterpart to evictIfExpired: it reports
// what the TTL decision would be (kept | evicted) without taking a lock or
// mutating anything. Sharing the gcRowResult vocabulary with the live path
// keeps the two tables byte-aligned.
func predictGCAction(e corpusDirEntry, cutoff time.Time) gcRowResult {
	if e.usedAt.After(cutoff) {
		return gcRowResult{action: actionKept}
	}
	return gcRowResult{action: actionEvicted}
}

// emitGCRow renders one row and folds it into the running stats, using the
// same gcRowResult on both paths so the "counts as evicted" predicate lives
// in exactly one place.
func emitGCRow(w io.Writer, stats *gcTableStats, r gcRow, now time.Time, res gcRowResult) {
	renderGCRow(w, r.entry, r.info, r.size, now, res.String())
	stats.totalBytes += r.size
	stats.rendered++
	if res.action == actionEvicted || res.action == actionTrashed {
		stats.evictBytes += r.size
		stats.evictCount++
	}
}

// reportGCPlan prints the dry-run table. No filesystem mutation. Display
// path is recovered from each corpus's first zoekt shard
// (Repository.Source) — see readCorpusDisplayInfo. Empty / crashed corpora
// without shards display as "[empty]"; corpora whose source root has been
// deleted on disk show as "[gone] <path>".
//
// Vocab is past tense (`kept`/`evicted`) — same words as the live path
// (`seek gc --force`), interpreted as "what would happen". The live path
// (runGC streaming) uses the same renderGCRow + column layout, so dry-run
// and live output stay byte-aligned across columns.
//
// Per-row cost is dominated by corpusDirSize (filepath.WalkDir subtree walk
// per corpus) plus readCorpusDisplayInfo (one zoekt shard metadata read). At
// ~700 corpora a cold sorted view takes ~1s; the default name order streams
// rows, so latency is amortized. A bounded errgroup over the per-row work
// remains the fix if sorted views grow too slow.
//
// The summary counts the rows actually rendered — both branches stop early
// on ctx cancellation, and the summary must describe what was measured.
func reportGCPlan(ctx context.Context, w io.Writer, cacheRoot string, entries []corpusDirEntry, cutoff time.Time, sortKey gcSortKey) error {
	printGCTableBanner(w, cacheRoot, len(entries))
	if len(entries) == 0 {
		return nil
	}
	now := time.Now()
	stats := &gcTableStats{}
	emit := func(r gcRow) { emitGCRow(w, stats, r, now, predictGCAction(r.entry, cutoff)) }
	if sortKey == sortByName {
		// Default order streams each row right after its own measurement —
		// incremental output on large caches, no frozen header.
		for _, e := range entries {
			if ctx.Err() != nil {
				break
			}
			emit(measureGCRow(e))
		}
	} else {
		// Sorted views must measure everything before rendering anything.
		rows := buildGCRows(ctx, entries)
		sortGCRows(rows, sortKey)
		for _, r := range rows {
			emit(r)
		}
	}
	stats.summary(w, "evictable")
	return nil
}

// formatRoot renders a corpusDisplayInfo for the table, marking missing
// sources with [gone] and truncating overlong paths from the LEFT so the
// recognizable basename always survives. Control characters are stripped
// (an interior newline is legal in POSIX paths but would break the
// one-row-per-corpus layout) and cuts land on rune boundaries so multibyte
// paths never render orphan continuation bytes.
func formatRoot(info corpusDisplayInfo, width int) string {
	if info.source == "" {
		return "[empty]"
	}
	path := sanitizeCell(info.source)
	if info.gone {
		path = "[gone] " + path
	}
	if len(path) <= width {
		return path
	}
	const ellipsis = "..."
	keep := width - len(ellipsis)
	if keep <= 0 {
		return path[:backupToRuneBoundary([]byte(path), width)]
	}
	cut := len(path) - keep
	// Advance to the next rune boundary so the cut never splits a rune
	// (keeps at most `keep` bytes, never more).
	for cut < len(path) && path[cut]&0xC0 == 0x80 {
		cut++
	}
	return ellipsis + path[cut:]
}

// sanitizeCell strips control characters from a table cell — stripRune's
// policy (C0 controls, DEL, bidi controls) plus tab, which would break
// column alignment. Clean strings (the overwhelming common case) are
// returned without allocation.
func sanitizeCell(s string) string {
	clean := true
	for _, r := range s {
		if r == '\t' || stripRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' || stripRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func truncateHash(h corpusID) string {
	s := string(h)
	if len(s) <= 10 {
		return s
	}
	var b strings.Builder
	b.Grow(10)
	b.WriteString(s[:5])
	b.WriteString("...")
	b.WriteString(s[len(s)-2:])
	return b.String()
}

func humanDuration(d time.Duration) string {
	switch {
	case d < 0:
		// Future .used (clock skew, cp -p restore, NTP step): render as
		// fresh, not as a negative raw-second blob.
		return "0s"
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// humanBytes keeps every value within the 6-char SIZE column (TB is the
// top tier — good through 9999TB): one decimal while it fits ("1.5GB",
// "99.9GB"), integer past that ("154GB", "1023GB").
func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	switch {
	case n >= tb:
		return humanBytesUnit(n, tb, "TB")
	case n >= gb:
		return humanBytesUnit(n, gb, "GB")
	case n >= mb:
		return fmt.Sprintf("%dMB", n/mb)
	case n >= kb:
		return fmt.Sprintf("%dKB", n/kb)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// humanBytesUnit formats with one decimal, dropping it when the result
// would overflow the 6-char cell (covers both ≥100 units and the %.1f
// rounding edge where 99.96 would render "100.0").
func humanBytesUnit(n, unit int64, suffix string) string {
	s := fmt.Sprintf("%.1f%s", float64(n)/float64(unit), suffix)
	if len(s) > 6 {
		return fmt.Sprintf("%d%s", n/unit, suffix)
	}
	return s
}

func corpusDirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Skip in-progress temp build dirs so the SIZE column reports the
			// honest steady-state footprint, not a transient 2x during a build.
			if strings.HasPrefix(d.Name(), buildDirPrefix) {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
