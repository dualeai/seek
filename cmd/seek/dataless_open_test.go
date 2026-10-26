package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// markDataless makes fileIsDataless report cloud.txt as cloud-only and its
// mode 000. noOpen is true when the test user cannot open a mode-000 file:
// then any open of cloud.txt fails, so a passing check proves no open. A user
// who ignores file modes, as root does, opens it.
func markDataless(t *testing.T, dir string) (path string, noOpen bool) {
	t.Helper()
	path = filepath.Join(dir, "cloud.txt")
	writeFixtureFile(t, path, "cloud bytes\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	noOpen = true
	if file, err := os.Open(path); err == nil {
		_ = file.Close()
		noOpen = false
	}
	realDataless := fileIsDataless
	fileIsDataless = func(name string, st *unix.Stat_t) bool {
		return filepath.Base(name) == "cloud.txt" || realDataless(name, st)
	}
	t.Cleanup(func() { fileIsDataless = realDataless })
	return path, noOpen
}

// TestDirtyContentBytesSkipsDatalessFiles checks that a cloud-only file adds
// no bytes and gets no open, with and without the binary probe. For a user who
// ignores file modes, the test checks the bytes only.
func TestDirtyContentBytesSkipsDatalessFiles(t *testing.T) {
	dir := t.TempDir()
	markDataless(t, dir)
	writeFixtureFile(t, filepath.Join(dir, "local.txt"), "local\n")
	for _, probe := range []bool{false, true} {
		if got := dirtyContentBytes(dir, []string{"cloud.txt", "local.txt"}, probe); got != int64(len("local\n")) {
			t.Fatalf("probe=%t: bytes=%d, want %d", probe, got, len("local\n"))
		}
	}
}

// TestStatOperandDoesNotOpenDatalessFiles checks that a cloud-only file
// operand resolves as a regular file with no open.
func TestStatOperandDoesNotOpenDatalessFiles(t *testing.T) {
	path, noOpen := markDataless(t, t.TempDir())
	if !noOpen {
		t.Fatal("the test user opens a mode-000 file, so the test cannot prove that statOperand does not open it")
	}
	if _, info, err := statOperand(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("statOperand: info=%v err=%v", info, err)
	}
}

// TestOnDiskPathOfFileReadsTheDirectory checks that onDiskPath gives a
// cloud-only file and its directory their stored letter case, with no open of
// the file. On a case-sensitive file system, the mistyped path names no file,
// and onDiskPath gives it back unchanged.
func TestOnDiskPathOfFileReadsTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Dir")
	markDataless(t, dir)
	root, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	typed := filepath.Join(root, "dir", "CLOUD.txt")
	want := filepath.Join(root, "Dir", "cloud.txt")
	if _, err := os.Lstat(typed); errors.Is(err, fs.ErrNotExist) {
		want = typed
	} else if err != nil {
		t.Fatal(err)
	}
	if got := onDiskPath(typed); got != want {
		t.Fatalf("onDiskPath=%q, want %q", got, want)
	}
}
