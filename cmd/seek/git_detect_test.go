package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDetectGitBoundary_DirForm(t *testing.T) {
	root := t.TempDir()
	writeGitTriadAt(t, filepath.Join(root, ".git"))

	b, status := detectGitBoundary(root)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.RepoDir != root {
		t.Errorf("RepoDir=%q, want %q", b.RepoDir, root)
	}
	if want := filepath.Join(root, ".git"); b.CommonDir != want {
		t.Errorf("CommonDir=%q, want %q", b.CommonDir, want)
	}
}

func TestDetectGitBoundary_NotBoundary(t *testing.T) {
	root := t.TempDir()
	_, status := detectGitBoundary(root)
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
	_, status := detectGitBoundary(root)
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
	_, status := detectGitBoundary(root)
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
	_, status := detectGitBoundary(root)
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
	_, status := detectGitBoundary(root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (HEAD is dir)", status)
	}
}

func TestDetectGitBoundary_WorktreeRelative(t *testing.T) {
	root := t.TempDir()
	// A submodule Git directory whose core.worktree names the worktree: the
	// boundary rule confirms it.
	realGitDir := filepath.Join(root, "super", ".git", "modules", "real-git")
	writeSubmoduleGitDir(t, realGitDir, filepath.Join(root, "wt"))

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	pointer := "gitdir: ../super/.git/modules/real-git\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(worktree)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.CommonDir != realGitDir {
		t.Errorf("CommonDir=%q, want %q", b.CommonDir, realGitDir)
	}
}

func TestDetectGitBoundary_WorktreeAbsolute(t *testing.T) {
	root := t.TempDir()
	// A submodule Git directory whose core.worktree names the worktree: the
	// boundary rule confirms it.
	realGitDir := filepath.Join(root, "super", ".git", "modules", "real-git")
	writeSubmoduleGitDir(t, realGitDir, filepath.Join(root, "wt"))

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(worktree)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.CommonDir != realGitDir {
		t.Errorf("CommonDir=%q, want %q", b.CommonDir, realGitDir)
	}
}

// TestDetectGitBoundary_ForeignModulePointerAsksGit points a `.git` file at a
// submodule Git directory that belongs to another directory, or that names no
// worktree. Neither is confirmed: Git decides.
func TestDetectGitBoundary_ForeignModulePointerAsksGit(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim", "sec")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "victim", ".git", "modules", "sec")
	writeSubmoduleGitDir(t, foreign, victim)
	bare := filepath.Join(root, "other", ".git", "modules", "plain")
	writeGitTriadAt(t, bare)
	for _, gitDir := range []string{foreign, bare} {
		evil := filepath.Join(root, "evil-"+filepath.Base(gitDir))
		if err := os.MkdirAll(evil, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evil, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, status := detectGitBoundary(evil); status != ambiguous {
			t.Fatalf("%s: status=%v, want ambiguous", gitDir, status)
		}
	}
}

func TestDetectGitBoundary_WorktreeCRLF(t *testing.T) {
	root := t.TempDir()
	// A submodule Git directory whose core.worktree names the worktree: the
	// boundary rule confirms it.
	realGitDir := filepath.Join(root, "super", ".git", "modules", "real-git")
	writeSubmoduleGitDir(t, realGitDir, filepath.Join(root, "wt"))

	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree)
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

	_, status := detectGitBoundary(worktree)
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

	_, status := detectGitBoundary(worktree)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (no gitdir: prefix)", status)
	}
}

func TestDetectGitBoundary_ExternalPointerAsksGit(t *testing.T) {
	// A pointer to a Git directory elsewhere proves nothing on its own: the
	// detector never trusts it, and Git decides.
	searchRoot := t.TempDir()
	outside := t.TempDir()
	realGitDir := filepath.Join(outside, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(searchRoot, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (external pointer)", status)
	}
}

func TestDetectGitBoundary_SymlinkedExternalPointerAsksGit(t *testing.T) {
	// The pointer names a path inside the worktree, but a symlink in that path
	// leads outside. The detector compares real paths, so the target is not
	// in the tree. Canonical temp directories make sure that only the symlink
	// makes the lexical path and the real path different.
	searchRoot := canonTempDir(t)
	outside := canonTempDir(t)
	realGitDir := filepath.Join(outside, "real-git")
	writeGitTriadAt(t, realGitDir)

	worktree := filepath.Join(searchRoot, "wt")
	linkDir := filepath.Join(worktree, "links")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkDir, "outside")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: links/outside/real-git\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status := detectGitBoundary(worktree)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (pointer through a symlink)", status)
	}
}

func TestDetectGitBoundary_SymlinkedGitAsksGit(t *testing.T) {
	// `.git` as symlink: Git follows it, so the boundary is ambiguous and Git
	// decides. notBoundary would hide the repository.
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

	_, status := detectGitBoundary(worktree)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (Git decides)", status)
	}
}

func TestDetectGitBoundary_SeparateGitDirOutsideTreeAsksGit(t *testing.T) {
	// A .git file that names a Git directory outside the worktree, with no
	// core.worktree that names the worktree and no linked-worktree
	// back-reference, proves nothing on its own: Git decides.
	root := t.TempDir()
	realGitDir := filepath.Join(root, "real-git")
	writeGitTriadAt(t, realGitDir)
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../real-git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, status := detectGitBoundary(worktree); status != ambiguous {
		t.Fatalf("status=%v, want ambiguous", status)
	}
}

func TestDetectGitBoundary_LargeGitFileNoOOM(t *testing.T) {
	// Pathological `.git` file: 64 MiB of junk. The detector must not read
	// the whole file into memory (DoS guard); it reads maxGitFileBytes. Only
	// the first KiB holds data; the rest is a sparse hole with no disk space.
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	gitFile := filepath.Join(worktree, ".git")
	if err := os.WriteFile(gitFile, []byte(strings.Repeat("x", 1024)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(gitFile, size); err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, status := detectGitBoundary(worktree)
	runtime.ReadMemStats(&after)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (no gitdir: prefix in junk)", status)
	}
	// 8 MiB is far above one bounded read and far below the file size.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("detectGitBoundary allocated %d bytes for a %d-byte .git file, want one bounded read", allocated, size)
	}
}

func TestDetectGitBoundary_LongGitdirLineConfirmed(t *testing.T) {
	// A valid gitdir line longer than 1 KiB (PATH_MAX on darwin) names a real
	// submodule Git directory. maxGitFileBytes is a hostile-input bound, not a
	// path limit, so the line is read whole and the boundary is confirmed.
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	writeSubmoduleGitDir(t, filepath.Join(root, "super", ".git", "modules", "real-git"), worktree)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	long := "gitdir: " + strings.Repeat("./", 1000) + "../super/.git/modules/real-git\n"
	if len(long) <= 1024 {
		t.Fatalf("line has %d bytes, want more than 1 KiB", len(long))
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, status := detectGitBoundary(worktree); status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
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
	_, status := detectGitBoundary(worktree)
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
	_, status := detectGitBoundary(worktree)
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
	_, status := detectGitBoundary(worktree)
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
	_, status := detectGitBoundary(root)
	if status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (HEAD symlink refused)", status)
	}
}

func TestDetectBareRepoAt_True(t *testing.T) {
	// Bare repo layout: HEAD + objects/ + refs/ at top-level, no `.git`.
	// detectBareRepoAt confirms it; detectGitBoundary reads only a `.git`
	// entry.
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
	// No `.git` entry: detectGitBoundary returns notBoundary, not ambiguous.
	if _, status := detectGitBoundary(root); status != notBoundary {
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

func TestDetectGitBoundary_CommonDirRead(t *testing.T) {
	// Worktree with explicit commondir file: detector reads it and reports
	// the resolved common dir.
	root := t.TempDir()
	parentGit := filepath.Join(root, "parent.git")
	writeGitTriadAt(t, parentGit)

	worktree := filepath.Join(root, "feature-wt")
	wtGit := writeLinkedWorktreeAdmin(t, parentGit, worktree)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(worktree)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.CommonDir != parentGit {
		t.Errorf("CommonDir=%q, want %q (resolved from commondir file)", b.CommonDir, parentGit)
	}
}

func TestDetectGitBoundary_LinkedWorktreeBackrefAcceptedAnywhere(t *testing.T) {
	searchRoot := t.TempDir()
	outside := t.TempDir()
	parentGit := filepath.Join(outside, "parent.git")
	writeGitTriadAt(t, parentGit)

	worktree := filepath.Join(searchRoot, "feature-wt")
	wtGit := writeLinkedWorktreeAdmin(t, parentGit, worktree)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, status := detectGitBoundary(worktree)
	if status != boundaryConfirmed {
		t.Fatalf("status=%v, want boundaryConfirmed", status)
	}
	if b.CommonDir != parentGit {
		t.Errorf("CommonDir=%q, want %q", b.CommonDir, parentGit)
	}
}

func TestDetectGitBoundary_LinkedWorktreeMissingBackrefRejected(t *testing.T) {
	for _, tc := range []struct {
		name            string
		parentAlongside bool
	}{
		{name: "parent elsewhere"},
		{name: "parent alongside", parentAlongside: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searchRoot := t.TempDir()
			parentRoot := t.TempDir()
			if tc.parentAlongside {
				parentRoot = searchRoot
			}
			parentGit := filepath.Join(parentRoot, "parent.git")
			writeGitTriadAt(t, parentGit)

			worktree := filepath.Join(searchRoot, "feature-wt")
			wtGit := writeLinkedWorktreeAdmin(t, parentGit, worktree)
			if err := os.Remove(filepath.Join(wtGit, "gitdir")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(worktree, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, status := detectGitBoundary(worktree); status != ambiguous {
				t.Fatalf("status=%v, want ambiguous (missing linked-worktree backref)", status)
			}
		})
	}
}

func TestDetectGitBoundary_LinkedWorktreeWrongBackrefRejected(t *testing.T) {
	searchRoot := t.TempDir()
	outside := t.TempDir()
	parentGit := filepath.Join(outside, "parent.git")
	writeGitTriadAt(t, parentGit)

	worktree := filepath.Join(searchRoot, "feature-wt")
	otherWorktree := filepath.Join(searchRoot, "other-wt")
	wtGit := writeLinkedWorktreeAdmin(t, parentGit, otherWorktree)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, status := detectGitBoundary(worktree); status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (wrong linked-worktree backref)", status)
	}
}

func writeLinkedWorktreeAdmin(t testing.TB, commonDir, worktree string) string {
	t.Helper()
	const name = "feature"
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

	if _, status := detectGitBoundary(worktree); status != ambiguous {
		t.Fatalf("status=%v, want ambiguous (commondir escape)", status)
	}
}
