package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
)

// errNoMatch is returned by run when the query executed successfully but
// produced zero results. Following the POSIX grep convention, this maps to
// exit code 1 — distinguishing "no match" from both success (0) and error (2).
// This lets callers use seek reliably in shell pipelines and conditionals:
//
//	if seek "TODO"; then … fi       # runs body only when matches exist
//	seek "pattern" || echo "nope"   # "nope" printed only on no-match
var errNoMatch = errors.New("no match")

// Set via ldflags (-X main.version=...) by make build / GoReleaser.
var version = ""

func versionString() string {
	if version != "" {
		return "seek " + version
	}
	// Fallback to VCS info embedded by go build.
	if info, ok := debug.ReadBuildInfo(); ok {
		v := info.Main.Version
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				v += " (" + s.Value[:7] + ")"
			}
		}
		if v != "" {
			return "seek " + v
		}
	}
	return "seek (unknown)"
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <pattern>\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	verbose := flag.Bool("verbose", false, "enable debug logging")
	flag.BoolVar(verbose, "v", false, "alias for -verbose")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return
	}

	// Configure logging: warn+ by default, debug+ with -verbose.
	logLevel := slog.LevelWarn
	if *verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	// Silence zoekt's log.Printf output by default; bridge to slog when verbose.
	if *verbose {
		log.SetOutput(newSlogWriter(logger))
		log.SetFlags(0)
	} else {
		log.SetOutput(io.Discard)
	}

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	pattern := flag.Arg(0)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, pattern); err != nil {
		if errors.Is(err, errNoMatch) {
			os.Exit(1)
		}
		slog.Error(err.Error())
		os.Exit(2)
	}
}

// slogWriter bridges Go's standard log package to slog. Each log.Printf call
// becomes a single slog.Info message.
type slogWriter struct {
	logger *slog.Logger
}

func newSlogWriter(l *slog.Logger) *slogWriter {
	return &slogWriter{logger: l}
}

func (w *slogWriter) Write(p []byte) (int, error) {
	// Trim trailing newline added by log.Printf
	msg := string(p)
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	w.logger.Info(msg)
	return len(p), nil
}

func run(ctx context.Context, pattern string) error {
	paths, err := resolveGitPathsFromCWD(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	repoDir := paths.RepoDir

	// Use absolute paths to avoid dependence on process CWD
	indexDir := filepath.Join(repoDir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return fmt.Errorf("create index directory: %w", err)
	}

	// Check for existing index state. If present, one-time setup
	// (ensureGitExclude, ensureUntrackedCache, ensureFSMonitor) was
	// already applied by the indexing run that created the state file.
	// Skipping them on the warm path saves ~150µs of file I/O.
	cachedState := readStateFile(indexDir)
	if cachedState == "" {
		// First run or corrupted state — apply one-time setup before
		// computing git status, so the cache dir is excluded.
		ensureGitExclude(paths, cacheDir)
		ensureUntrackedCache(ctx, paths)
		ensureFSMonitor(ctx, paths)
	}

	// Compute state hash from a single atomic git status call.
	state := gitRepoState(ctx)
	currentState := computeStateHash(repoStateFingerprint(repoDir, state))

	// Re-index if the cached state differs from the current working tree.
	if currentState != cachedState {
		if err := runIndexing(ctx, paths, indexDir, state, currentState); err != nil {
			slog.Warn("Indexing failed", "error", err)
			// Continue to search with whatever shards exist
		}
	}

	// Execute search with LOCK_SH so concurrent indexers (which hold LOCK_EX)
	// finish before we read shards. Multiple searchers can hold LOCK_SH
	// simultaneously — no contention between readers. Uses non-blocking
	// poll with timeout to prevent indefinite hang if an indexer is stuck.
	searchLockPath := filepath.Join(indexDir, lockFile)
	searchLockFd, err := os.OpenFile(searchLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open search lock: %w", err)
	}
	defer func() {
		unlockFile(searchLockFd)
		_ = searchLockFd.Close()
	}()
	if err := acquireSearchLock(ctx, searchLockFd); err != nil {
		return fmt.Errorf("acquire search lock: %w", err)
	}

	results, err := executeSearch(ctx, indexDir, pattern)
	if err != nil {
		return err
	}

	if len(results) == 0 {
		return errNoMatch
	}

	// Build dirty-file set so formatResults can suppress stale committed
	// results for files that have been modified in the working tree.
	var dirtyFiles map[string]bool
	if len(state.Files) > 0 {
		dirtyFiles = make(map[string]bool, len(state.Files))
		for _, f := range state.Files {
			dirtyFiles[f] = true
		}
	}

	// formatResults returns "" when all results were stale committed
	// matches for dirty files — treat as no match (exit code 1).
	output := formatResults(results, dirtyFiles)
	if output == "" {
		return errNoMatch
	}

	fmt.Print(output)
	return nil
}
