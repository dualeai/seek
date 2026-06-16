package main

import (
	"fmt"
	"hash/crc32"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// planSynthCorpus wraps the recurring "os.Lstat + planFolderCorpus +
// fresh cache/index dirs" sequence used by every E2E folder test.
// Returns a plan with empty caches so each test exercises a cold path.
//
// Internal invariants check: production planFolderCorpus regression
// (e.g. root mismatch, cacheDir == indexDir collision) would otherwise
// be invisible to callers that treat the plan as an opaque token.
func planSynthCorpus(t *testing.T, root string) corpusPlan {
	t.Helper()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatalf("planSynthCorpus lstat: %v", err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("planSynthCorpus planFolderCorpus: %v", err)
	}
	plan.cacheDir = t.TempDir()
	plan.indexDir = t.TempDir()
	// Invariants: catch a regression where planFolderCorpus loses
	// the root, collides cache and index dirs, or returns blanks.
	if plan.root == "" {
		t.Fatal("planSynthCorpus: production returned empty plan.root")
	}
	if plan.cacheDir == "" || plan.indexDir == "" {
		t.Fatal("planSynthCorpus: empty cacheDir or indexDir after t.TempDir overrides")
	}
	if plan.cacheDir == plan.indexDir {
		t.Fatalf("planSynthCorpus: cacheDir collides with indexDir: %q", plan.cacheDir)
	}
	return plan
}

// writeRandomFolder synthesises a temp directory containing files whose
// cumulative Lstat sizes equal `total`. Each file holds `fileSize`
// bytes (the last file is truncated to make the sum exact).
//
// Mode:
//   - testing.Short() — uses os.Truncate to create SPARSE files. The
//     readSemaphore Acquire path keys off Lstat.Size, so sparse files
//     drive the deadlock just as well as real bytes, but cost ~zero
//     disk space and CPU. Use this for CI.
//   - long mode — fills with deterministic pseudo-random bytes seeded
//     from t.Name() so reproductions are stable.
//
// Caller owns the returned path via t.TempDir(); no cleanup required.
func writeRandomFolder(t *testing.T, total, fileSize int64) string {
	t.Helper()
	if total <= 0 {
		t.Fatalf("writeRandomFolder: non-positive total=%d", total)
	}
	if fileSize <= 0 {
		t.Fatalf("writeRandomFolder: non-positive fileSize=%d", fileSize)
	}

	root := t.TempDir()
	seed := uint64(crc32.ChecksumIEEE([]byte(t.Name())))
	if seed == 0 {
		seed = 1
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))

	var written int64
	for i := 0; written < total; i++ {
		sz := fileSize
		if remaining := total - written; remaining < sz {
			sz = remaining
		}
		p := filepath.Join(root, fmt.Sprintf("f%05d.bin", i))
		if testing.Short() {
			writeSparseFile(t, p, sz)
		} else {
			buf := make([]byte, sz)
			for j := range buf {
				buf[j] = byte(rng.Uint32())
			}
			if err := os.WriteFile(p, buf, 0o644); err != nil {
				t.Fatalf("writeRandomFolder: %v", err)
			}
		}
		written += sz
	}
	return root
}

// writeSparseFile creates path with size `size` bytes using os.Truncate.
// The result reports `size` from Lstat but consumes essentially zero
// disk space. Used by writeRandomFolder under -short and by property
// tests that explore size boundaries without paying real I/O.
func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("writeSparseFile create %q: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("writeSparseFile close %q: %v", path, err)
	}
	if size == 0 {
		return
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("writeSparseFile truncate %q to %d: %v", path, size, err)
	}
}
