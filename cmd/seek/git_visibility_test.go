package main

import (
	"context"
	"path/filepath"
	"testing"
)

// TestClassifyVisibility_Matrix checks every routing case against a real Git
// repository. The cases are:
//   - tracked, tracked and ignored, untracked, and untracked and ignored files;
//   - fully and partly ignored directories;
//   - force-added files under an ignored directory;
//   - a path that does not exist and matches an ignore rule;
//   - the worktree root, and a path outside the worktree.
func TestClassifyVisibility_Matrix(t *testing.T) {
	requireGit(t)

	repo := initGitRepoNoRemote(t, "tracked.go", "package main\n")

	// The second commit adds a file that .gitignore matches later (tracked and
	// ignored). It also adds a tracked file in a directory that will also hold
	// ignored content (a partly ignored directory).
	writeFileAt(t, repo, "config.yaml", "k: v\n")
	writeFileAt(t, repo, "keepdir/keep.go", "package keep\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-m", "second")

	// Ignore rules committed after config.yaml is already tracked.
	writeFileAt(t, repo, ".gitignore", "config.yaml\nignored.txt\n*.log\nfullyignored/\ndist/\n")
	gitRunIn(t, repo, "add", ".gitignore")
	gitRunIn(t, repo, "commit", "-m", "ignore")

	// Untracked content (visible and ignored variants).
	writeFileAt(t, repo, "untracked.txt", "x\n")
	writeFileAt(t, repo, "ignored.txt", "x\n")
	writeFileAt(t, repo, "app.log", "x\n")
	writeFileAt(t, repo, "fullyignored/x.txt", "x\n")
	writeFileAt(t, repo, "dist/keep.txt", "x\n")
	writeFileAt(t, repo, "dist/other.txt", "x\n")
	writeFileAt(t, repo, "keepdir/skip.log", "x\n")

	// Force a tracked file under an otherwise-ignored dir, so dist/ becomes
	// partially ignored (has tracked content) while its siblings stay ignored.
	gitRunIn(t, repo, "add", "-f", "dist/keep.txt")
	gitRunIn(t, repo, "commit", "-m", "forced")

	cases := []struct {
		rel  string
		want bool // true: the Git corpus holds the path
	}{
		{"tracked.go", true},    // tracked-clean
		{"config.yaml", true},   // tracked-but-ignored (index wins)
		{".gitignore", true},    // tracked
		{"untracked.txt", true}, // untracked-non-ignored
		{"ignored.txt", false},  // untracked-ignored
		{"app.log", false},      // ignored by *.log
		{"ghost.log", false},    // non-existent, matches *.log
		{"fullyignored", false}, // fully-ignored dir
		{"fullyignored/x.txt", false},
		{"dist", true},          // partially-ignored dir (forced child)
		{"dist/keep.txt", true}, // force-added tracked file
		{"dist/other.txt", false},
		{"keepdir", true},         // partially-ignored dir (tracked child)
		{"keepdir/keep.go", true}, // tracked
		{"keepdir/skip.log", false},
	}

	paths := make([]string, 0, len(cases)+2)
	wantByPath := make(map[string]bool, len(cases)+2)
	for _, c := range cases {
		p := filepath.Join(repo, c.rel)
		paths = append(paths, p)
		wantByPath[p] = c.want
	}
	// The worktree root itself is never ignored.
	paths = append(paths, repo)
	wantByPath[repo] = true
	// classifyVisibility decides a path outside the worktree before Git runs.
	outside := filepath.Dir(repo)
	paths = append(paths, outside)
	wantByPath[outside] = false

	got, err := classifyVisibility(context.Background(), repo, paths)
	if err != nil {
		t.Fatalf("classifyVisibility: %v", err)
	}
	for p, want := range wantByPath {
		if v, ok := got[p]; !ok || v != want {
			t.Errorf("%s: got %v (present %t), want %v", p, v, ok, want)
		}
	}
	if len(got) != len(wantByPath) {
		t.Errorf("result has %d entries, want %d", len(got), len(wantByPath))
	}
}

// TestGitVisibility_SubmoduleInteriorRoutesOutside covers the exit-128 path.
// A path inside a submodule aborts the batch check, and the retry of each
// path classifies it false. The caller then routes it to a folder corpus.
func TestGitVisibility_SubmoduleInteriorRoutesOutside(t *testing.T) {
	requireGit(t)

	inner := initGitRepoNoRemote(t, "inner.txt", "x\n")
	outer := initGitRepoNoRemote(t, "outer.txt", "y\n")
	// Git refuses file-protocol submodules by default; the add allows them.
	gitRunIn(t, outer, "-c", "protocol.file.allow=always", "submodule", "add", inner, "modsub")
	gitRunIn(t, outer, "commit", "-m", "add submodule")
	// An ignored file alongside the submodule so the exit-128 per-path retry
	// also exercises the ignored arm (not only the aborted and visible arms).
	ignored := writeIgnoredFile(t, outer, "secret.txt", "shh\n")

	interior := filepath.Join(outer, "modsub", "inner.txt")
	tracked := filepath.Join(outer, "outer.txt")

	got, err := classifyVisibility(context.Background(), outer, []string{interior, tracked, ignored})
	if err != nil {
		t.Fatalf("classifyVisibility: %v", err)
	}
	if v, ok := got[interior]; !ok || v {
		t.Errorf("submodule interior: got %v (present %t), want false", v, ok)
	}
	if v, ok := got[tracked]; !ok || !v {
		t.Errorf("sibling tracked file: got %v (present %t), want true", v, ok)
	}
	if v, ok := got[ignored]; !ok || v {
		t.Errorf("ignored sibling (128 retry ignored arm): got %v (present %t), want false", v, ok)
	}
}

// TestGitVisibility_AllPrefilteredNoSubprocess gives only the worktree root
// and an outside path. classifyVisibility decides both before Git runs, so it
// starts no check-ignore process: it returns early with no path within.
func TestGitVisibility_AllPrefilteredNoSubprocess(t *testing.T) {
	requireGit(t)

	repo := initGitRepoNoRemote(t, "a.go", "package main\n")
	outside := filepath.Dir(repo)
	// No Git executable from here: a check-ignore subprocess would fail the
	// call.
	t.Setenv("PATH", t.TempDir())

	got, err := classifyVisibility(context.Background(), repo, []string{repo, outside})
	if err != nil {
		t.Fatalf("classifyVisibility: %v", err)
	}
	if v, ok := got[repo]; !ok || !v {
		t.Errorf("worktree root: got %v (present %t), want true", v, ok)
	}
	if v, ok := got[outside]; !ok || v {
		t.Errorf("outside path: got %v (present %t), want false", v, ok)
	}
	if len(got) != 2 {
		t.Errorf("result has %d entries, want 2", len(got))
	}
}

// TestGitVisibility_LeadingColonOperands checks that check-ignore does not
// read an operand whose repository-relative path starts with ':' as a Git
// pathspec. ":weird" is a real file, and ":!evil" looks like exclude magic.
// Both must classify by their real ignore status.
func TestGitVisibility_LeadingColonOperands(t *testing.T) {
	requireGit(t)

	repo := initGitRepoNoRemote(t, "a.go", "package main\n")
	writeFileAt(t, repo, ".gitignore", ":weird\n")
	gitRunIn(t, repo, "add", ".gitignore")
	gitRunIn(t, repo, "commit", "-m", "ignore")
	writeFileAt(t, repo, ":weird", "x\n") // matches the ignore rule
	writeFileAt(t, repo, ":!evil", "y\n") // untracked, not ignored

	ignored := filepath.Join(repo, ":weird")
	visible := filepath.Join(repo, ":!evil")
	got, err := classifyVisibility(context.Background(), repo, []string{ignored, visible})
	if err != nil {
		t.Fatalf("classifyVisibility: %v", err)
	}
	if v, ok := got[ignored]; !ok || v {
		t.Errorf(":weird (ignored, leading colon): got %v (present %t), want false", v, ok)
	}
	if v, ok := got[visible]; !ok || !v {
		t.Errorf(":!evil (untracked, pathspec-magic name): got %v (present %t), want true", v, ok)
	}
}

// writeFileAt is writeFixtureFile for the path rel below repo.
func writeFileAt(tb testing.TB, repo, rel, content string) {
	tb.Helper()
	writeFixtureFile(tb, filepath.Join(repo, rel), content)
}
