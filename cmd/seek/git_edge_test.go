package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ===========================================================================
// NORMAL CASES — expected workflows not yet covered
// ===========================================================================

// TestIntegration_DetachedHEAD verifies seek works in detached HEAD state,
// which changes the branch.oid header format.
func TestIntegration_DetachedHEAD(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// detached_marker\n")

	// Detach HEAD
	gitRun(t, dir, "checkout", "--detach", "HEAD")

	files, err := runSeekInRepo(t, dir, "detached_marker")
	if err != nil {
		t.Fatalf("search failed in detached HEAD: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match in detached HEAD state")
	}
}

// TestIntegration_InitialCommitEmpty verifies seek handles a repo with only
// an initial commit containing a single empty tree (e.g. git commit --allow-empty).
func TestIntegration_InitialCommitEmpty(t *testing.T) {
	requireTools(t)

	dir := t.TempDir()
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "remote", "add", "origin", "https://github.com/test/repo.git"},
		{"git", "commit", "--allow-empty", "-m", "empty initial"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}
	ensureGitExclude(fallbackGitPaths(dir), cacheDir)

	// Add an untracked file — should be findable even with empty initial commit
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package main\n// empty_initial_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "empty_initial_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for untracked file in repo with empty initial commit")
	}
}

// TestIntegration_StagedButNotCommitted verifies that staged (git add) but
// not yet committed files are detected by git status and searchable.
func TestIntegration_StagedButNotCommitted(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// original\n")

	// Create new file and stage it
	if err := os.WriteFile(filepath.Join(dir, "staged.go"), []byte("package main\n// staged_only_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "staged.go")

	files, err := runSeekInRepo(t, dir, "staged_only_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for staged-but-not-committed file")
	}
}

// TestIntegration_MultipleBranches verifies seek indexes HEAD correctly
// when switching branches.
func TestIntegration_MultipleBranches(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// main_branch_marker\n")
	defaultBranch := gitCurrentBranch(t, dir)

	// Create and switch to a new branch with different content
	gitRun(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// feature_branch_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "app.go")
	gitRun(t, dir, "commit", "-m", "feature change")

	// Search should find feature branch content
	files, err := runSeekInRepo(t, dir, "feature_branch_marker")
	if err != nil {
		t.Fatalf("search on feature branch failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for feature branch content")
	}

	// Switch back to main — search should find main content
	gitRun(t, dir, "checkout", defaultBranch)

	files, err = runSeekInRepo(t, dir, "main_branch_marker")
	if err != nil {
		t.Fatalf("search on default branch after branch switch failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for main branch content after switch")
	}
}

// TestIntegration_NestedDirectoryStructure verifies deeply nested committed
// paths are correctly indexed and searchable.
func TestIntegration_NestedDirectoryStructure(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "root.go", "package main\n// root_marker\n")

	// Create deeply nested file and commit it — untracked nested dirs may
	// appear as a single directory entry in git status (depending on
	// showUntrackedFiles setting), so committed files are the reliable test.
	nested := filepath.Join(dir, "a", "b", "c", "d", "e")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.go"), []byte("package main\n// deep_nested_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "add nested file")

	files, err := runSeekInRepo(t, dir, "deep_nested_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for deeply nested file")
	}
}

// ===========================================================================
// EDGE CASES — boundary conditions and unusual-but-valid states
// ===========================================================================

// TestParseGitStatusV2_TrailingGarbageAfterLastNul verifies that content
// after the last NUL byte (no final NUL terminator) is safely ignored.
func TestParseGitStatusV2_TrailingGarbageAfterLastNul(t *testing.T) {
	raw := "# branch.oid abc123\x00? file.go\x00trailing garbage without nul"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "abc123" {
		t.Errorf("expected HeadSHA abc123, got %q", state.HeadSHA)
	}
	// trailing garbage has no NUL terminator so it should be ignored
	assertContains(t, state.Files, "file.go")
	if len(state.Files) != 1 {
		t.Errorf("expected 1 file, got %d: %v", len(state.Files), state.Files)
	}
}

// TestParseGitStatusV2_ConsecutiveNulBytes verifies empty records between
// NUL bytes are skipped (len < 2).
func TestParseGitStatusV2_ConsecutiveNulBytes(t *testing.T) {
	raw := "# branch.oid abc\x00\x00\x00? file.go\x00\x00"
	state := parseGitStatusV2(raw)
	if state.HeadSHA != "abc" {
		t.Errorf("expected HeadSHA abc, got %q", state.HeadSHA)
	}
	assertContains(t, state.Files, "file.go")
	if len(state.Files) != 1 {
		t.Errorf("expected 1 file, got %d: %v", len(state.Files), state.Files)
	}
}

// TestParseGitStatusV2_PathWithSpacesAndUnicode verifies real-world
// Unicode filenames are preserved through parsing.
func TestParseGitStatusV2_PathWithSpacesAndUnicode(t *testing.T) {
	paths := []string{
		"dir with spaces/file name.go",
		"café/résumé.txt",
		"日本語/ファイル.go",
		"emoji_🎉/test.go",
	}
	var raw strings.Builder
	raw.WriteString("# branch.oid abc123\x00")
	for _, p := range paths {
		fmt.Fprintf(&raw, "? %s\x00", p)
	}
	state := parseGitStatusV2(raw.String())
	for _, p := range paths {
		assertContains(t, state.Files, p)
	}
}

// TestExtractV2Path_TooFewFields verifies that when entry has fewer fields
// than skipFields, empty string is returned (not a panic).
func TestExtractV2Path_TooFewFields(t *testing.T) {
	cases := []struct {
		entry      string
		skipFields int
	}{
		{"1 .M N...", 8},                // only 3 fields, need 8
		{"", 1},                         // empty entry
		{"single", 2},                   // 1 field, need 2
		{"a b c", 10},                   // 3 fields, need 10
		{"1 .M N... 100644", 8},         // only 4 fields, need 8
		{"u UU N... 100644 100644", 10}, // only 5 fields, need 10
	}
	for _, tc := range cases {
		result := extractV2Path(tc.entry, tc.skipFields)
		if result != "" {
			t.Errorf("extractV2Path(%q, %d) = %q, expected empty", tc.entry, tc.skipFields, result)
		}
	}
}

// TestRepoStateFingerprint_EmptyFiles verifies fingerprint with zero
// dirty files returns raw output unchanged.
func TestRepoStateFingerprint_EmptyFiles(t *testing.T) {
	state := repoState{
		HeadSHA:   "abc123",
		RawOutput: "# branch.oid abc123\x00",
		Files:     nil,
	}
	fp := repoStateFingerprint(t.TempDir(), state)
	if fp != state.RawOutput {
		t.Error("expected fingerprint to equal raw output when no files")
	}
}

// TestRepoStateFingerprint_AllFilesDeleted verifies that when all dirty
// files are deleted between git status and fingerprinting, each gets the
// "deleted" sentinel.
func TestRepoStateFingerprint_AllFilesDeleted(t *testing.T) {
	dir := t.TempDir()
	state := repoState{
		RawOutput: "raw",
		Files:     []string{"gone1.go", "gone2.go", "gone3.go"},
	}
	fp := repoStateFingerprint(dir, state)
	for _, f := range state.Files {
		sentinel := fmt.Sprintf("\x00%s\x00deleted\x00", f)
		if !strings.Contains(fp, sentinel) {
			t.Errorf("missing deleted sentinel for %q", f)
		}
	}
}

// TestStateFile_CorruptContent verifies that a corrupted state file
// (non-hex content) is treated as a cache miss (different from any valid hash).
func TestStateFile_CorruptContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("CORRUPT!@#$%"), 0o644); err != nil {
		t.Fatal(err)
	}
	cached := readStateFile(dir)
	// Corrupt content is read as-is (trimmed); it simply won't match any valid hash
	if cached == "" {
		t.Error("expected non-empty read of corrupt state file")
	}
	// Verify it doesn't match a real hash
	realHash := computeStateHash("anything")
	if cached == realHash {
		t.Error("corrupt state file should not match a valid hash")
	}
}

// TestStateFile_PartialWrite verifies behavior when the tmp file exists
// but the rename hasn't happened yet (simulating a crash mid-write).
func TestStateFile_PartialWrite(t *testing.T) {
	dir := t.TempDir()

	// Write only the tmp file (simulating crash before rename)
	if err := os.WriteFile(filepath.Join(dir, stateTmpFile), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	// readStateFile should return empty (only reads .state, not .state.tmp)
	cached := readStateFile(dir)
	if cached != "" {
		t.Errorf("expected empty state when only tmp file exists, got %q", cached)
	}

	// deleteStateFiles should clean up the orphan
	deleteStateFiles(dir)
	if _, err := os.Stat(filepath.Join(dir, stateTmpFile)); !os.IsNotExist(err) {
		t.Error("expected .state.tmp to be deleted")
	}
}

// TestReadUncommittedFiles_FIFOSkipped verifies that FIFO (named pipe) files
// are skipped without blocking or error.
func TestReadUncommittedFiles_FIFOSkipped(t *testing.T) {
	dir := t.TempDir()

	// Create a regular file alongside
	if err := os.WriteFile(filepath.Join(dir, "regular.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a FIFO — readUncommittedFiles should not block on it.
	// Lstat returns ModeNamedPipe which is not ModeDir or ModeSymlink,
	// so it passes the type check. But ReadFile on a FIFO blocks.
	// This test documents whether the current code handles this.
	fifoPath := filepath.Join(dir, "fifo")
	cmd := exec.Command("mkfifo", fifoPath)
	if err := cmd.Run(); err != nil {
		t.Skipf("mkfifo not available: %v", err)
	}

	// Use a timeout to detect if readUncommittedFiles blocks on FIFO
	done := make(chan []fileContent, 1)
	go func() {
		done <- readUncommittedFiles(dir, []string{"regular.go", "fifo"}, 2)
	}()

	select {
	case results := <-done:
		// Should have at least the regular file
		found := false
		for _, r := range results {
			if r.name == "regular.go" {
				found = true
			}
		}
		if !found {
			t.Error("expected regular.go in results")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readUncommittedFiles blocked — likely stuck reading FIFO")
	}
}

// TestReadUncommittedFiles_HardlinkContent verifies hardlinked files
// are read correctly (content matches, different paths).
func TestReadUncommittedFiles_HardlinkContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.go")
	link := filepath.Join(dir, "hardlink.go")

	if err := os.WriteFile(src, []byte("package main\n// hardlink_content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, link); err != nil {
		t.Skipf("hardlinks not supported: %v", err)
	}

	results := readUncommittedFiles(dir, []string{"source.go", "hardlink.go"}, 2)
	if len(results) != 2 {
		t.Fatalf("expected 2 results for hardlinked files, got %d", len(results))
	}
	for _, r := range results {
		if string(r.content) != "package main\n// hardlink_content\n" {
			t.Errorf("unexpected content for %s: %q", r.name, r.content)
		}
	}
}

// TestRepoStateFingerprint_HardlinkSameInode verifies that hardlinked files
// have the same inode in the fingerprint (expected behavior — they ARE the
// same file at the filesystem level).
func TestRepoStateFingerprint_HardlinkSameInode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	link := filepath.Join(dir, "b.go")

	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, link); err != nil {
		t.Skipf("hardlinks not supported: %v", err)
	}

	state := repoState{
		RawOutput: "raw",
		Files:     []string{"a.go", "b.go"},
	}
	fp := repoStateFingerprint(dir, state)
	// Both files should produce fingerprint entries (even with same inode)
	if strings.Count(fp, "\x00a.go\x00") != 1 {
		t.Error("expected a.go in fingerprint")
	}
	if strings.Count(fp, "\x00b.go\x00") != 1 {
		t.Error("expected b.go in fingerprint")
	}
}

// ===========================================================================
// WEIRD CASES — unusual but possible states
// ===========================================================================

// TestIntegration_GitignoredFileNotIndexed verifies that .gitignore'd files
// don't appear in git status and thus aren't indexed as uncommitted.
func TestIntegration_GitignoredFileNotIndexed(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed\n")

	// Add .gitignore
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".gitignore")
	gitRun(t, dir, "commit", "-m", "add gitignore")

	// Create ignored file
	if err := os.MkdirAll(filepath.Join(dir, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored", "secret.go"), []byte("package secret\n// gitignored_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "gitignored_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("gitignored file should not appear in search results, got %v", files)
	}
}

// TestIntegration_EmptyFileCommittedAndUncommitted verifies that empty files
// (0 bytes) don't cause issues in either committed or uncommitted paths.
func TestIntegration_EmptyFileCommittedAndUncommitted(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// has_content\n")

	// Create empty committed file
	if err := os.WriteFile(filepath.Join(dir, "empty.go"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "empty.go")
	gitRun(t, dir, "commit", "-m", "add empty file")

	// Create empty untracked file
	if err := os.WriteFile(filepath.Join(dir, "empty_new.go"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should not crash; search should still work for the content file
	files, err := runSeekInRepo(t, dir, "has_content")
	if err != nil {
		t.Fatalf("search failed with empty files present: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for content file when empty files exist")
	}
}

// TestIntegration_BinaryFileDoesNotCorruptIndex verifies that binary files
// (with NUL bytes, random data) don't corrupt the index or cause panics.
func TestIntegration_BinaryFileDoesNotCorruptIndex(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// binary_test_marker\n")

	// Create a binary file with NUL bytes and random-ish data
	binary := make([]byte, 4096)
	for i := range binary {
		binary[i] = byte(i % 256)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}

	// Should not crash or corrupt
	files, err := runSeekInRepo(t, dir, "binary_test_marker")
	if err != nil {
		t.Fatalf("search failed with binary file present: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for text file when binary file exists")
	}
}

// TestIntegration_MergeConflictState verifies seek works during a merge
// conflict (unmerged entries in git status).
func TestIntegration_MergeConflictState(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// base_content\n")
	defaultBranch := gitCurrentBranch(t, dir)

	// Create a branch with a conflicting change
	gitRun(t, dir, "checkout", "-b", "conflict-branch")
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// conflict_branch_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "app.go")
	gitRun(t, dir, "commit", "-m", "branch change")

	// Go back and make a conflicting change on the default branch
	gitRun(t, dir, "checkout", defaultBranch)
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// master_conflict_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "app.go")
	gitRun(t, dir, "commit", "-m", "default branch change")

	// Attempt merge — will conflict
	cmd := exec.Command("git", "merge", "conflict-branch")
	cmd.Dir = dir
	_ = cmd.Run() // ignore error, conflict is expected

	// Seek should still work during merge conflict — should not crash
	_, err := runSeekInRepo(t, dir, "conflict")
	if err != nil {
		t.Fatalf("search during merge conflict should not error: %v", err)
	}
}

// TestIntegration_SubmoduleDirtySkipped verifies that a dirty submodule
// (which appears as a directory in git status) is skipped by
// readUncommittedFiles without error.
func TestIntegration_SubmoduleDirtySkipped(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// submodule_host_marker\n")

	// Create a sub-repo to use as submodule source
	subSrc := t.TempDir()
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = subSrc
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(subSrc, "sub.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, subSrc, "add", ".")
	gitRunIn(t, subSrc, "commit", "-m", "sub initial")

	// Allow local file transport and add submodule
	gitRun(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", subSrc, "mysub")
	gitRun(t, dir, "commit", "-m", "add submodule")

	// Make the submodule dirty
	if err := os.WriteFile(filepath.Join(dir, "mysub", "dirty.go"), []byte("package sub\n// dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seek should work — dirty submodule directory should be skipped
	files, err := runSeekInRepo(t, dir, "submodule_host_marker")
	if err != nil {
		t.Fatalf("search with dirty submodule failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for host repo content")
	}
}

// TestGitRepoState_CancelledContext verifies that a cancelled context
// returns a safe default state rather than panicking.
func TestGitRepoState_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	state := gitRepoState(ctx)
	if state.HeadSHA != "no-head" {
		t.Errorf("expected no-head for cancelled context, got %q", state.HeadSHA)
	}
	if len(state.Files) != 0 {
		t.Errorf("expected no files for cancelled context, got %v", state.Files)
	}
}

// TestEnsureGitExclude_Idempotent verifies that calling ensureGitExclude
// multiple times doesn't duplicate the pattern.
func TestEnsureGitExclude_Idempotent(t *testing.T) {
	dir := t.TempDir()
	// Create .git/info structure
	gitDir := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Call 5 times
	for range 5 {
		ensureGitExclude(fallbackGitPaths(dir), cacheDir)
	}

	data, err := os.ReadFile(filepath.Join(gitDir, "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	needle := "/" + cacheDir
	count := strings.Count(string(data), needle)
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of %q in exclude, got %d\ncontents: %q", needle, count, data)
	}
}

// TestEnsureGitExclude_PreExistingContentPreserved verifies that existing
// exclude patterns are preserved when adding the cache directory.
func TestEnsureGitExclude_PreExistingContentPreserved(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "*.log\n*.tmp\nbuild/\n"
	if err := os.WriteFile(filepath.Join(gitDir, "exclude"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	ensureGitExclude(fallbackGitPaths(dir), cacheDir)

	data, err := os.ReadFile(filepath.Join(gitDir, "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.HasPrefix(content, existing) {
		t.Errorf("existing content was not preserved: %q", content)
	}
	if !strings.Contains(content, "/"+cacheDir) {
		t.Errorf("cache pattern not added: %q", content)
	}
}

// TestEnsureGitExclude_NoTrailingNewline verifies that a file without
// trailing newline gets one added before the pattern.
func TestEnsureGitExclude_NoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No trailing newline
	if err := os.WriteFile(filepath.Join(gitDir, "exclude"), []byte("*.log"), 0o644); err != nil {
		t.Fatal(err)
	}

	ensureGitExclude(fallbackGitPaths(dir), cacheDir)

	data, err := os.ReadFile(filepath.Join(gitDir, "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	// Should have newline separator between existing content and new pattern
	if !strings.Contains(content, "*.log\n/"+cacheDir) {
		t.Errorf("expected newline separator: %q", content)
	}
}

// ===========================================================================
// OUT OF BOUNDS — stress, limits, and adversarial inputs
// ===========================================================================

// TestParseGitStatusV2_MassiveFileList verifies parsing with thousands
// of entries doesn't degrade or panic.
func TestParseGitStatusV2_MassiveFileList(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("# branch.oid abc123\x00")
	const numFiles = 10_000
	for i := range numFiles {
		fmt.Fprintf(&raw, "1 .M N... 100644 100644 100644 abc def src/pkg%d/file%d.go\x00", i/100, i)
	}
	state := parseGitStatusV2(raw.String())
	if state.HeadSHA != "abc123" {
		t.Errorf("expected HeadSHA abc123, got %q", state.HeadSHA)
	}
	if len(state.Files) != numFiles {
		t.Errorf("expected %d files, got %d", numFiles, len(state.Files))
	}
}

// TestRepoStateFingerprint_ManyDeletedFiles verifies fingerprinting with
// a large number of deleted files (all hitting the error path) is fast
// and correct.
func TestRepoStateFingerprint_ManyDeletedFiles(t *testing.T) {
	dir := t.TempDir()
	files := make([]string, 1000)
	for i := range files {
		files[i] = fmt.Sprintf("deleted_%d.go", i)
	}
	state := repoState{
		RawOutput: "raw",
		Files:     files,
	}

	start := time.Now()
	fp := repoStateFingerprint(dir, state)
	elapsed := time.Since(start)

	// All files are deleted — should get sentinel for each
	for _, f := range files {
		if !strings.Contains(fp, f+"\x00deleted\x00") {
			t.Errorf("missing deleted sentinel for %s", f)
			break
		}
	}
	// Should be fast (< 100ms for 1000 Lstats on non-existent files)
	if elapsed > 500*time.Millisecond {
		t.Errorf("fingerprinting 1000 deleted files took %v (expected < 500ms)", elapsed)
	}
}

// TestComputeStateHash_CollisionResistance verifies that similar inputs
// produce different hashes (not a crypto guarantee, but a sanity check).
func TestComputeStateHash_CollisionResistance(t *testing.T) {
	hashes := make(map[string]string)
	for i := range 10_000 {
		input := fmt.Sprintf("# branch.oid %040d\x00", i)
		h := computeStateHash(input)
		if prev, ok := hashes[h]; ok {
			t.Fatalf("collision: %q and %q both hash to %s", prev, input, h)
		}
		hashes[h] = input
	}
}

// TestReadUncommittedFiles_HighConcurrency verifies that reading many
// files with high parallelism doesn't miss or duplicate results.
func TestReadUncommittedFiles_HighConcurrency(t *testing.T) {
	dir := t.TempDir()
	const numFiles = 500
	for i := range numFiles {
		path := filepath.Join(dir, fmt.Sprintf("file_%03d.go", i))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("package main\n// marker_%03d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files := make([]string, numFiles)
	for i := range numFiles {
		files[i] = fmt.Sprintf("file_%03d.go", i)
	}

	results := readUncommittedFiles(dir, files, 16)
	if len(results) != numFiles {
		t.Errorf("expected %d results, got %d", numFiles, len(results))
	}

	// Verify no duplicates
	seen := make(map[string]bool)
	for _, r := range results {
		if seen[r.name] {
			t.Errorf("duplicate result for %s", r.name)
		}
		seen[r.name] = true
	}
}

// TestReadUncommittedFiles_RacyDeletion simulates files being deleted
// between git status and readUncommittedFiles (TOCTOU). Some files
// should be read, deleted ones should be silently skipped.
func TestReadUncommittedFiles_RacyDeletion(t *testing.T) {
	dir := t.TempDir()
	const numFiles = 100
	fileNames := make([]string, numFiles)
	for i := range numFiles {
		name := fmt.Sprintf("file_%03d.go", i)
		fileNames[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Delete half the files concurrently while reading
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numFiles; i += 2 {
			_ = os.Remove(filepath.Join(dir, fileNames[i]))
		}
	}()

	results := readUncommittedFiles(dir, fileNames, 4)
	wg.Wait()

	// Should not panic; should have between 50 and 100 results depending on timing
	if len(results) == 0 {
		t.Error("expected at least some results despite concurrent deletion")
	}
	if len(results) > numFiles {
		t.Errorf("more results than files: %d > %d", len(results), numFiles)
	}
}

// TestAcquireLock_ContextCancellation verifies that lock acquisition
// respects context cancellation during the polling loop.
func TestAcquireLock_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	// Hold exclusive lock
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		unlockFile(holder)
		_ = holder.Close()
	}()
	if err := lockFileExclusive(holder); err != nil {
		t.Fatal(err)
	}

	// No shards — acquireLock will poll
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, acquired, err := acquireLock(ctx, dir, lockPath)
	elapsed := time.Since(start)

	if acquired {
		t.Error("should not have acquired lock while held")
	}
	if err == nil {
		t.Error("expected error on timeout")
	}
	// Should respect the 200ms context timeout, not wait the full 60s
	if elapsed > 2*time.Second {
		t.Errorf("took %v to timeout, expected ~200ms", elapsed)
	}
}

// TestIntegration_VeryLargeUncommittedFile verifies that files exceeding
// maxUncommittedFileSize are skipped but other files are still indexed.
func TestIntegration_VeryLargeUncommittedFile(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// normal_content_marker\n")

	// Create a file just over the limit
	large := make([]byte, maxUncommittedFileSize+1)
	for i := range large {
		large[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, "huge.dat"), large, 0o644); err != nil {
		t.Fatal(err)
	}

	// Also create a small untracked file
	if err := os.WriteFile(filepath.Join(dir, "small.go"), []byte("package main\n// small_file_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should index the small file, skip the large one
	files, err := runSeekInRepo(t, dir, "small_file_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for small file when large file is present")
	}
}

// TestIntegration_ConcurrentSeekInvocations simulates multiple seek
// processes running against the same repo simultaneously.
func TestIntegration_ConcurrentSeekInvocations(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// concurrent_seek_marker\n")

	const goroutines = 8
	var wg sync.WaitGroup
	errors := make(chan error, goroutines)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			files, err := runSeekInRepo(t, dir, "concurrent_seek_marker")
			if err != nil {
				errors <- err
				return
			}
			if len(files) == 0 {
				errors <- fmt.Errorf("no results found")
			}
		}()
	}

	wg.Wait()
	close(errors)
	for err := range errors {
		t.Errorf("concurrent seek failed: %v", err)
	}
}

// TestIntegration_UnicodeFilenames verifies that files with Unicode names
// are correctly detected via git status -z and indexed.
func TestIntegration_UnicodeFilenames(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// base\n")

	// Create file with Unicode name
	if err := os.WriteFile(filepath.Join(dir, "café.go"), []byte("package main\n// unicode_filename_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "unicode_filename_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for file with Unicode name")
	}
}

// TestParseGitStatusV2_PathWithNewlineInName verifies that -z format
// correctly handles filenames containing literal newlines (which would
// break line-based parsing).
func TestParseGitStatusV2_PathWithNewlineInName(t *testing.T) {
	// With -z, the newline in the path is just a regular byte
	raw := "# branch.oid abc123\x00? file\nwith\nnewlines.go\x00"
	state := parseGitStatusV2(raw)
	assertContains(t, state.Files, "file\nwith\nnewlines.go")
}

// ===========================================================================
// GIT CONFIG INFLUENCE — user config that changes git status output
// ===========================================================================

// TestIntegration_CoreQuotePath verifies that core.quotePath=true doesn't
// break parsing of non-ASCII filenames. When quotePath is true, git
// C-escapes non-ASCII bytes in human-readable output, but -z format uses
// raw bytes.
func TestIntegration_CoreQuotePath(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// base\n")
	gitRun(t, dir, "config", "core.quotePath", "true")

	// Create file with non-ASCII name
	if err := os.WriteFile(filepath.Join(dir, "données.go"), []byte("package main\n// quotepath_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "quotepath_marker")
	if err != nil {
		t.Fatalf("search with core.quotePath=true failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match — core.quotePath should not affect -z format")
	}
}

// TestIntegration_StatusShowUntrackedFilesNo verifies behavior when user
// has status.showUntrackedFiles=no. Untracked files won't appear in
// git status, so they won't be indexed as uncommitted.
func TestIntegration_StatusShowUntrackedFilesNo(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_marker\n")
	gitRun(t, dir, "config", "status.showUntrackedFiles", "no")

	// Create untracked file
	if err := os.WriteFile(filepath.Join(dir, "untracked.go"), []byte("package main\n// invisible_untracked_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "invisible_untracked_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	// With showUntrackedFiles=no, the untracked file is invisible to seek.
	// This documents the current behavior — not necessarily desired.
	if len(files) != 0 {
		t.Log("note: untracked file was found despite status.showUntrackedFiles=no")
	} else {
		t.Log("confirmed: status.showUntrackedFiles=no hides untracked files from seek")
	}
}
