package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureFolderCorpusFresh_MultiShardEarlyBeaconSurvives checks a build of
// several shards whose content is larger than the read budget. The first
// marker sorts into the first shard, and the last marker sorts into the final
// shard. Both must remain searchable.
func TestEnsureFolderCorpusFresh_MultiShardEarlyBeaconSurvives(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t)()

	const earlyBeacon = "EARLY_BEACON_A_F00DCAFE"
	const lateBeacon = "LATE_BEACON_Z_BAADBEEF"

	root := t.TempDir()
	// Early beacon: prefix 'a_' so it sorts first.
	if err := os.WriteFile(filepath.Join(root, "a_early.txt"), []byte(earlyBeacon+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The middle payload is 12 MiB in 256 KiB files. That is three times the
	// test budget and more than the 10 MiB shardMax, so the build makes two or
	// more shards.
	const fileSize int64 = 256 * 1024
	const totalPayload int64 = 12 * 1024 * 1024
	written := int64(0)
	for i := 0; written < totalPayload; i++ {
		name := filepath.Join(root, fmt.Sprintf("m_payload_%05d.txt", i))
		payload := make([]byte, fileSize)
		for j := range payload {
			payload[j] = byte('a' + i%26)
		}
		if err := os.WriteFile(name, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		written += fileSize
	}
	// Late beacon: prefix 'z_' so it sorts last.
	if err := os.WriteFile(filepath.Join(root, "z_late.txt"), []byte(lateBeacon+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planSynthCorpus(t, root)
	ctx := t.Context()

	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("ensureFolderCorpusFresh: %v", err)
	}
	// The fixture must make two or more shards. With one shard, the test
	// proves nothing about the shards that follow the first.
	if n := repositoryShardCount(plan.indexDir, folderRepoName(plan)); n < 2 {
		t.Fatalf("expected >= 2 shards, got %d", n)
	}

	// Both beacons must be found. The early beacon proves that the later shards
	// left the first shard searchable, and the late beacon proves that the
	// final shard flushed.
	for _, b := range []struct{ name, marker string }{{"early", earlyBeacon}, {"late", lateBeacon}} {
		results, err := searchPlannedCorpusForTest(ctx, plan, b.marker)
		if err != nil {
			t.Fatalf("search %s beacon: %v", b.name, err)
		}
		if len(results) == 0 {
			t.Fatalf("%s beacon not found; cross-window shard chain is corrupt", b.name)
		}
	}
}
