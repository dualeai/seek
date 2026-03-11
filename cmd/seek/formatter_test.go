package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
)

func TestFormatResults_Empty(t *testing.T) {
	result := formatResults(nil)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestFormatResults_BasicFileMatch(t *testing.T) {
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

	result := formatResults(files)
	expected := "## src/main.go (Go)\n  5 func main() {"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_UncommittedTag(t *testing.T) {
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

	result := formatResults(files)
	expected := "## lib/utils.py (Python) [uncommitted]\n  10 def helper():"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_Deduplication_UncommittedWins(t *testing.T) {
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

	result := formatResults(files)
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

func TestFormatResults_ScoreSorting(t *testing.T) {
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

	result := formatResults(files)
	highIdx := strings.Index(result, "high.go")
	lowIdx := strings.Index(result, "low.go")
	if highIdx > lowIdx {
		t.Error("expected high-score file to appear first")
	}
}

func TestFormatResults_SymbolKind(t *testing.T) {
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

	result := formatResults(files)
	expected := "## router.go (Go)\n  15 [function] func CoreRouter() {"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_LanguageFallback(t *testing.T) {
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

	result := formatResults(files)
	if !strings.Contains(result, "(unknown)") {
		t.Error("expected language fallback to 'unknown'")
	}
}

func TestFormatResults_MultiFile(t *testing.T) {
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

	result := formatResults(files)
	if !strings.Contains(result, "## a.go (Go)") {
		t.Error("expected a.go header")
	}
	if !strings.Contains(result, "## b.py (Python) [uncommitted]") {
		t.Error("expected b.py header with uncommitted tag")
	}
}

func TestFormatResults_NoTrailingNewline(t *testing.T) {
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

	result := formatResults(files)
	if len(result) > 0 && result[len(result)-1] == '\n' {
		t.Error("output must not end with trailing newline")
	}
}

func TestFormatResults_ZeroLineMatches(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "empty.go",
			Repository: "repo",
			Language:   "Go",
			Score:      1,
		},
	}

	result := formatResults(files)
	expected := "## empty.go (Go)"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestFormatResults_ManyFiles_SortedByScore(t *testing.T) {
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

	result := formatResults(files)

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

func TestDeduplicateFiles_OrderIndependence(t *testing.T) {
	committed := zoekt.FileMatch{
		FileName:   "app.go",
		Repository: "repo",
		Language:   "Go",
		Score:      10,
		LineMatches: []zoekt.LineMatch{
			{Line: []byte("committed\n"), LineNumber: 1},
		},
	}
	uncommitted := zoekt.FileMatch{
		FileName:   "app.go",
		Repository: repoUncommitted,
		Language:   "Go",
		Score:      5,
		LineMatches: []zoekt.LineMatch{
			{Line: []byte("uncommitted\n"), LineNumber: 1},
		},
	}

	// committed first
	r1 := deduplicateFiles([]zoekt.FileMatch{committed, uncommitted})
	// uncommitted first
	r2 := deduplicateFiles([]zoekt.FileMatch{uncommitted, committed})

	if len(r1) != 1 || len(r2) != 1 {
		t.Fatalf("expected 1 result each, got %d and %d", len(r1), len(r2))
	}
	if r1[0].Repository != repoUncommitted {
		t.Error("committed-first: expected uncommitted to win")
	}
	if r2[0].Repository != repoUncommitted {
		t.Error("uncommitted-first: expected uncommitted to win")
	}
}

func TestDeduplicateFiles_CommittedOnly(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Score: 10},
	}
	result := deduplicateFiles(files)
	if len(result) != 1 || result[0].Repository != "repo" {
		t.Error("single committed entry should pass through unchanged")
	}
}

func TestDeduplicateFiles_UncommittedOnly(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: repoUncommitted, Score: 5},
	}
	result := deduplicateFiles(files)
	if len(result) != 1 || result[0].Repository != repoUncommitted {
		t.Error("single uncommitted entry should pass through unchanged")
	}
}

func TestFormatResults_ScoreTiebreaking_Stable(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "b.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("b\n"), LineNumber: 1}}},
		{FileName: "a.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("a\n"), LineNumber: 1}}},
	}
	result := formatResults(files)
	aIdx := strings.Index(result, "a.go")
	bIdx := strings.Index(result, "b.go")
	if aIdx > bIdx {
		t.Error("expected alphabetical tiebreaking for equal scores")
	}
}

func TestDeduplicateFiles_TwoCommittedSameFile(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("first\n"), LineNumber: 1}}},
		{FileName: "a.go", Repository: "repo", Score: 5,
			LineMatches: []zoekt.LineMatch{{Line: []byte("second\n"), LineNumber: 2}}},
	}
	result := deduplicateFiles(files)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	// First-seen wins
	if !strings.Contains(string(result[0].LineMatches[0].Line), "first") {
		t.Error("expected first-seen committed entry to win")
	}
}

func TestFormatResults_ContextLines_BeforeAndAfter(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## server.go (Go)",
		"  13 ",
		"  14 // handleRequest processes incoming HTTP requests.",
		"  15 [function] func handleRequest(w http.ResponseWriter, r *http.Request) {",
		`  16     ctx := r.Context()`,
		`  17     log.Info("handling request")`,
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_NoContext(t *testing.T) {
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

	result := formatResults(files)
	expected := "## main.go (Go)\n  5 func main() {"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_OverlappingContext(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"  10 first match",
		"  11 line eleven",
		"  12 second match",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_NonContiguousRegions(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"  5 first match",
		"  6 after first",
		"",
		"  49 before second",
		"  50 second match",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_AdjacentMatches(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"  10 line one",
		"  11 line two match",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_ThreeContextLines(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## main.go (Go)",
		"  17 ctx line 17",
		"  18 ctx line 18",
		"  19 ctx line 19",
		"  20     target line",
		"  21 ctx line 21",
		"  22 ctx line 22",
		"  23 ctx line 23",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_MatchOnLine1(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## main.go (Go)",
		"  1 package main",
		"  2 import \"fmt\"",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_MatchOnLine1_ExcessBefore(t *testing.T) {
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

	result := formatResults(files)
	// Before lines should be silently dropped — no negative line numbers
	if strings.Contains(result, "phantom") {
		t.Errorf("expected excess Before lines to be dropped, got:\n%s", result)
	}
	if strings.Contains(result, "  0 ") || strings.Contains(result, "  -") {
		t.Errorf("expected no zero or negative line numbers, got:\n%s", result)
	}
	if !strings.Contains(result, "  1 package main") {
		t.Errorf("expected match line to still be present, got:\n%s", result)
	}
}

func TestFormatResults_ContextLines_MatchOnLine2_PartialBefore(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## main.go (Go)",
		"  1 package main",
		"  2 import \"fmt\"",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_ThreeConsecutiveMatches(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"  10 match A",
		"  11 between AB",
		"  12 match B",
		"  13 between BC",
		"  14 match C",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_OnlyBefore(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"  99 penultimate",
		"  100 last line",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_OnlyAfter(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"  1 first line",
		"  2 second line",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_EmptyLinesInContext(t *testing.T) {
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"  2 import \"fmt\"",
		"  3 ",
		"  4 ",
		"  5 func main() {",
		"  6 ",
		"  7     fmt.Println()",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_EmptyByteSlice(t *testing.T) {
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

	result := formatResults(files)
	expected := "## app.go (Go)\n  5 match"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_ContextLines_BeforeNoTrailingNewline(t *testing.T) {
	// Before bytes without trailing newline — splitContextLines should still work.
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

	result := formatResults(files)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"  1 line one",
		"  2 line two",
		"  3 match",
	}, "\n")
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_FileOnlyMatch_LineNumberZero(t *testing.T) {
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
	result := formatResults(files)
	if !strings.Contains(result, "## path/to/file.go (Go)") {
		t.Errorf("expected file header, got: %s", result)
	}
}

func TestDeduplicateFiles_EmptyInput(t *testing.T) {
	result := deduplicateFiles(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 results, got %d", len(result))
	}
	result2 := deduplicateFiles([]zoekt.FileMatch{})
	if len(result2) != 0 {
		t.Errorf("expected 0 results for empty slice, got %d", len(result2))
	}
}

func TestDeduplicateFiles_ManyDuplicates(t *testing.T) {
	files := make([]zoekt.FileMatch, 200)
	for i := range 100 {
		files[i] = zoekt.FileMatch{FileName: "dup.go", Repository: "repo", Score: float64(i)}
	}
	for i := range 100 {
		files[100+i] = zoekt.FileMatch{FileName: "dup.go", Repository: repoUncommitted, Score: float64(i)}
	}
	result := deduplicateFiles(files)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Repository != repoUncommitted {
		t.Error("expected uncommitted to win")
	}
}

func TestDeduplicateFiles_DifferentFiles(t *testing.T) {
	files := make([]zoekt.FileMatch, 10)
	for i := range 10 {
		files[i] = zoekt.FileMatch{
			FileName:   fmt.Sprintf("file_%d.go", i),
			Repository: "repo",
			Score:      float64(i),
		}
	}
	result := deduplicateFiles(files)
	if len(result) != 10 {
		t.Errorf("expected 10 results (no duplicates), got %d", len(result))
	}
}

func TestFormatResults_VeryLongFileName(t *testing.T) {
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
	result := formatResults(files)
	if !strings.Contains(result, longName) {
		t.Error("expected long filename to appear in output without truncation")
	}
}

func TestFormatResults_VeryLongLine(t *testing.T) {
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
	result := formatResults(files)
	if !strings.Contains(result, strings.Repeat("x", 10000)) {
		t.Error("expected long line to appear without truncation")
	}
}

func TestFormatResults_SpecialCharsInLine(t *testing.T) {
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
	result := formatResults(files)
	if !strings.Contains(result, "tab\there unicode: 日本語 emoji: 🎉") {
		t.Errorf("expected special chars preserved, got: %s", result)
	}
}

func TestFormatResults_LineNumber_MaxUint32(t *testing.T) {
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
	result := formatResults(files)
	if !strings.Contains(result, "match") {
		t.Error("expected match to appear in output")
	}
}

func TestSplitContextLines(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected []string
	}{
		{"nil", nil, nil},
		{"empty", []byte{}, nil},
		{"single newline", []byte("\n"), []string{""}},
		{"single line with newline", []byte("hello\n"), []string{"hello"}},
		{"single line no newline", []byte("hello"), []string{"hello"}},
		{"two lines", []byte("a\nb\n"), []string{"a", "b"}},
		{"two lines no trailing newline", []byte("a\nb"), []string{"a", "b"}},
		{"empty lines", []byte("\n\n\n"), []string{"", "", ""}},
		{"mixed empty", []byte("a\n\nb\n"), []string{"a", "", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitContextLines(tt.input)
			if tt.expected == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %d lines, got %d: %v", len(tt.expected), len(got), got)
			}
			for i := range tt.expected {
				if got[i] != tt.expected[i] {
					t.Errorf("line %d: expected %q, got %q", i, tt.expected[i], got[i])
				}
			}
		})
	}
}
