package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEnsureFolderCorpusFresh_MultiWindowEarlyBeaconSurvives — the
// windowed indexer chains IsDelta=false (window 0) + IsDelta=true
// (windows 1..N). Each delta-window's Finish rewrites the .meta of
// every prior shard (zoekt/index/builder.go:684-742). A regression
// in the chain that corrupted earlier shards' content would silently
// pass every existing beacon test, because every other beacon is
// sorted LAST and lands in the final window.
//
// This test writes an EARLY beacon (file name sorting first) so it
// lands in window 0, then writes enough payload to force ≥3 windows,
// then writes a LATE beacon last. Searching both proves the entire
// shard chain remains queryable post-rotation.
func TestEnsureFolderCorpusFresh_MultiWindowEarlyBeaconSurvives(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 4 * 1024 * 1024 // 4 MiB
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 60*time.Second)()

	const earlyBeacon = "EARLY_BEACON_A_F00DCAFE"
	const lateBeacon = "LATE_BEACON_Z_BAADBEEF"

	root := t.TempDir()
	// Early beacon: prefix 'a_' so it sorts first.
	if err := os.WriteFile(filepath.Join(root, "a_early.txt"), []byte(earlyBeacon+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Middle payload: force >= 3 windows. With testBudget=4 MiB →
	// indexWindowBytes=2 MiB. ~12 MiB of payload across many files.
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
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatalf("ensureFolderCorpusFresh: %v", err)
	}
	// Sanity: fixture must produce multiple shards (windows). Single
	// shard would mean the rotation path didn't fire and the test
	// asserts nothing about cross-window survival.
	if n := repositoryShardCount(plan.indexDir, folderRepoName(plan)); n < 2 {
		t.Fatalf("expected >= 2 shards (rotation didn't fire), got %d", n)
	}

	// Both beacons MUST be findable. The early beacon proves window 0's
	// content was not corrupted by subsequent IsDelta=true Finish .meta
	// rewrites; the late beacon proves the final window flushed.
	for _, b := range []struct{ name, marker string }{{"early", earlyBeacon}, {"late", lateBeacon}} {
		results, err := searchPlannedCorpusForTest(ctx, plan, b.marker)
		if err != nil {
			t.Fatalf("search %s beacon: %v", b.name, err)
		}
		if len(results) == 0 {
			t.Fatalf("%s beacon NOT found — cross-window shard chain corrupted", b.name)
		}
	}
}
