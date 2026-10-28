package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sourcegraph/zoekt/query"
)

// TestFolderDrift_NextBuildIndexesTheDrift adds a file while a folder build
// runs. The build succeeds with the state of its walk. The file is a change
// for the next build, which is a delta on the published family. A change
// during a build is not an error.
//
// The builds are text builds. The test holds a read lock, so the build waits
// to publish after its walk and its reads. The test adds the file during that
// wait.
func TestFolderDrift_NextBuildIndexesTheDrift(t *testing.T) {
	requireTools(t)

	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// DRIFT_STATE_MARKER\n")
	plan := planFolderTestCorpus(t, folder)
	ctx := context.Background()

	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("first folder build: %v", err)
	}
	published := readStateFile(plan.cacheDir)
	if published == "" {
		t.Fatal("the first build wrote no state")
	}
	if !folderFamilyUsable(plan.indexDir) {
		t.Fatal("the first build left no usable family")
	}

	// second.go makes the next build run, and late.go arrives while that
	// build waits to publish.
	writeFileAt(t, folder, "second.go", "package sample\n// forces a rebuild\n")
	release := holdReadLock(t, plan.cacheDir)
	_, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitReaders, filepath.Join(plan.cacheDir, lockFile))
	acted := onLockWait(t, waiting, func() error {
		return os.WriteFile(filepath.Join(folder, "late.go"), []byte("package sample\n// arrived during the build\n"), 0o644)
	}, release)

	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("the drifted build failed: %v", err)
	}
	lockWaitActed(t, acted)
	if got := readStateFile(plan.cacheDir); got == "" || got == published {
		t.Fatalf("the drifted build published no new state: %q", got)
	}

	// The next build indexes the late file as a delta. A delta seeds the
	// prior shards with hard links, so the base shard stays the same file. A
	// full rebuild writes a new one.
	baseBefore := firstShardInfo(t, plan.indexDir)
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("next build: %v", err)
	}
	baseAfter := firstShardInfo(t, plan.indexDir)
	if !os.SameFile(baseBefore, baseAfter) {
		t.Fatal("the next build rewrote the base shard, so it was not a delta")
	}
	if readStateFile(plan.cacheDir) == "" {
		t.Fatal("the next build left no state")
	}
	// The corpus answers for the file that arrived during the build. The
	// search reads the node under its read lock (searchPlannedCorpusParsed).
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("warm build: %v", err)
	}
	files, err := searchPlannedCorpusParsed(ctx, plan, nil, &query.Substring{Pattern: "arrived during the build"}, defaultSearchConfig())
	if err != nil || len(files) != 1 || files[0].FileName != "late.go" {
		t.Fatalf("files=%v err=%v, want late.go indexed by the next build", files, err)
	}
}

// firstShardInfo returns the FileInfo of shard 0, which a delta build keeps and
// a full build replaces. It asks the family scan for the shard number rather
// than sorting names, so it does not restate the rule of shard names.
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
