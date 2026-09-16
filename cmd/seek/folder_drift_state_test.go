package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestFolderDrift_KeepsStateWhenNothingWasPublished checks that a folder build
// interrupted by drift keeps its .state file.
//
// Nothing that drifted was published: the folder builder writes into a
// temporary directory and publishes only at the end, so the family on disk
// still matches the manifest and still matches .state. Deleting .state anyway
// removes the delta base, and the next build then rebuilds in full a family
// that was already correct.
//
// The drift is driven through the semantic scorer, which the builder calls
// while the build runs. That needs no production test hook.
func TestFolderDrift_KeepsStateWhenNothingWasPublished(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// DRIFT_STATE_MARKER\n")
	plan := planFolderTestCorpus(t, folder)
	ctx := context.Background()

	newExecution := func(scorer *hybridTestScorer) searchExecution {
		return testSemanticSearchExecution(t, func(context.Context) (semanticModel, error) {
			return scorer, nil
		})
	}

	if _, err := ensureFolderCorpusFreshWithExecution(ctx, plan, newExecution(&hybridTestScorer{})); err != nil {
		t.Fatalf("first folder build: %v", err)
	}
	published := readStateFile(plan.cacheDir)
	if published == "" {
		t.Fatal("the first build wrote no state")
	}
	if !folderFamilyUsable(plan.indexDir) {
		t.Fatal("the first build left no usable family")
	}

	// Add a file while the build runs, so the post-build fingerprint disagrees
	// with the one the build started from.
	var once sync.Once
	drifting := &hybridTestScorer{
		vectorForUnit: func(unit semanticUnit) semanticVector {
			once.Do(func() {
				writeFileAt(t, folder, "late.go", "package sample\n// arrived during the build\n")
			})
			return semanticVector{}
		},
	}
	writeFileAt(t, folder, "second.go", "package sample\n// forces a rebuild\n")

	if _, err := ensureFolderCorpusFreshWithExecution(ctx, plan, newExecution(drifting)); err == nil {
		t.Fatal("the drifted build reported no error")
	}

	if got := readStateFile(plan.cacheDir); got == "" {
		t.Fatal("the drifted build deleted .state although it published nothing")
	}

	// The point of keeping .state is the delta base, so assert that and not
	// only the file. A delta seeds the prior shards with hard links, so the
	// base shard stays the same file. A full rebuild writes a new one.
	baseBefore := firstShardInfo(t, plan.indexDir)
	if _, err := ensureFolderCorpusFreshWithExecution(ctx, plan, newExecution(&hybridTestScorer{})); err != nil {
		t.Fatalf("recovery build: %v", err)
	}
	baseAfter := firstShardInfo(t, plan.indexDir)
	if !os.SameFile(baseBefore, baseAfter) {
		t.Fatal("the recovery build rewrote the base shard, so it lost the delta base")
	}
	if readStateFile(plan.cacheDir) == "" {
		t.Fatal("the recovery build left no state")
	}
	// The corpus must still answer for the file that arrived during the build.
	if _, err := ensureFolderCorpusFreshWithExecution(ctx, plan, newExecution(&hybridTestScorer{})); err != nil {
		t.Fatalf("second recovery build: %v", err)
	}
}

// firstShardInfo returns the FileInfo of shard 0, which a delta build keeps and
// a full build replaces. It asks the family scan for the shard number rather
// than sorting names, so it does not restate how a shard name is formed.
func firstShardInfo(tb testing.TB, indexDir string) os.FileInfo {
	tb.Helper()
	scan, err := scanFamily(indexDir)
	if err != nil {
		tb.Fatalf("scan family in %s: %v", indexDir, err)
	}
	for _, member := range scan.members {
		if !member.shard || !member.numbered || member.num != 0 {
			continue
		}
		info, statErr := os.Stat(filepath.Join(indexDir, member.name))
		if statErr != nil {
			tb.Fatalf("stat %s: %v", member.name, statErr)
		}
		return info
	}
	tb.Fatalf("no shard 0 in %s", indexDir)
	return nil
}
