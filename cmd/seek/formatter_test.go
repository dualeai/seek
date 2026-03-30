package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
)

func TestFormatResults_Empty(t *testing.T) {
	result := formatResults(nil, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)

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
	r1 := deduplicateFiles([]zoekt.FileMatch{committed, uncommitted}, nil)
	// uncommitted first
	r2 := deduplicateFiles([]zoekt.FileMatch{uncommitted, committed}, nil)

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
	result := deduplicateFiles(files, nil)
	if len(result) != 1 || result[0].Repository != "repo" {
		t.Error("single committed entry should pass through unchanged")
	}
}

func TestDeduplicateFiles_UncommittedOnly(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: repoUncommitted, Score: 5},
	}
	result := deduplicateFiles(files, nil)
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
	result := formatResults(files, nil, 0, 0)
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
	result := deduplicateFiles(files, nil)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"   5 first match",
		"   6 after first",
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
	expected := strings.Join([]string{
		"## app.go (Go)",
		"   99 penultimate",
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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

	result := formatResults(files, nil, 0, 0)
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
	result := formatResults(files, nil, 0, 0)
	if !strings.Contains(result, "## path/to/file.go (Go)") {
		t.Errorf("expected file header, got: %s", result)
	}
}

func TestDeduplicateFiles_StaleCommittedSuppressed(t *testing.T) {
	// Bug: a file is dirty (edited locally) but the query only matches the
	// committed (HEAD) version — the local edit changed the matched content.
	// Example: user renames _MIGRATIONS to _get_migrations(), then searches
	// for "_MIGRATIONS". The committed shard still has _MIGRATIONS but the
	// uncommitted shard does not. The stale committed result must be suppressed.
	files := []zoekt.FileMatch{
		{
			FileName:   "repositories/sqlite.py",
			Repository: "github.com/org/repo",
			Language:   "Python",
			Score:      10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("_MIGRATIONS: list[Migration] = [\n"), LineNumber: 60},
			},
		},
	}
	dirtyFiles := map[string]bool{"repositories/sqlite.py": true}
	result := deduplicateFiles(files, dirtyFiles)
	if len(result) != 0 {
		t.Errorf("expected dirty file's stale committed result to be suppressed, got %d results", len(result))
	}
}

func TestDeduplicateFiles_DirtyFileUncommittedWins(t *testing.T) {
	// When both shards match a dirty file, uncommitted still wins (existing behavior).
	files := []zoekt.FileMatch{
		{FileName: "app.go", Repository: "repo", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("old\n"), LineNumber: 1}}},
		{FileName: "app.go", Repository: repoUncommitted, Score: 5,
			LineMatches: []zoekt.LineMatch{{Line: []byte("new\n"), LineNumber: 1}}},
	}
	dirtyFiles := map[string]bool{"app.go": true}
	result := deduplicateFiles(files, dirtyFiles)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Repository != repoUncommitted {
		t.Error("expected uncommitted to win")
	}
}

func TestDeduplicateFiles_CleanFileCommittedKept(t *testing.T) {
	// A committed-only match for a CLEAN file is fine — no suppression.
	files := []zoekt.FileMatch{
		{FileName: "clean.go", Repository: "repo", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("content\n"), LineNumber: 1}}},
	}
	dirtyFiles := map[string]bool{"other.go": true}
	result := deduplicateFiles(files, dirtyFiles)
	if len(result) != 1 {
		t.Errorf("expected clean file's committed result to be kept, got %d", len(result))
	}
}

func TestDeduplicateFiles_NilDirtyFiles(t *testing.T) {
	// nil dirtyFiles — all committed results pass through (backward compat).
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Score: 10},
	}
	result := deduplicateFiles(files, nil)
	if len(result) != 1 {
		t.Errorf("expected 1 result with nil dirtyFiles, got %d", len(result))
	}
}

func TestFormatResults_StaleDirtyFileSuppressed(t *testing.T) {
	// End-to-end: formatResults should produce no output when the only match
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
	dirtyFiles := map[string]bool{"sqlite.py": true}
	result := formatResults(files, dirtyFiles, 0, 0)
	if result != "" {
		t.Errorf("expected empty output for stale dirty file, got:\n%s", result)
	}
}

func TestDeduplicateFiles_DeletedDirtyFile(t *testing.T) {
	// A deleted file appears in git status (dirty). The committed shard may
	// still match old content. Must be suppressed — file no longer exists.
	files := []zoekt.FileMatch{
		{FileName: "removed.go", Repository: "repo", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("old code\n"), LineNumber: 5}}},
	}
	dirtyFiles := map[string]bool{"removed.go": true}
	result := deduplicateFiles(files, dirtyFiles)
	if len(result) != 0 {
		t.Errorf("expected deleted dirty file to be suppressed, got %d results", len(result))
	}
}

func TestDeduplicateFiles_MixedDirtyAndClean(t *testing.T) {
	// Mix of dirty and clean files: only dirty committed-only results suppressed.
	files := []zoekt.FileMatch{
		{FileName: "dirty.go", Repository: "repo", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("stale\n"), LineNumber: 1}}},
		{FileName: "clean.go", Repository: "repo", Score: 8,
			LineMatches: []zoekt.LineMatch{{Line: []byte("valid\n"), LineNumber: 1}}},
		{FileName: "also_dirty.go", Repository: repoUncommitted, Score: 6,
			LineMatches: []zoekt.LineMatch{{Line: []byte("fresh\n"), LineNumber: 1}}},
	}
	dirtyFiles := map[string]bool{"dirty.go": true, "also_dirty.go": true}
	result := deduplicateFiles(files, dirtyFiles)
	if len(result) != 2 {
		t.Fatalf("expected 2 results (clean + uncommitted), got %d", len(result))
	}
	for _, fm := range result {
		if fm.FileName == "dirty.go" {
			t.Error("stale committed result for dirty.go should have been suppressed")
		}
	}
}

func TestDeduplicateFiles_EmptyDirtyFilesMap(t *testing.T) {
	// Empty (non-nil) dirtyFiles map — no suppression, same as nil.
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Score: 10},
	}
	result := deduplicateFiles(files, map[string]bool{})
	if len(result) != 1 {
		t.Errorf("expected 1 result with empty dirtyFiles, got %d", len(result))
	}
}

func TestDeduplicateFiles_UncommittedOnlyDirtyFile(t *testing.T) {
	// New untracked file — only in uncommitted shard, also in dirtyFiles.
	// Should pass through (it IS the uncommitted entry).
	files := []zoekt.FileMatch{
		{FileName: "new_file.go", Repository: repoUncommitted, Score: 5,
			LineMatches: []zoekt.LineMatch{{Line: []byte("new\n"), LineNumber: 1}}},
	}
	dirtyFiles := map[string]bool{"new_file.go": true}
	result := deduplicateFiles(files, dirtyFiles)
	if len(result) != 1 {
		t.Errorf("expected uncommitted-only dirty file to pass through, got %d", len(result))
	}
	if result[0].Repository != repoUncommitted {
		t.Error("expected uncommitted entry")
	}
}

func TestDeduplicateFiles_AllSuppressed(t *testing.T) {
	// All results are stale committed for dirty files — empty output.
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Score: 10},
		{FileName: "b.go", Repository: "repo", Score: 8},
		{FileName: "c.go", Repository: "repo", Score: 6},
	}
	dirtyFiles := map[string]bool{"a.go": true, "b.go": true, "c.go": true}
	result := deduplicateFiles(files, dirtyFiles)
	if len(result) != 0 {
		t.Errorf("expected all stale results suppressed, got %d", len(result))
	}
}

func TestFormatResults_AllSuppressedReturnsEmpty(t *testing.T) {
	// When all results are suppressed, formatResults returns "" so the caller
	// can detect "no valid results" and return errNoMatch (exit code 1).
	files := []zoekt.FileMatch{
		{FileName: "a.py", Repository: "repo", Language: "Python", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("old\n"), LineNumber: 1}}},
		{FileName: "b.py", Repository: "repo", Language: "Python", Score: 5,
			LineMatches: []zoekt.LineMatch{{Line: []byte("stale\n"), LineNumber: 2}}},
	}
	dirtyFiles := map[string]bool{"a.py": true, "b.py": true}
	result := formatResults(files, dirtyFiles, 0, 0)
	if result != "" {
		t.Errorf("expected empty string when all results suppressed, got:\n%s", result)
	}
}

func TestFormatResults_PartialSuppression(t *testing.T) {
	// Some results suppressed, some kept.
	files := []zoekt.FileMatch{
		{FileName: "stale.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("old\n"), LineNumber: 1}}},
		{FileName: "valid.go", Repository: "repo", Language: "Go", Score: 5,
			LineMatches: []zoekt.LineMatch{{Line: []byte("good\n"), LineNumber: 1}}},
	}
	dirtyFiles := map[string]bool{"stale.go": true}
	result := formatResults(files, dirtyFiles, 0, 0)
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

func TestDeduplicateFiles_BothShardsMatchDirtyFile_OrderIndependence(t *testing.T) {
	// When both shards match a dirty file, uncommitted wins regardless of order.
	committed := zoekt.FileMatch{FileName: "f.go", Repository: "repo", Score: 10,
		LineMatches: []zoekt.LineMatch{{Line: []byte("old\n"), LineNumber: 1}}}
	uncommitted := zoekt.FileMatch{FileName: "f.go", Repository: repoUncommitted, Score: 5,
		LineMatches: []zoekt.LineMatch{{Line: []byte("new\n"), LineNumber: 1}}}
	dirtyFiles := map[string]bool{"f.go": true}

	r1 := deduplicateFiles([]zoekt.FileMatch{committed, uncommitted}, dirtyFiles)
	r2 := deduplicateFiles([]zoekt.FileMatch{uncommitted, committed}, dirtyFiles)

	for _, r := range [][]zoekt.FileMatch{r1, r2} {
		if len(r) != 1 {
			t.Fatalf("expected 1 result, got %d", len(r))
		}
		if r[0].Repository != repoUncommitted {
			t.Error("expected uncommitted to win for dirty file")
		}
	}
}

func TestDeduplicateFiles_EmptyInput(t *testing.T) {
	result := deduplicateFiles(nil, nil)
	if len(result) != 0 {
		t.Errorf("expected 0 results, got %d", len(result))
	}
	result2 := deduplicateFiles([]zoekt.FileMatch{}, nil)
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
	result := deduplicateFiles(files, nil)
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
	result := deduplicateFiles(files, nil)
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
	result := formatResults(files, nil, 0, 0)
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
	result := formatResults(files, nil, 0, 0)
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
	result := formatResults(files, nil, 0, 0)
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
	result := formatResults(files, nil, 0, 0)
	if !strings.Contains(result, "match") {
		t.Error("expected match to appear in output")
	}
}

func TestWriteLineNum(t *testing.T) {
	tests := []struct {
		lineNum int
		width   int
		want    string
	}{
		{1, 1, "1"},
		{1, 3, "  1"},
		{10, 3, " 10"},
		{100, 3, "100"},
		{100, 2, "100"}, // overflow: no truncation, no panic
		{0, 1, "0"},
		{1, 0, "1"}, // zero width: no padding
	}
	for _, tt := range tests {
		var sb strings.Builder
		writeLineNum(&sb, tt.lineNum, tt.width)
		if got := sb.String(); got != tt.want {
			t.Errorf("writeLineNum(%d, %d) = %q, want %q", tt.lineNum, tt.width, got, tt.want)
		}
	}
}

func TestMaxLineNumWidth(t *testing.T) {
	tests := []struct {
		name  string
		files []zoekt.FileMatch
		want  int
	}{
		{"nil", nil, 1},
		{"empty", []zoekt.FileMatch{}, 1},
		{"single digit", []zoekt.FileMatch{
			{LineMatches: []zoekt.LineMatch{{LineNumber: 5}}},
		}, 1},
		{"boundary 9 to 10 via after-context", []zoekt.FileMatch{
			{LineMatches: []zoekt.LineMatch{{LineNumber: 9, After: []byte("line10\n")}}},
		}, 2},
		{"triple digit", []zoekt.FileMatch{
			{LineMatches: []zoekt.LineMatch{{LineNumber: 99, After: []byte("line100\nline101\n")}}},
		}, 3},
		{"zero line number", []zoekt.FileMatch{
			{LineMatches: []zoekt.LineMatch{{LineNumber: 0}}},
		}, 1},
		{"cross-file max", []zoekt.FileMatch{
			{LineMatches: []zoekt.LineMatch{{LineNumber: 5}}},
			{LineMatches: []zoekt.LineMatch{{LineNumber: 100}}},
		}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maxLineNumWidth(tt.files); got != tt.want {
				t.Errorf("maxLineNumWidth() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFormatResults_GlobalAlignment_CrossFile(t *testing.T) {
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
	result := formatResults(files, nil, 0, 0)
	// Both should be padded to width 3 (len("100") == 3)
	if !strings.Contains(result, "    5 near top") {
		t.Errorf("expected shallow match padded to width 3, got:\n%s", result)
	}
	if !strings.Contains(result, "  100 far down") {
		t.Errorf("expected deep match at width 3, got:\n%s", result)
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

func TestFormatResults_Limit_TopN(t *testing.T) {
	files := make([]zoekt.FileMatch, 10)
	for i := range 10 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("file_%02d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte("match\n"), LineNumber: 1}},
		}
	}
	result := formatResults(files, nil, 3, 0)
	for _, want := range []string{"file_09.go", "file_08.go", "file_07.go"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %s in limited output", want)
		}
	}
	for _, noWant := range []string{"file_06.go", "file_05.go", "file_00.go"} {
		if strings.Contains(result, noWant) {
			t.Errorf("did not expect %s in limited output", noWant)
		}
	}
}

func TestFormatResults_NonPositiveLimitsAreUnlimited(t *testing.T) {
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
			result := formatResults(files, nil, tc.limit, tc.maxMatches)
			for i := range 5 {
				if !strings.Contains(result, fmt.Sprintf("f%d.go", i)) {
					t.Errorf("expected f%d.go in output", i)
				}
			}
		})
	}
}

func TestFormatResults_Limit_ExceedsResults(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "only.go", Repository: "repo", Language: "Go", Score: 1,
			LineMatches: []zoekt.LineMatch{{Line: []byte("m\n"), LineNumber: 1}}},
	}
	result := formatResults(files, nil, 100, 0)
	if !strings.Contains(result, "only.go") {
		t.Error("expected only.go in output")
	}
}

func TestFormatResults_Limit_EqualsResults(t *testing.T) {
	files := make([]zoekt.FileMatch, 3)
	for i := range 3 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("f%d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte("m\n"), LineNumber: 1}},
		}
	}
	result := formatResults(files, nil, 3, 0)
	for i := range 3 {
		if !strings.Contains(result, fmt.Sprintf("f%d.go", i)) {
			t.Errorf("expected f%d.go in output when limit == count", i)
		}
	}
}

func TestFormatResults_Limit_EmptyInput(t *testing.T) {
	if result := formatResults(nil, nil, 5, 0); result != "" {
		t.Errorf("expected empty output, got %q", result)
	}
}

func TestFormatResults_Limit_DedupReducesBelowLimit(t *testing.T) {
	files := make([]zoekt.FileMatch, 10)
	for i := range 5 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("f%d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte("committed\n"), LineNumber: 1}},
		}
		files[5+i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("f%d.go", i), Repository: repoUncommitted, Language: "Go",
			Score:       float64(i + 1),
			LineMatches: []zoekt.LineMatch{{Line: []byte("uncommitted\n"), LineNumber: 1}},
		}
	}
	result := formatResults(files, nil, 8, 0)
	headers := extractFileHeaders(result)
	if len(headers) != 5 {
		t.Errorf("expected 5 file headers after dedup (limit=8), got %d", len(headers))
	}
}

func TestFormatResults_Limit_AllSuppressedByDirty(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "a.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("old\n"), LineNumber: 1}}},
	}
	dirtyFiles := map[string]bool{"a.go": true}
	result := formatResults(files, dirtyFiles, 5, 0)
	if result != "" {
		t.Errorf("expected empty output, got %q", result)
	}
}

func TestFormatResults_Limit_TiedScores(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "b.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("b\n"), LineNumber: 1}}},
		{FileName: "a.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("a\n"), LineNumber: 1}}},
		{FileName: "c.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{{Line: []byte("c\n"), LineNumber: 1}}},
	}
	result := formatResults(files, nil, 2, 0)
	if !strings.Contains(result, "a.go") || !strings.Contains(result, "b.go") {
		t.Errorf("expected a.go and b.go in output, got:\n%s", result)
	}
	if strings.Contains(result, "c.go") {
		t.Error("c.go should be excluded by limit=2")
	}
}

func TestFormatResults_Limit_PropertySameFilesAndOrder(t *testing.T) {
	files := make([]zoekt.FileMatch, 20)
	for i := range 20 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("f%02d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte("m\n"), LineNumber: 1}},
		}
	}
	unlimited := formatResults(files, nil, 0, 0)
	limited := formatResults(files, nil, 5, 0)
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

func TestFormatResults_Limit_WithUncommittedAndSymbols(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "top.go", Repository: repoUncommitted, Language: "Go", Score: 100,
			LineMatches: []zoekt.LineMatch{{Line: []byte("func Top() {\n"), LineNumber: 1,
				LineFragments: []zoekt.LineFragmentMatch{{SymbolInfo: &zoekt.Symbol{Kind: "function"}}}}}},
		{FileName: "mid.go", Repository: "repo", Language: "Go", Score: 50,
			LineMatches: []zoekt.LineMatch{{Line: []byte("mid\n"), LineNumber: 1}}},
		{FileName: "low.go", Repository: "repo", Language: "Go", Score: 1,
			LineMatches: []zoekt.LineMatch{{Line: []byte("low\n"), LineNumber: 1}}},
	}
	result := formatResults(files, nil, 2, 0)
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

func TestFormatResults_Limit_FileOnlyMatch(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "top.go", Repository: "repo", Language: "Go", Score: 100},
		{FileName: "bot.go", Repository: "repo", Language: "Go", Score: 1},
	}
	result := formatResults(files, nil, 1, 0)
	if !strings.Contains(result, "top.go") {
		t.Error("expected top.go")
	}
	if strings.Contains(result, "bot.go") {
		t.Error("did not expect bot.go")
	}
}

func TestFormatResults_Limit_MixedDirtyClean(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "dirty.go", Repository: "repo", Language: "Go", Score: 100,
			LineMatches: []zoekt.LineMatch{{Line: []byte("stale\n"), LineNumber: 1}}},
		{FileName: "clean1.go", Repository: "repo", Language: "Go", Score: 50,
			LineMatches: []zoekt.LineMatch{{Line: []byte("ok\n"), LineNumber: 1}}},
		{FileName: "clean2.go", Repository: "repo", Language: "Go", Score: 25,
			LineMatches: []zoekt.LineMatch{{Line: []byte("ok2\n"), LineNumber: 1}}},
	}
	dirtyFiles := map[string]bool{"dirty.go": true}
	result := formatResults(files, dirtyFiles, 1, 0)
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

func TestFormatResults_MaxMatches_Basic(t *testing.T) {
	matches := make([]zoekt.LineMatch, 10)
	for i := range 10 {
		matches[i] = zoekt.LineMatch{Line: []byte(fmt.Sprintf("line%d\n", i)), LineNumber: i + 1}
	}
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10, LineMatches: matches},
	}
	result := formatResults(files, nil, 0, 3)
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

func TestFormatResults_MaxMatches_ExceedsMatches(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("only\n"), LineNumber: 1},
			}},
	}
	result := formatResults(files, nil, 0, 100)
	if !strings.Contains(result, "only") {
		t.Error("expected match when maxMatches > match count")
	}
}

func TestFormatResults_MaxMatches_One(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("first\n"), LineNumber: 1},
				{Line: []byte("second\n"), LineNumber: 10},
				{Line: []byte("third\n"), LineNumber: 20},
			}},
	}
	result := formatResults(files, nil, 0, 1)
	if !strings.Contains(result, "first") {
		t.Error("expected first match")
	}
	if strings.Contains(result, "second") {
		t.Error("did not expect second match with maxMatches=1")
	}
}

func TestFormatResults_MaxMatches_PreservesContext(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("match\n"), LineNumber: 5,
					Before: []byte("before_ctx\n"),
					After:  []byte("after_ctx\n")},
				{Line: []byte("dropped\n"), LineNumber: 20},
			}},
	}
	result := formatResults(files, nil, 0, 1)
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

func TestFormatResults_MaxMatches_MultipleFiles(t *testing.T) {
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
	result := formatResults(files, nil, 0, 2)
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

func TestFormatResults_MaxMatches_FileOnlyMatch(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "f.go", Repository: "repo", Language: "Go", Score: 10},
	}
	result := formatResults(files, nil, 0, 3)
	if !strings.Contains(result, "f.go") {
		t.Error("expected file header even with no matches")
	}
}

// --- Combined limit + maxMatches tests ---

func TestFormatResults_LimitAndMaxMatches_Combined(t *testing.T) {
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
	result := formatResults(files, nil, 3, 2)
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
