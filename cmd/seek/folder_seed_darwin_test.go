package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFolderSeedThatWentAwayBuildsInFull builds a folder in full, then as a
// delta, so the family has two shards or more. It adds a file and puts a
// dangling symlink in place of the last live member, with a family manifest
// that still matches: the delta's seed then fails to link it (link(2) follows
// the symlink on darwin), as for a shard that went away between the scan and
// the seed. The next build is then a full build: one debug line for the
// seed failure, one shard, every current file found, the removed file not
// found, and no warning.
func TestFolderSeedThatWentAwayBuildsInFull(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.go", "package a\n// zqseeda\n")
	writeFileAt(t, folder, "b.go", "package b\n// zqseedb\n")
	plan := planFolderTestCorpus(t, folder)
	ctx := t.Context()
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(folder, "b.go")); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, folder, "c.go", "package c\n// zqseedc\n")
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	family := func() []string {
		t.Helper()
		scan, err := scanFamily(plan.indexDir)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(scan.members))
		for _, member := range scan.members {
			names = append(names, member.name)
		}
		return names
	}
	members := family()
	if len(members) < 2 {
		t.Fatalf("family %v, want the delta's later members", members)
	}
	writeFileAt(t, folder, "d.go", "package d\n// zqseedd\n")
	last := filepath.Join(plan.indexDir, members[len(members)-1])
	if err := os.Remove(last); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(last+".gone", last); err != nil {
		t.Fatal(err)
	}
	if err := writeFamilyManifest(plan.indexDir, family()); err != nil {
		t.Fatal(err)
	}
	if scan, err := scanFamily(plan.indexDir); err != nil || !scan.matchesManifest(plan.indexDir) {
		t.Fatalf("the family with the dangling member is not intact: %v", err)
	}
	logs := captureTestLogs(t, slog.LevelDebug)
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	seeds := 0
	for _, record := range logs.Records() {
		if record.Level >= slog.LevelWarn {
			t.Fatalf("the build after the seed error gave the warning %q", record.Message)
		}
		if record.Message == "Folder delta seed failed; full rebuild" {
			seeds++
		}
	}
	if seeds != 1 {
		t.Fatalf("seed fallbacks %d, want 1", seeds)
	}
	if got := family(); len(got) != 1 {
		t.Fatalf("family %v, want the one shard of a full build", got)
	}
	for pattern, want := range map[string]int{"zqseeda": 1, "zqseedb": 0, "zqseedc": 1, "zqseedd": 1} {
		files, err := searchPlannedCorpusForTest(ctx, plan, pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != want {
			names := make([]string, 0, len(files))
			for _, file := range files {
				names = append(names, file.FileName)
			}
			t.Fatalf("%s found %s, want %d files", pattern, strings.Join(names, ", "), want)
		}
	}
}
