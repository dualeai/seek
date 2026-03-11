package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
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
	// Step 2: cd to git repo root
	repoDir, err := gitRepoRoot()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	if err := os.Chdir(repoDir); err != nil {
		return fmt.Errorf("chdir to repo root: %w", err)
	}

	indexDir := ".zoekt-index"
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return fmt.Errorf("create index directory: %w", err)
	}

	// Step 4: compute state hash
	headSHA := gitHeadSHA(ctx)
	statusOutput := gitStatusPorcelain(ctx)
	currentState := computeStateHash(headSHA, statusOutput)

	// Step 5: check cached state
	cachedState := readStateFile(indexDir)
	if currentState != cachedState {
		// Step 6: run indexing
		if err := runIndexing(ctx, repoDir, indexDir, headSHA, statusOutput, currentState); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: indexing failed: %v\n", err)
			// Continue to search with whatever shards exist
		}
	}

	// Step 7-8: execute search and format results
	repoPrefix := deriveRepoPrefix(ctx, repoDir)
	results, err := executeSearch(ctx, indexDir, pattern)
	if err != nil {
		return err
	}

	output := formatResults(results, repoPrefix)
	if output != "" {
		fmt.Print(output)
	}

	return nil
}
