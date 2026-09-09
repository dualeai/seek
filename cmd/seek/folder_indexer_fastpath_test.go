package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestEnsureFolderCorpusFresh_WarmCallRemainsSearchable checks that a built
// corpus with a stored state remains searchable on a warm call.
func TestEnsureFolderCorpusFresh_WarmCallRemainsSearchable(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes Zoekt indexer + ctags subprocess")
	}
	requireTools(t)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := planFolderTestCorpus(t, root)
	ctx := context.Background()

	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// After the first call the state file must be populated; the
	// warm-path gate depends on this.
	if cached := readStateFile(plan.cacheDir); cached == "" {
		t.Fatal("cold call did not write the state file; cache-gate test would be invalid")
	}
	state, err := ensureFolderCorpusFresh(ctx, plan)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if state != corpusSearchable {
		t.Fatalf("warm call state = %v, want corpusSearchable", state)
	}
}

// TestFolderCorpusStateParallel_HighFanout_NoStallNoDrop checks that the
// parallel dispatcher returns every file when there are more entries than
// workers.
func TestFolderCorpusStateParallel_HighFanout_NoStallNoDrop(t *testing.T) {
	root := t.TempDir()
	const N = 200
	for i := 0; i < N; i++ {
		sub := filepath.Join(root, "d"+strconv.Itoa(i))
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFolderTestCorpus(t, root)
	state, selected, err := folderCorpusStateParallel(t.Context(), plan)
	if err != nil {
		t.Fatalf("folderCorpusStateParallel: %v", err)
	}
	if state == "" {
		t.Fatal("expected non-empty stateHash")
	}
	if got := len(selected); got != N {
		t.Fatalf("selected count = %d, want %d (parallel dispatcher dropped entries)", got, N)
	}
}

// TestFolderCorpusStateParallel_IdempotentSelectedAndStateHash checks that two
// scans of an unchanged tree return the same state and selected file count.
func TestFolderCorpusStateParallel_IdempotentSelectedAndStateHash(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(root, "f"+strconv.Itoa(i)+".txt"), []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFolderTestCorpus(t, root)
	ctx := t.Context()
	hash1, sel1, err := folderCorpusStateParallel(ctx, plan)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	hash2, sel2, err := folderCorpusStateParallel(ctx, plan)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if hash1 != hash2 {
		t.Fatalf("stateHash drift: %q vs %q", hash1, hash2)
	}
	if len(sel1) != len(sel2) {
		t.Fatalf("selected count drift: %d vs %d", len(sel1), len(sel2))
	}
}

type fakeDirEntry struct {
	name        string
	isDir       bool
	typ         os.FileMode
	infoCalls   int
	failOnInfo  bool
	regInfoFile string
}

func (f *fakeDirEntry) Name() string      { return f.name }
func (f *fakeDirEntry) IsDir() bool       { return f.isDir }
func (f *fakeDirEntry) Type() os.FileMode { return f.typ }
func (f *fakeDirEntry) Info() (os.FileInfo, error) {
	f.infoCalls++
	if f.failOnInfo {
		return nil, errors.New("Info() must not be called for non-regular entries")
	}
	return os.Lstat(f.regInfoFile)
}

// Check each non-zero type bit from io/fs.FileMode. Modifier bits must also
// return before Info because production checks Type() != 0.
// ModeDir is omitted because fingerprintRootEntry checks IsDir first.
func TestFingerprintRootEntry_NonRegular_SkipsEntryInfo(t *testing.T) {
	plan := planFolderTestCorpus(t, t.TempDir())

	cases := []struct {
		name string
		typ  os.FileMode
	}{
		{"symlink", os.ModeSymlink},
		{"named-pipe", os.ModeNamedPipe},
		{"socket", os.ModeSocket},
		{"device", os.ModeDevice},
		{"char-device", os.ModeDevice | os.ModeCharDevice},
		{"irregular", os.ModeIrregular},
		{"setuid-marker", os.ModeSetuid},
		{"setgid-marker", os.ModeSetgid},
		{"sticky-marker", os.ModeSticky},
		{"temporary-marker", os.ModeTemporary},
		{"append-marker", os.ModeAppend},
		{"exclusive-marker", os.ModeExclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := &fakeDirEntry{
				name:       "x",
				typ:        tc.typ,
				failOnInfo: true,
			}
			piece, err := fingerprintRootEntry(t.Context(), plan, entry, false, false)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if piece.present {
				t.Fatalf("expected piece.present=false for non-regular type %v", tc.typ)
			}
			if entry.infoCalls != 0 {
				t.Fatalf("entry.Info() called %d times for non-regular; want 0", entry.infoCalls)
			}
		})
	}
}

// TestFingerprintRootEntry_RegularFile_CallsInfo checks that a regular entry
// loads its file information.
func TestFingerprintRootEntry_RegularFile_CallsInfo(t *testing.T) {
	plan := planFolderTestCorpus(t, t.TempDir())
	regFile := filepath.Join(t.TempDir(), "real.txt")
	if err := os.WriteFile(regFile, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := &fakeDirEntry{
		name:        "real.txt",
		typ:         0, // regular
		regInfoFile: regFile,
	}
	piece, err := fingerprintRootEntry(t.Context(), plan, entry, false, false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !piece.present {
		t.Fatalf("expected piece.present=true for regular file")
	}
	if entry.infoCalls != 1 {
		t.Fatalf("entry.Info() called %d times for regular; want 1", entry.infoCalls)
	}
}

// TestWalkDirectory_GitContentNotInStateHash checks that content in .git does
// not change the parent folder state.
func TestWalkDirectory_GitContentNotInStateHash(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "sentinel"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := planFolderTestCorpus(t, root)

	preHash, _, err := folderCorpusStateParallel(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "sentinel"), []byte("v2-mutated"), 0o644); err != nil {
		t.Fatal(err)
	}
	postHash, _, err := folderCorpusStateParallel(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if preHash != postHash {
		t.Fatalf(".git sentinel mutation leaked into stateHash:\n  pre  = %q\n  post = %q", preHash, postHash)
	}
}

// TestWalkDirectory_GitPresenceAffectsBoundaryMarker checks that adding .git
// changes the boundary marker even though its content is excluded.
func TestWalkDirectory_GitPresenceAffectsBoundaryMarker(t *testing.T) {
	noGit := t.TempDir()
	if err := os.WriteFile(filepath.Join(noGit, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	withGit := t.TempDir()
	if err := os.WriteFile(filepath.Join(withGit, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(withGit, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	hashNoGit, _, err := folderCorpusStateParallel(t.Context(), planFolderTestCorpus(t, noGit))
	if err != nil {
		t.Fatal(err)
	}
	hashWithGit, _, err := folderCorpusStateParallel(t.Context(), planFolderTestCorpus(t, withGit))
	if err != nil {
		t.Fatal(err)
	}
	if hashNoGit == hashWithGit {
		t.Fatalf("boundary marker did not contribute: same hash for trees with/without .git (%q)", hashNoGit)
	}
}

// ---------------------------------------------------------------------
// End-to-end smoke: cumulative cold + warm path.
// ---------------------------------------------------------------------

func TestEnsureFolderCorpusFresh_ColdThenWarm_SearchableBoth(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes Zoekt indexer + ctags subprocess")
	}
	requireTools(t)

	root := t.TempDir()
	for i := 0; i < 25; i++ {
		if err := os.WriteFile(
			filepath.Join(root, "f"+strconv.Itoa(i)+".txt"),
			[]byte("e2e payload "+strconv.Itoa(i)),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFolderTestCorpus(t, root)
	ctx := t.Context()

	cold, err := ensureFolderCorpusFresh(ctx, plan)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	if cold != corpusSearchable {
		t.Fatalf("cold state = %v, want corpusSearchable", cold)
	}
	warm, err := ensureFolderCorpusFresh(ctx, plan)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if warm != corpusSearchable {
		t.Fatalf("warm state = %v, want corpusSearchable", warm)
	}
}
