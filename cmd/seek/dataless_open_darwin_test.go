package main

import (
	"path/filepath"
	"testing"
)

// TestNamesFromEntriesOnThisVolume checks that storedName reads the stored
// name from the metadata: a lookup as STORED.TXT gives Stored.txt. It skips
// when the temporary volume is not APFS or HFS+.
func TestNamesFromEntriesOnThisVolume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Stored.txt")
	writeFixtureFile(t, path, "x\n")
	if !namesFromEntries(path) {
		t.Skip("the temporary directory is not on APFS or HFS+")
	}
	if name, ok := storedName(filepath.Join(filepath.Dir(path), "STORED.TXT")); !ok || name != "Stored.txt" {
		t.Fatalf("storedName=%q ok=%t, want Stored.txt", name, ok)
	}
}
