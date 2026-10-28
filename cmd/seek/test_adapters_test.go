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

// parseSearchQuery is a test adapter for parseQueryHalves. It returns only
// the expanded query.
func parseSearchQuery(pattern string) (query.Q, error) {
	_, expanded, _, err := parseQueryHalves(pattern)
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
	return searchPlannedCorpusParsed(ctx, plan, nil, userQ, config)
}

// formatGitCorpusResultsForTest is a test adapter for the result format. It
// puts files in testGitCorpusPlan, hides the corpus context, and uses
// plainPalette.
func formatGitCorpusResultsForTest(files []zoekt.FileMatch, limit, maxMatches int) string {
	results := withHeaders(wrapCorpusResults(testGitCorpusPlan, files))
	return formatCorpusResultsWithContext(results, limit, maxMatches, hideCorpusContext, plainPalette)
}

// withHeaders sets the headers of results as the command does before it
// prints them (applyHeaders), with no path operand: each header is the path
// joined to its display root. It returns results.
func withHeaders(results []corpusSearchResult) []corpusSearchResult {
	applyHeaders(results, displayOperands{})
	return results
}

// searchPlannedCorpusForTest is a test adapter for searchPlannedCorpusParsed.
// It parses pattern and searches plan with the default search config.
func searchPlannedCorpusForTest(ctx context.Context, plan node, pattern string) ([]zoekt.FileMatch, error) {
	q, err := parseSearchQuery(pattern)
	if err != nil {
		return nil, err
	}
	return searchPlannedCorpusParsed(ctx, plan, nil, q, defaultSearchConfig())
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
	limit int,
	maxMatches int,
	displayMode corpusDisplayMode,
	pal palette,
) string {
	var sb strings.Builder
	_, _ = writeCorpusResultsWithContext(&sb, results, limit, maxMatches, displayMode, pal)
	return sb.String()
}

// parseGitStatusV2 is a test adapter for parseGitStatusV2Skipping. It
// parses git status --porcelain=v2 --branch --no-renames -z output and drops
// no entry.
func parseGitStatusV2(raw string) repoState {
	return parseGitStatusV2Skipping(raw, nil)
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
