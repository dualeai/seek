package main

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctags "github.com/sourcegraph/go-ctags"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// testGitCorpusPlan is the one Git node that the test format adapter puts
// results in.
var testGitCorpusPlan = node{id: "test-git", kind: corpusKindGit}

// run is a test adapter for runSearchCommand. It searches pathOperands with
// no file or match limit, the default search config, and the default run
// config.
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

// runWithSearchConfig is a test adapter for runSearchCommand. It is the same
// as run, but it uses config.
func runWithSearchConfig(
	ctx context.Context,
	pattern string,
	pathOperands []string,
	config searchConfig,
) error {
	return runSearchCommand(ctx, pattern, pathOperands, 0, 0, config, searchRunConfig{})
}

// runGCCommand is a test adapter for the gc command (newGCCmd). It runs the
// command with args, writes its output to os.Stdout, and discards its error
// output: the caller reads the returned error.
func runGCCommand(ctx context.Context, args []string) error {
	cmd := newGCCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(io.Discard)
	return cmd.ExecuteContext(ctx)
}

// planGitOperands is a test adapter for expandGraph. It expands the graph
// from one Git operand.
func planGitOperands(ctx context.Context, paths gitPaths, operand string) ([]node, error) {
	return expandGraph(ctx, plannedExternalOperands{gitRoots: []externalGitRoot{{
		paths:    paths,
		operands: []string{operand},
	}}})
}

// parseSearchQuery is a test adapter for parseQueryHalves. It returns only
// the expanded query.
func parseSearchQuery(pattern string) (query.Q, error) {
	_, expanded, err := parseSearchQueryForms(pattern)
	return expanded, err
}

// executeParsedShardSearchForTest is a test adapter for
// searchPlannedCorpusParsed. It searches one index directory as a node with no
// scope. The node's cache directory is the parent of indexDir, as in a planned
// node (cacheDir/index), so the read lock file goes there.
func executeParsedShardSearchForTest(
	ctx context.Context,
	indexDir string,
	userQ query.Q,
	config searchConfig,
) ([]zoekt.FileMatch, error) {
	plan := node{cacheDir: filepath.Dir(indexDir), indexDir: indexDir}
	return searchPlannedCorpusParsed(ctx, plan, userQ, config)
}

// formatGitCorpusResultsForTest is a test adapter for the result format. It
// puts files in testGitCorpusPlan, drops a committed result of a file in
// dirtyFiles, hides the corpus context, and uses plainPalette.
func formatGitCorpusResultsForTest(files []zoekt.FileMatch, dirtyFiles dirtyFileSet, limit, maxMatches int) string {
	results := wrapCorpusResults(testGitCorpusPlan, files)
	dirtyByCorpus := dirtyFilesByCorpus{}
	if dirtyFiles != nil {
		dirtyByCorpus[testGitCorpusPlan.id] = dirtyFiles
	}
	return formatCorpusResultsWithContext(results, dirtyByCorpus, limit, maxMatches, hideCorpusContext, plainPalette)
}

// searchPlannedCorpusForTest is a test adapter for searchPlannedCorpusParsed.
// It parses pattern and searches plan with the default search config.
func searchPlannedCorpusForTest(ctx context.Context, plan node, pattern string) ([]zoekt.FileMatch, error) {
	q, err := parseSearchQuery(pattern)
	if err != nil {
		return nil, err
	}
	return searchPlannedCorpusParsed(ctx, plan, q, defaultSearchConfig())
}

// familyShardFilesForTest is a test adapter for familyShardFiles. Production
// code must handle a directory-read failure, so it keeps the two-value form.
// This adapter stops the test on a read failure.
func familyShardFilesForTest(tb testing.TB, dir string, fam shardFamily) []string {
	tb.Helper()
	files, err := familyShardFiles(dir, fam)
	if err != nil {
		tb.Fatalf("familyShardFiles(%s): %v", dir, err)
	}
	return files
}

// formatCorpusResultsWithContext is a test adapter for
// writeCorpusResultsWithContext. It returns the text that the command writes.
func formatCorpusResultsWithContext(
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
	limit int,
	maxMatches int,
	displayMode corpusDisplayMode,
	pal palette,
) string {
	var sb strings.Builder
	_, _ = writeCorpusResultsWithContext(&sb, results, dirtyByCorpus, limit, maxMatches, displayMode, pal)
	return sb.String()
}

// parseGitStatusV2 is a test adapter for parseGitStatusV2Skipping. It
// parses git status --porcelain=v2 --branch --no-renames -z output and drops
// no entry.
func parseGitStatusV2(raw string) repoState {
	return parseGitStatusV2Skipping(raw, nil)
}

// parseSearchQueryForms is a test adapter for parseQueryHalves. It returns
// the raw and the expanded query.
func parseSearchQueryForms(pattern string) (query.Q, query.Q, error) {
	raw, strict, _, err := parseQueryHalves(pattern)
	return raw, strict, err
}

// extractSemanticUnits is a test adapter for extractSemanticUnitsWithID. It
// hashes content to get the content ID. The caller gives eligible content: the
// build checks eligibility (semanticContentIsEligible) before it extracts.
func extractSemanticUnits(
	path string,
	content []byte,
	entries []*ctags.Entry,
	parseErr error,
) []semanticUnit {
	return extractSemanticUnitsWithID(path, content, semanticContentID(sha256.Sum256(content)), entries, parseErr)
}
