package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestEnsureFolderCorpusFresh_WarmCallRemainsSearchable checks that a built
// corpus with a stored state remains searchable on a warm call, and that the
// warm call returns before it takes a build slot.
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
	// With every build slot held, only the warm return before the slot can
	// answer.
	release := holdEveryBuildSlot(t)
	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	state, err := ensureFolderCorpusFresh(warmCtx, plan)
	cancel()
	release()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if state != corpusSearchable {
		t.Fatalf("warm call state = %v, want corpusSearchable", state)
	}
}

// holdEveryBuildSlot takes the whole build window, so a build waits for a
// slot; release frees it.
func holdEveryBuildSlot(t *testing.T) (release func()) {
	t.Helper()
	// A slot that another test kept fails here, not at the suite timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := buildWindows.Acquire(ctx, buildWindowBytes); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { buildWindows.Release(buildWindowBytes) }) }
	t.Cleanup(release)
	return release
}

// TestFolderCorpusState_HighFanout_NoStallNoDrop checks that the parallel
// walk returns every file when there are more directories than workers.
func TestFolderCorpusState_HighFanout_NoStallNoDrop(t *testing.T) {
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
	state, selected, err := folderCorpusState(t.Context(), plan)
	if err != nil {
		t.Fatalf("folderCorpusState: %v", err)
	}
	if state == "" {
		t.Fatal("expected non-empty stateHash")
	}
	if got := len(selected); got != N {
		t.Fatalf("selected count = %d, want %d (the parallel walk dropped entries)", got, N)
	}
}

// TestFolderCorpusState_IdempotentSelectedAndStateHash checks that two
// scans of an unchanged tree return the same state and selected file count.
func TestFolderCorpusState_IdempotentSelectedAndStateHash(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(root, "f"+strconv.Itoa(i)+".txt"), []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFolderTestCorpus(t, root)
	ctx := t.Context()
	hash1, sel1, err := folderCorpusState(ctx, plan)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	hash2, sel2, err := folderCorpusState(ctx, plan)
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

	preHash, _, err := folderCorpusState(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "sentinel"), []byte("v2-mutated"), 0o644); err != nil {
		t.Fatal(err)
	}
	postHash, _, err := folderCorpusState(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if preHash != postHash {
		t.Fatalf(".git sentinel mutation leaked into stateHash:\n  pre  = %q\n  post = %q", preHash, postHash)
	}
}

// ---------------------------------------------------------------------
// End-to-end smoke: cumulative cold + warm path.
// ---------------------------------------------------------------------

// TestEnsureFolderCorpusFresh_ColdThenWarm_SearchableBoth builds a folder of
// 25 files cold, then calls again warm: both calls give corpusSearchable, and
// the warm call returns before it takes a build slot.
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
	// With every build slot held, only the warm return before the slot can
	// answer.
	release := holdEveryBuildSlot(t)
	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	warm, err := ensureFolderCorpusFresh(warmCtx, plan)
	cancel()
	release()
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if warm != corpusSearchable {
		t.Fatalf("warm state = %v, want corpusSearchable", warm)
	}
}
