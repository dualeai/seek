package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// markDataless makes fileIsDataless report cloud.txt as cloud-only and its
// mode 000, so any open of it fails: a passing check proves no open.
func markDataless(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "cloud.txt")
	writeFixtureFile(t, path, "cloud bytes\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if file, err := os.Open(path); err == nil {
		_ = file.Close()
		t.Skip("the test user can open a mode 000 file")
	}
	realDataless := fileIsDataless
	fileIsDataless = func(name string, st *unix.Stat_t) bool {
		return filepath.Base(name) == "cloud.txt" || realDataless(name, st)
	}
	t.Cleanup(func() { fileIsDataless = realDataless })
	return path
}

// TestDirtyContentBytesSkipsDatalessFiles checks that a cloud-only file adds
// no bytes and gets no open, with and without the binary probe.
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
	path := markDataless(t, t.TempDir())
	if _, info, err := statOperand(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("statOperand: info=%v err=%v", info, err)
	}
}

// TestOnDiskPathOfFileReadsTheDirectory checks that onDiskPath gives a
// cloud-only file and its directory their stored letter case, with no open of
// the file.
func TestOnDiskPathOfFileReadsTheDirectory(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only macOS stores a letter case apart from the typed one")
	}
	dir := filepath.Join(t.TempDir(), "Dir")
	markDataless(t, dir)
	root, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := onDiskPath(filepath.Join(root, "dir", "CLOUD.txt")), filepath.Join(root, "Dir", "cloud.txt"); got != want {
		t.Fatalf("onDiskPath=%q, want %q", got, want)
	}
}
