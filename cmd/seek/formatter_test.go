package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
)

func testDirtyFileSet(paths ...string) dirtyFileSet {
	files := make(dirtyFileSet, len(paths))
	for _, path := range paths {
		files[path] = struct{}{}
	}
	return files
}

func TestGitCorpusFormatting_Empty(t *testing.T) {
	result := formatGitCorpusResultsForTest(nil, nil, 0, 0)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestGitCorpusFormatting_BasicFileMatch(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "src/main.go",
			Repository: "github.com/org/repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("func main() {\n"),
					LineNumber: 5,
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := "## src/main.go (Go)\n5 func main() {"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_UncommittedTag(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "lib/utils.py",
			Repository: repoUncommitted,
			Language:   "Python",
			Score:      5,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("def helper():\n"),
					LineNumber: 10,
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := "## lib/utils.py (Python) [uncommitted]\n10 def helper():"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_Deduplication_UncommittedWins(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "src/app.go",
			Repository: "github.com/org/repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("old content from repo\n"), LineNumber: 1},
			},
		},
		{
			FileName:   "src/app.go",
			Repository: repoUncommitted,
			Language:   "Go",
			Score:      5,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("new local changes\n"), LineNumber: 1},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if !strings.Contains(result, "[uncommitted]") {
		t.Error("expected uncommitted version to win deduplication")
	}
	if strings.Contains(result, "old content from repo") {
		t.Error("committed version should not appear when uncommitted exists")
	}
	if !strings.Contains(result, "new local changes") {
		t.Error("uncommitted content should appear")
	}
}

func TestGitCorpusFormatting_ScoreSorting(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "low.go",
			Repository: "repo",
			Language:   "Go",
			Score:      1,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("low\n"), LineNumber: 1},
			},
		},
		{
			FileName:   "high.go",
			Repository: "repo",
			Language:   "Go",
			Score:      100,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("high\n"), LineNumber: 1},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	highIdx := strings.Index(result, "high.go")
	lowIdx := strings.Index(result, "low.go")
	if highIdx > lowIdx {
		t.Error("expected high-score file to appear first")
	}
}

func TestGitCorpusFormatting_SymbolKind(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "router.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("func CoreRouter() {\n"),
					LineNumber: 15,
					LineFragments: []zoekt.LineFragmentMatch{
						{
							SymbolInfo: &zoekt.Symbol{Kind: "function"},
						},
					},
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := "## router.go (Go)\n15 [function] func CoreRouter() {"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_LanguageFallback(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "data.txt",
			Repository: "repo",
			Language:   "",
			Score:      1,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("hello\n"), LineNumber: 1},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if !strings.Contains(result, "(unknown)") {
		t.Error("expected language fallback to 'unknown'")
	}
}

func TestGitCorpusFormatting_MultiFile(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "a.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("line a\n"), LineNumber: 1},
			},
		},
		{
			FileName:   "b.py",
			Repository: repoUncommitted,
			Language:   "Python",
			Score:      5,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("line b\n"), LineNumber: 2},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if !strings.Contains(result, "## a.go (Go)") {
		t.Error("expected a.go header")
	}
	if !strings.Contains(result, "## b.py (Python) [uncommitted]") {
		t.Error("expected b.py header with uncommitted tag")
	}
}

func TestGitCorpusFormatting_NoTrailingNewline(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "a.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("hello\n"), LineNumber: 1},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if len(result) > 0 && result[len(result)-1] == '\n' {
		t.Error("output must not end with trailing newline")
	}
}

func TestGitCorpusFormatting_ZeroLineMatches(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "empty.go",
			Repository: "repo",
			Language:   "Go",
			Score:      1,
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := "## empty.go (Go)"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestGitCorpusFormatting_ManyFiles_SortedByScore(t *testing.T) {
	files := make([]zoekt.FileMatch, 1000)
	for i := range files {
		files[i] = zoekt.FileMatch{
			FileName:   fmt.Sprintf("file_%04d.go", i),
			Repository: "repo",
			Language:   "Go",
			Score:      float64(i), // ascending scores
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("match\n"), LineNumber: 1},
			},
		}
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)

	// Highest score (999) should appear before lowest (0)
	highIdx := strings.Index(result, "file_0999.go")
	lowIdx := strings.Index(result, "file_0000.go")
	if highIdx < 0 || lowIdx < 0 {
		t.Fatal("expected both file_0999.go and file_0000.go in output")
	}
	if highIdx > lowIdx {
		t.Error("expected highest score file to appear first")
	}
}

func TestGitCorpusFormatting_UncommittedWinsRegardlessOfOrder(t *testing.T) {
	committed := zoekt.FileMatch{
		FileName:   "app.go",
		Repository: "repo",
		Language:   "Go",
		Score:      10,
		LineMatches: []zoekt.LineMatch{
			{Line: []byte("head_only\n"), LineNumber: 1},
		},
	}
	uncommitted := zoekt.FileMatch{
		FileName:   "app.go",
		Repository: repoUncommitted,
		Language:   "Go",
		Score:      5,
		LineMatches: []zoekt.LineMatch{
			{Line: []byte("worktree_only\n"), LineNumber: 1},
		},
	}

	for _, files := range [][]zoekt.FileMatch{
		{committed, uncommitted},
		{uncommitted, committed},
	} {
		result := formatGitCorpusResultsForTest(files, nil, 0, 0)
		if !strings.Contains(result, "[uncommitted]") {
			t.Fatalf("expected uncommitted tag, got:\n%s", result)
		}
		if !strings.Contains(result, "worktree_only") {
			t.Fatalf("expected uncommitted content, got:\n%s", result)
		}
		if strings.Contains(result, "head_only") {
			t.Fatalf("committed content should be hidden when uncommitted match exists, got:\n%s", result)
		}
	}
}

func TestGitCorpusFormatting_ScoreTiebreaking_Stable(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "b.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("b\n"), LineNumber: 1}}},
		{FileName: "a.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("a\n"), LineNumber: 1}}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	aIdx := strings.Index(result, "a.go")
	bIdx := strings.Index(result, "b.go")
	if aIdx > bIdx {
		t.Error("expected alphabetical tiebreaking for equal scores")
	}
}

func TestGitCorpusFormatting_TwoCommittedMatchesSameFileKeepsOneVisibleResult(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("first\n"), LineNumber: 1}}},
		{FileName: "a.go", Repository: "repo", Score: 5,
			LineMatches: []zoekt.LineMatch{{Line: []byte("second\n"), LineNumber: 2}}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if strings.Count(result, "## a.go") != 1 {
		t.Fatalf("expected one visible file header, got:\n%s", result)
	}
	if !strings.Contains(result, "first") {
		t.Fatalf("expected first visible match, got:\n%s", result)
	}
	if strings.Contains(result, "second") {
		t.Fatalf("duplicate committed match should be hidden, got:\n%s", result)
	}
}

func TestGitCorpusFormatting_ContextLines_BeforeAndAfter(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "server.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("func handleRequest(w http.ResponseWriter, r *http.Request) {\n"),
					LineNumber: 15,
					Before:     []byte("\n// handleRequest processes incoming HTTP requests.\n"),
					After:      []byte("    ctx := r.Context()\n    log.Info(\"handling request\")\n"),
					LineFragments: []zoekt.LineFragmentMatch{
						{SymbolInfo: &zoekt.Symbol{Kind: "function"}},
					},
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## server.go (Go)",
		"13 ",
		"14 // handleRequest processes incoming HTTP requests.",
		"15 [function] func handleRequest(w http.ResponseWriter, r *http.Request) {",
		`16     ctx := r.Context()`,
		`17     log.Info("handling request")`,
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_NoContext(t *testing.T) {
	// When Before/After are empty, output should be the same as before
	files := []zoekt.FileMatch{
		{
			FileName:   "main.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("func main() {\n"),
					LineNumber: 5,
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := "## main.go (Go)\n5 func main() {"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_OverlappingContext(t *testing.T) {
	// Two matches close together: context should not duplicate lines.
	// Match on line 10 with 2 lines after, match on line 12 with 2 lines before.
	// Lines 11 should only appear once.
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("first match\n"),
					LineNumber: 10,
					After:      []byte("line eleven\nline twelve\n"),
				},
				{
					Line:       []byte("second match\n"),
					LineNumber: 12,
					Before:     []byte("line eleven\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"10 first match",
		"11 line eleven",
		"12 second match",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_NonContiguousRegions(t *testing.T) {
	// Two matches far apart: should have a blank separator between regions.
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("first match\n"),
					LineNumber: 5,
					After:      []byte("after first\n"),
				},
				{
					Line:       []byte("second match\n"),
					LineNumber: 50,
					Before:     []byte("before second\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"5 first match",
		"6 after first",
		"",
		"49 before second",
		"50 second match",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_AdjacentMatches(t *testing.T) {
	// Two matches on adjacent lines: no blank separator, no duplicated context.
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("line one\n"),
					LineNumber: 10,
					After:      []byte("line two content\n"),
				},
				{
					Line:       []byte("line two match\n"),
					LineNumber: 11,
					Before:     []byte("line one content\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"10 line one",
		"11 line two match",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_ThreeContextLines(t *testing.T) {
	// Full 3-line context before and after a match.
	files := []zoekt.FileMatch{
		{
			FileName:   "main.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("    target line\n"),
					LineNumber: 20,
					Before:     []byte("ctx line 17\nctx line 18\nctx line 19\n"),
					After:      []byte("ctx line 21\nctx line 22\nctx line 23\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## main.go (Go)",
		"17 ctx line 17",
		"18 ctx line 18",
		"19 ctx line 19",
		"20     target line",
		"21 ctx line 21",
		"22 ctx line 22",
		"23 ctx line 23",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_MatchOnLine1(t *testing.T) {
	// Match on line 1 — no room for before-context.
	files := []zoekt.FileMatch{
		{
			FileName:   "main.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("package main\n"),
					LineNumber: 1,
					After:      []byte("import \"fmt\"\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## main.go (Go)",
		"1 package main",
		"2 import \"fmt\"",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_MatchOnLine1_ExcessBefore(t *testing.T) {
	// Match on line 1 with Before bytes that would produce negative line numbers.
	// The guard should clamp firstBeforeLine to 1 and slice off excess.
	files := []zoekt.FileMatch{
		{
			FileName:   "main.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("package main\n"),
					LineNumber: 1,
					Before:     []byte("phantom line 1\nphantom line 2\nphantom line 3\n"),
					After:      []byte("import \"fmt\"\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	// Before lines should be silently dropped — no negative line numbers
	if strings.Contains(result, "phantom") {
		t.Errorf("expected excess Before lines to be dropped, got:\n%s", result)
	}
	if strings.Contains(result, "0 ") || strings.Contains(result, "  -") {
		t.Errorf("expected no zero or negative line numbers, got:\n%s", result)
	}
	if !strings.Contains(result, "1 package main") {
		t.Errorf("expected match line to still be present, got:\n%s", result)
	}
}

func TestGitCorpusFormatting_ContextLines_MatchOnLine2_PartialBefore(t *testing.T) {
	// Match on line 2 with 3 lines of Before — only 1 line fits (line 1).
	files := []zoekt.FileMatch{
		{
			FileName:   "main.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("import \"fmt\"\n"),
					LineNumber: 2,
					Before:     []byte("excess\nmore excess\npackage main\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## main.go (Go)",
		"1 package main",
		"2 import \"fmt\"",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_ThreeConsecutiveMatches(t *testing.T) {
	// Three matches close together — context should flow without duplication or gaps.
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("match A\n"),
					LineNumber: 10,
					After:      []byte("between AB\n"),
				},
				{
					Line:       []byte("match B\n"),
					LineNumber: 12,
					Before:     []byte("between AB\n"),
					After:      []byte("between BC\n"),
				},
				{
					Line:       []byte("match C\n"),
					LineNumber: 14,
					Before:     []byte("between BC\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"10 match A",
		"11 between AB",
		"12 match B",
		"13 between BC",
		"14 match C",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_OnlyBefore(t *testing.T) {
	// Match with Before but no After.
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("last line\n"),
					LineNumber: 100,
					Before:     []byte("penultimate\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"99 penultimate",
		"100 last line",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_OnlyAfter(t *testing.T) {
	// Match with After but no Before.
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("first line\n"),
					LineNumber: 1,
					After:      []byte("second line\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"1 first line",
		"2 second line",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_EmptyLinesInContext(t *testing.T) {
	// Context containing empty lines (blank lines in source code).
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("func main() {\n"),
					LineNumber: 5,
					Before:     []byte("import \"fmt\"\n\n\n"),
					After:      []byte("\n    fmt.Println()\n"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"2 import \"fmt\"",
		"3 ",
		"4 ",
		"5 func main() {",
		"6 ",
		"7     fmt.Println()",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_EmptyByteSlice(t *testing.T) {
	// Before/After as empty []byte{} (not nil) — should behave like nil.
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("match\n"),
					LineNumber: 5,
					Before:     []byte{},
					After:      []byte{},
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := "## app.go (Go)\n5 match"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_ContextLines_BeforeNoTrailingNewline(t *testing.T) {
	// Before bytes without trailing newline should still be counted correctly.
	files := []zoekt.FileMatch{
		{
			FileName:   "app.go",
			Repository: "repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("match\n"),
					LineNumber: 3,
					Before:     []byte("line one\nline two"),
				},
			},
		},
	}

	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"1 line one",
		"2 line two",
		"3 match",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestGitCorpusFormatting_FileOnlyMatch_LineNumberZero(t *testing.T) {
	// Reproduces panic on file-only queries where zoekt returns LineNumber=0
	// with empty Before/After (e.g. "file:foo" with no content term).
	files := []zoekt.FileMatch{
		{
			FileName:   "path/to/file.go",
			Repository: "github.com/example/repo",
			Language:   "Go",
			Score:      10,
			LineMatches: []zoekt.LineMatch{{
				LineNumber: 0,
				Line:       nil,
				Before:     nil,
				After:      nil,
			}},
		},
	}

	// Should not panic
	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if !strings.Contains(result, "## path/to/file.go (Go)") {
		t.Errorf("expected file header, got: %s", result)
	}
}

func TestGitCorpusFormatting_StaleDirtyFileSuppressed(t *testing.T) {
	// End-to-end: formatting should produce no output when the only match
	// is a stale committed result for a dirty file.
	files := []zoekt.FileMatch{
		{
			FileName:   "sqlite.py",
			Repository: "repo",
			Language:   "Python",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("_MIGRATIONS = []\n"), LineNumber: 60},
			},
		},
	}
	dirtyFiles := testDirtyFileSet("sqlite.py")
	result := formatGitCorpusResultsForTest(files, dirtyFiles, 0, 0)
	if result != "" {
		t.Errorf("expected empty output for stale dirty file, got:\n%s", result)
	}
}

func TestGitCorpusFormatting_AllSuppressedReturnsEmpty(t *testing.T) {
	// When all results are suppressed, formatting returns "" so the caller
	// can detect "no valid results" and return errNoMatch (exit code 1).
	files := []zoekt.FileMatch{
		{FileName: "a.py", Repository: "repo", Language: "Python", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("old\n"), LineNumber: 1}}},
		{FileName: "b.py", Repository: "repo", Language: "Python", Score: 5,
			LineMatches: []zoekt.LineMatch{{Line: []byte("stale\n"), LineNumber: 2}}},
	}
	dirtyFiles := testDirtyFileSet("a.py", "b.py")
	result := formatGitCorpusResultsForTest(files, dirtyFiles, 0, 0)
	if result != "" {
		t.Errorf("expected empty string when all results suppressed, got:\n%s", result)
	}
}

func TestGitCorpusFormatting_PartialSuppression(t *testing.T) {
	// Some results suppressed, some kept.
	files := []zoekt.FileMatch{
		{FileName: "stale.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("old\n"), LineNumber: 1}}},
		{FileName: "valid.go", Repository: "repo", Language: "Go", Score: 5,
			LineMatches: []zoekt.LineMatch{{Line: []byte("good\n"), LineNumber: 1}}},
	}
	dirtyFiles := testDirtyFileSet("stale.go")
	result := formatGitCorpusResultsForTest(files, dirtyFiles, 0, 0)
	if strings.Contains(result, "stale.go") {
		t.Error("stale.go should be suppressed")
	}
	if !strings.Contains(result, "valid.go") {
		t.Error("valid.go should be present")
	}
	if !strings.Contains(result, "good") {
		t.Error("valid content should be present")
	}
}

func TestGitCorpusFormatting_VeryLongFileName(t *testing.T) {
	longName := strings.Repeat("a", 1000) + ".go"
	files := []zoekt.FileMatch{
		{
			FileName:   longName,
			Repository: "repo",
			Language:   "Go",
			Score:      1,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("match\n"), LineNumber: 1},
			},
		},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if !strings.Contains(result, longName) {
		t.Error("expected long filename to appear in output without truncation")
	}
}

func TestGitCorpusFormatting_VeryLongLine(t *testing.T) {
	longLine := strings.Repeat("x", 10000) + "\n"
	files := []zoekt.FileMatch{
		{
			FileName:   "long.go",
			Repository: "repo",
			Language:   "Go",
			Score:      1,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte(longLine), LineNumber: 1},
			},
		},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if !strings.Contains(result, strings.Repeat("x", 10000)) {
		t.Error("expected long line to appear without truncation")
	}
}

func TestGitCorpusFormatting_SpecialCharsInLine(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "special.go",
			Repository: "repo",
			Language:   "Go",
			Score:      1,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("tab\there unicode: 日本語 emoji: 🎉\n"), LineNumber: 1},
			},
		},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if !strings.Contains(result, "tab\there unicode: 日本語 emoji: 🎉") {
		t.Errorf("expected special chars preserved, got: %s", result)
	}
}

func TestGitCorpusFormatting_LineNumber_MaxUint32(t *testing.T) {
	// Large line number — verify no overflow in context line arithmetic
	files := []zoekt.FileMatch{
		{
			FileName:   "big.go",
			Repository: "repo",
			Language:   "Go",
			Score:      1,
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("match\n"),
					LineNumber: 1<<31 - 1, // max int32
					Before:     []byte("before\n"),
				},
			},
		},
	}
	// Should not panic
	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	if !strings.Contains(result, "match") {
		t.Error("expected match to appear in output")
	}
}

func TestGitCorpusFormatting_LineNumbers_NoPaddingNoIndent(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName: "shallow.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("near top\n"), LineNumber: 5},
			},
		},
		{
			FileName: "deep.go", Repository: "repo", Language: "Go", Score: 9,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("far down\n"), LineNumber: 100},
			},
		},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 0)
	// Dense gutter: line numbers are flush-left with a single space, never
	// indented or right-aligned to a shared cross-file width.
	if !strings.Contains(result, "\n5 near top") || !strings.Contains(result, "\n100 far down") {
		t.Errorf("expected flush-left line numbers, got:\n%s", result)
	}
	if strings.Contains(result, "\n 5 near top") {
		t.Errorf("line numbers must not be padded/indented, got:\n%s", result)
	}
}

// --- Limit / MaxMatches tests ---

// extractFileHeaders returns the "## filename" headers from formatted output.
func extractFileHeaders(output string) []string {
	var headers []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "## ") {
			headers = append(headers, line)
		}
	}
	return headers
}

func dedupFixtureFiles(count int, nameFormat, committedLine, uncommittedLine string) []zoekt.FileMatch {
	files := make([]zoekt.FileMatch, count*2)
	for i := range count {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf(nameFormat, i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte(committedLine), LineNumber: 1}},
		}
		files[count+i] = zoekt.FileMatch{
			FileName: fmt.Sprintf(nameFormat, i), Repository: repoUncommitted, Language: "Go",
			Score:       float64(i + 1),
			LineMatches: []zoekt.LineMatch{{Line: []byte(uncommittedLine), LineNumber: 1}},
		}
	}
	return files
}

func TestGitCorpusFormatting_Limit_TopN(t *testing.T) {
	files := make([]zoekt.FileMatch, 10)
	for i := range 10 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("file_%02d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte("match\n"), LineNumber: 1}},
		}
	}
	result := formatGitCorpusResultsForTest(files, nil, 3, 0)
	// Anchor on the "## " header so e.g. "file_09.go" can't alias "file_900.go".
	for _, want := range []string{"## file_09.go", "## file_08.go", "## file_07.go"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %s in limited output", want)
		}
	}
	for _, noWant := range []string{"## file_06.go", "## file_05.go", "## file_00.go"} {
		if strings.Contains(result, noWant) {
			t.Errorf("did not expect %s in limited output", noWant)
		}
	}
}

func TestGitCorpusFormatting_NonPositiveLimitsAreUnlimited(t *testing.T) {
	files := make([]zoekt.FileMatch, 5)
	for i := range 5 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("f%d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte(fmt.Sprintf("line%d\n", i)), LineNumber: i + 1}},
		}
	}
	for _, tc := range []struct {
		name              string
		limit, maxMatches int
	}{
		{"limit_zero", 0, 0},
		{"limit_negative", -5, 0},
		{"maxMatches_negative", 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := formatGitCorpusResultsForTest(files, nil, tc.limit, tc.maxMatches)
			for i := range 5 {
				if !strings.Contains(result, fmt.Sprintf("f%d.go", i)) {
					t.Errorf("expected f%d.go in output", i)
				}
			}
		})
	}
}

func TestGitCorpusFormatting_Limit_ExceedsResults(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "only.go", Repository: "repo", Language: "Go", Score: 1,
			LineMatches: []zoekt.LineMatch{{Line: []byte("m\n"), LineNumber: 1}}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 100, 0)
	if !strings.Contains(result, "only.go") {
		t.Error("expected only.go in output")
	}
}

func TestGitCorpusFormatting_Limit_EqualsResults(t *testing.T) {
	files := make([]zoekt.FileMatch, 3)
	for i := range 3 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("f%d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte("m\n"), LineNumber: 1}},
		}
	}
	result := formatGitCorpusResultsForTest(files, nil, 3, 0)
	for i := range 3 {
		if !strings.Contains(result, fmt.Sprintf("f%d.go", i)) {
			t.Errorf("expected f%d.go in output when limit == count", i)
		}
	}
}

func TestGitCorpusFormatting_Limit_EmptyInput(t *testing.T) {
	if result := formatGitCorpusResultsForTest(nil, nil, 5, 0); result != "" {
		t.Errorf("expected empty output, got %q", result)
	}
}

func TestGitCorpusFormatting_Limit_DedupReducesBelowLimit(t *testing.T) {
	files := dedupFixtureFiles(5, "f%d.go", "committed\n", "uncommitted\n")
	result := formatGitCorpusResultsForTest(files, nil, 8, 0)
	headers := extractFileHeaders(result)
	if len(headers) != 5 {
		t.Errorf("expected 5 file headers after dedup (limit=8), got %d", len(headers))
	}
}

func TestGitCorpusFormatting_Limit_AllSuppressedByDirty(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("old\n"), LineNumber: 1}}},
	}
	dirtyFiles := testDirtyFileSet("a.go")
	result := formatGitCorpusResultsForTest(files, dirtyFiles, 5, 0)
	if result != "" {
		t.Errorf("expected empty output, got %q", result)
	}
}

func TestGitCorpusFormatting_Limit_TiedScores(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "b.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("b\n"), LineNumber: 1}}},
		{FileName: "a.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("a\n"), LineNumber: 1}}},
		{FileName: "c.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("c\n"), LineNumber: 1}}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 2, 0)
	if !strings.Contains(result, "a.go") || !strings.Contains(result, "b.go") {
		t.Errorf("expected a.go and b.go in output, got:\n%s", result)
	}
	if strings.Contains(result, "c.go") {
		t.Error("c.go should be excluded by limit=2")
	}
}

func TestGitCorpusFormatting_Limit_PropertySameFilesAndOrder(t *testing.T) {
	files := make([]zoekt.FileMatch, 20)
	for i := range 20 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("f%02d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte("m\n"), LineNumber: 1}},
		}
	}
	unlimited := formatGitCorpusResultsForTest(files, nil, 0, 0)
	limited := formatGitCorpusResultsForTest(files, nil, 5, 0)
	unlimitedHeaders := extractFileHeaders(unlimited)
	limitedHeaders := extractFileHeaders(limited)
	if len(limitedHeaders) != 5 {
		t.Fatalf("expected 5 headers, got %d", len(limitedHeaders))
	}
	for i, h := range limitedHeaders {
		if h != unlimitedHeaders[i] {
			t.Errorf("header %d: limited=%q, unlimited=%q", i, h, unlimitedHeaders[i])
		}
	}
}

func TestGitCorpusFormatting_Limit_WithUncommittedAndSymbols(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "top.go", Repository: repoUncommitted, Language: "Go", Score: 100,
			LineMatches: []zoekt.LineMatch{{Line: []byte("func Top() {\n"), LineNumber: 1,
				LineFragments: []zoekt.LineFragmentMatch{{SymbolInfo: &zoekt.Symbol{Kind: "function"}}}}}},
		{FileName: "mid.go", Repository: "repo", Language: "Go", Score: 50,
			LineMatches: []zoekt.LineMatch{{Line: []byte("mid\n"), LineNumber: 1}}},
		{FileName: "low.go", Repository: "repo", Language: "Go", Score: 1,
			LineMatches: []zoekt.LineMatch{{Line: []byte("low\n"), LineNumber: 1}}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 2, 0)
	if !strings.Contains(result, "[uncommitted]") {
		t.Error("expected uncommitted tag")
	}
	if !strings.Contains(result, "[function]") {
		t.Error("expected symbol annotation")
	}
	if strings.Contains(result, "low.go") {
		t.Error("low.go should be excluded by limit=2")
	}
}

func TestGitCorpusFormatting_Limit_FileOnlyMatch(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "top.go", Repository: "repo", Language: "Go", Score: 100},
		{FileName: "bot.go", Repository: "repo", Language: "Go", Score: 1},
	}
	result := formatGitCorpusResultsForTest(files, nil, 1, 0)
	if !strings.Contains(result, "top.go") {
		t.Error("expected top.go")
	}
	if strings.Contains(result, "bot.go") {
		t.Error("did not expect bot.go")
	}
}

func TestGitCorpusFormatting_Limit_MixedDirtyClean(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "dirty.go", Repository: "repo", Language: "Go", Score: 100,
			LineMatches: []zoekt.LineMatch{{Line: []byte("stale\n"), LineNumber: 1}}},
		{FileName: "clean1.go", Repository: "repo", Language: "Go", Score: 50,
			LineMatches: []zoekt.LineMatch{{Line: []byte("ok\n"), LineNumber: 1}}},
		{FileName: "clean2.go", Repository: "repo", Language: "Go", Score: 25,
			LineMatches: []zoekt.LineMatch{{Line: []byte("ok2\n"), LineNumber: 1}}},
	}
	dirtyFiles := testDirtyFileSet("dirty.go")
	result := formatGitCorpusResultsForTest(files, dirtyFiles, 1, 0)
	if !strings.Contains(result, "clean1.go") {
		t.Error("expected clean1.go (highest after suppression)")
	}
	if strings.Contains(result, "clean2.go") {
		t.Error("did not expect clean2.go with limit=1")
	}
	if strings.Contains(result, "dirty.go") {
		t.Error("dirty.go should be suppressed")
	}
}

// --- MaxMatches tests ---

func TestGitCorpusFormatting_MaxMatches_Basic(t *testing.T) {
	matches := make([]zoekt.LineMatch, 10)
	for i := range 10 {
		matches[i] = zoekt.LineMatch{Line: []byte(fmt.Sprintf("line%d\n", i)), LineNumber: i + 1}
	}
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10, LineMatches: matches},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 3)
	if !strings.Contains(result, "line0") {
		t.Error("expected first match")
	}
	if !strings.Contains(result, "line2") {
		t.Error("expected third match")
	}
	if strings.Contains(result, "line3") {
		t.Error("did not expect fourth match with maxMatches=3")
	}
}

func TestGitCorpusFormatting_MaxMatches_ExceedsMatches(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("only\n"), LineNumber: 1},
			}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 100)
	if !strings.Contains(result, "only") {
		t.Error("expected match when maxMatches > match count")
	}
}

func TestGitCorpusFormatting_MaxMatches_One(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("first\n"), LineNumber: 1},
				{Line: []byte("second\n"), LineNumber: 10},
				{Line: []byte("third\n"), LineNumber: 20},
			}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 1)
	if !strings.Contains(result, "first") {
		t.Error("expected first match")
	}
	if strings.Contains(result, "second") {
		t.Error("did not expect second match with maxMatches=1")
	}
}

func TestGitCorpusFormatting_MaxMatches_PreservesContext(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("match\n"), LineNumber: 5,
					Before: []byte("before_ctx\n"),
					After:  []byte("after_ctx\n")},
				{Line: []byte("dropped\n"), LineNumber: 20},
			}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 1)
	if !strings.Contains(result, "before_ctx") {
		t.Error("expected before-context on kept match")
	}
	if !strings.Contains(result, "after_ctx") {
		t.Error("expected after-context on kept match")
	}
	if strings.Contains(result, "dropped") {
		t.Error("did not expect dropped match")
	}
}

func TestGitCorpusFormatting_MaxMatches_MultipleFiles(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("a1\n"), LineNumber: 1},
				{Line: []byte("a2\n"), LineNumber: 10},
				{Line: []byte("a3\n"), LineNumber: 20},
			}},
		{FileName: "b.go", Repository: "repo", Language: "Go", Score: 5,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("b1\n"), LineNumber: 1},
				{Line: []byte("b2\n"), LineNumber: 10},
			}},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 2)
	if !strings.Contains(result, "a1") || !strings.Contains(result, "a2") {
		t.Error("expected first 2 matches in a.go")
	}
	if strings.Contains(result, "a3") {
		t.Error("did not expect third match in a.go")
	}
	if !strings.Contains(result, "b1") || !strings.Contains(result, "b2") {
		t.Error("expected both matches in b.go (under limit)")
	}
}

func TestGitCorpusFormatting_MaxMatches_FileOnlyMatch(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10},
	}
	result := formatGitCorpusResultsForTest(files, nil, 0, 3)
	if !strings.Contains(result, "f.go") {
		t.Error("expected file header even with no matches")
	}
}

// --- Combined limit + maxMatches tests ---

func TestGitCorpusFormatting_LimitAndMaxMatches_Combined(t *testing.T) {
	files := make([]zoekt.FileMatch, 10)
	for i := range 10 {
		matches := make([]zoekt.LineMatch, 5)
		for j := range 5 {
			matches[j] = zoekt.LineMatch{
				Line:       []byte(fmt.Sprintf("f%d_m%d\n", i, j)),
				LineNumber: j + 1,
			}
		}
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("f%02d.go", i), Repository: "repo", Language: "Go",
			Score: float64(i), LineMatches: matches,
		}
	}
	result := formatGitCorpusResultsForTest(files, nil, 3, 2)
	headers := extractFileHeaders(result)
	if len(headers) != 3 {
		t.Fatalf("expected 3 file headers, got %d", len(headers))
	}
	if !strings.Contains(result, "f09.go") || !strings.Contains(result, "f08.go") || !strings.Contains(result, "f07.go") {
		t.Error("expected top 3 files by score")
	}
	if strings.Contains(result, "f9_m2") {
		t.Error("did not expect third match in any file")
	}
}

func TestFormatCorpusResults_SameRelativePathDifferentCorpora(t *testing.T) {
	results := []corpusSearchResult{
		{
			corpusID:    corpusID("corpus-a"),
			kind:        corpusKindFolder,
			displayRoot: "/tmp/a",
			file: zoekt.FileMatch{FileName: "same.txt", Language: "Text", Score: 10,
				LineMatches: []zoekt.LineMatch{{Line: []byte("needle a\n"), LineNumber: 1}}},
		},
		{
			corpusID:    corpusID("corpus-b"),
			kind:        corpusKindFolder,
			displayRoot: "/tmp/b",
			file: zoekt.FileMatch{FileName: "same.txt", Language: "Text", Score: 9,
				LineMatches: []zoekt.LineMatch{{Line: []byte("needle b\n"), LineNumber: 1}}},
		},
	}

	out := formatCorpusResultsWithContext(results, nil, 0, 0, showCorpusContext, plainPalette)
	if !strings.Contains(out, "## /tmp/a/same.txt") ||
		!strings.Contains(out, "## /tmp/b/same.txt") {
		t.Fatalf("expected absolute same-path headers, got:\n%s", out)
	}
	if strings.Count(out, "[folder]") != 2 {
		t.Fatalf("expected folder corpus tags, got:\n%s", out)
	}
}

func TestFormatCorpusResults_GitCorpusContext(t *testing.T) {
	results := []corpusSearchResult{
		{
			corpusID:    corpusID("git-a"),
			kind:        corpusKindGit,
			displayRoot: "/tmp/repo-a",
			file: zoekt.FileMatch{FileName: "same.go", Language: "Go", Score: 10,
				LineMatches: []zoekt.LineMatch{{Line: []byte("needle a\n"), LineNumber: 1}}},
		},
		{
			corpusID:    corpusID("git-b"),
			kind:        corpusKindGit,
			displayRoot: "/tmp/repo-b",
			file: zoekt.FileMatch{FileName: "same.go", Language: "Go", Score: 9,
				LineMatches: []zoekt.LineMatch{{Line: []byte("needle b\n"), LineNumber: 1}}},
		},
	}

	out := formatCorpusResultsWithContext(results, nil, 0, 0, showCorpusContext, plainPalette)
	if !strings.Contains(out, "## /tmp/repo-a/same.go") ||
		!strings.Contains(out, "## /tmp/repo-b/same.go") {
		t.Fatalf("expected absolute git paths, got:\n%s", out)
	}
	if strings.Count(out, "[git]") != 2 {
		t.Fatalf("expected git corpus tags, got:\n%s", out)
	}
}

func TestFormatCorpusResults_DirtySuppressionIsCorpusScoped(t *testing.T) {
	results := []corpusSearchResult{
		{
			corpusID:    corpusID("external"),
			kind:        corpusKindFolder,
			displayRoot: "/tmp/external",
			file: zoekt.FileMatch{FileName: "same.go", Language: "Go", Score: 1,
				LineMatches: []zoekt.LineMatch{{Line: []byte("external\n"), LineNumber: 1}}},
		},
	}
	dirtyByCorpus := dirtyFilesByCorpus{
		"git": testDirtyFileSet("same.go"),
	}

	out := formatCorpusResultsWithContext(results, dirtyByCorpus, 0, 0, showCorpusContext, plainPalette)
	if out == "" {
		t.Fatal("external result should not be suppressed by Git dirty state")
	}
	if !strings.Contains(out, "external") {
		t.Fatalf("expected external result, got:\n%s", out)
	}
}

func TestFormatCorpusResults_LimitAppliesAfterMerge(t *testing.T) {
	results := []corpusSearchResult{
		{
			corpusID:    corpusID("low"),
			kind:        corpusKindFolder,
			displayRoot: "/tmp/low",
			file: zoekt.FileMatch{FileName: "low.txt", Language: "Text", Score: 1,
				LineMatches: []zoekt.LineMatch{{Line: []byte("low\n"), LineNumber: 1}}},
		},
		{
			corpusID:    corpusID("high"),
			kind:        corpusKindFolder,
			displayRoot: "/tmp/high",
			file: zoekt.FileMatch{FileName: "high.txt", Language: "Text", Score: 10,
				LineMatches: []zoekt.LineMatch{{Line: []byte("high\n"), LineNumber: 1}}},
		},
	}

	out := formatCorpusResultsWithContext(results, nil, 1, 0, showCorpusContext, plainPalette)
	if !strings.Contains(out, "## /tmp/high/high.txt") || strings.Contains(out, "low.txt") {
		t.Fatalf("expected limit after merged sort, got:\n%s", out)
	}
}
