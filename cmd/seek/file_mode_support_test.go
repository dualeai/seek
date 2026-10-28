package main

import (
	"io/fs"
	"os"
	"testing"
)

// chmodUnreadableOrFatal sets mode on path, a file or a folder, and puts the
// old mode back when the test ends. mode has no read bit, so a test user who
// obeys file modes cannot open path. The helper fails the test when the open
// works, as it does for root: the test needs a path that the user cannot read.
func chmodUnreadableOrFatal(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, info.Mode().Perm()) })
	if file, err := os.Open(path); err == nil {
		_ = file.Close()
		t.Fatalf("the test user opens %s at mode %#o, as root does, so the test cannot make it unreadable", path, mode)
	}
}
