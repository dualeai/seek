package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestNamesFromEntriesOnThisVolume checks that storedName reads the stored
// name from the metadata: on a case-insensitive volume, a lookup as
// STORED.TXT gives Stored.txt. On a case-sensitive volume, STORED.TXT names no
// file, and storedName gives no name. The temporary volume must be APFS or
// HFS+.
func TestNamesFromEntriesOnThisVolume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Stored.txt")
	writeFixtureFile(t, path, "x\n")
	if !namesFromEntries(path) {
		t.Fatalf("the temporary directory %s is not on APFS or HFS+", filepath.Dir(path))
	}
	typed := filepath.Join(filepath.Dir(path), "STORED.TXT")
	name, ok := storedName(typed)
	if _, err := os.Lstat(typed); errors.Is(err, fs.ErrNotExist) {
		if ok {
			t.Fatalf("storedName=%q for a name that the case-sensitive volume does not hold, want none", name)
		}
		return
	} else if err != nil {
		t.Fatal(err)
	}
	if !ok || name != "Stored.txt" {
		t.Fatalf("storedName=%q ok=%t, want Stored.txt", name, ok)
	}
}
