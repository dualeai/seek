package main

import (
	"context"
	"fmt"
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
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Println(versionString())
		return
	}

	if len(os.Args) < 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "Usage: seek <pattern>")
		os.Exit(2)
	}
	pattern := os.Args[1]

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, pattern); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
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
			fmt.Fprintf(os.Stderr, "Warning: indexing failed: %v\n", err)
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
