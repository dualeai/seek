package main

import (
	"testing"

	"github.com/sourcegraph/zoekt"
)

func TestFormatResults_Empty(t *testing.T) {
	result := formatResults(nil, "github.com/org/repo")
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

	result := formatResults(files, "github.com/org/repo")
	expected := "## src/main.go (Go)\n  5 func main() {"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatResults_UncommittedTag(t *testing.T) {
	files := []zoekt.FileMatch{
		{
			FileName:   "lib/utils.py",
			Repository: "uncommitted",
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

	result := formatResults(files, "github.com/org/repo")
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
			Repository: "uncommitted",
			Language:   "Go",
			Score:      5,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("new local changes\n"), LineNumber: 1},
			},
		},
	}

	result := formatResults(files, "github.com/org/repo")
	if !contains(result, "[uncommitted]") {
		t.Error("expected uncommitted version to win deduplication")
	}
	if contains(result, "old content from repo") {
		t.Error("committed version should not appear when uncommitted exists")
	}
	if !contains(result, "new local changes") {
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

	result := formatResults(files, "repo")
	highIdx := indexOf(result, "high.go")
	lowIdx := indexOf(result, "low.go")
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

	result := formatResults(files, "repo")
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

	result := formatResults(files, "repo")
	if !contains(result, "(unknown)") {
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
			Repository: "uncommitted",
			Language:   "Python",
			Score:      5,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("line b\n"), LineNumber: 2},
			},
		},
	}

	result := formatResults(files, "repo")
	if !contains(result, "## a.go (Go)") {
		t.Error("expected a.go header")
	}
	if !contains(result, "## b.py (Python) [uncommitted]") {
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

	result := formatResults(files, "repo")
	if len(result) > 0 && result[len(result)-1] == '\n' {
		t.Error("output must not end with trailing newline (§8 rule 4)")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOfStr(s, substr) >= 0
}

func indexOf(s, substr string) int {
	return indexOfStr(s, substr)
}

func indexOfStr(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
