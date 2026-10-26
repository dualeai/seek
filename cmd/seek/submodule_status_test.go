package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRun_GitlinkReplacedByFileIsSearched replaces a checked-out submodule with
// a regular file and does not stage it. The parent status must still report
// the path (--ignore-submodules=dirty keeps the typechange record), so a
// search finds the new file.
func TestRun_GitlinkReplacedByFileIsSearched(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	sub := f.GitRepo("sub", "s.txt", "submodule content\n")
	parent := f.GitRepo("parent", "p.txt", "parent content\n")
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", sub, "mysub")
	gitRunIn(t, parent, "commit", "-m", "add submodule")
	if err := os.RemoveAll(filepath.Join(parent, "mysub")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(parent, "mysub"), "zqtypechangemarker\n")
	setTestUserCache(t)
	logs := captureTestLogs(t, slog.LevelWarn)

	out, err := seek(t, "--lexical-only", "zqtypechangemarker", parent)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "mysub") || !strings.Contains(out, "[uncommitted]") {
		t.Fatalf("replaced gitlink file is not searched as uncommitted:\n%s", out)
	}
	// The file is content of the parent, not a failed child node.
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("unexpected warnings: %v", records)
	}
}

// TestGitlinkSymlinkIsNoEdge replaces a checked-out submodule with a symlink:
// to its own repository (a cycle), and to a repository outside the search.
// A symlink is no edge: the search ends, and the outside files stay absent.
func TestGitlinkSymlinkIsNoEdge(t *testing.T) {
	requireTools(t)
	const marker = "zqgitlinksymlinkmarker"
	for _, tc := range []struct {
		name   string
		target func(f *graphFixture) string
	}{
		{name: "own repository", target: func(*graphFixture) string { return "." }},
		{name: "outside repository", target: func(f *graphFixture) string { return f.graphRepo("outside", "o.txt", marker) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGraphFixture(t)
			sub := f.GitRepo("sub", "s.txt", "submodule content\n")
			parent := f.graphRepo("parent", "p.txt", marker)
			gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "mysub")
			gitRunIn(t, parent, "commit", "-q", "-m", "add submodule")
			if err := os.RemoveAll(filepath.Join(parent, "mysub")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target(f), filepath.Join(parent, "mysub")); err != nil {
				t.Fatal(err)
			}
			setTestUserCache(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out, err := captureStdout(t, func() error {
				return seekRoot(ctx, t, nil, "--lexical-only", marker, parent)
			})
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if !strings.Contains(out, "p.txt") || strings.Contains(out, "o.txt") {
				t.Fatalf("want only the parent's file:\n%s", out)
			}
		})
	}
}
