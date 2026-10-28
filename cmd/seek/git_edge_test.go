package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// ===========================================================================
// NORMAL CASES: common workflows
// ===========================================================================

// TestIntegration_DetachedHEAD verifies seek works in detached HEAD state,
// where git status prints "# branch.head (detached)".
func TestIntegration_DetachedHEAD(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// detached_marker\n")

	// Detach HEAD
	gitRunIn(t, dir, "checkout", "--detach", "HEAD")

	files, err := runSeekInRepo(t, dir, "detached_marker")
	if err != nil {
		t.Fatalf("search failed in detached HEAD: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match in detached HEAD state")
	}
}

// TestIntegration_InitialCommitEmpty searches a repository whose one commit
// has an empty tree (git commit --allow-empty). The search finds its
// untracked file.
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
	// An untracked file beside the empty commit.
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

func TestRun_EmptyGitRepoReturnsNoMatch(t *testing.T) {
	requireTools(t)

	dir := t.TempDir()
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "empty initial"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}

	setTestUserCache(t)
	t.Chdir(dir)

	_, err := seek(t, "--lexical-only", "absent_empty_repo_marker")
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("expected no match for empty git repo, got %v", err)
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
	gitRunIn(t, dir, "add", "staged.go")

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
	defaultBranch := gitOutputIn(t, dir, "symbolic-ref", "--quiet", "--short", "HEAD")

	// Create and switch to a new branch with different content
	gitRunIn(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// feature_branch_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", "app.go")
	gitRunIn(t, dir, "commit", "-m", "feature change")

	// Search should find feature branch content
	files, err := runSeekInRepo(t, dir, "feature_branch_marker")
	if err != nil {
		t.Fatalf("search on feature branch failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for feature branch content")
	}

	// Switch back to the default branch: the search finds its content.
	gitRunIn(t, dir, "checkout", defaultBranch)

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

	// Create deeply nested file and commit it.
	nested := filepath.Join(dir, "a", "b", "c", "d", "e")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.go"), []byte("package main\n// deep_nested_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", ".")
	gitRunIn(t, dir, "commit", "-m", "add nested file")

	files, err := runSeekInRepo(t, dir, "deep_nested_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for deeply nested file")
	}
}

func TestIntegration_NestedUntrackedFileSearchable(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "root.go", "package main\n// root_marker\n")
	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "new.go"), []byte("package main\n// nested_untracked_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "nested_untracked_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for nested untracked file")
	}
}

// ===========================================================================
// EDGE CASES: boundary conditions and unusual but valid states
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

// TestParseGitStatusV2_PathWithSpacesAndUnicode verifies that the parser
// keeps paths with spaces and Unicode characters unchanged.
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

func TestRepoStateFingerprint_AllFilesDeleted(t *testing.T) {
	dir := t.TempDir()
	state := repoState{
		RawOutput: "raw",
		Files:     []string{"gone1.go", "gone2.go", "gone3.go"},
	}
	fp := repoStateFingerprint(dir, state)
	if fp == state.RawOutput {
		t.Fatal("expected deleted files to affect fingerprint")
	}
	if fp != repoStateFingerprint(dir, state) {
		t.Fatal("expected deleted-file fingerprint to be deterministic")
	}

	if err := os.WriteFile(filepath.Join(dir, "gone2.go"), []byte("restored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fp == repoStateFingerprint(dir, state) {
		t.Fatal("expected restored file to change fingerprint")
	}
}

// TestStateFile_CorruptContent verifies that a corrupt state file (not hex)
// is a cache miss. The next freshness check builds again and writes the valid
// state back.
func TestStateFile_CorruptContent(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "app.go", "package main\n// corrupt_state_marker\n")
	paths, plan := planGitTestCorpus(t, dir)
	if _, err := ensureGitCorpusFresh(t.Context(), plan, paths); err != nil {
		t.Fatal(err)
	}
	valid := readStateFile(plan.cacheDir)
	if valid == "" {
		t.Fatal("the first build wrote no state")
	}
	if err := os.WriteFile(filepath.Join(plan.cacheDir, stateFile), []byte("CORRUPT!@#$%"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureGitCorpusFresh(t.Context(), plan, paths); err != nil {
		t.Fatal(err)
	}
	if got := readStateFile(plan.cacheDir); got != valid {
		t.Fatalf("state after a corrupt record=%q, want the rebuilt state %q", got, valid)
	}
}

// TestStateFile_PartialWrite verifies behavior when the tmp file exists
// but the rename hasn't happened yet (simulating a crash mid-write).
func TestStateFile_PartialWrite(t *testing.T) {
	dir := t.TempDir()

	// Write only the tmp file (simulating crash before rename)
	if err := os.WriteFile(filepath.Join(dir, ".state.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	// readStateFile should return empty (only reads .state, not .state.tmp)
	cached := readStateFile(dir)
	if cached != "" {
		t.Errorf("expected empty state when only tmp file exists, got %q", cached)
	}

	// deleteStateFiles should clean up the orphan
	deleteStateFiles(dir)
	if _, err := os.Stat(filepath.Join(dir, ".state.tmp")); !os.IsNotExist(err) {
		t.Error("expected .state.tmp to be deleted")
	}
}

// TestStreamDirtyFiles_FIFOSkipped verifies that the reader skips a FIFO
// (named pipe) with no block and no error. It sends no document for the FIFO,
// not even its name. It reads the regular file beside it.
func TestStreamDirtyFiles_FIFOSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "regular.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Lstat returns ModeNamedPipe, so the reader skips the FIFO before any
	// open: only a regular file passes. A plain open of a FIFO with no writer
	// blocks, so a reader that opens it stops the test at the go test
	// timeout.
	fifoPath := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		t.Fatal(err)
	}

	results := collectStreamFilesForTest(t, t.Context(), dir, []string{"regular.go", "fifo"}, 2)
	if len(results) != 1 || results[0].name != "regular.go" || string(results[0].content) != "package main\n" {
		t.Fatalf("results=%+v, want regular.go with its content only", results)
	}
}

// TestStreamDirtyFiles_HardlinkContent verifies hardlinked files
// are read correctly (content matches, different paths).
func TestStreamDirtyFiles_HardlinkContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.go")
	link := filepath.Join(dir, "hardlink.go")

	if err := os.WriteFile(src, []byte("package main\n// hardlink_content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, link); err != nil {
		t.Fatal(err)
	}

	results := collectStreamFilesForTest(t, t.Context(), dir, []string{"source.go", "hardlink.go"}, 2)
	if len(results) != 2 {
		t.Fatalf("expected 2 results for hardlinked files, got %d", len(results))
	}
	for _, r := range results {
		if string(r.content) != "package main\n// hardlink_content\n" {
			t.Errorf("unexpected content for %s: %q", r.name, r.content)
		}
	}
}

// TestRepoStateFingerprint_HardlinkSameInode fingerprints two hard links to
// one file. The links share an inode, but each path name goes into the
// fingerprint. Each link alone gives its own fingerprint, and the two links
// together give a third.
func TestRepoStateFingerprint_HardlinkSameInode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	link := filepath.Join(dir, "b.go")

	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, link); err != nil {
		t.Fatal(err)
	}

	state := repoState{
		RawOutput: "raw",
		Files:     []string{"a.go", "b.go"},
	}
	fpBoth := repoStateFingerprint(dir, state)
	fpA := repoStateFingerprint(dir, repoState{RawOutput: "raw", Files: []string{"a.go"}})
	fpB := repoStateFingerprint(dir, repoState{RawOutput: "raw", Files: []string{"b.go"}})
	if fpA == fpB {
		t.Fatal("expected hardlinked paths with different names to fingerprint differently")
	}
	if fpBoth == fpA || fpBoth == fpB {
		t.Fatal("expected both hardlinked paths to contribute to fingerprint")
	}
}

// ===========================================================================
// WEIRD CASES: unusual but possible states
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
	gitRunIn(t, dir, "add", ".gitignore")
	gitRunIn(t, dir, "commit", "-m", "add gitignore")

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
	gitRunIn(t, dir, "add", "empty.go")
	gitRunIn(t, dir, "commit", "-m", "add empty file")

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

// TestIntegration_BinaryFileDoesNotCorruptIndex verifies that an untracked
// binary file (every byte value, NUL included) does not stop the search of a
// text file.
func TestIntegration_BinaryFileDoesNotCorruptIndex(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// binary_test_marker\n")

	// 4 KiB that hold each byte value 16 times.
	binary := make([]byte, 4096)
	for i := range binary {
		binary[i] = byte(i % 256)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}

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
	defaultBranch := gitOutputIn(t, dir, "symbolic-ref", "--quiet", "--short", "HEAD")

	// Create a branch with a conflicting change
	gitRunIn(t, dir, "checkout", "-b", "conflict-branch")
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// conflict_branch_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", "app.go")
	gitRunIn(t, dir, "commit", "-m", "branch change")

	// Go back and make a conflicting change on the default branch
	gitRunIn(t, dir, "checkout", defaultBranch)
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// master_conflict_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", "app.go")
	gitRunIn(t, dir, "commit", "-m", "default branch change")

	// Try a merge. It conflicts.
	cmd := exec.Command("git", "merge", "conflict-branch")
	cmd.Dir = dir
	_ = cmd.Run() // ignore error, conflict is expected
	// Real Git output is the oracle: app.go is an unmerged ("u") entry.
	if status := gitOutputIn(t, dir, "status", "--porcelain=v2", "-z"); !strings.HasPrefix(status, "u UU ") {
		t.Fatalf("merge left no unmerged entry: %q", status)
	}

	// The work-tree app.go holds both sides between conflict markers. Only
	// the work-tree copy holds the branch side: HEAD holds the default
	// branch side.
	files, err := runSeekInRepo(t, dir, "conflict_branch_marker")
	if err != nil {
		t.Fatalf("search during merge conflict should not error: %v", err)
	}
	if len(files) != 1 || files[0] != "app.go" {
		t.Fatalf("files=%q, want the unmerged work-tree app.go", files)
	}
}

// TestIntegration_SubmoduleDirtySkipped verifies that a submodule with an
// untracked file does not stop a search of the host repository. Git status
// (--ignore-submodules=dirty) does not list the submodule, so the host's
// dirty-file reader never meets it.
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
	gitRunIn(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", subSrc, "mysub")
	gitRunIn(t, dir, "commit", "-m", "add submodule")

	// Make the submodule dirty
	if err := os.WriteFile(filepath.Join(dir, "mysub", "dirty.go"), []byte("package sub\n// dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seek must work and skip the dirty submodule directory.
	files, err := runSeekInRepo(t, dir, "submodule_host_marker")
	if err != nil {
		t.Fatalf("search with dirty submodule failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for host repo content")
	}
}

// TestGitRepoState_CancelledContext verifies that cancellation is not hidden as
// an empty repository state.
func TestGitRepoState_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := gitRepoStateIn(ctx, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

// ===========================================================================
// OUT OF BOUNDS: stress, limits, and adversarial inputs
// ===========================================================================

// TestParseGitStatusV2_MassiveFileList verifies that the parser keeps each of
// 10,000 changed entries.
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

// TestRepoStateFingerprint_ManyDeletedFiles fingerprints 1,000 deleted files,
// so each Lstat fails: the files change the fingerprint, and two calls give
// the same fingerprint.
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

	fp := repoStateFingerprint(dir, state)
	if fp == state.RawOutput {
		t.Fatal("expected deleted files to affect fingerprint")
	}
	if fp != repoStateFingerprint(dir, state) {
		t.Fatal("expected many-deleted-files fingerprint to be deterministic")
	}
}

// TestComputeStateHash_CollisionResistance verifies that 10,000 similar inputs
// give 10,000 different hashes. This is a basic check, not a cryptographic
// guarantee.
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

// TestStreamDirtyFiles_HighConcurrency verifies that reading many
// files with high parallelism doesn't miss or duplicate results.
func TestStreamDirtyFiles_HighConcurrency(t *testing.T) {
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

	results := collectStreamFilesForTest(t, t.Context(), dir, files, 16)
	if len(results) != numFiles {
		t.Errorf("expected %d results, got %d", numFiles, len(results))
	}

	// Verify no duplicates
	seen := make(map[string]struct{})
	for _, r := range results {
		if _, ok := seen[r.name]; ok {
			t.Errorf("duplicate result for %s", r.name)
		}
		seen[r.name] = struct{}{}
	}
}

// TestStreamDirtyFiles_RacyDeletion deletes the 50 even-numbered files of 100
// while the reader reads them, as a file can go between git status and the
// read. The reader reads each of the 50 files that stay. A deleted file gives
// no document, or its content when the read came first, and no error.
// TestStreamDirtyFiles_MixedExistingAndMissing checks a file that is gone
// before the read starts.
func TestStreamDirtyFiles_RacyDeletion(t *testing.T) {
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
	wg.Go(func() {
		for i := 0; i < numFiles; i += 2 {
			_ = os.Remove(filepath.Join(dir, fileNames[i]))
		}
	})

	results := collectStreamFilesForTest(t, t.Context(), dir, fileNames, 4)
	wg.Wait()

	// collectStreamFilesForTest fails the test on a read error.
	read := make(map[string]bool, len(results))
	for _, r := range results {
		if read[r.name] || string(r.content) != "content" {
			t.Fatalf("result %s: duplicate=%t content=%q skip=%v, want one result with the file content", r.name, read[r.name], r.content, r.skipReason)
		}
		read[r.name] = true
	}
	for i := 1; i < numFiles; i += 2 {
		if !read[fileNames[i]] {
			t.Errorf("%s was never deleted but has no result", fileNames[i])
		}
	}
}

// TestAcquireBuildLock_ContextCancellation verifies that build-lock acquisition
// respects context cancellation during the polling loop (cold path, no shards).
// At the debug line of the wait, the test cancels the context. The test keeps
// the lock until it ends, so the wait can end only through the context.
func TestAcquireBuildLock_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	indexDir := filepath.Join(dir, "index")

	// Hold the build lock so a second acquire must poll.
	holdBuildLock(t, dir)

	// No shards exist, so acquireBuildLock polls (it does not skip).
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitBuild, filepath.Join(dir, buildLockFile))
	acted := onLockWait(t, waiting, func() error { return nil }, cancel)

	_, acquired, err := corpusStore{cacheDir: dir, indexDir: indexDir}.acquireBuildLock(ctx)
	lockWaitActed(t, acted)
	if acquired {
		t.Error("should not have acquired build lock while held")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled", err)
	}
}

// TestIntegration_VeryLargeUncommittedFile verifies that other files are still
// indexed when an uncommitted file exceeds testDocumentContentLimit; that file
// is indexed by name only.
func TestIntegration_VeryLargeUncommittedFile(t *testing.T) {
	swapForTest(t, &documentContentLimit, testDocumentContentLimit)
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// normal_content_marker\n")

	// Create a file just over the limit
	large := make([]byte, testDocumentContentLimit+1)
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

	// The small file keeps its content; the large one has only its name.
	files, err := runSeekInRepo(t, dir, "small_file_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected match for small file when large file is present")
	}
	// The large file is found by its name and not by its content.
	named, err := seek(t, "--lexical-only", "file:huge.dat", dir)
	if err != nil || !strings.Contains(named, "huge.dat") {
		t.Fatalf("the large file is not found by name: err=%v\n%s", err, named)
	}
	content, err := seek(t, "--lexical-only", strings.Repeat("x", 32), dir)
	if err != nil && !errors.Is(err, errNoMatch) || strings.Contains(content, "huge.dat") {
		t.Fatalf("the content of the large file is indexed: err=%v\n%s", err, content)
	}
}

// TestCLIProcess_ConcurrentColdGitSearches starts eight search processes of one
// cold repository with one cache at the same time. One process builds, each
// other one waits for that build (acquireBuildLock), and each one prints the
// match. In one process, the build slots order the builds before the build
// lock does, so the test uses processes.
func TestCLIProcess_ConcurrentColdGitSearches(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "app.go", "package main\n// concurrent_seek_marker\n")
	cacheDir := t.TempDir()

	type run struct {
		stdout, stderr string
		err            error
	}
	runs := make([]run, 8)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Go(func() {
			cmd := cliProcessCommand(t.Context(), cacheDir, dir, []string{"--lexical-only", "concurrent_seek_marker"}, nil)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			runs[i] = run{stdout: stdout.String(), stderr: stderr.String(), err: err}
		})
	}
	wg.Wait()
	for i, r := range runs {
		if code := cliProcessExitCode(t, r.err); code != 0 || !strings.Contains(r.stdout, "## app.go") {
			t.Errorf("process %d: exit=%d stdout=%q stderr=%q, want exit 0 and app.go", i, code, r.stdout, r.stderr)
		}
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
// GIT CONFIG INFLUENCE: user config that changes git status output
// ===========================================================================

// TestIntegration_CoreQuotePath verifies that core.quotePath=true doesn't
// break parsing of non-ASCII filenames. When quotePath is true, git
// C-escapes non-ASCII bytes in human-readable output, but -z format uses
// raw bytes.
func TestIntegration_CoreQuotePath(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// base\n")
	gitRunIn(t, dir, "config", "core.quotePath", "true")

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

// TestIntegration_StatusShowUntrackedFilesNo verifies seek's explicit
// untracked-file mode is not weakened by user Git status configuration.
func TestIntegration_StatusShowUntrackedFilesNo(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_marker\n")
	gitRunIn(t, dir, "config", "status.showUntrackedFiles", "no")

	// Create untracked file
	if err := os.WriteFile(filepath.Join(dir, "untracked.go"), []byte("package main\n// invisible_untracked_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "invisible_untracked_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("status.showUntrackedFiles=no should not hide untracked files from seek")
	}
}

func TestIntegration_StatusShowUntrackedFilesNoNestedFileSearchable(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_marker\n")
	gitRunIn(t, dir, "config", "status.showUntrackedFiles", "no")

	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "new.go"), []byte("package main\n// config_nested_untracked_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := runSeekInRepo(t, dir, "config_nested_untracked_marker")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("status.showUntrackedFiles=no should not hide nested untracked files from seek")
	}
}
