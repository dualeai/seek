package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// graphSearch runs a search and returns each fixture path that a match line
// names (see graphFixture.graphFile), with the tags of its file header. A path
// found twice fails the test. printedResults drops the text before the first
// header, so output that does not start with a header also fails the test.
func graphSearch(t *testing.T, marker string, operands ...string) map[string]string {
	t.Helper()
	out, err := seek(t, slices.Concat([]string{"--lexical-only", marker}, operands)...)
	if err != nil {
		t.Fatalf("run %q: %v\n%s", operands, err, out)
	}
	if out != "" && !strings.HasPrefix(out, "## ") {
		t.Fatalf("run %q: the output does not start with a result header:\n%s", operands, out)
	}
	hits := make(map[string]string)
	pattern := regexp.MustCompile(regexp.QuoteMeta(marker) + ` (\S+)`)
	for _, result := range printedResults(out) {
		for _, line := range result.body {
			for _, match := range pattern.FindAllStringSubmatch(line, -1) {
				if _, dup := hits[match[1]]; dup {
					t.Fatalf("%s found twice:\n%s", match[1], out)
				}
				hits[match[1]] = result.tags
			}
		}
	}
	return hits
}

// TestGraphEdgeTable searches "." from the root of the graph that edgeGraph
// builds. Each file of each edge kind is found once with its corpus tag. The
// files of a Git-ignored folder, which holds a repository, and the files behind
// a symlink stay absent.
func TestGraphEdgeTable(t *testing.T) {
	requireTools(t)
	const marker = "zqgraphedgemarker"
	f := newGraphFixture(t)
	want := f.edgeGraph(marker)
	setTestUserCache(t)
	t.Chdir(f.root)
	if got := graphSearch(t, marker, "."); !maps.Equal(got, want) {
		t.Fatalf("found %v\nwant %v", got, want)
	}
}

// TestGraphExpandDotFromEveryFolder searches "." from each folder of the graph
// that edgeGraph builds. A search finds every file below its folder, except
// the files below a Git-ignored folder, which only a search from inside that
// folder finds.
func TestGraphExpandDotFromEveryFolder(t *testing.T) {
	requireTools(t)
	const marker = "zqgraphdotmarker"
	f := newGraphFixture(t)
	all := slices.Collect(maps.Keys(f.edgeGraph(marker)))
	all = append(all, "repoA/ignored/loose.txt", "repoA/ignored/repoE/m.txt")
	setTestUserCache(t)
	for _, dir := range []string{
		".config", ".ssh", "folder1", "folder1/folder2", "folder1/folder2/repoC", "folder1/wt",
		"repoA", "repoA/sub", "repoA/sub/folder", "repoA/sub/folder/repoD",
		"repoA/repoB", "repoA/repoB/deep", "repoA/ignored", "repoA/ignored/repoE",
	} {
		t.Run(dir, func(t *testing.T) {
			var want []string
			for _, rel := range all {
				inIgnored := strings.HasPrefix(rel, "repoA/ignored/")
				if strings.HasPrefix(rel, dir+"/") && (!inIgnored || strings.HasPrefix(dir+"/", "repoA/ignored/")) {
					want = append(want, rel)
				}
			}
			slices.Sort(want)
			t.Chdir(f.path(dir))
			got := slices.Sorted(maps.Keys(graphSearch(t, marker, ".")))
			if !slices.Equal(got, want) {
				t.Fatalf("found %q, want %q", got, want)
			}
		})
	}
}

// TestGraphIgnoredFolderSearchedFromInside names a Git-ignored folder as the
// operand: its loose file and the repository inside it are found.
func TestGraphIgnoredFolderSearchedFromInside(t *testing.T) {
	requireTools(t)
	const marker = "zqgraphignoredmarker"
	f := newGraphFixture(t)
	f.edgeGraph(marker)
	setTestUserCache(t)
	got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("repoA/ignored"))))
	if want := []string{"repoA/ignored/loose.txt", "repoA/ignored/repoE/m.txt"}; !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
}

// TestGraphParentStateStableWhenChildChanges changes a submodule and an
// untracked nested repository. The children are their own nodes, so the parent
// state does not change and the parent is not rebuilt.
func TestGraphParentStateStableWhenChildChanges(t *testing.T) {
	requireTools(t)
	const marker = "zqgraphchildmarker"
	f := newGraphFixture(t)
	child := f.graphRepo("source", "c.txt", marker)
	parent := f.graphRepo("parent", "p.txt", marker)
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", child, "sub")
	gitRunIn(t, parent, "commit", "-m", "add submodule")
	nested := f.graphRepo("parent/nested", "n.txt", marker)
	setTestUserCache(t)

	graphSearch(t, marker, parent)
	plan, err := planGitCorpus(gitPaths{RepoDir: parent, CommonDir: filepath.Join(parent, ".git")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := readStateFile(plan.cacheDir)
	if before == "" {
		t.Fatal("parent has no state after the first search")
	}

	sub := filepath.Join(parent, "sub")
	f.graphFile("parent/sub/c2.txt", marker)
	gitRunIn(t, sub, "add", "c2.txt")
	gitRunIn(t, sub, "commit", "-m", "child change")
	f.graphFile("parent/nested/n2.txt", marker)
	gitRunIn(t, nested, "add", "n2.txt")
	gitRunIn(t, nested, "commit", "-m", "nested change")

	got := graphSearch(t, marker, parent)
	for _, rel := range []string{"parent/sub/c2.txt", "parent/nested/n2.txt"} {
		if _, ok := got[rel]; !ok {
			t.Fatalf("child change %s not found: %v", rel, got)
		}
	}
	if after := readStateFile(plan.cacheDir); after != before {
		t.Fatalf("parent state changed when only its children changed:\nbefore %q\nafter  %q", before, after)
	}
}

// TestGraphBareRepoRunsNoGit puts a bare repository below a folder root. Its
// files are Git internals: none is found, and no Git process runs in it.
func TestGraphBareRepoRunsNoGit(t *testing.T) {
	requireTools(t)
	const marker = "zqgraphbaremarker"
	f := newGraphFixture(t)
	f.graphFile("folder/loose.txt", marker)
	bare := f.path("folder/bare.git")
	gitRunIn(t, f.root, "init", "--bare", "-q", bare)
	writeFixtureFile(t, filepath.Join(bare, "description"), marker+" folder/bare.git/description\n")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	cwdLog := filepath.Join(t.TempDir(), "git-cwd.log")
	shim := t.TempDir()
	writeExecutable(t, filepath.Join(shim, "git"),
		"#!/bin/sh\npwd >> '"+cwdLog+"'\nexec '"+realGit+"' \"$@\"\n")
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	setTestUserCache(t)

	// A folder that only looks like a bare repository is content.
	f.graphFile("folder/fake/HEAD", marker)
	for _, dir := range []string{"objects", "refs"} {
		f.graphFile("folder/fake/"+dir+"/note.txt", marker)
	}

	got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("folder"))))
	if want := []string{"folder/fake/HEAD", "folder/fake/objects/note.txt", "folder/fake/refs/note.txt", "folder/loose.txt"}; !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
	data, err := os.ReadFile(cwdLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for dir := range strings.Lines(string(data)) {
		if pathWithin(resolvedPath(t, bare), resolvedPath(t, strings.TrimSpace(dir))) {
			t.Fatalf("Git ran in the bare repository: %s", data)
		}
	}
}

// TestGraphAmbiguousBoundaryAsksGit covers `.git` files that the file-system
// check cannot confirm: Git decides, and each repository is its own node.
func TestGraphAmbiguousBoundaryAsksGit(t *testing.T) {
	requireTools(t)
	const marker = "zqgraphambiguousmarker"

	t.Run("separate Git directory and its submodule", func(t *testing.T) {
		f := newGraphFixture(t)
		child := f.graphRepo("source", "c.txt", marker)
		repo := f.path("work/sep")
		if err := os.MkdirAll(f.path("meta"), 0o755); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, f.root, "init", "-q", "--separate-git-dir", f.path("meta/sep.git"), repo)
		f.graphFile("work/sep/s.txt", marker)
		writeFixtureFile(t, filepath.Join(repo, ".gitignore"), "ign.txt\n")
		gitRunIn(t, repo, "add", "s.txt", ".gitignore")
		gitRunIn(t, repo, "commit", "-m", "content")
		gitRunIn(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", child, "sub")
		gitRunIn(t, repo, "commit", "-m", "add submodule")
		f.graphFile("work/sep/ign.txt", marker)
		setTestUserCache(t)

		out, err := seek(t, "--lexical-only", marker, f.path("work"))
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
		for _, want := range []string{filepath.Join(repo, "s.txt"), filepath.Join(repo, "sub", "c.txt")} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %s in output:\n%s", want, out)
			}
		}
		if strings.Contains(out, "ign.txt") {
			t.Fatalf("Git-ignored file found:\n%s", out)
		}
	})

	t.Run("pointer to another project's submodule", func(t *testing.T) {
		f := newGraphFixture(t)
		child := f.graphRepo("source", "private.txt", marker)
		victim := f.graphRepo("victim", "v.txt", marker)
		gitRunIn(t, victim, "-c", "protocol.file.allow=always", "submodule", "add", child, "sec")
		gitRunIn(t, victim, "commit", "-m", "add submodule")
		evil := f.path("dl/evil")
		writeFixtureFile(t, filepath.Join(evil, ".git"), "gitdir: "+filepath.Join(victim, ".git", "modules", "sec")+"\n")
		f.graphFile("dl/evil/local.txt", marker)
		f.graphFile("dl/loose.txt", marker)
		setTestUserCache(t)
		logs := captureTestLogs(t, slog.LevelWarn)

		got := graphSearch(t, marker, f.path("dl"))
		if _, ok := got["source/private.txt"]; ok {
			t.Fatalf("another project's file found: %v", got)
		}
		if _, ok := got["dl/loose.txt"]; !ok {
			t.Fatalf("loose file not found: %v", got)
		}
		if records := logs.Records(); len(records) == 0 {
			t.Fatal("no warning for the pointer that Git does not use for this directory")
		}
	})

	// A `.git` entry below a folder root can name the Git directory of
	// another work tree, in any form. Its node fails, and the files of the
	// other repository stay absent.
	for _, form := range []string{"file pointer", "pointer through a symlink", ".git symlink", "another worktree's admin dir", ".git directory with commondir"} {
		t.Run("foreign Git directory: "+form, func(t *testing.T) {
			f := newGraphFixture(t)
			victim := f.graphRepo("victim", "v.txt", marker)
			// A skip-worktree file is clean for Git in any work tree, so a
			// search that accepted the pointer would show its committed
			// content under dl/evil.
			gitRunIn(t, victim, "update-index", "--skip-worktree", "v.txt")
			indexBefore, err := os.ReadFile(filepath.Join(victim, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			evil := f.path("dl/evil")
			if err := os.MkdirAll(evil, 0o755); err != nil {
				t.Fatal(err)
			}
			gitFile := filepath.Join(evil, ".git")
			switch form {
			case "file pointer":
				writeFixtureFile(t, gitFile, "gitdir: "+filepath.Join(victim, ".git")+"\n")
			case "pointer through a symlink":
				if err := os.Symlink(filepath.Join(victim, ".git"), f.path("dl/alias")); err != nil {
					t.Fatal(err)
				}
				writeFixtureFile(t, gitFile, "gitdir: ../alias\n")
			case ".git symlink":
				if err := os.Symlink(filepath.Join(victim, ".git"), gitFile); err != nil {
					t.Fatal(err)
				}
			case "another worktree's admin dir":
				gitRunIn(t, victim, "worktree", "add", "-q", "-b", "wt", f.path("victim-wt"))
				writeFixtureFile(t, gitFile, "gitdir: "+filepath.Join(victim, ".git", "worktrees", "victim-wt")+"\n")
			case ".git directory with commondir":
				// Objects and refs from another repository.
				writeGitTriadAt(t, gitFile)
				writeFixtureFile(t, filepath.Join(gitFile, "commondir"), filepath.Join(victim, ".git")+"\n")
			}
			f.graphFile("dl/loose.txt", marker)
			setTestUserCache(t)
			logs := captureTestLogs(t, slog.LevelWarn)

			got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("dl"))))
			if want := []string{"dl/loose.txt"}; !slices.Equal(got, want) {
				t.Fatalf("found %q, want %q", got, want)
			}
			// The skipped node gets one WARN record whose cause names the
			// foreign Git directory, not a Git failure.
			var skipped []slog.Record
			for _, record := range logs.Records() {
				if record.Message == "Skipped a search node; its results are missing" {
					skipped = append(skipped, record)
				}
			}
			if len(skipped) != 1 || !logsMention(skipped, "points to the Git directory of") {
				t.Fatalf("%d skip warnings, want one foreign-directory warning for a .git entry of another work tree: %v",
					len(skipped), logs.Records())
			}
			if indexAfter, err := os.ReadFile(filepath.Join(victim, ".git", "index")); err != nil || !bytes.Equal(indexAfter, indexBefore) {
				t.Fatalf("the search rewrote the other repository's index (err=%v)", err)
			}
		})
	}

	// A `.git` symlink to a store of Git directories, or to a Git directory
	// kept inside the work tree, is the work tree's own.
	for _, layout := range []string{"external store", "inside the work tree"} {
		t.Run(".git symlink to "+layout, func(t *testing.T) {
			f := newGraphFixture(t)
			repo := f.path("folder/repo")
			gitDir := f.path("store/repo/.git")
			if layout == "inside the work tree" {
				gitDir = filepath.Join(repo, "meta", ".git")
			}
			if err := os.MkdirAll(filepath.Dir(gitDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(repo, 0o755); err != nil {
				t.Fatal(err)
			}
			gitRunIn(t, f.root, "init", "-q", "--separate-git-dir", gitDir, repo)
			if err := os.Remove(filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(gitDir, filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
			f.graphFile("folder/repo/r.txt", marker)
			gitRunIn(t, repo, "add", "r.txt")
			gitRunIn(t, repo, "commit", "-q", "-m", "content")
			setTestUserCache(t)
			if got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("folder")))); !slices.Equal(got, []string{"folder/repo/r.txt"}) {
				t.Fatalf("found %q, want the repository's file", got)
			}
		})
	}

	// On a case-sensitive file system, the parent typed in another case
	// names no file.
	t.Run("nested repository below a parent typed in another case", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.graphRepo("repoC", "c.txt", marker)
		upper := filepath.Join(filepath.Dir(repo), "REPOC")
		// A separate Git directory makes the nested repository one that Git
		// classifies.
		if err := os.MkdirAll(f.path("meta"), 0o755); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(repo, "nested")
		gitRunIn(t, f.root, "init", "-q", "--separate-git-dir", f.path("meta/nested.git"), nested)
		f.graphFile("repoC/nested/n.txt", marker)
		gitRunIn(t, nested, "add", "n.txt")
		gitRunIn(t, nested, "commit", "-q", "-m", "nested")
		setTestUserCache(t)
		if _, err := os.Stat(upper); err != nil {
			requireMissingOperand(t, upper)
			return
		}
		got := slices.Sorted(maps.Keys(graphSearch(t, marker, upper)))
		if want := []string{"repoC/c.txt", "repoC/nested/n.txt"}; !slices.Equal(got, want) {
			t.Fatalf("found %q, want %q", got, want)
		}
	})

	// A case-insensitive file system maps ".GIT" to the Git directory, so Git
	// hides the ignored file. On a case-sensitive file system, ".GIT" is no
	// Git entry: the folder walk shows each file.
	t.Run("upper-case .GIT", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.graphRepo("folder/repo", "r.txt", marker)
		if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, ".GIT")); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(repo, ".gitignore"), "ign.txt\n")
		want := []string{"folder/repo/ign.txt", "folder/repo/r.txt"}
		if _, err := os.Stat(filepath.Join(repo, ".git")); err == nil {
			gitRunIn(t, repo, "add", ".gitignore")
			gitRunIn(t, repo, "commit", "-q", "-m", "ignore")
			want = []string{"folder/repo/r.txt"}
		}
		f.graphFile("folder/repo/ign.txt", marker)
		setTestUserCache(t)
		got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("folder"))))
		if !slices.Equal(got, want) {
			t.Fatalf("found %q, want %q", got, want)
		}
	})

	t.Run("symlinked .git from each ancestor", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.path("outer/mid/symgit")
		if err := os.MkdirAll(f.path("store"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, f.root, "init", "-q", "--separate-git-dir="+f.path("store/x.git"), repo)
		if err := os.Remove(filepath.Join(repo, ".git")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.path("store/x.git"), filepath.Join(repo, ".git")); err != nil {
			t.Fatal(err)
		}
		f.graphFile("outer/mid/symgit/inner.txt", marker)
		writeFixtureFile(t, filepath.Join(repo, ".gitignore"), "ign.txt\n")
		gitRunIn(t, repo, "add", "inner.txt", ".gitignore")
		gitRunIn(t, repo, "commit", "-m", "content")
		f.graphFile("outer/mid/symgit/ign.txt", marker)
		setTestUserCache(t)
		for _, operand := range []string{repo, f.path("outer/mid"), f.path("outer")} {
			got := slices.Sorted(maps.Keys(graphSearch(t, marker, operand)))
			if want := []string{"outer/mid/symgit/inner.txt"}; !slices.Equal(got, want) {
				t.Fatalf("operand %s: found %q, want %q", operand, got, want)
			}
		}
	})

	t.Run("submodule of a linked worktree", func(t *testing.T) {
		f := newGraphFixture(t)
		child := f.graphRepo("source", "c.txt", marker)
		parent := f.graphRepo("work/parent", "p.txt", marker)
		gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", child, "sub")
		gitRunIn(t, parent, "commit", "-m", "add submodule")
		worktree := f.path("work/wt")
		gitRunIn(t, parent, "worktree", "add", "-q", "-b", "wt", worktree)
		gitRunIn(t, worktree, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q")
		setTestUserCache(t)

		out, err := seek(t, "--lexical-only", marker, f.path("work"))
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
		for _, want := range []string{filepath.Join(parent, "sub", "c.txt"), filepath.Join(worktree, "sub", "c.txt")} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %s in output:\n%s", want, out)
			}
		}
	})
}

// TestGraphGitProcessCount runs a warm search over a folder with three
// repositories, one of them with a submodule. The command runs one git status
// and one git ls-files --stage per Git node. No status recurses into a
// submodule.
func TestGraphGitProcessCount(t *testing.T) {
	requireTools(t)
	const marker = "zqgraphprocessmarker"
	f := newGraphFixture(t)
	child := f.graphRepo("source", "c.txt", marker)
	for _, rel := range []string{"a", "b"} {
		f.graphRepo("folder/"+rel, "x.txt", marker)
	}
	parent := f.graphRepo("folder/parent", "p.txt", marker)
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", child, "sub")
	gitRunIn(t, parent, "commit", "-q", "-m", "add submodule")
	setTestUserCache(t)
	const nodes = 4 // a, b, parent, parent/sub
	if got := graphSearch(t, marker, f.path("folder")); len(got) != nodes {
		t.Fatalf("cold search found %v, want %d files", got, nodes)
	}

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	if got := graphSearch(t, marker, f.path("folder")); len(got) != nodes {
		t.Fatalf("warm search found %v, want %d files", got, nodes)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	// "status --porcelain" also matches Git's own recursive call into a
	// submodule (--porcelain=2), which must not run.
	for _, command := range []string{"status --porcelain", "ls-files -z --stage"} {
		if n := strings.Count(string(data), command); n != nodes {
			t.Fatalf("%q ran %d times for %d Git nodes:\n%s", command, n, nodes, data)
		}
	}
}

// TestGraphGitProcessCountCold counts Git processes on a cold search of a
// folder with two repositories. It runs with and without vectors; the vector
// run searches one of the repositories. Each Git node gets one status and one
// index listing, and the cold build runs no second status.
func TestGraphGitProcessCountCold(t *testing.T) {
	requireTools(t)
	const marker = "zqgraphcoldprocessmarker"
	for _, vectors := range []bool{false, true} {
		t.Run(fmt.Sprintf("vectors=%t", vectors), func(t *testing.T) {
			f := newGraphFixture(t)
			for _, rel := range []string{"a", "b"} {
				f.graphRepo("folder/"+rel, "x.txt", marker+" "+marker)
			}
			setTestUserCache(t)
			trace := filepath.Join(t.TempDir(), "trace")
			t.Setenv("GIT_TRACE", trace)
			t.Chdir(t.TempDir())
			query := marker
			search := func() error {
				_, err := seek(t, "--lexical-only", query, f.path("folder"))
				return err
			}
			if vectors {
				// The vector run searches one Git node.
				query = marker + " " + marker
				search = func() error {
					_, _, err := stubSearch(t, payloadFar, query, f.path("folder/a"))
					return err
				}
			}
			if err := search(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			nodes := 2
			if vectors {
				nodes = 1
			}
			for _, command := range []string{"status --porcelain", "ls-files -z --stage"} {
				if n := strings.Count(string(data), command); n != nodes {
					t.Fatalf("%q ran %d times for %d Git nodes:\n%s", command, n, nodes, data)
				}
			}
		})
	}
}

// TestCapturedStateLastsUntilAChange checks the node captures of the graph
// expansion. Every preparation of the command uses them, so a search pays one
// status and one walk per node. When a route sees the tree change
// (treeChanged), every later preparation reads the tree again. This holds for
// the Git node and the folder node alike.
func TestCapturedStateLastsUntilAChange(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "a\n")
	f.graphFile("folder/f.txt", "f")
	setTestUserCache(t)
	plans, err := planCorpora(context.Background(), []string{repo, f.path("folder")})
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	gitPlan, folderPlan := plans[0], plans[1]
	if gitPlan.kind != corpusKindGit {
		gitPlan, folderPlan = folderPlan, gitPlan
	}
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "b\n")
	f.graphFile("folder/g.txt", "g")
	for range 2 {
		state, err := gitNodeState(t.Context(), gitPlan, gitPlan.behavior.(*gitNode).paths)
		if err != nil || slices.Contains(state.Files, "b.txt") {
			t.Fatalf("files=%q err=%v, want the capture", state.Files, err)
		}
		if _, selected, ok := folderPlan.folder.current(); !ok || len(selected) != 1 {
			t.Fatalf("folder capture ok=%t selected=%d, want the capture", ok, len(selected))
		}
	}
	if !errors.Is(treeChanged(gitPlan), errGitIndexStateChanged) || !errors.Is(treeChanged(folderPlan), errGitIndexStateChanged) {
		t.Fatal("treeChanged must report the change")
	}
	state, err := gitNodeState(t.Context(), gitPlan, gitPlan.behavior.(*gitNode).paths)
	if err != nil || !slices.Contains(state.Files, "b.txt") {
		t.Fatalf("files=%q err=%v, want a fresh status", state.Files, err)
	}
	if _, _, ok := folderPlan.folder.current(); ok {
		t.Fatal("the folder capture survived a detected change")
	}
}

// TestGitNodeSkipsUntrackedBareRepo puts an untracked bare repository inside a
// Git work tree. Git lists its files as untracked, but they are Git internals:
// none is found; an ordinary untracked file still is.
func TestGitNodeSkipsUntrackedBareRepo(t *testing.T) {
	requireTools(t)
	const marker = "zqgitbaremarker"
	f := newGraphFixture(t)
	repo := f.graphRepo("repo", "a.txt", marker)
	bare := filepath.Join(repo, "inner.git")
	gitRunIn(t, repo, "init", "-q", "--bare", bare)
	writeFixtureFile(t, filepath.Join(bare, "description"), marker+" repo/inner.git/description\n")
	f.graphFile("repo/untracked.txt", marker)
	setTestUserCache(t)
	got := slices.Sorted(maps.Keys(graphSearch(t, marker, repo)))
	if want := []string{"repo/a.txt", "repo/untracked.txt"}; !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
}

// TestGraphFailedChildWarnsOnce reaches a repository with a broken submodule
// twice: through a scoped operand and through a folder walk that widens the
// scope. The broken child warns once.
func TestGraphFailedChildWarnsOnce(t *testing.T) {
	requireTools(t)
	const marker = "zqwarnoncemarker"
	f := newGraphFixture(t)
	child := f.graphRepo("source", "c.txt", marker)
	parent := f.graphRepo("folder/parent", "p.txt", marker)
	if err := os.MkdirAll(filepath.Join(parent, "libs"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", child, "libs/sub")
	gitRunIn(t, parent, "commit", "-q", "-m", "add submodule")
	sub := filepath.Join(parent, "libs", "sub")
	writeFixtureFile(t, filepath.Join(sub, ".git"), "gitdir: ../../missing\n")
	setTestUserCache(t)
	for range 5 {
		logs := captureTestLogs(t, slog.LevelWarn)
		graphSearch(t, marker, filepath.Join(parent, "libs"), f.path("folder"))
		var warnings int
		for _, record := range logs.Records() {
			if strings.Contains(record.Message, "its results are missing") {
				warnings++
			}
		}
		if warnings != 1 {
			t.Fatalf("got %d warnings for one broken child, want 1: %v", warnings, logs.Records())
		}
	}
}

// TestValidGitHead follows Git's validate_headref: a symbolic ref below refs/,
// or a line that starts with an object ID in hex of either case.
func TestValidGitHead(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	for head, want := range map[string]bool{
		"ref: refs/heads/main\n":        true,
		"ref:refs/heads/main\n":         true,
		"ref: HEAD\n":                   false,
		sha + "\n":                      true,
		strings.ToUpper(sha) + "\n":     true,
		sha + " trailing text\n":        true,
		strings.Repeat("ab", 32) + "\n": true,
		sha[:39] + "\n":                 false,
		"junk\n":                        false,
		"":                              false,
	} {
		path := filepath.Join(t.TempDir(), "HEAD")
		writeFixtureFile(t, path, head)
		if got := validGitHead(path); got != want {
			t.Errorf("HEAD %q: got %t, want %t", head, got, want)
		}
	}
}

// TestChildrenInScopeAddsEachChildOnce widens the scope of a captured node:
// only the children not returned before come back.
func TestChildrenInScopeAddsEachChildOnce(t *testing.T) {
	node := &externalGitRoot{
		captured: true,
		children: []string{"/r/a/x", "/r/b/y"},
		operands: []string{"/r/a"},
	}
	if got := node.childrenInScope(); !slices.Equal(got, []string{"/r/a/x"}) {
		t.Fatalf("first=%q", got)
	}
	node.operands = append(node.operands, "/r")
	if got := node.childrenInScope(); !slices.Equal(got, []string{"/r/b/y"}) {
		t.Fatalf("after widening=%q, want only the new child", got)
	}
	if got := node.childrenInScope(); len(got) != 0 {
		t.Fatalf("again=%q, want none", got)
	}
}

// TestPlansDropFolderNodesThatAWalkReaches gives the plan step folder nodes
// below a node at the file-system root "/". The walk of "/" reaches each one,
// except below its skip directory "/s". A node below "/s" stays, and the node
// "/s/h" reaches its own file. "/sx.txt" is not below "/s".
func TestPlansDropFolderNodesThatAWalkReaches(t *testing.T) {
	x := &graphExpansion{ctx: t.Context(), folders: make(map[corpusID]*node)}
	for _, root := range []string{"/", "/a", "/a/f.txt", "/s/g.txt", "/s/h", "/s/h/i.txt", "/sx.txt"} {
		capture := &folderCapture{skipDirs: make(map[string]struct{})}
		if root == "/" {
			capture.skipDirs["/s"] = struct{}{}
		}
		x.folders[corpusID(root)] = &node{id: corpusID(root), root: root, folder: capture}
	}
	plans, err := x.plans()
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, plan := range plans {
		roots = append(roots, plan.root)
	}
	if want := []string{"/", "/s/g.txt", "/s/h"}; !slices.Equal(roots, want) {
		t.Fatalf("roots=%q, want %q", roots, want)
	}
}

// TestValidGitHeadSymlinkAndNewline covers the HEAD forms that Git also
// accepts: a symbolic link into refs/, and "ref:" with the name on the next
// line.
func TestValidGitHeadSymlinkAndNewline(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "HEAD")
	if err := os.Symlink("refs/heads/main", link); err != nil {
		t.Fatal(err)
	}
	if !validGitHead(link) {
		t.Error("a HEAD symlink into refs/ is valid")
	}
	other := filepath.Join(t.TempDir(), "HEAD")
	if err := os.Symlink("/etc/passwd", other); err != nil {
		t.Fatal(err)
	}
	if validGitHead(other) {
		t.Error("a HEAD symlink outside refs/ is not valid")
	}
	split := filepath.Join(t.TempDir(), "HEAD")
	writeFixtureFile(t, split, "ref:\nrefs/heads/main\n")
	if !validGitHead(split) {
		t.Error("\"ref:\" with the name on the next line is valid")
	}
}

// logsMention reports a record whose message or attribute holds text.
func logsMention(records []slog.Record, text string) bool {
	for _, record := range records {
		found := strings.Contains(record.Message, text)
		record.Attrs(func(attr slog.Attr) bool {
			found = found || strings.Contains(attr.Value.String(), text)
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// TestBareRepoMarksAcceptSymbolicHead checks the two bare-repository checks
// with a HEAD symlink, as old Git writes it. detectBareRepoAt accepts a link
// into refs/ and refuses one elsewhere. gitListingMarks reads only the entry
// types, so it marks both as bare.
func TestBareRepoMarksAcceptSymbolicHead(t *testing.T) {
	for _, row := range []struct {
		target string
		bare   bool
	}{{"refs/heads/main", true}, {"/etc/passwd", false}} {
		dir := t.TempDir()
		for _, sub := range []string{"objects", "refs"} {
			if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(row.target, filepath.Join(dir, "HEAD")); err != nil {
			t.Fatal(err)
		}
		if got := detectBareRepoAt(dir); got != row.bare {
			t.Errorf("HEAD -> %s: detectBareRepoAt=%t, want %t", row.target, got, row.bare)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		// The listing sees only the entry types; the HEAD content check runs
		// later, so a symlink HEAD counts here.
		if gitEntry, bare := gitListingMarks(dir, entries); gitEntry || !bare {
			t.Errorf("HEAD -> %s: gitListingMarks=(%t, %t), want (false, true)", row.target, gitEntry, bare)
		}
	}
}
