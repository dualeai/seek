package main

import (
	"context"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestFolderBusyWriterColdSearchSucceeds edits a file after the walk capture
// and before the cold build ends. The search succeeds; the next search
// indexes the edit.
func TestFolderBusyWriterColdSearchSucceeds(t *testing.T) {
	requireTools(t)
	const marker = "zqbusywritermarker"
	f := newGraphFixture(t)
	f.graphFile("folder/a.txt", marker)
	setTestUserCache(t)
	edited := false
	folderBuildTestHook = func() {
		if !edited {
			edited = true
			writeFixtureFile(t, f.path("folder/a.txt"), marker+" folder/a.txt edited\n")
		}
	}
	t.Cleanup(func() { folderBuildTestHook = nil })

	if got := graphSearch(t, marker, f.path("folder")); len(got) != 1 {
		t.Fatalf("cold search found %v, want 1 file", got)
	}
	if !edited {
		t.Fatal("the build hook did not run")
	}
	out, err := seek(t, "--lexical-only", "edited", f.path("folder"))
	if err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("next search missed the edit: err=%v\n%s", err, out)
	}
}

// TestFolderUnreadableSubfolderWarns walks a folder with an unreadable
// subfolder: the other files are found, and one WARN gives the count and the
// first path.
func TestFolderUnreadableSubfolderWarns(t *testing.T) {
	requireTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	const marker = "zqunreadablemarker"
	f := newGraphFixture(t)
	f.graphFile("folder/ok.txt", marker)
	f.graphFile("folder/locked/hidden.txt", marker)
	locked := f.path("folder/locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	setTestUserCache(t)
	logs := captureTestLogs(t, slog.LevelWarn)

	if got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("folder")))); !slices.Equal(got, []string{"folder/ok.txt"}) {
		t.Fatalf("found %q, want only folder/ok.txt", got)
	}
	var warnings []string
	for _, record := range logs.Records() {
		attrs := testLogAttrs(record)
		if strings.Contains(record.Message, "could not be read") {
			warnings = append(warnings, record.Message)
			if attrs["folders"] != int64(1) || attrs["first"] != canonicalCorpusPath(locked) {
				t.Fatalf("warning attrs=%v, want folders=1 first=%s", attrs, locked)
			}
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d unreadable-folder warnings, want 1: %v", len(warnings), logs.Records())
	}
}

// TestFolderWalkVisitedSet reports two sibling directories with one
// (dev, ino), as two mounts of one directory would. The walk keeps the one
// with the smallest path, every time.
func TestFolderWalkVisitedSet(t *testing.T) {
	root := canonTempDir(t)
	writeFixtureFile(t, filepath.Join(root, "a", "x.txt"), "x\n")
	writeFixtureFile(t, filepath.Join(root, "b", "y.txt"), "y\n")
	realIdentity := folderDirIdentity
	folderDirIdentity = func(f *os.File) (dirIdentity, error) {
		if base := filepath.Base(f.Name()); base == "a" || base == "b" {
			return dirIdentity{dev: 1, ino: 1}, nil
		}
		return realIdentity(f)
	}
	t.Cleanup(func() { folderDirIdentity = realIdentity })
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for range 20 {
		capture, _, err := captureFolderNode(t.Context(), plan)
		if err != nil {
			t.Fatal(err)
		}
		if len(capture.selected) != 1 || capture.selected[0].name != "a/x.txt" {
			t.Fatalf("selected=%v, want only a/x.txt", capture.selected)
		}
		if first == "" {
			first = capture.stateHash
		} else if capture.stateHash != first {
			t.Fatal("the state hash depends on worker timing")
		}
	}
}

// TestFolderDatalessNameOnly marks a file as cloud-only (SF_DATALESS). The
// walk never reads it: it is searchable by name only, and counted.
func TestFolderDatalessNameOnly(t *testing.T) {
	requireTools(t)
	const marker = "zqdatalessmarker"
	f := newGraphFixture(t)
	f.graphFile("folder/local.txt", marker)
	f.graphFile("folder/cloud.txt", marker)
	realDataless := fileIsDataless
	fileIsDataless = func(name string, st *unix.Stat_t) bool {
		return name == "cloud.txt" || realDataless(name, st)
	}
	t.Cleanup(func() { fileIsDataless = realDataless })
	setTestUserCache(t)

	if got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("folder")))); !slices.Equal(got, []string{"folder/local.txt"}) {
		t.Fatalf("content search found %q, want only folder/local.txt", got)
	}
	out, err := seek(t, "--lexical-only", "file:cloud", f.path("folder"))
	if err != nil || !strings.Contains(out, "cloud.txt") {
		t.Fatalf("cloud-only file is not searchable by name: err=%v\n%s", err, out)
	}
	// The name-only notice counts the cloud-only file.
	out, err = seek(t, "--lexical-only", marker, f.path("folder"))
	if err != nil || !strings.Contains(out, "\n\n1 name-only file could also match") {
		t.Fatalf("the notice does not count the cloud-only file: err=%v\n%s", err, out)
	}
}

// TestFolderGitFileNeverIndexed puts `.git` files below a folder root and at
// its top. They name no repository, so the folder keeps its other files; the
// `.git` files themselves are never indexed.
func TestFolderGitFileNeverIndexed(t *testing.T) {
	requireTools(t)
	const marker = "zqgitfilemarker"
	f := newGraphFixture(t)
	f.graphFile("folder/.git", marker)
	f.graphFile("folder/sub/.git", marker)
	f.graphFile("folder/sub/a.txt", marker)
	setTestUserCache(t)
	if got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("folder")))); !slices.Equal(got, []string{"folder/sub/a.txt"}) {
		t.Fatalf("found %q, want only folder/sub/a.txt", got)
	}
}

// TestFolderSameSizeRewriteDetected rewrites a file with content of the same
// size and puts its old mtime back. The change time still moves, so the next
// search finds the new content.
func TestFolderSameSizeRewriteDetected(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	path := f.path("folder/f.txt")
	writeFixtureFile(t, path, "zqoldcontent1\n")
	setTestUserCache(t)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := seek(t, "--lexical-only", "zqoldcontent1", f.path("folder")); err != nil {
		t.Fatalf("first search: %v\n%s", err, out)
	}
	if err := os.WriteFile(path, []byte("zqnewcontent1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	out, err := seek(t, "--lexical-only", "zqnewcontent1", f.path("folder"))
	if err != nil || !strings.Contains(out, "f.txt") {
		t.Fatalf("same-size rewrite missed: err=%v\n%s", err, out)
	}
}

// TestFolderSeedBelowUnreadableDirStays names a folder and a seed below one
// of its directories that cannot be listed but can be entered. The outer walk
// does not reach the seed, so the seed stays its own node.
func TestFolderSeedBelowUnreadableDirStays(t *testing.T) {
	requireTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root lists a mode-0111 directory")
	}
	const marker = "zqunlistablemarker"
	f := newGraphFixture(t)
	f.graphFile("folder/ok.txt", marker)
	f.graphFile("folder/locked/inner/in.txt", marker)
	locked := f.path("folder/locked")
	if err := os.Chmod(locked, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	setTestUserCache(t)
	got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("folder"), f.path("folder/locked/inner"))))
	if want := []string{"folder/locked/inner/in.txt", "folder/ok.txt"}; !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
}

// TestFolderNestedSeedsInAnotherCase names a folder in another letter case
// and a folder below it. On a case-insensitive file system the inner seed is
// part of the outer node: each file is found once.
func TestFolderNestedSeedsInAnotherCase(t *testing.T) {
	requireTools(t)
	const marker = "zqnestedcasemarker"
	f := newGraphFixture(t)
	f.graphFile("folder/sub/a.txt", marker)
	upper := f.path("FOLDER")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("file system is case-sensitive")
	}
	setTestUserCache(t)
	got := slices.Sorted(maps.Keys(graphSearch(t, marker, upper, f.path("folder/sub"))))
	if want := []string{"folder/sub/a.txt"}; !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
}

// TestFolderWalkVisitedSetPrefixOrder reports two mounts of one directory at
// "a" and "a-b", and of its child at "a/s" and "a-b/s". "/" sorts after "-",
// so a whole-string order would pick "a" but "a-b/s". The component order
// picks "a" and "a/s" every run, at every worker count.
func TestFolderWalkVisitedSetPrefixOrder(t *testing.T) {
	root := canonTempDir(t)
	for _, dir := range []string{"a", "a-b"} {
		writeFixtureFile(t, filepath.Join(root, dir, "top.txt"), "top\n")
		writeFixtureFile(t, filepath.Join(root, dir, "s", "f.txt"), "f\n")
	}
	realIdentity := folderDirIdentity
	folderDirIdentity = func(f *os.File) (dirIdentity, error) {
		rel, _ := filepath.Rel(root, f.Name())
		switch rel {
		case "a", "a-b":
			// Jitter, so either sibling can claim first.
			time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond)
			return dirIdentity{dev: 1, ino: 1}, nil
		case filepath.Join("a", "s"), filepath.Join("a-b", "s"):
			return dirIdentity{dev: 1, ino: 2}, nil
		}
		return realIdentity(f)
	}
	t.Cleanup(func() { folderDirIdentity = realIdentity })
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]bool)
	for _, procs := range []int{1, 2, 4, 8} {
		old := runtime.GOMAXPROCS(procs)
		for range 50 {
			capture, _, err := captureFolderNode(t.Context(), plan)
			if err != nil {
				runtime.GOMAXPROCS(old)
				t.Fatal(err)
			}
			var names []string
			for _, candidate := range capture.selected {
				names = append(names, candidate.name)
			}
			if want := []string{"a/s/f.txt", "a/top.txt"}; !slices.Equal(names, want) {
				runtime.GOMAXPROCS(old)
				t.Fatalf("selected %q, want %q", names, want)
			}
			states[capture.stateHash] = true
		}
		runtime.GOMAXPROCS(old)
	}
	if len(states) != 1 {
		t.Fatalf("%d distinct states: the result depends on worker timing", len(states))
	}
}

// TestFolderUnreadableFileSearchableByName puts a mode-000 file below a folder
// root: its content cannot be read, but its name stays searchable.
func TestFolderUnreadableFileSearchableByName(t *testing.T) {
	requireTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	f := newGraphFixture(t)
	path := f.path("folder/locked.txt")
	writeFixtureFile(t, path, "secret\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	setTestUserCache(t)
	out, err := seek(t, "--lexical-only", "file:locked", f.path("folder"))
	if err != nil || !strings.Contains(out, "locked.txt") {
		t.Fatalf("unreadable file is not searchable by name: err=%v\n%s", err, out)
	}
}

// TestFolderUnreadableSubfolderWarnsOncePerCommand names a folder and a
// folder below it: both walks meet one unreadable folder, and the command
// warns once.
func TestFolderUnreadableSubfolderWarnsOncePerCommand(t *testing.T) {
	requireTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	const marker = "zqwarnonceunreadablemarker"
	f := newGraphFixture(t)
	f.graphFile("outer/inner/ok.txt", marker)
	locked := f.path("outer/inner/locked")
	writeFixtureFile(t, filepath.Join(locked, "hidden.txt"), marker+"\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	setTestUserCache(t)
	logs := captureTestLogs(t, slog.LevelWarn)
	graphSearch(t, marker, f.path("outer"), f.path("outer/inner"))
	var warnings int
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "could not be read") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("got %d unreadable-folder warnings, want 1", warnings)
	}
}

// TestFolderUnreadableFileInWarmFolderByName adds a mode-000 file to a warm
// folder: the delta indexes it by name, so a file-name search finds it.
func TestFolderUnreadableFileInWarmFolderByName(t *testing.T) {
	requireTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	f := newGraphFixture(t)
	f.graphFile("folder/a.txt", "zqwarmunreadablemarker")
	setTestUserCache(t)
	if got := graphSearch(t, "zqwarmunreadablemarker", f.path("folder")); len(got) != 1 {
		t.Fatalf("found %v", got)
	}
	plans, err := planCorpora(context.Background(), []string{f.path("folder")})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v, want one folder node", len(plans), err)
	}
	baseBefore := firstShardInfo(t, plans[0].indexDir)
	locked := f.path("folder/zqlockedname.txt")
	writeFixtureFile(t, locked, "secret\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	out, err := seek(t, "--lexical-only", "file:zqlockedname", f.path("folder"))
	if err != nil || !strings.Contains(out, "zqlockedname.txt") {
		t.Fatalf("output=%q err=%v, want the unreadable file by name", out, err)
	}
	// The delta indexes the file by name: no full rebuild. A delta keeps the
	// base shard file; a full rebuild writes a new one.
	if !os.SameFile(baseBefore, firstShardInfo(t, plans[0].indexDir)) {
		t.Fatal("the delta failed and the search rebuilt the folder")
	}
}

// TestFolderUnreadableTwoFoldersWarnOnce seeds outer and outer/z with an
// unreadable folder under each of outer/a and outer/z: only the kept node
// (outer) warns, once, whatever the name order. Each of three searches gives
// exactly one warning.
func TestFolderUnreadableTwoFoldersWarnOnce(t *testing.T) {
	requireTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	const marker = "zqtwounreadablemarker"
	f := newGraphFixture(t)
	f.graphFile("outer/z/ok.txt", marker)
	f.graphFile("outer/a/ok2.txt", marker)
	for _, rel := range []string{"outer/z/locked", "outer/a/locked"} {
		locked := f.path(rel)
		writeFixtureFile(t, filepath.Join(locked, "h.txt"), marker+"\n")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	}
	setTestUserCache(t)
	for run := range 3 {
		logs := captureTestLogs(t, slog.LevelWarn)
		graphSearch(t, marker, f.path("outer"), f.path("outer/z"))
		var warnings int
		for _, record := range logs.Records() {
			if strings.Contains(record.Message, "could not be read") {
				warnings++
			}
		}
		if warnings != 1 {
			t.Fatalf("run %d: got %d unreadable-folder warnings, want 1", run, warnings)
		}
	}
}

// TestFolderWalkCutsALoop reports a subdirectory with the identity of the
// root, as a bind mount of an ancestor would: the walk does not enter it
// again.
func TestFolderWalkCutsALoop(t *testing.T) {
	root := canonTempDir(t)
	writeFixtureFile(t, filepath.Join(root, "top.txt"), "top\n")
	writeFixtureFile(t, filepath.Join(root, "loop", "inner.txt"), "inner\n")
	realIdentity := folderDirIdentity
	folderDirIdentity = func(f *os.File) (dirIdentity, error) {
		if rel, _ := filepath.Rel(root, f.Name()); rel == "." || rel == "loop" {
			return dirIdentity{dev: 7, ino: 7}, nil
		}
		return realIdentity(f)
	}
	t.Cleanup(func() { folderDirIdentity = realIdentity })
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatal(err)
	}
	capture, _, err := captureFolderNode(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.selected) != 1 || capture.selected[0].name != "top.txt" {
		t.Fatalf("selected=%v, want only top.txt (the loop is cut)", capture.selected)
	}
}

// TestSFDatalessFlag pins the darwin SF_DATALESS bit (sys/stat.h); Linux has
// no cloud-only flag.
func TestSFDatalessFlag(t *testing.T) {
	want := runtime.GOOS == "darwin"
	if got := statDatalessFlags(0x40000000); got != want {
		t.Fatalf("statDatalessFlags(SF_DATALESS)=%t, want %t", got, want)
	}
	if statDatalessFlags(0) {
		t.Fatal("no flag is not dataless")
	}
}

// TestFolderFileReplacedByFIFOAfterWalkDoesNotBlock replaces a walked file
// with a FIFO just before the build opens it. The open does not block on the
// FIFO, the build reads the file as removed, and the search finds the other
// file. The query lists file names, so a FIFO indexed as an empty file shows.
func TestFolderFileReplacedByFIFOAfterWalkDoesNotBlock(t *testing.T) {
	const marker = "zqfifomarker"
	const query = "type:file txt"
	f := newGraphFixture(t)
	f.graphFile("folder/a.txt", marker)
	f.graphFile("folder/b.txt", marker)
	setTestUserCache(t)
	fifo := f.path("folder/a.txt")
	saved := folderOpenFile
	var replaced bool
	folderOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "a.txt" && !replaced {
			replaced = true
			if err := os.Remove(path); err != nil {
				return nil, err
			}
			if err := unix.Mkfifo(path, 0o644); err != nil {
				return nil, err
			}
		}
		return saved(path)
	}
	t.Cleanup(func() { folderOpenFile = saved })
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := seek(t, "--lexical-only", query, f.path("folder"))
		done <- result{out, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || !replaced || !strings.Contains(got.out, "b.txt") || strings.Contains(got.out, "a.txt") {
			t.Fatalf("replaced=%t err=%v output:\n%s", replaced, got.err, got.out)
		}
	case <-time.After(10 * time.Second):
		// Open the FIFO for writing so that the blocked open returns.
		if writer, err := os.OpenFile(fifo, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_ = writer.Close()
		}
		<-done
		t.Fatal("the build blocked on the FIFO")
	}
}

// TestFolderFileReplacedAfterWalkReadsAsRemoved replaces a walked file just
// before the build opens it: with a socket, with a symlink, or with a file in
// place of its directory. The open fails before any fstat; the build reads
// the file as removed and the search finds the other file. The query lists
// file names, so a file indexed by name only shows.
func TestFolderFileReplacedAfterWalkReadsAsRemoved(t *testing.T) {
	const marker = "zqreplacedmarker"
	const query = "type:file txt"
	for _, test := range []struct {
		name    string
		target  string
		replace func(t *testing.T, folder string) error
	}{
		{name: "socket", target: "a.txt", replace: func(t *testing.T, folder string) error {
			if err := os.Remove(filepath.Join(folder, "a.txt")); err != nil {
				return err
			}
			// A relative name keeps the socket path below the 104-byte limit.
			listener, err := net.Listen("unix", "a.txt")
			if err == nil {
				t.Cleanup(func() { _ = listener.Close() })
			}
			return err
		}},
		{name: "symlink", target: "a.txt", replace: func(_ *testing.T, folder string) error {
			path := filepath.Join(folder, "a.txt")
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(folder, "b.txt"), path)
		}},
		{name: "directory replaced by a file", target: "c.txt", replace: func(t *testing.T, folder string) error {
			if err := os.RemoveAll(filepath.Join(folder, "sub")); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(folder, "sub"), []byte("not a directory"), 0o644)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newGraphFixture(t)
			f.graphFile("folder/a.txt", marker)
			f.graphFile("folder/b.txt", marker)
			f.graphFile("folder/sub/c.txt", marker)
			setTestUserCache(t)
			folder := f.path("folder")
			t.Chdir(folder)
			saved := folderOpenFile
			var replaced bool
			folderOpenFile = func(path string) (*os.File, error) {
				if filepath.Base(path) == test.target && !replaced {
					replaced = true
					if err := test.replace(t, folder); err != nil {
						return nil, err
					}
				}
				return saved(path)
			}
			t.Cleanup(func() { folderOpenFile = saved })
			out, err := seek(t, "--lexical-only", query, folder)
			if err != nil || !replaced || !strings.Contains(out, "b.txt") || strings.Contains(out, test.target) {
				t.Fatalf("replaced=%t err=%v output:\n%s", replaced, err, out)
			}
		})
	}
}
