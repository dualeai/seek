package main

import (
	"context"
	"testing"

	"github.com/sourcegraph/zoekt"
)

var testGitCorpusPlan = corpusPlan{id: "test-git", kind: corpusKindGit}

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
	return executeParsedSearchScoped(ctx, indexDir, q, nil, defaultSearchConfig())
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
