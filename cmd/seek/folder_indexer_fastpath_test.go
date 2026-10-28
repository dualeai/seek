package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// TestEnsureFolderCorpusFresh_WarmCallRemainsSearchable checks that a warm
// call on a built corpus with a stored state gives corpusSearchable. The warm
// call must return before it takes a build slot. A warm call that waits for
// a slot runs until the go test timeout, which prints each goroutine.
func TestEnsureFolderCorpusFresh_WarmCallRemainsSearchable(t *testing.T) {
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
	// The warm return compares the state file with the walk, so the first
	// call must write it.
	if cached := readStateFile(plan.cacheDir); cached == "" {
		t.Fatal("the cold call did not write the state file")
	}
	// With every build slot held, only the warm return before the slot can
	// answer.
	release := holdEveryBuildSlot(t)
	state, err := ensureFolderCorpusFresh(ctx, plan)
	release()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if state != corpusSearchable {
		t.Fatalf("warm call state = %v, want corpusSearchable", state)
	}
}

// holdEveryBuildSlot takes all of buildWindowBytes from buildWindows, so a
// build waits for a slot; release frees it. A slot that another test kept
// makes it wait until the go test timeout, which prints each goroutine.
func holdEveryBuildSlot(t *testing.T) (release func()) {
	t.Helper()
	if err := buildWindows.Acquire(t.Context(), buildWindowBytes); err != nil {
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

// TestFolderCorpusState_GitContentNotInStateHash checks that content in .git
// does not change the parent folder state.
func TestFolderCorpusState_GitContentNotInStateHash(t *testing.T) {
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

// TestEnsureFolderCorpusFresh_ColdThenWarm_SearchableBoth builds a folder of
// 25 files cold, then calls again warm. Both calls give corpusSearchable, and
// the warm call returns before it takes a build slot. A warm call that waits
// for a slot runs until the go test timeout, which prints each goroutine.
func TestEnsureFolderCorpusFresh_ColdThenWarm_SearchableBoth(t *testing.T) {
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
	warm, err := ensureFolderCorpusFresh(ctx, plan)
	release()
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if warm != corpusSearchable {
		t.Fatalf("warm state = %v, want corpusSearchable", warm)
	}
}

// TestWarmFolderVectorSearchTakesNoBuildSlot runs a warm folder search that
// reads vectors while every build slot is held. The fast path finds the bound
// generation of the folder's semantic source with no build slot, so the
// search ends. b.go holds no word of the query, so only the vector read can
// print it. The warm search embeds no unit and prints what the cold search
// printed, b.go included. A search that waits for a slot never ends. Then go
// test -timeout stops the process with a dump of each goroutine, which shows
// the wait in acquireBuildSlot. The process ends there, so no later test
// shares os.Stdout with the waiting search.
func TestWarmFolderVectorSearchTakesNoBuildSlot(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.go", "package a\n// alpha beta handler\n")
	writeFileAt(t, folder, "b.go", "package a\n// dispatches a request to its handler\n")
	scorer := observeModel(&hybridTestScorer{mode: payloadAccept})
	model := scorer.factory()
	cold, err := seekWith(t, model, "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	embeds := scorer.embeds.Load()
	if embeds == 0 || !slices.Contains(headerPaths(cold), filepath.Join(folder, "b.go")) {
		t.Fatalf("the cold search embedded %d calls and printed %v, want b.go from the vectors:\n%s",
			embeds, headerPaths(cold), cold)
	}
	release := holdEveryBuildSlot(t)
	warm, err := seekWith(t, model, "alpha beta", folder)
	release()
	if err != nil {
		t.Fatalf("the warm vector search failed while every build slot was held: %v", err)
	}
	if warm != cold || scorer.embeds.Load() != embeds {
		t.Fatalf("the warm search embedded %d calls, want 0, and must print b.go from the vectors as the cold search did; cold:\n%s\nwarm:\n%s",
			scorer.embeds.Load()-embeds, cold, warm)
	}
}
