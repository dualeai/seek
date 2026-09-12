package main

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

var testGitCorpusPlan = corpusPlan{id: "test-git", kind: corpusKindGit}

func run(ctx context.Context, pattern string, pathOperands []string) error {
	return runSearchCommand(
		ctx,
		pattern,
		pathOperands,
		0,
		0,
		defaultSearchConfig(),
		searchRunConfig{},
	)
}

func runWithSearchConfig(
	ctx context.Context,
	pattern string,
	pathOperands []string,
	config searchConfig,
) error {
	return runSearchCommand(ctx, pattern, pathOperands, 0, 0, config, searchRunConfig{})
}

func runGCCommand(ctx context.Context, args []string) error {
	cmd := newGCCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(io.Discard)
	return cmd.ExecuteContext(ctx)
}

func planDiscoveredGitCorpus(boundary gitBoundary) (corpusPlan, error) {
	return planDiscoveredGitPaths(boundary.toGitPaths())
}

func planFolderCorpus(root string, info os.FileInfo) (corpusPlan, error) {
	return planFolderCorpusWithExclusions(root, info, nil)
}

func planCurrentGitCorpusWithOperands(paths gitPaths, operands []string) (corpusPlan, error) {
	return planCurrentGitCorpusWithExclusions(paths, operands, nil)
}

func parseSearchQuery(pattern string) (query.Q, error) {
	_, expanded, err := parseSearchQueryForms(pattern)
	return expanded, err
}

func executeParsedShardSearchForTest(
	ctx context.Context,
	indexDir string,
	userQ query.Q,
	config searchConfig,
) ([]zoekt.FileMatch, error) {
	return executeParsedSearchScopedDirs(ctx, []string{indexDir}, userQ, nil, config)
}

func formatGitCorpusResultsForTest(files []zoekt.FileMatch, dirtyFiles dirtyFileSet, limit, maxMatches int) string {
	if len(files) == 0 {
		return ""
	}
	results := wrapCorpusResults(testGitCorpusPlan, files)
	dirtyByCorpus := dirtyFilesByCorpus{}
	if dirtyFiles != nil {
		dirtyByCorpus[testGitCorpusPlan.id] = dirtyFiles
	}
	return formatCorpusResultsWithContext(results, dirtyByCorpus, limit, maxMatches, hideCorpusContext, plainPalette)
}

func executeUnscopedShardSearchForTest(ctx context.Context, indexDir, pattern string) ([]zoekt.FileMatch, error) {
	q, err := parseSearchQuery(pattern)
	if err != nil {
		return nil, err
	}
	return executeParsedShardSearchForTest(ctx, indexDir, q, defaultSearchConfig())
}

func searchPlannedCorpusForTest(ctx context.Context, plan corpusPlan, pattern string) ([]zoekt.FileMatch, error) {
	q, err := parseSearchQuery(pattern)
	if err != nil {
		return nil, err
	}
	return searchPlannedCorpusParsed(ctx, plan, q, defaultSearchConfig())
}

// familyShardFilesForTest is the assertion form of familyShardFiles. Production
// code must handle a directory-read failure explicitly, so the two-value form
// stays; tests treat a read failure as a fatal error instead.
func familyShardFilesForTest(tb testing.TB, dir string, fam shardFamily) []string {
	tb.Helper()
	files, err := familyShardFiles(dir, fam)
	if err != nil {
		tb.Fatalf("familyShardFiles(%s): %v", dir, err)
	}
	return files
}
