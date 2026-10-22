package main

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// planSynthCorpus plans root as a folder node with planFolderCorpus, checks
// the plan, and then gives it new empty cache and index directories, so each
// test starts with no index.
//
// The checks find a folder-plan regression (an empty root, an empty cache or
// index directory, or one directory for both) that a caller that uses the plan
// as an opaque value would not see.
func planSynthCorpus(t *testing.T, root string) node {
	t.Helper()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatalf("planSynthCorpus lstat: %v", err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatalf("planSynthCorpus planFolderCorpus: %v", err)
	}
	// Check the production plan before the t.TempDir directories below
	// replace its cache and index directories.
	if plan.root == "" {
		t.Fatal("planSynthCorpus: production returned empty plan.root")
	}
	if plan.cacheDir == "" || plan.indexDir == "" {
		t.Fatal("planSynthCorpus: production returned empty cacheDir or indexDir")
	}
	if plan.cacheDir == plan.indexDir {
		t.Fatalf("planSynthCorpus: cacheDir collides with indexDir: %q", plan.cacheDir)
	}
	plan.cacheDir = t.TempDir()
	plan.indexDir = t.TempDir()
	return plan
}

// synthFolderWords is the vocabulary of writeRandomFolder in long mode.
var synthFolderWords = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
}

// writeRandomFolder makes a temporary directory of files whose Lstat sizes
// add up to total. Each file holds fileSize bytes, except the last, which can
// hold less.
//
// Mode:
//   - testing.Short(): writeSparseFile, a text prefix and then a sparse rest.
//     The readSemaphore weight comes from the file size, so sparse files put
//     the same pressure on the read budget as real bytes, at almost no disk
//     space and CPU.
//   - long mode, which CI runs: words from synthFolderWords, picked by a
//     generator seeded from t.Name(), so each run writes the same bytes.
//     Callers check read weight, window rotation, and cancellation. None of
//     them searches the generated words; the beacon tests search a separate
//     file. The small vocabulary has few distinct trigrams, so Zoekt indexes
//     the full content at a low cost; random printable ASCII makes the callers
//     about 8 times slower under -race.
//
// The path comes from t.TempDir(), so the testing package removes it.
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
			// Words and spaces have no NUL byte, so the content is indexable
			// text, not a binary file.
			buf := make([]byte, 0, sz+16)
			for word := 0; int64(len(buf)) < sz; word++ {
				buf = append(buf, synthFolderWords[rng.IntN(len(synthFolderWords))]...)
				if word%10 == 9 {
					buf = append(buf, '\n')
				} else {
					buf = append(buf, ' ')
				}
			}
			if err := os.WriteFile(p, buf[:sz], 0o644); err != nil {
				t.Fatalf("writeRandomFolder: %v", err)
			}
		}
		written += sz
	}
	return root
}

// writeSparseFile creates path with size bytes. It writes a text prefix past
// the readers' binary probe, so readers read the file fully, and uses
// os.Truncate for the rest, which uses almost no disk space. Tests that need
// large files with little I/O use it: writeRandomFolder under -short, and the
// property, pressure and soak tests.
func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	prefix := bytes.Repeat([]byte("x"), int(min(size, 2*binaryProbeBytes)))
	if err := os.WriteFile(path, prefix, 0o644); err != nil {
		t.Fatalf("writeSparseFile create %q: %v", path, err)
	}
	if size == 0 {
		return
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("writeSparseFile truncate %q to %d: %v", path, size, err)
	}
}
