package main

import (
	"testing"
)

func TestComputeStateHash_Deterministic(t *testing.T) {
	h1 := computeStateHash("abc123", " M file.go\n")
	h2 := computeStateHash("abc123", " M file.go\n")
	if h1 != h2 {
		t.Errorf("expected deterministic hash, got %q and %q", h1, h2)
	}
}

func TestComputeStateHash_DifferentInputs(t *testing.T) {
	h1 := computeStateHash("abc123", " M file.go\n")
	h2 := computeStateHash("def456", " M file.go\n")
	if h1 == h2 {
		t.Error("expected different hashes for different HEAD SHAs")
	}

	h3 := computeStateHash("abc123", " M other.go\n")
	if h1 == h3 {
		t.Error("expected different hashes for different status outputs")
	}
}

func TestComputeStateHash_Length(t *testing.T) {
	h := computeStateHash("abc123", "")
	if len(h) != 32 {
		t.Errorf("expected 32-char hex hash, got %d chars: %q", len(h), h)
	}
}

func TestParseGitStatusFiles_Normal(t *testing.T) {
	input := " M src/main.go\n?? new_file.txt\n A staged.go\n"
	files := parseGitStatusFiles(input)
	assertContains(t, files, "src/main.go")
	assertContains(t, files, "new_file.txt")
	assertContains(t, files, "staged.go")
}

func TestParseGitStatusFiles_Renames(t *testing.T) {
	input := "R  old_name.go -> new_name.go\n"
	files := parseGitStatusFiles(input)
	assertContains(t, files, "new_name.go")
	if containsStr(files, "old_name.go") {
		t.Error("should not include old name from rename")
	}
}

func TestParseGitStatusFiles_Copies(t *testing.T) {
	input := "C  original.go -> copy.go\n"
	files := parseGitStatusFiles(input)
	assertContains(t, files, "copy.go")
}

func TestParseGitStatusFiles_Deletions(t *testing.T) {
	input := " D deleted.go\n"
	files := parseGitStatusFiles(input)
	assertContains(t, files, "deleted.go")
}

func TestParseGitStatusFiles_Empty(t *testing.T) {
	files := parseGitStatusFiles("")
	if len(files) != 0 {
		t.Errorf("expected no files, got %v", files)
	}
}

func TestParseGitStatusFiles_Deduplication(t *testing.T) {
	input := " M file.go\n M file.go\n"
	files := parseGitStatusFiles(input)
	count := 0
	for _, f := range files {
		if f == "file.go" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 occurrence of file.go, got %d", count)
	}
}

func TestIndexParallelism_Bounds(t *testing.T) {
	p := indexParallelism()
	if p < 1 {
		t.Errorf("parallelism should be at least 1, got %d", p)
	}
	if p > 16 {
		t.Errorf("parallelism should be at most 16, got %d", p)
	}
}

func TestStripRemoteURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://github.com/org/repo.git", "github.com/org/repo"},
		{"git@github.com:org/repo.git", "github.com/org/repo"},
		{"ssh://git@github.com/org/repo.git", "github.com/org/repo"},
		{"https://github.com/org/repo", "github.com/org/repo"},
		{"file:///home/user/repo.git", "/home/user/repo"},
	}

	for _, tt := range tests {
		result := stripRemoteURL(tt.input)
		if result != tt.expected {
			t.Errorf("stripRemoteURL(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestUnquoteGitPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"normal_file.go", "normal_file.go"},
		{`"quoted file.go"`, "quoted file.go"},
		{`"path\\with\\backslashes"`, "path\\with\\backslashes"},
		{`"tab\there"`, "tab\there"},
		{`"newline\nhere"`, "newline\nhere"},
	}

	for _, tt := range tests {
		result := unquoteGitPath(tt.input)
		if result != tt.expected {
			t.Errorf("unquoteGitPath(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func assertContains(t *testing.T, slice []string, item string) {
	t.Helper()
	if !containsStr(slice, item) {
		t.Errorf("expected %v to contain %q", slice, item)
	}
}

func containsStr(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
