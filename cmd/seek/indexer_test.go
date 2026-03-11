package main

import (
	"testing"
)

func TestComputeStateHash_Deterministic(t *testing.T) {
	h1 := computeStateHash("# branch.oid abc123\n")
	h2 := computeStateHash("# branch.oid abc123\n")
	if h1 != h2 {
		t.Errorf("expected deterministic hash, got %q and %q", h1, h2)
	}
}

func TestComputeStateHash_DifferentInputs(t *testing.T) {
	h1 := computeStateHash("# branch.oid abc123\n")
	h2 := computeStateHash("# branch.oid def456\n")
	if h1 == h2 {
		t.Error("expected different hashes for different HEAD SHAs")
	}

	h3 := computeStateHash("# branch.oid abc123\n1 .M N... 100644 100644 100644 abc def file.go\x00")
	if h1 == h3 {
		t.Error("expected different hashes for different status outputs")
	}
}

func TestComputeStateHash_Length(t *testing.T) {
	h := computeStateHash("# branch.oid abc123\n")
	if len(h) != 16 {
		t.Errorf("expected 16-char hex hash (xxHash64), got %d chars: %q", len(h), h)
	}
}

func TestComputeStateHash_EmptyInput(t *testing.T) {
	h := computeStateHash("")
	if len(h) != 16 {
		t.Errorf("expected 16-char hex hash (xxHash64) for empty input, got %d chars: %q", len(h), h)
	}
	h2 := computeStateHash("")
	if h != h2 {
		t.Errorf("expected deterministic hash for empty input, got %q and %q", h, h2)
	}
}

func TestParseGitStatusV2_BranchOid(t *testing.T) {
	raw := "# branch.oid abc123def456\n# branch.head main\n"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "abc123def456" {
		t.Errorf("expected HeadSHA %q, got %q", "abc123def456", state.HeadSHA)
	}
	if state.RawOutput != raw {
		t.Error("expected RawOutput to match input")
	}
}

func TestParseGitStatusV2_NoHead(t *testing.T) {
	raw := "# branch.oid (initial)\n# branch.head main\n"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "(initial)" {
		t.Errorf("expected HeadSHA %q, got %q", "(initial)", state.HeadSHA)
	}
}

func TestParseGitStatusV2_Empty(t *testing.T) {
	state := parseGitStatusV2("")
	if state.HeadSHA != "no-head" {
		t.Errorf("expected no-head, got %q", state.HeadSHA)
	}
	if len(state.Files) != 0 {
		t.Errorf("expected no files, got %v", state.Files)
	}
}

func TestParseGitStatusV2_Changed(t *testing.T) {
	raw := "# branch.oid abc123\n" +
		"1 .M N... 100644 100644 100644 abc123 def456 src/main.go\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "src/main.go")
}

func TestParseGitStatusV2_Untracked(t *testing.T) {
	raw := "# branch.oid abc123\n" +
		"? new_file.txt\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "new_file.txt")
}

func TestParseGitStatusV2_Unmerged(t *testing.T) {
	raw := "# branch.oid abc123\n" +
		"u UU N... 100644 100644 100644 100644 abc123 def456 ghi789 conflict.go\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "conflict.go")
}

func TestParseGitStatusV2_Mixed(t *testing.T) {
	raw := "# branch.oid abc123\n# branch.head develop\n" +
		"1 .M N... 100644 100644 100644 abc123 def456 modified.go\x00" +
		"? untracked.txt\x00"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "abc123" {
		t.Errorf("expected HeadSHA %q, got %q", "abc123", state.HeadSHA)
	}
	if len(state.Files) != 2 {
		t.Errorf("expected 2 files, got %d: %v", len(state.Files), state.Files)
	}
	assertContains(t, state.Files, "modified.go")
	assertContains(t, state.Files, "untracked.txt")
}

func TestParseGitStatusV2_Deduplication(t *testing.T) {
	raw := "# branch.oid abc123\n" +
		"1 .M N... 100644 100644 100644 abc123 def456 file.go\x00" +
		"1 M. N... 100644 100644 100644 abc123 def456 file.go\x00"
	state := parseGitStatusV2(raw)
	count := 0
	for _, f := range state.Files {
		if f == "file.go" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 occurrence of file.go, got %d", count)
	}
}

func TestParseGitStatusV2_SpecialCharsInPath(t *testing.T) {
	raw := "# branch.oid abc123\n" +
		"? path with spaces/file name.go\x00" +
		"? path/with -> arrow.go\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "path with spaces/file name.go")
	assertContains(t, state.Files, "path/with -> arrow.go")
}

func TestParseGitStatusV2_HeadersOnly(t *testing.T) {
	raw := "# branch.oid abc123\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -0\n"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "abc123" {
		t.Errorf("expected HeadSHA %q, got %q", "abc123", state.HeadSHA)
	}
	if len(state.Files) != 0 {
		t.Errorf("expected no files, got %v", state.Files)
	}
}

func TestParseGitStatusV2_BlankLineBetweenHeadersAndEntries(t *testing.T) {
	// Some git versions or wrappers may insert extra newlines between headers and entries.
	// The parser must still find the NUL-terminated entries.
	raw := "# branch.oid abc123\n# branch.head main\n\n\n" +
		"1 .M N... 100644 100644 100644 abc123 def456 file.go\x00" +
		"? new.txt\x00"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "abc123" {
		t.Errorf("expected HeadSHA %q, got %q", "abc123", state.HeadSHA)
	}
	if len(state.Files) != 2 {
		t.Errorf("expected 2 files, got %d: %v", len(state.Files), state.Files)
	}
	assertContains(t, state.Files, "file.go")
	assertContains(t, state.Files, "new.txt")
}

func TestExtractV2Path(t *testing.T) {
	tests := []struct {
		entry      string
		skipFields int
		expected   string
	}{
		{"1 .M N... 100644 100644 100644 abc123 def456 src/main.go", 8, "src/main.go"},
		{"u UU N... 100644 100644 100644 100644 abc123 def456 ghi789 conflict.go", 10, "conflict.go"},
		{"short", 5, ""},
		{"1 .M N... 100644 100644 100644 abc123 def456 path/with spaces/file.go", 8, "path/with spaces/file.go"},
	}

	for _, tt := range tests {
		result := extractV2Path(tt.entry, tt.skipFields)
		if result != tt.expected {
			t.Errorf("extractV2Path(%q, %d) = %q, want %q", tt.entry, tt.skipFields, result, tt.expected)
		}
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
