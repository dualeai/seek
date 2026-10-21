package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestNodeIdentityIgnoresTypedPath checks that the typed path (a symlink, or
// another letter case on a case-insensitive file system) never changes a
// node: one corpus, one result.
func TestNodeIdentityIgnoresTypedPath(t *testing.T) {
	requireTools(t)
	const marker = "zqnodeidentitymarker"

	t.Run("symlinked root", func(t *testing.T) {
		f := newGraphFixture(t)
		folder := f.path("folder")
		writeFixtureFile(t, filepath.Join(folder, "a.txt"), marker+"\n")
		link := f.path("link")
		if err := os.Symlink(folder, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		setTestUserCache(t)
		assertNodeIdentityOnce(t, marker, folder, link)
	})

	t.Run("letter case", func(t *testing.T) {
		f := newGraphFixture(t)
		repo := f.GitRepo("repoC", "c.txt", marker+"\n")
		upper := filepath.Join(filepath.Dir(repo), "REPOC")
		if _, err := os.Stat(upper); err != nil {
			t.Skip("file system is case-sensitive")
		}
		writeFixtureFile(t, filepath.Join(repo, "sub", "s.txt"), "sub\n")
		gitRunIn(t, repo, "add", "sub")
		gitRunIn(t, repo, "commit", "-q", "-m", "sub")
		setTestUserCache(t)
		assertNodeIdentityOnce(t, marker, repo, upper)
		// Operands of one node in two letter cases merge into one scope,
		// in either order and with "." in the work tree.
		for _, operands := range [][]string{
			{filepath.Join(upper, "sub"), repo},
			{repo, filepath.Join(upper, "sub")},
			{".", filepath.Join(upper, "sub")},
		} {
			t.Chdir(repo)
			if err := run(context.Background(), "sub", operands); err != nil {
				t.Fatalf("mixed-case operands %q of one repository: %v", operands, err)
			}
		}
		// The current directory typed in another case adds no corpus.
		for _, dir := range []string{repo, upper} {
			t.Chdir(dir)
			if got := planIDOf(t, "."); got != planIDOf(t, repo) {
				t.Fatalf("cwd %q: corpus ID %q, want %q", dir, got, planIDOf(t, repo))
			}
		}
	})
}

// assertNodeIdentityOnce checks that operands plan one corpus ID and that a
// search of them prints marker once.
func assertNodeIdentityOnce(t *testing.T, marker string, operands ...string) {
	t.Helper()
	id := planIDOf(t, operands[0])
	for _, operand := range operands[1:] {
		if got := planIDOf(t, operand); got != id {
			t.Fatalf("operands %q got corpus IDs %q and %q, want one ID", operands, id, got)
		}
	}
	out, err := captureStdout(t, func() error {
		return run(context.Background(), marker, operands)
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if n := strings.Count(out, marker); n != 1 {
		t.Fatalf("marker printed %d times, want 1:\n%s", n, out)
	}
}

// planIDOf returns the corpus ID of the one node that operand names.
func planIDOf(t *testing.T, operand string) corpusID {
	t.Helper()
	plans, err := planCorpora(context.Background(), []string{operand})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("operand %q planned %d nodes, want 1", operand, len(plans))
	}
	return plans[0].id
}

// TestOperandCaseInRepoThatGitClassifies types an operand in another letter
// case inside a repository whose `.git` Git decides (a separate Git
// directory). The operand stays a Git scope: the Git-ignored file is not
// shown.
func TestOperandCaseInRepoThatGitClassifies(t *testing.T) {
	requireTools(t)
	const marker = "zqoperandcasemarker"
	f := newGraphFixture(t)
	repo := f.path("repoS")
	if err := os.MkdirAll(f.path("meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, f.root, "init", "-q", "--separate-git-dir", f.path("meta/repoS.git"), repo)
	upper := filepath.Join(filepath.Dir(repo), "REPOS")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("file system is case-sensitive")
	}
	writeFixtureFile(t, filepath.Join(repo, ".gitignore"), "sub/ign.txt\n")
	f.graphFile("repoS/sub/kept.txt", marker)
	gitRunIn(t, repo, "add", ".gitignore", "sub/kept.txt")
	gitRunIn(t, repo, "commit", "-q", "-m", "content")
	f.graphFile("repoS/sub/ign.txt", marker)
	setTestUserCache(t)
	got := slices.Sorted(maps.Keys(graphSearch(t, marker, filepath.Join(upper, "sub"))))
	if want := []string{"repoS/sub/kept.txt"}; !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
}

// TestNodeIdentitySurvivesRename renames a searched Git repository and a
// searched folder. The identity is the file identity, not the path, so the
// node keeps its corpus and its state: the search after the rename reuses the
// index.
func TestNodeIdentitySurvivesRename(t *testing.T) {
	requireTools(t)
	const marker = "zqrenamemarker"
	f := newGraphFixture(t)
	repo := f.graphRepo("repo", "a.txt", marker)
	folder := f.path("folder")
	f.graphFile("folder/b.txt", marker)
	setTestUserCache(t)
	for _, root := range []string{repo, folder} {
		t.Run(filepath.Base(root), func(t *testing.T) {
			graphSearch(t, marker, root)
			plans, err := planCorpora(context.Background(), []string{root})
			if err != nil || len(plans) != 1 {
				t.Fatalf("plans=%d err=%v", len(plans), err)
			}
			before, state := plans[0].id, readStateFile(plans[0].cacheDir)
			renamed := root + "-renamed"
			if err := os.Rename(root, renamed); err != nil {
				t.Fatal(err)
			}
			if got := graphSearch(t, marker, renamed); len(got) != 1 {
				t.Fatalf("found %v after the rename, want 1 file", got)
			}
			if after := planIDOf(t, renamed); after != before {
				t.Fatalf("corpus ID changed with the path: %q -> %q", before, after)
			}
			if got := readStateFile(plans[0].cacheDir); state == "" || got != state {
				t.Fatalf("state changed with the path: %q -> %q", state, got)
			}
		})
	}
}

// TestLinkedWorktreesStayDistinct searches two linked worktrees of one
// repository: one shared Git directory, two work trees, two nodes.
func TestLinkedWorktreesStayDistinct(t *testing.T) {
	requireTools(t)
	const marker = "zqworktreesmarker"
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "shared\n")
	for _, name := range []string{"wt1", "wt2"} {
		worktree := f.path(name)
		gitRunIn(t, repo, "worktree", "add", "-q", "-b", name, worktree)
		f.graphFile(name+"/own.txt", marker)
		gitRunIn(t, worktree, "add", "own.txt")
		gitRunIn(t, worktree, "commit", "-q", "-m", name)
	}
	setTestUserCache(t)
	if planIDOf(t, f.path("wt1")) == planIDOf(t, f.path("wt2")) {
		t.Fatal("two linked worktrees share one corpus ID")
	}
	got := slices.Sorted(maps.Keys(graphSearch(t, marker, f.path("wt1"), f.path("wt2"))))
	if want := []string{"wt1/own.txt", "wt2/own.txt"}; !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
}
