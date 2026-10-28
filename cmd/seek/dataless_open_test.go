package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// markDataless writes dir/cloud.txt with mode 000 and makes fileIsDataless
// report it as cloud-only. noOpen is true when the test user cannot open a
// mode-000 file.
// Then any open of cloud.txt fails, so a passing check proves no open. A user
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
// no bytes, with and without the binary probe. The bytes do not show an open:
// the probe counts a file that it cannot open as binary, so 0 bytes too.
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

// TestOnDiskPathOfFileReadsTheDirectory checks that onDiskPath gives a file
// and its directory their stored letter case with no open of the file. The
// file has mode 000, so an open by the test user fails, and onDiskPath would
// give the typed path back. On a case-sensitive file system, the mistyped
// path names no file, and onDiskPath gives it back unchanged.
func TestOnDiskPathOfFileReadsTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Dir")
	if _, noOpen := markDataless(t, dir); !noOpen {
		t.Fatal("the test user opens a mode-000 file, so the test cannot prove that onDiskPath does not open it")
	}
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
