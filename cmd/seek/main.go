package main

import (
	"context"
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
		slog.Error(err.Error())
		os.Exit(1)
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
	repoDir, err := gitRepoRoot(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Use absolute paths to avoid dependence on process CWD
	indexDir := filepath.Join(repoDir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return fmt.Errorf("create index directory: %w", err)
	}

	// Step 4: compute state hash (single atomic git call)
	state := gitRepoState(ctx)
	currentState := computeStateHash(state.RawOutput)

	// Step 5: check cached state
	cachedState := readStateFile(indexDir)
	if currentState != cachedState {
		// Step 6: run indexing
		if err := runIndexing(ctx, repoDir, indexDir, state, currentState); err != nil {
			slog.Warn("Indexing failed", "error", err)
			// Continue to search with whatever shards exist
		}
	}

	// Step 7-8: execute search and format results
	results, err := executeSearch(ctx, indexDir, pattern)
	if err != nil {
		return err
	}

	output := formatResults(results)
	if output != "" {
		fmt.Print(output)
	}

	return nil
}
