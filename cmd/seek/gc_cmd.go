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
// passed in via opts; every failure inside this body is logged +
// swallowed (exit 0) to match historical behavior.
func runGCCommandCmd(ctx context.Context, opts gcCmdOptions) error {
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	if opts.dryRun {
		return reportGCPlan(os.Stdout, cacheRoot, entries, time.Now().Add(-maxAge))
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
	}, cfg.interval)
	return nil
}

// gcTableHeader is the column row shared by `seek gc --dry-run` and live
// `seek gc --force`. Widths align with the format string in renderGCRow.
const gcTableHeader = "  CORPUS      ROOT                                                  AGE     SIZE    ACTION"

// renderGCRow prints one corpus row using the column layout in gcTableHeader.
// Display info is passed in (not computed here) so live callers can capture
// it BEFORE eviction — once the dir moves to .trash, corpusDisplayName
// returns [empty].
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

// reportGCPlan prints the dry-run table. No filesystem mutation. Display
// path is recovered from each corpus's first zoekt shard
// (Repository.Source) — see corpusDisplayName. Empty / crashed corpora
// without shards display as "[empty]"; corpora whose source root has been
// deleted on disk show as "[gone] <path>".
//
// Vocab is past tense (`kept`/`evicted`) — same words as the live path
// (`seek gc --force`), interpreted as "what would happen". The live path
// (runGC streaming) uses the same renderGCRow + column layout, so dry-run
// and live output stay byte-aligned across columns.
//
// Per-row cost is dominated by corpusDirSize (filepath.WalkDir subtree walk
// per corpus) plus corpusDisplayName (one zoekt shard metadata read). For
// typical caches (<50 corpora) wall time is sub-second. The same per-row
// cost applies to the live path. If we ever see caches with 500+ corpora,
// parallelizing the per-row work with a bounded errgroup is the
// straightforward fix in both paths.
func reportGCPlan(w io.Writer, cacheRoot string, entries []corpusDirEntry, cutoff time.Time) error {
	_, _ = fmt.Fprintf(w, "seek gc: cache root %s\n", cacheRoot)
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "  no corpora")
		return nil
	}
	_, _ = fmt.Fprintln(w, gcTableHeader)
	now := time.Now()
	var totalBytes, evictBytes int64
	var evictCount int
	for _, e := range entries {
		size := corpusDirSize(e.path)
		totalBytes += size
		action := "kept"
		if !e.usedAt.After(cutoff) {
			action = "evicted"
			evictBytes += size
			evictCount++
		}
		renderGCRow(w, e, corpusDisplayName(e.path), size, now, action)
	}
	_, _ = fmt.Fprintf(w, "%d corpora, %s total, %d evictable (%s).\n",
		len(entries), humanBytes(totalBytes), evictCount, humanBytes(evictBytes))
	return nil
}

// formatRoot renders a corpusDisplayInfo for the table, marking missing
// sources with [gone] and truncating overlong paths from the LEFT so the
// recognizable basename always survives.
func formatRoot(info corpusDisplayInfo, width int) string {
	if info.source == "" {
		return "[empty]"
	}
	path := info.source
	if info.gone {
		path = "[gone] " + path
	}
	if len(path) <= width {
		return path
	}
	const ellipsis = "..."
	keep := width - len(ellipsis)
	if keep <= 0 {
		return path[:width]
	}
	return ellipsis + path[len(path)-keep:]
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

func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1fGB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%dMB", n/mb)
	case n >= kb:
		return fmt.Sprintf("%dKB", n/kb)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func corpusDirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
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

