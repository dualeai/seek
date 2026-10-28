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
		reportGCPlan(ctx, os.Stdout, cacheRoot, entries, evict, sortKey)
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

	// Live eviction: runGC takes the lock, empties the trash, and renames and
	// removes each corpus. `--force` and `--all` both skip the throttle gate:
	// `--all` is an explicit request, and .last-gc does not apply. writer is
	// not nil, so runGC streams the table of the corpora to stdout. It has the
	// form of `--dry-run`, and the ACTION column shows the real outcome.
	runGC(ctx, gcOptions{
		evictAll:     opts.all,
		skipThrottle: opts.force || opts.all,
		writer:       os.Stdout,
		sortKey:      sortKey,
	})
	return nil
}

// gcTableHeader is the column row of `seek gc --dry-run` and of a live
// `seek gc`. Its widths match the format string in emitGCRow.
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
// (sortByName) keeps the hash order of the dry-run and live paths. The
// opportunistic path uses it too.
type gcSortKey uint8

const (
	sortByName gcSortKey = iota // corpus hash, a fixed order
	sortByAge                   // oldest .used first, the eviction order
	sortBySize                  // largest first
)

// gcSortValues lists the values that --sort accepts. The parse error and the
// shell completion both read it, so a new key reaches both.
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

// gcRow is one table row: the corpus entry, and the size and display info
// that emitGCRow needs. A sorted view (--sort=age|size) builds all rows
// first, so it can sort them before it renders them. The default name order
// builds one short-lived row per entry.
type gcRow struct {
	entry corpusDirEntry
	size  int64
	info  corpusDisplayInfo
}

// measureGCRow reads a corpus's size and display info for one table row. It
// reads both before the eviction. After the rename to .trash, the old path is
// gone: corpusDirSize would return 0, and readCorpusDisplayInfo [empty].
func measureGCRow(e corpusDirEntry) gcRow {
	return gcRow{
		entry: e,
		size:  corpusDirSize(e.path),
		info:  readCorpusDisplayInfo(e.path),
	}
}

// buildGCRows builds a row for every entry. It checks ctx only between
// entries, because a corpusDirSize walk cannot stop. On a cancel, it returns
// the rows that it built.
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

// sortGCRows orders rows for the table. Callers give rows in hash order
// (enumerateCorpusDirs). So the stable sort orders rows of equal size or
// equal age by hash.
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

// predictGCAction is the dry-run form of evictIfPlanned. It reports what the
// eviction plan would do (kept or evicted), with no lock and no change. It
// uses the gcRowResult words of the live path, so the two tables align.
func predictGCAction(e corpusDirEntry, evict map[corpusID]bool) gcRowResult {
	if !evict[e.name] {
		return gcRowResult{action: actionKept}
	}
	return gcRowResult{action: actionEvicted}
}

// emitGCRow prints one corpus row in the column layout of gcTableHeader and
// adds it to stats. The row holds the display info, so a live caller reads it
// before the eviction (measureGCRow). Both paths give it a gcRowResult, so the
// rule of what counts as evicted is in one place.
func emitGCRow(w io.Writer, stats *gcTableStats, r gcRow, now time.Time, res gcRowResult) {
	_, _ = fmt.Fprintf(w, "  %-10s  %-*s  %-6s  %-6s  %s\n",
		truncateHash(r.entry.name),
		gcRootWidth, formatRoot(r.info),
		humanDuration(now.Sub(r.entry.usedAt)),
		humanBytes(r.size),
		res.String(),
	)
	stats.totalBytes += r.size
	stats.rendered++
	if res.action == actionEvicted || res.action == actionTrashed {
		stats.evictBytes += r.size
		stats.evictCount++
	}
}

// reportGCPlan prints the dry-run table and changes nothing on disk. The
// displayed path is Repository.Source of a committed Zoekt shard of each
// corpus, else of an uncommitted one (readCorpusDisplayInfo). A corpus with
// no shard (empty or crashed) shows as "[empty]". A corpus whose source root
// is gone from disk shows as "[gone] <path>".
//
// The rows use the words of the live path (`seek gc --force`), `kept` and
// `evicted`, for what would happen. The live path (runGC) uses the same
// emitGCRow and columns, so the dry-run and live output line up.
//
// Most of the time of a row is corpusDirSize (a filepath.WalkDir walk of the
// corpus) and readCorpusDisplayInfo (one read of Zoekt shard metadata). The
// plan measures the rows one after another. At about 700 corpora, a cold
// sorted view takes about 1 s. The default name order prints each row as
// soon as it measures it.
//
// The summary counts the rendered rows. Both branches stop early on a cancel
// of ctx, and the summary tells what the plan measured.
func reportGCPlan(ctx context.Context, w io.Writer, cacheRoot string, entries []corpusDirEntry, evict map[corpusID]bool, sortKey gcSortKey) {
	printGCTableBanner(w, cacheRoot, len(entries))
	if len(entries) == 0 {
		return
	}
	now := time.Now()
	stats := &gcTableStats{}
	emit := func(r gcRow) { emitGCRow(w, stats, r, now, predictGCAction(r.entry, evict)) }
	if sortKey == sortByName {
		// The default order prints each row after its own measurement, so a
		// large cache shows rows at once and not only the header.
		for _, e := range entries {
			if ctx.Err() != nil {
				break
			}
			emit(measureGCRow(e))
		}
	} else {
		// A sorted view measures all rows before it renders one.
		rows := buildGCRows(ctx, entries)
		sortGCRows(rows, sortKey)
		for _, r := range rows {
			emit(r)
		}
	}
	stats.summary(w, "evictable")
}

// gcRootWidth is the byte width of the ROOT column of gcTableHeader.
const gcRootWidth = 52

// formatRoot renders a corpusDisplayInfo for the ROOT column. It marks a
// missing source with [gone]. It cuts a path longer than gcRootWidth from the
// left and keeps its end. So a base name that fits in the kept bytes stays
// whole. It removes control characters: a newline is valid in a POSIX path,
// but it would break the layout of one row per corpus. The cut is on a rune
// boundary, so a multibyte path never shows a partial rune.
func formatRoot(info corpusDisplayInfo) string {
	if info.source == "" {
		return "[empty]"
	}
	path := sanitizeCell(info.source)
	if info.gone {
		path = "[gone] " + path
	}
	if len(path) <= gcRootWidth {
		return path
	}
	const ellipsis = "..."
	keep := gcRootWidth - len(ellipsis)
	cut := len(path) - keep
	// Move to the next rune boundary, so the cut never splits a rune. It
	// keeps at most `keep` bytes.
	for cut < len(path) && path[cut]&0xC0 == 0x80 {
		cut++
	}
	return ellipsis + path[cut:]
}

// sanitizeCell removes control characters from a table cell. It removes what
// stripRune removes (C0 and C1 controls, DEL, bidi controls, line and
// paragraph separators), and also tab, which would break the column
// alignment. A clean string (the usual case) returns with no allocation.
func sanitizeCell(s string) string {
	drop := func(r rune) bool { return r == '\t' || stripRune(r) }
	if !strings.ContainsFunc(s, drop) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !drop(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// truncateHash fits a corpus hash in the 10-byte CORPUS column: a longer hash
// keeps its first 5 and last 2 characters around "...".
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
		// A .used time in the future (clock skew, a cp -p restore, an NTP
		// step) shows as new, not as a negative number of seconds.
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

// humanBytes keeps every value within the 6-character SIZE column, up to
// 9999TB. It shows one decimal while it fits ("1.5GB", "99.9GB"), and an
// integer after that ("154GB", "1023GB").
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

// humanBytesUnit formats with one decimal. It drops the decimal when the
// result would not fit the 6-character cell: at 100 units or more, and when
// %.1f rounds 99.96 to "100.0".
func humanBytesUnit(n, unit int64, suffix string) string {
	s := fmt.Sprintf("%.1f%s", float64(n)/float64(unit), suffix)
	if len(s) > 6 {
		return fmt.Sprintf("%d%s", n/unit, suffix)
	}
	return s
}

// corpusDirSize returns the bytes of the files under path. It skips temporary
// build directories and the size ledger file.
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
// Without it, a plan would not show all that GC deletes. It prints nothing
// when no directory is due.
func reportDirPlan(w io.Writer, label string, due []string, footer string) {
	if len(due) == 0 {
		return
	}
	// Measure each entry once. corpusDirSize walks the whole subtree, and
	// that walk is most of the cost of a plan, so the total and the rows share
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
