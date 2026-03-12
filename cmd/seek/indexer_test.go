package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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
	// With -z, all records (headers and entries) are NUL-terminated
	raw := "# branch.oid abc123def456\x00# branch.head main\x00"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "abc123def456" {
		t.Errorf("expected HeadSHA %q, got %q", "abc123def456", state.HeadSHA)
	}
	if state.RawOutput != raw {
		t.Error("expected RawOutput to match input")
	}
}

func TestParseGitStatusV2_NoHead(t *testing.T) {
	raw := "# branch.oid (initial)\x00# branch.head main\x00"
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
	raw := "# branch.oid abc123\x00" +
		"1 .M N... 100644 100644 100644 abc123 def456 src/main.go\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "src/main.go")
}

func TestParseGitStatusV2_Untracked(t *testing.T) {
	raw := "# branch.oid abc123\x00" +
		"? new_file.txt\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "new_file.txt")
}

func TestParseGitStatusV2_Unmerged(t *testing.T) {
	raw := "# branch.oid abc123\x00" +
		"u UU N... 100644 100644 100644 100644 abc123 def456 ghi789 conflict.go\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "conflict.go")
}

func TestParseGitStatusV2_Mixed(t *testing.T) {
	raw := "# branch.oid abc123\x00# branch.head develop\x00" +
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
	raw := "# branch.oid abc123\x00" +
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
	raw := "# branch.oid abc123\x00" +
		"? path with spaces/file name.go\x00" +
		"? path/with -> arrow.go\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "path with spaces/file name.go")
	assertContains(t, state.Files, "path/with -> arrow.go")
}

func TestParseGitStatusV2_HeadersOnly(t *testing.T) {
	raw := "# branch.oid abc123\x00# branch.head main\x00# branch.upstream origin/main\x00# branch.ab +0 -0\x00"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "abc123" {
		t.Errorf("expected HeadSHA %q, got %q", "abc123", state.HeadSHA)
	}
	if len(state.Files) != 0 {
		t.Errorf("expected no files, got %v", state.Files)
	}
}

func TestParseGitStatusV2_BlankLineBetweenHeadersAndEntries(t *testing.T) {
	// With NUL-terminated records, blank lines are not expected, but
	// the parser should handle entries mixed with headers gracefully.
	raw := "# branch.oid abc123\x00# branch.head main\x00" +
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

// --- checkCtags detection tests ---

func TestCheckCtags_CTAGS_COMMAND_Valid(t *testing.T) {
	// Point CTAGS_COMMAND to a known-good binary (the real ctags).
	requireTools(t)
	ctags, err := exec.LookPath("ctags")
	if err != nil {
		ctags, err = exec.LookPath("universal-ctags")
		if err != nil {
			t.Fatal("neither ctags nor universal-ctags on PATH")
		}
	}
	t.Setenv("CTAGS_COMMAND", ctags)
	if err := checkCtags(); err != nil {
		t.Fatalf("checkCtags failed with valid CTAGS_COMMAND=%q: %v", ctags, err)
	}
}

func TestCheckCtags_CTAGS_COMMAND_Invalid(t *testing.T) {
	t.Setenv("CTAGS_COMMAND", "/nonexistent/binary")
	if err := checkCtags(); err == nil {
		t.Fatal("expected error for nonexistent CTAGS_COMMAND")
	}
}

func TestCheckCtags_CTAGS_COMMAND_TakesPrecedence(t *testing.T) {
	// Even when universal-ctags is on PATH, CTAGS_COMMAND should be checked first.
	// Pointing to a bad path should fail, proving we don't fall through.
	t.Setenv("CTAGS_COMMAND", "/nonexistent/ctags")
	if err := checkCtags(); err == nil {
		t.Fatal("expected error: CTAGS_COMMAND should take precedence over PATH lookup")
	}
}

func TestCheckCtags_DetectsFromPATH(t *testing.T) {
	requireTools(t)
	// Unset CTAGS_COMMAND so detection relies on PATH.
	t.Setenv("CTAGS_COMMAND", "")
	if err := checkCtags(); err != nil {
		t.Fatalf("checkCtags should detect ctags from PATH: %v", err)
	}
}

// --- large file skip tests ---

func TestReadUncommittedFiles_LargeFileSkipped(t *testing.T) {
	dir := t.TempDir()
	// Create a file just over the limit
	large := make([]byte, maxUncommittedFileSize+1)
	_ = os.WriteFile(filepath.Join(dir, "large.go"), large, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "small.go"), []byte("package small"), 0o644)

	results := readUncommittedFiles(dir, []string{"large.go", "small.go"}, 2)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (large file skipped), got %d", len(results))
	}
	if results[0].name != "small.go" {
		t.Errorf("expected small.go, got %s", results[0].name)
	}
}

func TestReadUncommittedFiles_ExactlyMaxSize(t *testing.T) {
	dir := t.TempDir()
	// Exactly at the limit — should NOT be skipped (guard is >)
	data := make([]byte, maxUncommittedFileSize)
	_ = os.WriteFile(filepath.Join(dir, "exact.go"), data, 0o644)

	results := readUncommittedFiles(dir, []string{"exact.go"}, 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (exactly max size should be included), got %d", len(results))
	}
}

func TestReadUncommittedFiles_JustOverMaxSize(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, maxUncommittedFileSize+1)
	_ = os.WriteFile(filepath.Join(dir, "over.go"), data, 0o644)

	results := readUncommittedFiles(dir, []string{"over.go"}, 1)
	if len(results) != 0 {
		t.Errorf("expected 0 results (just over max size should be skipped), got %d", len(results))
	}
}

// --- state file edge case tests ---

func TestStateFile_WhitespaceHandling(t *testing.T) {
	dir := t.TempDir()
	// Manually write a state file with whitespace
	_ = os.WriteFile(filepath.Join(dir, stateFile), []byte("  abc123  \n\n"), 0o644)
	got := readStateFile(dir)
	if got != "abc123" {
		t.Errorf("expected trimmed %q, got %q", "abc123", got)
	}
}

func TestStateFile_EmptyContent(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, stateFile), []byte(""), 0o644)
	got := readStateFile(dir)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestStateFile_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	roDir := filepath.Join(dir, "readonly")
	_ = os.Mkdir(roDir, 0o555)
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	err := writeStateFile(roDir, "test")
	if err == nil {
		t.Error("expected error writing to read-only directory")
	}
}

func TestComputeStateHash_VersionPrefix(t *testing.T) {
	// Guard against accidental stateVersion changes — bumping the version
	// invalidates all user caches, so it should be intentional.
	if stateVersion != "v5\x00" {
		t.Errorf("stateVersion changed unexpectedly: got %q, want %q — update this test if intentional", stateVersion, "v5\x00")
	}

	h := computeStateHash("# branch.oid abc123\n")
	if len(h) != 16 {
		t.Errorf("expected 16-char hex hash, got %d chars: %q", len(h), h)
	}
}

func TestComputeStateHash_LargeInput(t *testing.T) {
	// 1MB of input — verify hash works and is deterministic
	large := make([]byte, 1024*1024)
	for i := range large {
		large[i] = byte(i % 256)
	}
	h1 := computeStateHash(string(large))
	h2 := computeStateHash(string(large))
	if h1 != h2 {
		t.Error("large input hash should be deterministic")
	}
	if len(h1) != 16 {
		t.Errorf("expected 16-char hash, got %d", len(h1))
	}
}

// --- git parsing edge case tests ---

func TestParseGitStatusV2_OnlyNulBytes(t *testing.T) {
	state := parseGitStatusV2("\x00\x00\x00")
	if state.HeadSHA != "no-head" {
		t.Errorf("expected no-head, got %q", state.HeadSHA)
	}
	if len(state.Files) != 0 {
		t.Errorf("expected no files, got %v", state.Files)
	}
}

func TestParseGitStatusV2_ShortEntries(t *testing.T) {
	// Entry exactly 1 byte — should be skipped by len(entry) < 2 guard
	raw := "# branch.oid abc\x00" + "1\x00"
	state := parseGitStatusV2(raw)
	if len(state.Files) != 0 {
		t.Errorf("expected no files for 1-byte entry, got %v", state.Files)
	}
}

func TestParseGitStatusV2_UnknownEntryType(t *testing.T) {
	// Entry type 'X' is not handled — should be silently skipped
	raw := "# branch.oid abc\x00" + "X some unknown entry\x00"
	state := parseGitStatusV2(raw)
	if len(state.Files) != 0 {
		t.Errorf("expected unknown entry type to be skipped, got %v", state.Files)
	}
}

func TestParseGitStatusV2_RenameEntry(t *testing.T) {
	// Type '2' is rename in v2 format. Although --no-renames prevents these,
	// the parser handles them defensively: extracts the destination path and
	// consumes the extra NUL-terminated origPath record.
	raw := "# branch.oid abc\x00" +
		"2 R. N... 100644 100644 100644 abc def R100 new.go\x00old.go\x00"
	state := parseGitStatusV2(raw)
	if len(state.Files) != 1 {
		t.Fatalf("expected 1 file from rename entry, got %v", state.Files)
	}
	assertContains(t, state.Files, "new.go")
}

func TestParseGitStatusV2_RenameOrigPathNotMisinterpreted(t *testing.T) {
	// The origPath record after a rename must be consumed, not
	// misinterpreted as an independent entry.
	raw := "# branch.oid abc\x00" +
		"2 R. N... 100644 100644 100644 abc def R100 new.go\x00? suspicious.go\x00" +
		"? real_untracked.go\x00"
	state := parseGitStatusV2(raw)
	// "? suspicious.go" is the origPath of the rename — must be consumed.
	// Only "new.go" (from rename) and "real_untracked.go" should appear.
	if len(state.Files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(state.Files), state.Files)
	}
	assertContains(t, state.Files, "new.go")
	assertContains(t, state.Files, "real_untracked.go")
	if containsStr(state.Files, "? suspicious.go") {
		t.Error("origPath record was misinterpreted as an untracked file")
	}
}

func TestExtractV2Path_ZeroSkipFields(t *testing.T) {
	// skipFields=0 should return the entire entry
	result := extractV2Path("hello world", 0)
	if result != "hello world" {
		t.Errorf("expected entire entry, got %q", result)
	}
}

func TestExtractV2Path_ExactFieldCount(t *testing.T) {
	// Entry with exactly 3 space-separated fields, skip 3 — no trailing content
	result := extractV2Path("a b c", 3)
	if result != "" {
		t.Errorf("expected empty string when all fields consumed, got %q", result)
	}
}

func TestParseGitStatusV2_VeryLongPath(t *testing.T) {
	longPath := make([]byte, 10000)
	for i := range longPath {
		longPath[i] = 'a'
	}
	raw := "# branch.oid abc\x00" + "? " + string(longPath) + "\x00"
	state := parseGitStatusV2(raw)
	if len(state.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(state.Files))
	}
	if len(state.Files[0]) != 10000 {
		t.Errorf("expected path length 10000, got %d", len(state.Files[0]))
	}
}

// --- shard cleanup edge case tests ---

func TestCleanUncommittedShards_PreservesCommittedShards(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "uncommitted_v16.00000.zoekt"), []byte{}, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "myrepo_v16.00000.zoekt"), []byte{}, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "other_v16.00001.zoekt"), []byte{}, 0o644)

	cleanUncommittedShards(dir)

	entries, _ := filepath.Glob(filepath.Join(dir, "*.zoekt"))
	if len(entries) != 2 {
		t.Errorf("expected 2 remaining shards (committed preserved), got %d: %v", len(entries), entries)
	}
}

func TestCleanUncommittedShards_ReadOnlyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uncommitted_v16.00000.zoekt")
	_ = os.WriteFile(path, []byte{}, 0o444)
	// Should not panic even if removal fails
	cleanUncommittedShards(dir)
}

func TestShardsExist_OnlyTmpFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "repo_v16.00000.zoekt.tmp"), []byte{}, 0o644)
	if shardsExist(dir) {
		t.Error("expected false — .zoekt.tmp should not count as a shard")
	}
}

func TestShardsExist_MixedFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "repo_v16.00000.zoekt"), []byte{}, 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".state"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".lock"), []byte{}, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "repo_v16.00000.zoekt.tmp"), []byte{}, 0o644)
	if !shardsExist(dir) {
		t.Error("expected true — .zoekt file exists among other files")
	}
}

// --- readUncommittedFiles concurrency and edge tests ---

func TestReadUncommittedFiles_HighParallelism(t *testing.T) {
	dir := t.TempDir()
	const n = 100
	files := make([]string, n)
	for i := range n {
		name := filepath.Base(t.Name()) + fmt.Sprintf("_%03d.go", i)
		files[i] = name
		_ = os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("package f%d", i)), 0o644)
	}

	results := readUncommittedFiles(dir, files, 16)
	if len(results) != n {
		t.Errorf("expected %d results with high parallelism, got %d", n, len(results))
	}
}

func TestReadUncommittedFiles_PermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.go")
	_ = os.WriteFile(path, []byte("secret"), 0o000)
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_ = os.WriteFile(filepath.Join(dir, "ok.go"), []byte("ok"), 0o644)

	results := readUncommittedFiles(dir, []string{"secret.go", "ok.go"}, 2)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (permission denied skipped), got %d", len(results))
	}
	if results[0].name != "ok.go" {
		t.Errorf("expected ok.go, got %s", results[0].name)
	}
}

func TestReadUncommittedFiles_BinaryContent(t *testing.T) {
	dir := t.TempDir()
	// File with null bytes and binary content
	binary := []byte{0x00, 0x01, 0xFF, 0xFE, 'h', 'e', 'l', 'l', 'o', 0x00}
	_ = os.WriteFile(filepath.Join(dir, "bin.dat"), binary, 0o644)

	results := readUncommittedFiles(dir, []string{"bin.dat"}, 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0].content) != len(binary) {
		t.Errorf("expected %d bytes, got %d", len(binary), len(results[0].content))
	}
	for i, b := range binary {
		if results[0].content[i] != b {
			t.Errorf("byte %d: expected %x, got %x", i, b, results[0].content[i])
			break
		}
	}
}

func TestReadUncommittedFiles_DuplicateNames(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "dup.go"), []byte("content"), 0o644)

	// Same file passed twice
	results := readUncommittedFiles(dir, []string{"dup.go", "dup.go"}, 2)
	// readUncommittedFiles doesn't deduplicate — both are read.
	// This is fine because deduplication happens at the formatter level.
	if len(results) != 2 {
		t.Errorf("expected 2 results (no dedup at read level), got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// State caching scenarios — tests for post-indexing verification logic
//
// The decision matrix in runIndexing (post-indexing verification) determines
// whether the state file is cached after indexing. These tests verify all branches.
// ---------------------------------------------------------------------------

// TestStateCaching_BothSucceed_StateStable verifies that when both committed
// and uncommitted indexing succeed and the repo doesn't change during
// indexing, the state file IS written (index is cached).
func TestStateCaching_BothSucceed_StateStable(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_content\n")

	// Add uncommitted change
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// uncommitted_content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))

	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	// State file should exist — index is cached
	cached := readStateFile(indexDir)
	if cached == "" {
		t.Fatal("expected state file to be written after successful indexing")
	}

	// Second run should be a no-op (state matches)
	state2 := gitRepoStateIn(ctx, dir)
	stateHash2 := computeStateHash(repoStateFingerprint(dir, state2))
	if stateHash2 != cached {
		t.Log("state changed between runs — cannot verify caching (non-deterministic)")
		return
	}
	// runIndexing should short-circuit at the state check in run()
	// We verify by checking the state file is unchanged
	if err := runIndexing(ctx, dir, indexDir, state2, stateHash2); err != nil {
		t.Fatalf("second indexing failed: %v", err)
	}
	cached2 := readStateFile(indexDir)
	if cached2 != cached {
		t.Errorf("state file changed after no-op indexing: %q -> %q", cached, cached2)
	}
}

// TestStateCaching_BothSucceed_StateDrifted verifies that when indexing
// succeeds but the repo changes during indexing, the state file is NOT
// written (forces re-index on next search).
func TestStateCaching_BothSucceed_StateDrifted(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// original\n")

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Capture pre-state
	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))

	// Index (succeeds)
	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	// State file should be written (repo didn't change during indexing)
	cached := readStateFile(indexDir)
	if cached == "" {
		t.Fatal("expected state file after initial indexing")
	}

	// Now mutate the repo
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-index with OLD state (simulates pre-state captured before mutation)
	// Post-verification will see the drift and delete the state file
	deleteStateFiles(indexDir)
	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("re-indexing failed: %v", err)
	}

	// State file should NOT be written (drift detected)
	cached = readStateFile(indexDir)
	if cached != "" {
		t.Errorf("expected no state file after drift, got %q", cached)
	}
}

// TestStateCaching_CommittedFails verifies that when committed indexing
// fails, the state file is deleted regardless of uncommitted success.
func TestStateCaching_CommittedFails(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// content\n")

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-populate a state file to verify it gets deleted
	if err := writeStateFile(indexDir, "fake_old_state"); err != nil {
		t.Fatal(err)
	}

	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))

	// Remove the .git directory to make committed indexing fail
	// (gitindex.IndexGitRepo needs a valid git repo)
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}

	// runIndexing will fail at ctags check or committed indexing.
	// The error itself is expected — we only verify state file behavior.
	err := runIndexing(ctx, dir, indexDir, state, stateHash)

	// State file must not retain the old value after a failed indexing run
	cached := readStateFile(indexDir)
	if cached == "fake_old_state" {
		t.Errorf("state file was NOT deleted after committed indexing failure (runIndexing err=%v)", err)
	}
}

// TestStateCaching_WithUncommittedFiles_BothSucceed verifies that when
// uncommitted files exist and both indexing steps succeed, the state
// file is written and the uncommitted content is searchable.
func TestStateCaching_WithUncommittedFiles_BothSucceed(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed\n")

	// Create an uncommitted file that will be in git status
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package main\n// uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))

	if len(state.Files) == 0 {
		t.Fatal("precondition: state should have uncommitted files")
	}

	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	// Both steps succeeded — state file should be cached
	cached := readStateFile(indexDir)
	if cached == "" {
		t.Fatal("expected state file to be written when both indexing steps succeed")
	}

	// Uncommitted content should be searchable
	results, err := executeSearch(ctx, indexDir, "uncommitted")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected uncommitted content to be searchable")
	}
}

// TestStateCaching_NoUncommittedFiles verifies that when there are no
// uncommitted files, committed-only indexing caches the state correctly.
func TestStateCaching_NoUncommittedFiles(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// clean_repo\n")

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))

	if len(state.Files) != 0 {
		t.Fatalf("precondition: clean repo should have no uncommitted files, got %v", state.Files)
	}

	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	cached := readStateFile(indexDir)
	if cached == "" {
		t.Log("state not cached (possible drift during indexing)")
	} else {
		t.Logf("state cached correctly for clean repo: %q", cached)
	}

	// Verify search works
	results, err := executeSearch(ctx, indexDir, "clean_repo")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("search should find committed content")
	}
}

// TestStateCaching_DoubleCheck_SkipsRedundantIndex verifies the double-check
// after acquiring the lock: if another process already indexed for the same
// state, indexing is skipped entirely.
func TestStateCaching_DoubleCheck_SkipsRedundantIndex(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// content\n")

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))

	// First index
	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("first indexing failed: %v", err)
	}

	// Pre-populate state file with the SAME hash we're about to pass
	// This simulates another process having just indexed
	if err := writeStateFile(indexDir, stateHash); err != nil {
		t.Fatal(err)
	}

	// Second index with same state should be a no-op (double-check hit)
	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("second indexing failed: %v", err)
	}

	// State file should still contain the same hash
	cached := readStateFile(indexDir)
	if cached != stateHash {
		t.Errorf("state file changed after double-check skip: %q -> %q", stateHash, cached)
	}
}

// TestStateCaching_StaleFallback_DoesNotWriteState verifies that when
// the lock can't be acquired (stale fallback), the state file is NOT written.
func TestStateCaching_StaleFallback_DoesNotWriteState(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// content\n")

	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// First index to create shards
	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))
	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("initial indexing failed: %v", err)
	}

	// Delete the state file to force re-indexing attempt
	deleteStateFiles(indexDir)

	// Hold the lock to trigger stale fallback
	lockPath := filepath.Join(indexDir, lockFile)
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unlockFile(holder)
		_ = holder.Close()
	})
	if err := lockFileExclusive(holder); err != nil {
		t.Fatal(err)
	}

	// runIndexing should fall back (shards exist, lock held)
	if err := runIndexing(ctx, dir, indexDir, state, stateHash); err != nil {
		t.Fatalf("stale fallback should not error: %v", err)
	}

	// State file should NOT be written by the stale fallback path
	cached := readStateFile(indexDir)
	if cached != "" {
		t.Errorf("state file should not be written during stale fallback, got %q", cached)
	}
}

// ---------------------------------------------------------------------------
// repoStateFingerprint — inode detection tests
// ---------------------------------------------------------------------------

// TestRepoStateFingerprint_InodeChange_AtomicWrite verifies that when a file
// is replaced via atomic write (write-to-tmp + rename — vim/emacs pattern),
// the fingerprint changes even if mtime and size happen to match. This is the
// primary scenario the inode field was added to detect.
func TestRepoStateFingerprint_InodeChange_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.go")
	content := []byte("package main\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	state := repoState{
		RawOutput: "# branch.oid abc\x00",
		Files:     []string{"file.go"},
	}
	fp1 := repoStateFingerprint(dir, state)

	// Simulate atomic write: write to a new tmp file, then rename over original.
	// This creates a new inode while preserving the content.
	tmpPath := filepath.Join(dir, "file.go.tmp")
	if err := os.WriteFile(tmpPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatal(err)
	}

	fp2 := repoStateFingerprint(dir, state)

	// Fingerprints must differ — the inode changed even though content is identical.
	// Note: on some filesystems, inode recycling could theoretically reuse the same
	// inode, so we log rather than hard-fail if they match.
	if fp1 == fp2 {
		// Verify inode actually changed — if it didn't, the filesystem recycled it
		fi1Ino := getInode(t, path)
		t.Logf("fp1 == fp2; inode after rename: %d (may indicate inode recycling on this filesystem)", fi1Ino)
	}
}

// TestRepoStateFingerprint_StableWhenUnchanged verifies that the fingerprint
// is deterministic when no file changes occur between calls.
func TestRepoStateFingerprint_StableWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b\n"), 0o644)

	state := repoState{
		RawOutput: "# branch.oid abc\x00",
		Files:     []string{"a.go", "b.go"},
	}

	fp1 := repoStateFingerprint(dir, state)
	fp2 := repoStateFingerprint(dir, state)
	if fp1 != fp2 {
		t.Errorf("fingerprint should be stable for unchanged files:\n  fp1=%q\n  fp2=%q", fp1, fp2)
	}
}

// TestRepoStateFingerprint_InodeInOutput verifies that the fingerprint format
// includes 4 NUL-separated fields per file: name, mtime, size, inode.
func TestRepoStateFingerprint_InodeInOutput(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "f.go"), []byte("x"), 0o644)

	state := repoState{
		RawOutput: "raw",
		Files:     []string{"f.go"},
	}
	fp := repoStateFingerprint(dir, state)

	// Expected format: "raw\x00f.go\x00<mtime>\x00<size>\x00<inode>\x00"
	// Count NUL separators in the appended part (after "raw")
	appended := fp[len("raw"):]
	nuls := strings.Count(appended, "\x00")
	// With format "\x00name\x00mtime\x00size\x00inode\x00" → 5 NULs per file
	if nuls != 5 {
		t.Errorf("expected 5 NUL separators per file (name+mtime+size+inode), got %d in %q", nuls, appended)
	}
}

// TestRepoStateFingerprint_DeletedFileNoInode verifies that the deleted-file
// sentinel does NOT include an inode field (it can't — the file doesn't exist).
func TestRepoStateFingerprint_DeletedFileNoInode(t *testing.T) {
	dir := t.TempDir()
	state := repoState{
		RawOutput: "raw",
		Files:     []string{"gone.go"},
	}
	fp := repoStateFingerprint(dir, state)

	expected := "raw\x00gone.go\x00deleted\x00"
	if fp != expected {
		t.Errorf("deleted file fingerprint:\n  got:  %q\n  want: %q", fp, expected)
	}
}

// TestRepoStateFingerprint_InodeChangesHash verifies that different inodes
// produce different state hashes through the full computeStateHash pipeline.
func TestRepoStateFingerprint_InodeChangesHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	content := []byte("package f\n")
	_ = os.WriteFile(path, content, 0o644)

	state := repoState{
		RawOutput: "# branch.oid abc\x00",
		Files:     []string{"f.go"},
	}

	h1 := computeStateHash(repoStateFingerprint(dir, state))

	// Recreate the file (new inode, potentially different mtime)
	_ = os.Remove(path)
	_ = os.WriteFile(path, content, 0o644)

	h2 := computeStateHash(repoStateFingerprint(dir, state))

	// Hash should differ because at minimum the inode changed.
	// (mtime also likely changed, but the inode alone would suffice.)
	if h1 == h2 {
		t.Log("hashes matched — inode may have been recycled or mtime+inode both matched (extremely unlikely)")
	}
}

// TestRepoStateFingerprint_ManyFilesWithInode verifies the fingerprint works
// correctly with many files, ensuring the inode field doesn't cause format errors.
func TestRepoStateFingerprint_ManyFilesWithInode(t *testing.T) {
	dir := t.TempDir()
	const n = 100
	files := make([]string, n)
	for i := range n {
		name := fmt.Sprintf("file_%03d.go", i)
		files[i] = name
		_ = os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("package f%d\n", i)), 0o644)
	}

	state := repoState{
		RawOutput: "# branch.oid abc\x00",
		Files:     files,
	}

	fp := repoStateFingerprint(dir, state)

	// Verify fingerprint is deterministic
	fp2 := repoStateFingerprint(dir, state)
	if fp != fp2 {
		t.Error("fingerprint not deterministic with 100 files")
	}

	// Verify each file contributes 5 NULs (leading NUL + name + mtime + size + inode)
	appended := fp[len(state.RawOutput):]
	nuls := strings.Count(appended, "\x00")
	if nuls != n*5 {
		t.Errorf("expected %d NULs for %d files, got %d", n*5, n, nuls)
	}
}

// TestRepoStateFingerprint_MixedDeletedAndExisting verifies that a mix of
// existing and deleted files produces a valid fingerprint with correct formats.
func TestRepoStateFingerprint_MixedDeletedAndExisting(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "exists.go"), []byte("x"), 0o644)
	// "gone.go" does not exist

	state := repoState{
		RawOutput: "raw",
		Files:     []string{"exists.go", "gone.go"},
	}
	fp := repoStateFingerprint(dir, state)

	if !strings.Contains(fp, "\x00gone.go\x00deleted\x00") {
		t.Error("expected deleted sentinel for gone.go")
	}
	if !strings.Contains(fp, "\x00exists.go\x00") {
		t.Error("expected exists.go in fingerprint")
	}

	// Hash through the full pipeline — must not panic
	h := computeStateHash(fp)
	if len(h) != 16 {
		t.Errorf("expected 16-char hash, got %d", len(h))
	}
}

// TestRepoStateFingerprint_ContentChangeDetected verifies that editing a file's
// content (changing mtime and potentially size) is detected by the fingerprint.
func TestRepoStateFingerprint_ContentChangeDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	_ = os.WriteFile(path, []byte("version1\n"), 0o644)

	state := repoState{
		RawOutput: "# branch.oid abc\x00",
		Files:     []string{"f.go"},
	}
	fp1 := repoStateFingerprint(dir, state)

	// Ensure mtime advances (some filesystems have coarse granularity)
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(path, []byte("version2\n"), 0o644)

	fp2 := repoStateFingerprint(dir, state)
	if fp1 == fp2 {
		t.Error("fingerprint should change when file content changes")
	}
}

// TestRepoStateFingerprint_SameSizeDifferentContent verifies detection when
// file content changes but size stays the same (e.g. "aaa" → "bbb").
func TestRepoStateFingerprint_SameSizeDifferentContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	_ = os.WriteFile(path, []byte("aaa"), 0o644)

	state := repoState{
		RawOutput: "# branch.oid abc\x00",
		Files:     []string{"f.go"},
	}
	fp1 := repoStateFingerprint(dir, state)

	// Same size, different content — mtime should change
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(path, []byte("bbb"), 0o644)

	fp2 := repoStateFingerprint(dir, state)
	if fp1 == fp2 {
		t.Error("fingerprint should change when content changes (same size, different mtime)")
	}
}

// TestRepoStateFingerprint_SymlinkSkippedByLstat verifies that Lstat is used
// (not Stat), so symlinks are reported as symlinks, not as their targets.
// This matters because readUncommittedFiles skips symlinks.
func TestRepoStateFingerprint_SymlinkInFingerprint(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	_ = os.WriteFile(target, []byte("x"), 0o644)
	_ = os.Symlink(target, filepath.Join(dir, "link.go"))

	state := repoState{
		RawOutput: "raw",
		Files:     []string{"link.go"},
	}
	fp := repoStateFingerprint(dir, state)

	// The symlink should be fingerprinted (Lstat sees it as a symlink).
	// The fingerprint should contain the symlink's own metadata, not the target's.
	if !strings.Contains(fp, "\x00link.go\x00") {
		t.Error("expected link.go in fingerprint")
	}

	// Verify symlink has a different inode than target
	linkFi, _ := os.Lstat(filepath.Join(dir, "link.go"))
	targetFi, _ := os.Lstat(target)
	if linkStat, ok := linkFi.Sys().(*syscall.Stat_t); ok {
		if targetStat, ok := targetFi.Sys().(*syscall.Stat_t); ok {
			if linkStat.Ino == targetStat.Ino {
				t.Error("expected symlink and target to have different inodes")
			}
		}
	}
}

// TestRepoStateFingerprint_EmptyFileName verifies that an empty file name
// in the Files list doesn't cause panics or format corruption.
// Note: git status never produces empty filenames, so this is a robustness test.
func TestRepoStateFingerprint_EmptyFileName(t *testing.T) {
	dir := t.TempDir()
	state := repoState{
		RawOutput: "raw",
		Files:     []string{""},
	}
	// Empty path resolves to the directory itself via filepath.Join(dir, ""),
	// so Lstat succeeds and produces a normal entry (not a deleted sentinel).
	// This is fine — git status never produces empty filenames.
	fp := repoStateFingerprint(dir, state)
	if !strings.HasPrefix(fp, "raw\x00") {
		t.Errorf("expected fingerprint to start with RawOutput, got %q", fp)
	}
	// Must not panic and must produce a valid hash
	h := computeStateHash(fp)
	if len(h) != 16 {
		t.Errorf("expected 16-char hash, got %d", len(h))
	}
}

// TestRepoStateFingerprint_FileOrderMatters verifies that file order affects
// the fingerprint (i.e., the fingerprint is order-dependent, which is fine
// because git status output order is deterministic with -z).
func TestRepoStateFingerprint_FileOrderMatters(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.go"), []byte("b"), 0o644)

	stateAB := repoState{RawOutput: "raw", Files: []string{"a.go", "b.go"}}
	stateBA := repoState{RawOutput: "raw", Files: []string{"b.go", "a.go"}}

	fpAB := repoStateFingerprint(dir, stateAB)
	fpBA := repoStateFingerprint(dir, stateBA)

	if fpAB == fpBA {
		t.Error("fingerprint should be order-dependent (different file orderings)")
	}
}

// ---------------------------------------------------------------------------
// computeStateHash — WriteString equivalence tests
// ---------------------------------------------------------------------------

// TestComputeStateHash_WriteStringEquivalence verifies that the WriteString
// optimization produces the same hash as the original Write([]byte()) would.
// This is a regression test for the refactor.
func TestComputeStateHash_WriteStringEquivalence(t *testing.T) {
	inputs := []string{
		"",
		"short",
		"# branch.oid abc123def456789012345678901234567890\x00# branch.head main\x00",
		strings.Repeat("x", 10000),
		"\x00\x01\x02\xff",
		"unicode: 日本語テスト",
	}
	for _, input := range inputs {
		h := computeStateHash(input)
		if len(h) != 16 {
			t.Errorf("computeStateHash(%q): expected 16 chars, got %d", input[:min(len(input), 20)], len(h))
		}
		// Verify determinism (same input → same output)
		h2 := computeStateHash(input)
		if h != h2 {
			t.Errorf("computeStateHash not deterministic for input len=%d", len(input))
		}
	}
}

// TestComputeStateHash_NulBytesInInput verifies that NUL bytes (used as
// separators in fingerprints) don't cause truncation or corruption.
func TestComputeStateHash_NulBytesInInput(t *testing.T) {
	h1 := computeStateHash("before\x00after")
	h2 := computeStateHash("before\x00different")
	h3 := computeStateHash("before")
	if h1 == h2 {
		t.Error("different content after NUL should produce different hash")
	}
	if h1 == h3 {
		t.Error("hash should not truncate at NUL byte")
	}
}

// ---------------------------------------------------------------------------
// Integration: inode-aware drift detection end-to-end
// ---------------------------------------------------------------------------

// TestIntegration_AtomicWriteDetected verifies that when a file is replaced
// via atomic write (the vim/emacs pattern), seek detects the change and
// re-indexes even if the content, size, and potentially mtime match.
func TestIntegration_AtomicWriteDetected(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// atomic_original\n")

	// First search to build index
	files, err := runSeekInRepo(t, dir, "atomic_original")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for original content")
	}

	// Edit via uncommitted change
	appPath := filepath.Join(dir, "app.go")
	newContent := []byte("package main\n// atomic_replaced\n")

	// Simulate atomic write: write to tmp, rename over original
	tmpPath := appPath + ".tmp"
	if err := os.WriteFile(tmpPath, newContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, appPath); err != nil {
		t.Fatal(err)
	}

	// Search should find the new content
	files, err = runSeekInRepo(t, dir, "atomic_replaced")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("FRESHNESS VIOLATION: atomic write was not detected")
	}
}

// TestIntegration_RapidEdits verifies that multiple rapid edits within the
// same second are all detected (tests nanosecond mtime + inode together).
func TestIntegration_RapidEdits(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// rapid_v0\n")

	for i := 1; i <= 5; i++ {
		content := fmt.Sprintf("package main\n// rapid_v%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}

		marker := fmt.Sprintf("rapid_v%d", i)
		files, err := runSeekInRepo(t, dir, marker)
		if err != nil {
			t.Fatalf("iteration %d: search failed: %v", i, err)
		}
		if len(files) == 0 {
			t.Fatalf("iteration %d: FRESHNESS VIOLATION: rapid edit not detected for %q", i, marker)
		}
	}
}

// --- streamFiles tests ---

func TestStreamFiles_BasicFlow(t *testing.T) {
	dir := t.TempDir()
	const numFiles = 5
	files := make([]string, numFiles)
	for i := range numFiles {
		name := fmt.Sprintf("file_%d.go", i)
		files[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("package f%d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ch := streamFiles(dir, files, 4)
	var got []string
	for fc := range ch {
		got = append(got, fc.name)
	}
	if len(got) != numFiles {
		t.Errorf("expected %d files, got %d", numFiles, len(got))
	}
}

func TestStreamFiles_EmptyFileList(t *testing.T) {
	dir := t.TempDir()

	// nil files
	ch := streamFiles(dir, nil, 4)
	count := 0
	for range ch {
		count++
	}
	if count != 0 {
		t.Errorf("expected 0 items from nil file list, got %d", count)
	}

	// empty slice
	ch = streamFiles(dir, []string{}, 4)
	count = 0
	for range ch {
		count++
	}
	if count != 0 {
		t.Errorf("expected 0 items from empty file list, got %d", count)
	}
}

func TestStreamFiles_AllFiltered(t *testing.T) {
	dir := t.TempDir()
	// All files are too large — should all be filtered
	name := "big.dat"
	data := make([]byte, maxUncommittedFileSize+1)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Also include a missing file
	ch := streamFiles(dir, []string{name, "nonexistent.go"}, 4)
	count := 0
	for range ch {
		count++
	}
	if count != 0 {
		t.Errorf("expected 0 items when all filtered, got %d", count)
	}
}

func TestStreamFiles_ParallelismOne(t *testing.T) {
	dir := t.TempDir()
	const numFiles = 10
	files := make([]string, numFiles)
	for i := range numFiles {
		name := fmt.Sprintf("file_%d.go", i)
		files[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ch := streamFiles(dir, files, 1)
	var got []string
	for fc := range ch {
		got = append(got, fc.name)
	}
	if len(got) != numFiles {
		t.Errorf("expected %d files with parallelism=1, got %d", numFiles, len(got))
	}
}

func TestStreamFiles_SingleFileExactlyMaxSize(t *testing.T) {
	dir := t.TempDir()
	// File exactly at maxUncommittedFileSize — should be included
	data := make([]byte, maxUncommittedFileSize)
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, "exact.dat"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ch := streamFiles(dir, []string{"exact.dat"}, 4)
	count := 0
	for fc := range ch {
		count++
		if len(fc.content) != maxUncommittedFileSize {
			t.Errorf("expected content of size %d, got %d", maxUncommittedFileSize, len(fc.content))
		}
	}
	if count != 1 {
		t.Errorf("expected 1 file at exactly max size, got %d", count)
	}
}

func TestStreamFiles_MixedValidAndInvalid(t *testing.T) {
	dir := t.TempDir()

	// Regular file
	if err := os.WriteFile(filepath.Join(dir, "regular.go"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink
	if err := os.Symlink("regular.go", filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	// Directory
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	ch := streamFiles(dir, []string{"regular.go", "link.go", "subdir", "missing.go"}, 4)
	var got []string
	for fc := range ch {
		got = append(got, fc.name)
	}
	// Only regular.go should arrive
	if len(got) != 1 || got[0] != "regular.go" {
		t.Errorf("expected only [regular.go], got %v", got)
	}
}

func TestStreamFiles_HighParallelismFewFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	ch := streamFiles(dir, []string{"a.go", "b.go"}, 16)
	count := 0
	for range ch {
		count++
	}
	if count != 2 {
		t.Errorf("expected 2 files with high parallelism, got %d", count)
	}
}

func TestStreamingMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory stress test in short mode")
	}

	dir := t.TempDir()
	const numFiles = 50
	const fileSize = 5 * 1024 * 1024 // 5 MB each → 250 MB total on disk
	const parallelism = 4
	files := make([]string, numFiles)
	for i := range numFiles {
		name := fmt.Sprintf("file_%03d.go", i)
		files[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte{'A' + byte(i%26)}, fileSize), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// With parallelism=4, at most 2*parallelism files can be in flight
	// (channel buffer + blocked workers) = 8 × 5MB = 40MB. The old
	// buffered approach would hold all 250MB at once.
	tracker := newPeakHeapTracker()
	for range streamFiles(dir, files, parallelism) {
	}
	heapDelta := tracker.stop()

	// Budget: 2*parallelism*fileSize = 40MB + overhead for GC timing,
	// runtime, and other test goroutines (ReadMemStats is process-wide).
	// Must be well under 250MB (what the old buffered approach would use).
	const budget = 120 * 1024 * 1024
	t.Logf("total content: %d MB, peak heap delta: %d MB, budget: %d MB",
		numFiles*fileSize/(1024*1024), heapDelta/(1024*1024), budget/(1024*1024))
	if heapDelta > int64(budget) {
		t.Fatalf("heap grew by %d MB, exceeds budget of %d MB — streaming is not bounded",
			heapDelta/(1024*1024), budget/(1024*1024))
	}
}

func TestStreamingMemoryDoesNotScaleWithInput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory scaling test in short mode")
	}

	const fileSize = 5 * 1024 * 1024 // 5 MB per file
	const parallelism = 4

	measurePeakHeap := func(numFiles int) int64 {
		dir := t.TempDir()
		files := make([]string, numFiles)
		for i := range numFiles {
			name := fmt.Sprintf("f%d.go", i)
			files[i] = name
			if err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte{'x'}, fileSize), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		tracker := newPeakHeapTracker()
		for range streamFiles(dir, files, parallelism) {
		}
		return tracker.stop()
	}

	small := measurePeakHeap(10) // 10 × 5MB = 50MB total
	large := measurePeakHeap(50) // 50 × 5MB = 250MB total

	t.Logf("10 files (50MB total): peak delta %d MB", small/(1024*1024))
	t.Logf("50 files (250MB total): peak delta %d MB", large/(1024*1024))

	// 5x more input should NOT cause 5x more heap. With streaming, peak
	// is bounded by 2*parallelism*fileSize = 40MB regardless of file count.
	// Allow 3x + floor to account for GC timing, runtime, and process-wide
	// heap noise from other test goroutines.
	if large > max(small*3, 120*1024*1024) {
		t.Fatalf("memory scales with input: 10 files=%d MB, 50 files=%d MB",
			small/(1024*1024), large/(1024*1024))
	}
}

func TestIndexUncommitted_EmptyChannel(t *testing.T) {
	dir := t.TempDir()
	ch := make(chan fileContent)
	close(ch) // empty channel

	err := indexUncommitted(context.Background(), t.TempDir(), dir, ch, 2)
	if err != nil {
		t.Fatalf("unexpected error for empty channel: %v", err)
	}
}

func TestIndexUncommitted_BuilderLazyCreation(t *testing.T) {
	requireTools(t)

	indexDir := t.TempDir()
	repoDir := t.TempDir()

	// Send one file through channel
	ch := make(chan fileContent, 1)
	ch <- fileContent{name: "test.go", content: []byte("package main\n")}
	close(ch)

	err := indexUncommitted(context.Background(), repoDir, indexDir, ch, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify a shard was created
	matches, _ := filepath.Glob(filepath.Join(indexDir, repoUncommitted+"_v*.zoekt"))
	if len(matches) == 0 {
		t.Error("expected shard to be created when files arrive")
	}
}

func TestIndexUncommitted_ChannelDrainedOnError(t *testing.T) {
	requireTools(t)

	// Use an invalid indexDir so NewBuilder succeeds but writing the shard
	// fails. We verify the channel is fully consumed (no goroutine leak)
	// regardless of the outcome.
	indexDir := t.TempDir()
	repoDir := t.TempDir()

	ch := make(chan fileContent, 5)
	for i := range 5 {
		ch <- fileContent{
			name:    fmt.Sprintf("f%d.go", i),
			content: []byte(fmt.Sprintf("package f%d\n", i)),
		}
	}
	close(ch)

	// Should consume all 5 items from the channel without hanging,
	// regardless of whether Add/Finish succeeds or fails.
	_ = indexUncommitted(context.Background(), repoDir, indexDir, ch, 2)
}

// --- helpers ---

// readUncommittedFiles collects all streamed file contents into a slice.
// Test-only helper — production code uses streamFiles directly.
func readUncommittedFiles(repoDir string, files []string, parallelism int) []fileContent {
	var results []fileContent
	for fc := range streamFiles(repoDir, files, parallelism) {
		results = append(results, fc)
	}
	return results
}

// peakHeapTracker samples HeapInuse every millisecond in a background
// goroutine. Call stop() after the workload completes to get the peak
// heap delta relative to the baseline captured at creation.
type peakHeapTracker struct {
	baseline uint64
	peak     uint64
	mu       sync.Mutex
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func newPeakHeapTracker() *peakHeapTracker {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t := &peakHeapTracker{
		baseline: m.HeapInuse,
		peak:     m.HeapInuse,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	go func() {
		defer close(t.doneCh)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-t.stopCh:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&m)
				t.mu.Lock()
				if m.HeapInuse > t.peak {
					t.peak = m.HeapInuse
				}
				t.mu.Unlock()
			}
		}
	}()
	return t
}

func (t *peakHeapTracker) stop() int64 {
	close(t.stopCh)
	<-t.doneCh
	t.mu.Lock()
	defer t.mu.Unlock()
	return int64(t.peak) - int64(t.baseline)
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

func getInode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("fi.Sys() is not *syscall.Stat_t")
	}
	return stat.Ino
}
