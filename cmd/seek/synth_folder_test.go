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

// planSynthCorpus plans root as a folder node with planFolderCorpus and checks
// the plan. Then it gives the plan new empty cache and index directories, so
// each test starts with no index.
//
// The checks find a defect of the folder plan that a caller would not see.
// The defects are an empty root, an empty cache or index directory, and one
// directory for both.
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

// synthFolderWords is the vocabulary of writeRandomFolder.
var synthFolderWords = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
}

// writeRandomFolder makes a temporary directory of files whose Lstat sizes
// add up to total. Each file holds fileSize bytes, except the last, which can
// hold less.
//
// The files hold words from synthFolderWords, picked by a generator seeded
// from t.Name(), so each run writes the same bytes. Callers check read weight,
// window rotation, and cancellation. None of them searches the generated
// words; the beacon tests search a separate file. The small vocabulary has few
// distinct trigrams, so Zoekt indexes the full content at a low cost. Random
// printable ASCII makes the callers about 8 times slower under -race.
//
// The path comes from t.TempDir(), so the testing package removes it.
func writeRandomFolder(t *testing.T, total, fileSize int64) string {
	t.Helper()
	root := t.TempDir()
	seed := uint64(crc32.ChecksumIEEE([]byte(t.Name())))
	rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))

	var written int64
	for i := 0; written < total; i++ {
		sz := min(fileSize, total-written)
		p := filepath.Join(root, fmt.Sprintf("f%05d.bin", i))
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
		written += sz
	}
	return root
}

// writeSparseFile creates path with size bytes. It writes text in the first
// 2*binaryProbeBytes bytes, past the binary probe of the readers, so the
// readers read the whole file. os.Truncate makes the rest, which uses almost no
// disk space. The property, pressure and soak tests use it for large files with
// little I/O.
func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	prefix := bytes.Repeat([]byte("x"), int(min(size, 2*binaryProbeBytes)))
	if err := os.WriteFile(path, prefix, 0o644); err != nil {
		t.Fatalf("writeSparseFile create %q: %v", path, err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("writeSparseFile truncate %q to %d: %v", path, size, err)
	}
}
