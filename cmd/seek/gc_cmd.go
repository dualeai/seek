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

// runGCCommandCmd implements `seek gc` with the flags that Cobra parsed
// (opts). Flag validation and setup failures (cache root, enumeration, the
// dry-run plan) return an error and exit non-zero. runGC logs the failures
// inside the live sweep and does not return them.
func runGCCommandCmd(ctx context.Context, opts gcCmdOptions) error {
	// Validate flags before the throttle, NFS, and enumeration work, so an
	// invalid --sort fails even on a run that would otherwise do nothing.
	sortKey, err := parseGCSortKey(opts.sort)
	if err != nil {
		return err
	}

	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return fmt.Errorf("resolve cache root: %w", err)
	}

	corporaPath := filepath.Join(cacheRoot, corporaDir)
	if opts.dryRun {
		// An absent cache is an empty plan. Do not create it only to report
		// that it contains no corpora.
		entries, err := enumerateCorpusDirs(corporaPath)
		if err != nil {
			return fmt.Errorf("enumerate corpora: %w", err)
		}
		evict, err := planCorpusEviction(cacheRoot, entries, opts.all, nil, false)
		if err != nil {
			return fmt.Errorf("compute the cache budget: %w", err)
		}
		if err := reportGCPlan(ctx, os.Stdout, cacheRoot, entries, evict, sortKey); err != nil {
			return err
		}
		before := time.Now().Add(-providerArtifactAge(opts.all))
		reportDirPlan(os.Stdout, "compiled model files", agedProviderArtifacts(ctx, cacheRoot, before),
			"Seek compiles these again when it needs them.")
		reportDirPlan(os.Stdout, "retired runtimes", retiredRuntimes(cacheRoot, before),
			"Seek extracts these again when a version that needs them runs.")
		return nil
	}

	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return fmt.Errorf("create cache root: %w", err)
	}
	if isOnNFS(cacheRoot) {
		slog.Warn(
			"seek cache on network filesystem; gc disabled. "+gcLocalCacheHint,
			"cache_root", cacheRoot,
		)
		return nil
	}
	if _, err := enumerateCorpusDirs(corporaPath); err != nil {
		return fmt.Errorf("enumerate corpora: %w", err)
	}

	// Live eviction: reuse runGC for trash drain + lock + per-corpus
	// rename/RemoveAll. `--force` and `--all` both bypass the throttle
	// gate — `--all` is an explicit user wipe, not subject to .last-gc.
	// writer is non-nil → runGC streams the per-corpus table to stdout
	// (same shape as `--dry-run`, ACTION column shows real outcome).
	runGC(ctx, gcOptions{
		evictAll:     opts.all,
		skipThrottle: opts.force || opts.all,
		writer:       os.Stdout,
		sortKey:      sortKey,
	})
	return nil
}

// gcTableHeader is the column row shared by `seek gc --dry-run` and live
// `seek gc --force`. Widths align with the format string in renderGCRow.
const gcTableHeader = "  CORPUS      ROOT                                                  AGE     SIZE    ACTION"

// gcTableStats holds the counters of the table summary line. The dry-run and
// live paths share it, so their summaries differ only in the verb
// ("evictable" or "evicted").
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
// (sortByName) keeps the hash order shared by the dry-run and live paths —
// and is what the opportunistic path implicitly uses.
type gcSortKey uint8

const (
	sortByName gcSortKey = iota // corpus hash — deterministic
	sortByAge                   // oldest .used first, the eviction order
	sortBySize                  // largest first
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

// predictGCAction is the dry-run counterpart to evictIfPlanned: it reports
// what the eviction plan would do (kept | evicted) without taking a lock or
// mutating anything. Sharing the gcRowResult vocabulary with the live path
// keeps the two tables byte-aligned.
func predictGCAction(e corpusDirEntry, evict map[corpusID]bool) gcRowResult {
	if !evict[e.name] {
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
// path is recovered from a committed zoekt shard of each corpus, else from an
// uncommitted one (Repository.Source) — see readCorpusDisplayInfo. Empty /
// crashed corpora without shards display as "[empty]"; corpora whose source
// root has been deleted on disk show as "[gone] <path>".
//
// The rows use the words of the live path (`seek gc --force`), `kept` and
// `evicted`, for what would happen. The live path (runGC) uses the same
// renderGCRow and columns, so the dry-run and live output line up.
//
// Most of the time of a row is corpusDirSize (a filepath.WalkDir walk of the
// corpus) and readCorpusDisplayInfo (one Zoekt shard metadata read). The rows
// are measured one after another: at about 700 corpora, a cold sorted view
// takes about 1 s, and the default name order prints each row as soon as it
// is measured.
//
// The summary counts the rows actually rendered — both branches stop early
// on ctx cancellation, and the summary must describe what was measured.
func reportGCPlan(ctx context.Context, w io.Writer, cacheRoot string, entries []corpusDirEntry, evict map[corpusID]bool, sortKey gcSortKey) error {
	printGCTableBanner(w, cacheRoot, len(entries))
	if len(entries) == 0 {
		return nil
	}
	now := time.Now()
	stats := &gcTableStats{}
	emit := func(r gcRow) { emitGCRow(w, stats, r, now, predictGCAction(r.entry, evict)) }
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
// policy (C0 and C1 controls, DEL, bidi controls, line and paragraph
// separators) plus tab, which would break column alignment. A clean string
// (the usual case) returns with no allocation.
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
	return s[:5] + "..." + s[len(s)-2:]
}

func humanDuration(d time.Duration) string {
	switch {
	case d < 0:
		// Future .used (clock skew, cp -p restore, NTP step): render as
		// fresh, not as a negative number of seconds.
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
			// size between builds, not the temporary extra size of a build.
			if strings.HasPrefix(d.Name(), buildDirPrefix) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == corpusSizeFile {
			// The ledger entry describes the corpus; it is not part of it.
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

// reportDirPlan lists the directories outside the corpus table that a
// collection would remove (sweepAgedProviderArtifacts, sweepRetiredRuntimes).
// Without it a plan under-reports the deletion. It prints nothing when no
// directory is due.
func reportDirPlan(w io.Writer, label string, due []string, footer string) {
	if len(due) == 0 {
		return
	}
	// Measure once per entry. corpusDirSize walks the whole subtree, and that
	// walk is most of the cost of a plan, so the total and the rows must share
	// it.
	sizes := make([]int64, len(due))
	var total int64
	for i, path := range due {
		sizes[i] = corpusDirSize(path)
		total += sizes[i]
	}
	_, _ = fmt.Fprintf(w, "\n%s to remove: %d (%s)\n", label, len(due), humanBytes(total))
	for i, path := range due {
		_, _ = fmt.Fprintf(w, "  %s  %s\n", humanBytes(sizes[i]), path)
	}
	_, _ = fmt.Fprintln(w, footer)
}
