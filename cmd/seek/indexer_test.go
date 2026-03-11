package main

import (
	"os"
	"path/filepath"
	"sort"
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

// --- State file tests ---

func TestStateFile_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	state := "abc123def456789a"
	if err := writeStateFile(dir, state); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}
	got := readStateFile(dir)
	if got != state {
		t.Errorf("expected %q, got %q", state, got)
	}
}

func TestStateFile_ReadNonexistent(t *testing.T) {
	dir := t.TempDir()
	got := readStateFile(dir)
	if got != "" {
		t.Errorf("expected empty string for nonexistent state file, got %q", got)
	}
}

func TestStateFile_WriteRemovesTmp(t *testing.T) {
	dir := t.TempDir()
	if err := writeStateFile(dir, "test"); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}
	// .state.tmp should not exist after successful atomic write (rename removes it)
	if _, err := os.Stat(filepath.Join(dir, stateTmpFile)); !os.IsNotExist(err) {
		t.Error("expected .state.tmp to not exist after successful write")
	}
	// .state should exist
	if _, err := os.Stat(filepath.Join(dir, stateFile)); err != nil {
		t.Error("expected .state to exist after write")
	}
}

func TestStateFile_Delete(t *testing.T) {
	dir := t.TempDir()
	// Write both files
	_ = os.WriteFile(filepath.Join(dir, stateFile), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, stateTmpFile), []byte("y"), 0o644)

	deleteStateFiles(dir)

	if _, err := os.Stat(filepath.Join(dir, stateFile)); !os.IsNotExist(err) {
		t.Error("expected .state to be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, stateTmpFile)); !os.IsNotExist(err) {
		t.Error("expected .state.tmp to be deleted")
	}
}

func TestStateFile_DeleteNonexistent(t *testing.T) {
	dir := t.TempDir()
	// Should not panic on nonexistent files
	deleteStateFiles(dir)
}

func TestStateFile_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	if err := writeStateFile(dir, "first"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeStateFile(dir, "second"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got := readStateFile(dir)
	if got != "second" {
		t.Errorf("expected %q after overwrite, got %q", "second", got)
	}
}

// --- readUncommittedFiles tests ---

func TestReadUncommittedFiles_RegularFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b"), 0o644)

	results := readUncommittedFiles(dir, []string{"a.go", "b.go"}, 2)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Sort for deterministic assertion
	sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })
	if results[0].name != "a.go" || string(results[0].content) != "package a" {
		t.Errorf("unexpected result[0]: %+v", results[0])
	}
	if results[1].name != "b.go" || string(results[1].content) != "package b" {
		t.Errorf("unexpected result[1]: %+v", results[1])
	}
}

func TestReadUncommittedFiles_EmptyList(t *testing.T) {
	dir := t.TempDir()
	results := readUncommittedFiles(dir, nil, 2)
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestReadUncommittedFiles_NonexistentFile(t *testing.T) {
	dir := t.TempDir()
	results := readUncommittedFiles(dir, []string{"does_not_exist.go"}, 1)
	if len(results) != 0 {
		t.Errorf("expected 0 results for nonexistent file, got %d", len(results))
	}
}

func TestReadUncommittedFiles_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "subdir"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "real.go"), []byte("package real"), 0o644)

	results := readUncommittedFiles(dir, []string{"subdir", "real.go"}, 2)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (directory skipped), got %d", len(results))
	}
	if results[0].name != "real.go" {
		t.Errorf("expected real.go, got %s", results[0].name)
	}
}

func TestReadUncommittedFiles_SkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	_ = os.WriteFile(target, []byte("package target"), 0o644)
	_ = os.Symlink(target, filepath.Join(dir, "link.go"))

	results := readUncommittedFiles(dir, []string{"link.go", "target.go"}, 2)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (symlink skipped), got %d", len(results))
	}
	if results[0].name != "target.go" {
		t.Errorf("expected target.go, got %s", results[0].name)
	}
}

func TestReadUncommittedFiles_NestedPath(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "src", "pkg"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "src", "pkg", "main.go"), []byte("package main"), 0o644)

	results := readUncommittedFiles(dir, []string{"src/pkg/main.go"}, 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].name != "src/pkg/main.go" {
		t.Errorf("expected src/pkg/main.go, got %s", results[0].name)
	}
}

func TestReadUncommittedFiles_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "empty.go"), []byte{}, 0o644)

	results := readUncommittedFiles(dir, []string{"empty.go"}, 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0].content) != 0 {
		t.Errorf("expected empty content, got %d bytes", len(results[0].content))
	}
}

func TestReadUncommittedFiles_MixedExistingAndMissing(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "exists.go"), []byte("yes"), 0o644)

	results := readUncommittedFiles(dir, []string{"exists.go", "gone.go", "also_gone.go"}, 2)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].name != "exists.go" {
		t.Errorf("expected exists.go, got %s", results[0].name)
	}
}

func TestReadUncommittedFiles_PreservesRelativeName(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a", "b", "c.go"), []byte("x"), 0o644)

	results := readUncommittedFiles(dir, []string{"a/b/c.go"}, 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	// Name must be the relative path as passed in, not an absolute path
	if results[0].name != "a/b/c.go" {
		t.Errorf("expected relative name a/b/c.go, got %s", results[0].name)
	}
}

func TestReadUncommittedFiles_SpacesInPath(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "my dir"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "my dir", "my file.go"), []byte("hello"), 0o644)

	results := readUncommittedFiles(dir, []string{"my dir/my file.go"}, 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].name != "my dir/my file.go" {
		t.Errorf("expected name with spaces preserved, got %s", results[0].name)
	}
}

func TestReadUncommittedFiles_Parallelism1(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		_ = os.WriteFile(filepath.Join(dir, filepath.Base(t.Name())+string(rune('a'+i))+".go"), []byte("x"), 0o644)
	}

	files := make([]string, 10)
	for i := 0; i < 10; i++ {
		files[i] = filepath.Base(t.Name()) + string(rune('a'+i)) + ".go"
	}

	results := readUncommittedFiles(dir, files, 1)
	if len(results) != 10 {
		t.Errorf("expected 10 results with parallelism=1, got %d", len(results))
	}
}

// --- cleanUncommittedShards tests ---

func TestCleanUncommittedShards_RemovesMatching(t *testing.T) {
	dir := t.TempDir()
	// Create shard files matching the pattern
	_ = os.WriteFile(filepath.Join(dir, "uncommitted_v16.00000.zoekt"), []byte{}, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "uncommitted_v16.00001.zoekt"), []byte{}, 0o644)
	// Create a non-matching shard that should be preserved
	_ = os.WriteFile(filepath.Join(dir, "myrepo_v16.00000.zoekt"), []byte{}, 0o644)

	cleanUncommittedShards(dir)

	entries, _ := filepath.Glob(filepath.Join(dir, "*.zoekt"))
	if len(entries) != 1 {
		t.Errorf("expected 1 remaining shard, got %d: %v", len(entries), entries)
	}
	if filepath.Base(entries[0]) != "myrepo_v16.00000.zoekt" {
		t.Errorf("wrong shard preserved: %s", entries[0])
	}
}

func TestCleanUncommittedShards_NoShards(t *testing.T) {
	dir := t.TempDir()
	// Should not panic on empty directory
	cleanUncommittedShards(dir)
}

func TestCleanUncommittedShards_NonexistentDir(t *testing.T) {
	// Should not panic
	cleanUncommittedShards("/nonexistent/path/that/does/not/exist")
}

// --- shardsExist tests ---

func TestShardsExist_WithShards(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "repo_v16.00000.zoekt"), []byte{}, 0o644)
	if !shardsExist(dir) {
		t.Error("expected shardsExist to return true")
	}
}

func TestShardsExist_Empty(t *testing.T) {
	dir := t.TempDir()
	if shardsExist(dir) {
		t.Error("expected shardsExist to return false for empty dir")
	}
}

func TestShardsExist_NonZoektFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".state"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".lock"), []byte{}, 0o644)
	if shardsExist(dir) {
		t.Error("expected shardsExist to return false with only non-zoekt files")
	}
}

func TestShardsExist_NonexistentDir(t *testing.T) {
	if shardsExist("/nonexistent/path") {
		t.Error("expected shardsExist to return false for nonexistent dir")
	}
}

// --- helpers ---

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
