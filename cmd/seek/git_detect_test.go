package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectGitBoundary_DirForm(t *testing.T) {
	root := t.TempDir()
	writeGitTriadAt(t, filepath.Join(root, ".git"))

	b, status := detectGitBoundary(root, root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.Mode != rootTypeDirectory {
		t.Errorf("Mode=%v, want rootTypeDirectory", b.Mode)
	}
	if b.RepoDir != root {
		t.Errorf("RepoDir=%q, want %q", b.RepoDir, root)
	}
	if want := filepath.Join(root, ".git"); b.GitDir != want {
		t.Errorf("GitDir=%q, want %q", b.GitDir, want)
	}
	if b.CommonDir != b.GitDir {
		t.Errorf("CommonDir=%q, want %q", b.CommonDir, b.GitDir)
	}
}

func TestDetectGitBoundary_NotBoundary(t *testing.T) {
	root := t.TempDir()
	_, status := detectGitBoundary(root, root)
	if status != notBoundary {
		t.Fatalf("empty dir status=%v, want notBoundary", status)
	}
}

func TestDetectGitBoundary_DirMissingHEAD(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No HEAD file.
	_, status := detectGitBoundary(root, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (missing HEAD)", status)
	}
}

func TestDetectGitBoundary_DirMissingObjects(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No objects/ dir.
	_, status := detectGitBoundary(root, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (missing objects/)", status)
	}
}

func TestDetectGitBoundary_DirMissingRefs(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, status := detectGitBoundary(root, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (missing refs/)", status)
	}
}

func TestDetectGitBoundary_HEADIsDir(t *testing.T) {
	// A directory named HEAD (not a regular file) must NOT satisfy the
	// triad. Guards against attacker-crafted trees with HEAD/ as a dir.
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "HEAD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, status := detectGitBoundary(root, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (HEAD is dir)", status)
	}
}

func TestDetectGitBoundary_WorktreeRelative(t *testing.T) {
	root := t.TempDir()
	realGitDir := filepath.Join(root, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	pointer := "gitdir: ../real-git\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(worktree, root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.Mode != rootTypeWorktree {
		t.Errorf("Mode=%v, want rootTypeWorktree", b.Mode)
	}
	if b.GitDir != realGitDir {
		t.Errorf("GitDir=%q, want %q", b.GitDir, realGitDir)
	}
}

func TestDetectGitBoundary_WorktreeAbsolute(t *testing.T) {
	root := t.TempDir()
	realGitDir := filepath.Join(root, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(worktree, root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.GitDir != realGitDir {
		t.Errorf("GitDir=%q, want %q", b.GitDir, realGitDir)
	}
}

func TestDetectGitBoundary_WorktreeCRLF(t *testing.T) {
	root := t.TempDir()
	realGitDir := filepath.Join(root, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree, root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed (CRLF)", status)
	}
}

func TestDetectGitBoundary_WorktreeBrokenPointer(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+filepath.Join(root, "does-not-exist")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (dangling pointer)", status)
	}
}

func TestDetectGitBoundary_WorktreeMissingPrefix(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("not a gitdir line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (no gitdir: prefix)", status)
	}
}

func TestDetectGitBoundary_WorktreeEscapeRejected(t *testing.T) {
	// Pointer resolves outside the scan root. Must be rejected to prevent
	// seek from following an attacker-crafted pointer to /etc or similar.
	scanRoot := t.TempDir()
	outside := t.TempDir()
	realGitDir := filepath.Join(outside, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(scanRoot, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree, scanRoot)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (pointer escape)", status)
	}
}

func TestDetectGitBoundary_WorktreeSymlinkEscapeRejected(t *testing.T) {
	scanRoot := t.TempDir()
	outside := t.TempDir()
	realGitDir := filepath.Join(outside, "real-git")
	writeGitTriadAt(t, realGitDir)

	linkDir := filepath.Join(scanRoot, "links")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	worktree := filepath.Join(scanRoot, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../links/outside/real-git\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree, scanRoot)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (symlink pointer escape)", status)
	}
}

func TestDetectGitBoundary_WorktreeEscapeAmbiguousWithEmptyScope(t *testing.T) {
	// CLI/operand callers pass scanRoot="", but the fast detector should not
	// blindly trust arbitrary external pointers. Ambiguous lets those callers
	// fall back to git rev-parse for validation.
	root := t.TempDir()
	realGitDir := filepath.Join(root, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree, "")
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (empty scope external pointer)", status)
	}
}

func TestDetectGitBoundary_SymlinkedGitRefused(t *testing.T) {
	// `.git` as symlink: refused (notBoundary) without following.
	root := t.TempDir()
	realGitDir := filepath.Join(root, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realGitDir, filepath.Join(worktree, ".git")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, status := detectGitBoundary(worktree, root)
	if status != notBoundary {
		t.Fatalf("status=%v, want notBoundary (symlink refused)", status)
	}
}

func TestDetectGitBoundary_LargeGitFileNoOOM(t *testing.T) {
	// Pathological `.git` file: 10 MiB of junk. Detector must NOT slurp
	// the whole thing into memory (DoS guard); reads only first 1 KiB.
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 10*1024*1024)
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (no gitdir: prefix in junk)", status)
	}
}

func TestDetectGitBoundary_LongGitdirLineCappedAt1KiB(t *testing.T) {
	// First line longer than 1 KiB: detector reads at most 1 KiB, so the
	// (incomplete) line lacks a closing newline; ReadString returns EOF
	// with the buffered prefix. Parser still extracts the gitdir path if
	// the prefix is intact, or fails cleanly. Either way: no panic, no OOM.
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	long := "gitdir: " + strings.Repeat("a", 5000) + "\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (truncated pointer cannot resolve)", status)
	}
}

func TestGitBoundary_ToGitPaths(t *testing.T) {
	root := t.TempDir()
	writeGitTriadAt(t, filepath.Join(root, ".git"))
	b, status := detectGitBoundary(root, root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v", status)
	}
	p := b.toGitPaths()
	if p.ConfigPath != filepath.Join(b.CommonDir, "config") {
		t.Errorf("ConfigPath=%q", p.ConfigPath)
	}
}

func TestDetectGitBoundary_EmptyPointerFile(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, status := detectGitBoundary(worktree, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (empty .git file)", status)
	}
}

func TestDetectGitBoundary_WhitespaceOnlyPointer(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir:    \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, status := detectGitBoundary(worktree, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (whitespace-only pointer)", status)
	}
}

func TestDetectGitBoundary_NULBytePointer(t *testing.T) {
	root := t.TempDir()
	realGitDir := filepath.Join(root, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	// `/tmp/foo\x00/etc/passwd` is a syscall-API truncation attack vector.
	// Even though our path lands at the resolvable prefix, NUL in a path
	// is malformed and must be refused.
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\x00/etc/passwd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, status := detectGitBoundary(worktree, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (NUL in pointer)", status)
	}
}

func TestDetectGitBoundary_SymlinkedHEADRefused(t *testing.T) {
	// HEAD as a symlink to an attacker-readable file (e.g. /etc/passwd
	// shape): hasGitTriad must refuse via Lstat.
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(gitDir, "HEAD")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, status := detectGitBoundary(root, root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (HEAD symlink refused)", status)
	}
}

func TestDetectBareRepoAt_True(t *testing.T) {
	// Bare repo layout: HEAD + objects/ + refs/ at top-level, no `.git`.
	// detectBareRepoAt is the corpus-root-only check that callers use
	// to refuse bare repos cleanly. detectGitBoundary intentionally
	// does NOT include this check on its hot path (walker burns ~1µs
	// per subdir otherwise on large trees).
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectBareRepoAt(root) {
		t.Fatal("detectBareRepoAt=false, want true (bare repo layout)")
	}
	// detectGitBoundary returns notBoundary, NOT ambiguous, because the
	// bare-repo check moved out of its hot path.
	if _, status := detectGitBoundary(root, root); status != notBoundary {
		t.Fatalf("detectGitBoundary status=%v, want notBoundary", status)
	}
}

func TestDetectBareRepoAt_FalseOnRegularRepo(t *testing.T) {
	// A regular repo with `.git/` must NOT be classified as bare.
	root := t.TempDir()
	writeGitTriadAt(t, filepath.Join(root, ".git"))
	if detectBareRepoAt(root) {
		t.Fatal("regular repo with .git/ misclassified as bare")
	}
}

func TestDetectBareRepoAt_FalseOnPlainDir(t *testing.T) {
	root := t.TempDir()
	if detectBareRepoAt(root) {
		t.Fatal("empty dir misclassified as bare repo")
	}
}

func TestDetectStatusString(t *testing.T) {
	cases := []struct {
		status detectStatus
		want   string
	}{
		{notBoundary, "notBoundary"},
		{boundaryConfirmed, "boundaryConfirmed"},
		{ambiguous, "ambiguous"},
		{detectStatus(99), "detectStatus(99)"},
	}
	for _, c := range cases {
		if got := c.status.String(); got != c.want {
			t.Errorf("detectStatus(%d).String()=%q, want %q", c.status, got, c.want)
		}
	}
}

func TestDetectGitBoundary_CommonDirRead(t *testing.T) {
	// Worktree with explicit commondir file: detector reads it and reports
	// the resolved common dir.
	root := t.TempDir()
	parentGit := filepath.Join(root, "parent.git")
	writeGitTriadAt(t, parentGit)

	worktree := filepath.Join(root, "feature-wt")
	wtGit := writeLinkedWorktreeAdmin(t, parentGit, worktree, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(worktree, root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.GitDir != wtGit {
		t.Errorf("GitDir=%q, want %q", b.GitDir, wtGit)
	}
	if b.CommonDir != parentGit {
		t.Errorf("CommonDir=%q, want %q (resolved from commondir file)", b.CommonDir, parentGit)
	}
}

func TestDetectGitBoundary_LinkedWorktreeBackrefAcceptedOutsideScanRoot(t *testing.T) {
	scanRoot := t.TempDir()
	outside := t.TempDir()
	parentGit := filepath.Join(outside, "parent.git")
	writeGitTriadAt(t, parentGit)

	worktree := filepath.Join(scanRoot, "feature-wt")
	wtGit := writeLinkedWorktreeAdmin(t, parentGit, worktree, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(worktree, scanRoot)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.GitDir != wtGit {
		t.Errorf("GitDir=%q, want %q", b.GitDir, wtGit)
	}
	if b.CommonDir != parentGit {
		t.Errorf("CommonDir=%q, want %q", b.CommonDir, parentGit)
	}
}

func TestDetectGitBoundary_LinkedWorktreeMissingBackrefRejected(t *testing.T) {
	for _, tc := range []struct {
		name             string
		parentInsideScan bool
	}{
		{name: "outside scan root"},
		{name: "inside scan root", parentInsideScan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scanRoot := t.TempDir()
			parentRoot := t.TempDir()
			if tc.parentInsideScan {
				parentRoot = scanRoot
			}
			parentGit := filepath.Join(parentRoot, "parent.git")
			writeGitTriadAt(t, parentGit)

			worktree := filepath.Join(scanRoot, "feature-wt")
			wtGit := writeLinkedWorktreeAdmin(t, parentGit, worktree, "feature")
			if err := os.Remove(filepath.Join(wtGit, "gitdir")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(worktree, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, status := detectGitBoundary(worktree, scanRoot); status != ambiguous {
				t.Fatalf("status=%v, want ambiguous (missing linked-worktree backref)", status)
			}
		})
	}
}

func TestDetectGitBoundary_LinkedWorktreeWrongBackrefRejected(t *testing.T) {
	scanRoot := t.TempDir()
	outside := t.TempDir()
	parentGit := filepath.Join(outside, "parent.git")
	writeGitTriadAt(t, parentGit)

	worktree := filepath.Join(scanRoot, "feature-wt")
	otherWorktree := filepath.Join(scanRoot, "other-wt")
	wtGit := writeLinkedWorktreeAdmin(t, parentGit, otherWorktree, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, status := detectGitBoundary(worktree, scanRoot); status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (wrong linked-worktree backref)", status)
	}
}

func writeLinkedWorktreeAdmin(t testing.TB, commonDir, worktree, name string) string {
	t.Helper()
	wtGit := filepath.Join(commonDir, "worktrees", name)
	if err := os.MkdirAll(filepath.Join(wtGit, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGit, "HEAD"), []byte("ref: refs/heads/"+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGit, "gitdir"), []byte(filepath.Join(worktree, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wtGit
}

func TestDetectGitBoundary_CommonDirEscapeRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideGit := filepath.Join(outside, "parent.git")
	writeGitTriadAt(t, outsideGit)

	wtGit := filepath.Join(root, "gitdir")
	if err := os.MkdirAll(filepath.Join(wtGit, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGit, "HEAD"), []byte("ref: refs/heads/feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGit, "commondir"), []byte(outsideGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	worktree := filepath.Join(root, "feature-wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, status := detectGitBoundary(worktree, root); status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (commondir escape)", status)
	}
}
