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
		t.Error("output must not end with trailing newline (§8 rule 4)")
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
