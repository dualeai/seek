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

func writeGCUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: seek gc [--force] [--dry-run] [--all]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Flags:")
	_, _ = fmt.Fprintln(w, "      --force       bypass throttle gate (.last-gc)")
	_, _ = fmt.Fprintln(w, "      --dry-run     print plan, evict nothing")
	_, _ = fmt.Fprintln(w, "      --all         evict every corpus not actively locked (TTL=0)")
}

func parseGCFlags(args []string) (gcCmdOptions, error) {
	var opts gcCmdOptions
	for _, arg := range args {
		switch arg {
		case "--force", "-force":
			opts.force = true
		case "--dry-run", "-dry-run":
			opts.dryRun = true
		case "--all", "-all":
			opts.all = true
		case "-h", "--help", "-help":
			writeGCUsage(os.Stdout)
			os.Exit(0)
		default:
			return opts, fmt.Errorf("unknown flag: %s", arg)
		}
	}
	return opts, nil
}

// runGCCommand implements `seek gc ...`. Returns an error for invalid flags;
// every other failure is logged + swallowed (exit 0).
func runGCCommand(ctx context.Context, args []string) error {
	opts, err := parseGCFlags(args)
	if err != nil {
		writeGCUsage(os.Stderr)
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	if opts.dryRun {
		return reportGCPlan(os.Stdout, cacheRoot, entries, time.Now().Add(-maxAge))
	}

	// Live eviction: reuse runGC for trash drain + lock + per-corpus
	// rename/RemoveAll. `--force` and `--all` both bypass the throttle
	// gate — `--all` is an explicit user wipe, not subject to .last-gc.
	runGC(ctx, gcOptions{maxAge: maxAge, skipThrottle: opts.force || opts.all}, cfg.interval)
	return nil
}

// reportGCPlan prints the dry-run table. No filesystem mutation. Display
// path is recovered from each corpus's first zoekt shard
// (Repository.Source) — see corpusDisplayName. Empty / crashed corpora
// without shards display as "[empty]"; corpora whose source root has been
// deleted on disk show as "[gone] <path>".
//
// Per-row cost is dominated by corpusDirSize (filepath.WalkDir subtree walk
// per corpus). For typical caches (<50 corpora) wall time is sub-second.
// If we ever see caches with 500+ corpora, parallelizing the per-row work
// with a bounded errgroup is the straightforward fix.
func reportGCPlan(w io.Writer, cacheRoot string, entries []corpusDirEntry, cutoff time.Time) error {
	_, _ = fmt.Fprintf(w, "seek gc: cache root %s\n", cacheRoot)
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "  no corpora")
		return nil
	}
	_, _ = fmt.Fprintln(w, "  CORPUS      ROOT                                                  AGE     SIZE    ACTION")
	now := time.Now()
	var totalBytes, evictBytes int64
	var evictCount int
	for _, e := range entries {
		size := corpusDirSize(e.path)
		totalBytes += size
		action := "keep"
		if !e.usedAt.After(cutoff) {
			action = "evict"
			evictBytes += size
			evictCount++
		}
		age := now.Sub(e.usedAt)
		_, _ = fmt.Fprintf(w, "  %-10s  %-52s  %-6s  %-6s  %s\n",
			truncateHash(e.name),
			formatRoot(corpusDisplayName(e.path), 52),
			humanDuration(age),
			humanBytes(size),
			action,
		)
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

