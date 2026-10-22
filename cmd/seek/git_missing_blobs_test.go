package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestMissingBlobRecordRoundTrip checks that a record reads back as written,
// quoted paths included, and that an empty record removes the file.
func TestMissingBlobRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	oid := gitObjectID(strings.Repeat("a", 40))
	record := missingBlobRecord{head: strings.Repeat("b", 40), fingerprint: "count=0,in-pack=3", blobs: []missingBlob{
		{oid: oid, path: "dir/plain.txt"},
		{oid: oid, path: "dir/a name\nwith a line break \"quoted\".txt"},
	}}
	if err := writeMissingBlobRecord(dir, lexicalMissingBlobsFile, record); err != nil {
		t.Fatal(err)
	}
	got, ok := readMissingBlobRecord(dir, lexicalMissingBlobsFile)
	if !ok || !reflect.DeepEqual(got, record) {
		t.Fatalf("read %+v ok=%t, want %+v", got, ok, record)
	}
	// A build that read every blob removes the record.
	if err := writeMissingBlobRecord(dir, lexicalMissingBlobsFile, missingBlobRecord{head: record.head}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readMissingBlobRecord(dir, lexicalMissingBlobsFile); ok {
		t.Fatal("an empty record stayed on disk")
	}
}

// TestMissingBlobRecordDamaged checks that a damaged record, which cannot name
// its blobs, marks its data stale.
func TestMissingBlobRecordDamaged(t *testing.T) {
	oid := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	for name, body := range map[string]string{
		"empty":         "",
		"other version": "v2\nhead " + head + "\nstore s\n" + oid + " \"a\"\n",
		"no head":       "v1\nstore s\n" + oid + " \"a\"\n",
		"no store":      "v1\nhead " + head + "\n" + oid + " \"a\"\n",
		"bad object":    "v1\nhead " + head + "\nstore s\nnot-an-object \"a\"\n",
		"bad path":      "v1\nhead " + head + "\nstore s\n" + oid + " unquoted\n",
		"no objects":    "v1\nhead " + head + "\nstore s\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, semanticMissingBlobsFile), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok := readMissingBlobRecord(dir, semanticMissingBlobsFile)
			if !ok || got.head != "" || got.fingerprint != unknownObjectStore {
				t.Fatalf("read %+v ok=%t, want a stale record", got, ok)
			}
			if records := nodeMissingBlobRecords(dir, head, true); len(records) != 1 {
				t.Fatalf("records=%v, want the damaged semantic record", records)
			}
		})
	}
}

// TestPendingMissingBlobRecordKeepsADamagedRecord writes the pending record
// of a build over a live record: a valid one takes the union of both blob
// sets with a store that forces a check, and a damaged one stays damaged, so
// a stop before the publish still makes the next search rebuild the data,
// whether the build read missing blobs or none.
func TestPendingMissingBlobRecordKeepsADamagedRecord(t *testing.T) {
	head := strings.Repeat("b", 40)
	live := missingBlob{oid: gitObjectID(strings.Repeat("a", 40)), path: "live.txt"}
	built := missingBlob{oid: gitObjectID(strings.Repeat("c", 40)), path: "built.txt"}
	for _, row := range []struct {
		name  string
		blobs []missingBlob
	}{
		{name: "build read missing blobs", blobs: []missingBlob{built}},
		{name: "build read every blob"},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, lexicalMissingBlobsFile), []byte("v1\nstore s\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := writePendingMissingBlobRecord(dir, lexicalMissingBlobsFile, head, row.blobs); err != nil {
				t.Fatal(err)
			}
			if got, ok := readMissingBlobRecord(dir, lexicalMissingBlobsFile); !ok || got.head != "" {
				t.Fatalf("read %+v ok=%t, want the damaged record that marks the data stale", got, ok)
			}
		})
	}
	dir := t.TempDir()
	if err := writeMissingBlobRecord(dir, lexicalMissingBlobsFile, missingBlobRecord{head: head, fingerprint: "s", blobs: []missingBlob{live}}); err != nil {
		t.Fatal(err)
	}
	if err := writePendingMissingBlobRecord(dir, lexicalMissingBlobsFile, head, []missingBlob{built}); err != nil {
		t.Fatal(err)
	}
	got, ok := readMissingBlobRecord(dir, lexicalMissingBlobsFile)
	if want := []missingBlob{built, live}; !ok || got.fingerprint != unknownObjectStore || !reflect.DeepEqual(got.blobs, want) {
		t.Fatalf("read %+v ok=%t, want blobs %v with an unknown store", got, ok, want)
	}
}

// TestDeltaMissingBlobsDropsChangedPaths keeps the base blobs of the paths
// that a delta did not change and adds the delta's own: the blob of a deleted
// or changed path does not force a full build when it arrives.
func TestDeltaMissingBlobsDropsChangedPaths(t *testing.T) {
	kept := missingBlob{oid: gitObjectID(strings.Repeat("a", 40)), path: "kept.txt"}
	deleted := missingBlob{oid: gitObjectID(strings.Repeat("b", 40)), path: "gone.txt"}
	shared := missingBlob{oid: deleted.oid, path: "same-content.txt"}
	added := gitBlobInfo{entry: gitTreeEntry{oid: gitObjectID(strings.Repeat("c", 40)), path: "new.txt"}, missing: true}
	read := gitBlobInfo{entry: gitTreeEntry{oid: gitObjectID(strings.Repeat("d", 40)), path: "read.txt"}}
	got := deltaMissingBlobs([]missingBlob{kept, deleted, shared}, []string{"gone.txt"}, []gitBlobInfo{added, read})
	want := []missingBlob{{oid: added.entry.oid, path: "new.txt"}, kept, shared}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delta record=%v, want %v", got, want)
	}
}

// TestObjectStoreFingerprintSeesAlternates gives a repository an alternate
// object store: an object that arrives there changes the fingerprint, which
// is a real one, not the always-checked unknown store.
func TestObjectStoreFingerprintSeesAlternates(t *testing.T) {
	requireGit(t)
	repo := initGitRepo(t, "a.txt", "a\n")
	alternate := initGitRepo(t, "b.txt", "b\n")
	objects := filepath.Join(alternate, ".git", "objects")
	writeFixtureFile(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), objects+"\n")
	before, err := gitObjectStoreFingerprint(t.Context(), repo)
	if err != nil || before == unknownObjectStore || !strings.Contains(before, "alternate=") {
		t.Fatalf("fingerprint=%q err=%v, want one with the alternate", before, err)
	}
	if again, _ := gitObjectStoreFingerprint(t.Context(), repo); again != before {
		t.Fatalf("an unchanged store gave %q then %q", before, again)
	}
	writeFixtureFile(t, filepath.Join(alternate, "late.txt"), "a blob that arrives in the alternate\n")
	gitRunIn(t, alternate, "hash-object", "-w", "late.txt")
	if after, _ := gitObjectStoreFingerprint(t.Context(), repo); after == before {
		t.Fatal("an object that arrived in the alternate left the fingerprint as it was")
	}
}
