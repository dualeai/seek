package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestClassifyVisibility_Matrix exercises every routing cell against a real
// git repo: tracked, tracked-but-ignored, untracked-visible, untracked-
// ignored, fully- vs partially-ignored dirs, force-added files under an
// ignored dir, a non-existent path matching an ignore rule, the worktree
// root, and a path outside the worktree.
func TestClassifyVisibility_Matrix(t *testing.T) {
	requireGit(t)

	repo := initGitRepoNoRemote(t, "tracked.go", "package main\n")

	// Second commit: a file that will later be matched by .gitignore
	// (tracked-but-ignored) and a tracked file inside a dir that also holds
	// ignored content (partially-ignored dir).
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
		want visibility
	}{
		{"tracked.go", visGit},        // tracked-clean
		{"config.yaml", visGit},       // tracked-but-ignored (index wins)
		{".gitignore", visGit},        // tracked
		{"untracked.txt", visGit},     // untracked-non-ignored
		{"ignored.txt", visFallback},  // untracked-ignored
		{"app.log", visFallback},      // ignored by *.log
		{"ghost.log", visFallback},    // non-existent, matches *.log
		{"fullyignored", visFallback}, // fully-ignored dir
		{"fullyignored/x.txt", visFallback},
		{"dist", visGit},          // partially-ignored dir (forced child)
		{"dist/keep.txt", visGit}, // force-added tracked file
		{"dist/other.txt", visFallback},
		{"keepdir", visGit},         // partially-ignored dir (tracked child)
		{"keepdir/keep.go", visGit}, // tracked
		{"keepdir/skip.log", visFallback},
	}

	paths := make([]string, 0, len(cases)+2)
	wantByPath := make(map[string]visibility, len(cases)+2)
	for _, c := range cases {
		p := filepath.Join(repo, c.rel)
		paths = append(paths, p)
		wantByPath[p] = c.want
	}
	// The worktree root itself is never ignored.
	paths = append(paths, repo)
	wantByPath[repo] = visGit
	// A path outside the worktree is short-circuited before git runs.
	outside := filepath.Dir(repo)
	paths = append(paths, outside)
	wantByPath[outside] = visOutside

	got, err := classifyVisibility(context.Background(), repo, paths)
	if err != nil {
		t.Fatalf("classifyVisibility: %v", err)
	}
	for p, want := range wantByPath {
		if got[p] != want {
			t.Errorf("%s: got %v, want %v", p, got[p], want)
		}
	}
	if len(got) != len(wantByPath) {
		t.Errorf("result has %d entries, want %d", len(got), len(wantByPath))
	}
}

// TestGitVisibility_SubmoduleInteriorRoutesOutside covers the exit-128 path:
// a path inside a submodule aborts the batch check, and the per-path retry
// classifies it visOutside (the caller then routes it to a folder corpus).
func TestGitVisibility_SubmoduleInteriorRoutesOutside(t *testing.T) {
	requireGit(t)

	inner := initGitRepoNoRemote(t, "inner.txt", "x\n")
	outer := initGitRepoNoRemote(t, "outer.txt", "y\n")
	// File-protocol submodules are disabled by default; allow it for the add.
	gitRunIn(t, outer, "-c", "protocol.file.allow=always", "submodule", "add", inner, "modsub")
	gitRunIn(t, outer, "commit", "-m", "add submodule")
	// An ignored file alongside the submodule so the exit-128 per-path retry
	// also exercises the visFallback arm (not just visOutside/visGit).
	ignored := writeIgnoredFile(t, outer, "secret.txt", "shh\n")

	interior := filepath.Join(outer, "modsub", "inner.txt")
	tracked := filepath.Join(outer, "outer.txt")

	got, err := classifyVisibility(context.Background(), outer, []string{interior, tracked, ignored})
	if err != nil {
		t.Fatalf("classifyVisibility: %v", err)
	}
	if got[interior] != visOutside {
		t.Errorf("submodule interior: got %v, want visOutside", got[interior])
	}
	if got[tracked] != visGit {
		t.Errorf("sibling tracked file: got %v, want visGit", got[tracked])
	}
	if got[ignored] != visFallback {
		t.Errorf("ignored sibling (128 retry visFallback arm): got %v, want visFallback", got[ignored])
	}
}

// All operands prefilter out (worktree root + an outside path), so no
// check-ignore subprocess runs and the empty-within early return is taken.
func TestGitVisibility_AllPrefilteredNoSubprocess(t *testing.T) {
	requireGit(t)

	repo := initGitRepoNoRemote(t, "a.go", "package main\n")
	outside := filepath.Dir(repo)

	got, err := classifyVisibility(context.Background(), repo, []string{repo, outside})
	if err != nil {
		t.Fatalf("classifyVisibility: %v", err)
	}
	if got[repo] != visGit {
		t.Errorf("worktree root: got %v, want visGit", got[repo])
	}
	if got[outside] != visOutside {
		t.Errorf("outside path: got %v, want visOutside", got[outside])
	}
	if len(got) != 2 {
		t.Errorf("result has %d entries, want 2", len(got))
	}
}

// An operand whose repo-relative path starts with ':' must not be parsed as
// a git pathspec by check-ignore (":weird" is a real file, ":!evil" looks
// like exclude magic). Both must classify by their actual ignore status.
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
	if got[ignored] != visFallback {
		t.Errorf(":weird (ignored, leading colon): got %v, want visFallback", got[ignored])
	}
	if got[visible] != visGit {
		t.Errorf(":!evil (untracked, pathspec-magic name): got %v, want visGit", got[visible])
	}
}

func writeFileAt(tb testing.TB, repo, rel, content string) {
	tb.Helper()
	p := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tb.Fatal(err)
	}
}
