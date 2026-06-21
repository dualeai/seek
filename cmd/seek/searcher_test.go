package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadShards_FailsClosedOnPartialCorruptShard(t *testing.T) {
	requireTools(t)

	indexDir := t.TempDir()
	docs := make(chan fileContent, 1)
	docs <- fileContent{name: "good.go", content: []byte("package good\n// load_shards_marker\n")}
	close(docs)

	indexedAny, err := indexDocuments(context.Background(), indexDir, "test", t.TempDir(), docs, 1)
	if err != nil {
		t.Fatalf("indexDocuments: %v", err)
	}
	if !indexedAny {
		t.Fatal("expected a valid shard")
	}

	corruptPath := filepath.Join(indexDir, "corrupt.zoekt")
	if err := os.WriteFile(corruptPath, []byte("not a zoekt shard"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := executeUnscopedShardSearchForTest(context.Background(), indexDir, "load_shards_marker")
	if err == nil {
		t.Fatal("expected corrupt shard to fail closed")
	}
	if len(results) != 0 {
		t.Fatalf("corrupt shard search should not return partial results: %#v", results)
	}
	if !strings.Contains(err.Error(), "load shard") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteParsedSearchScopedDirs_IgnoresEmptyOptionalDir(t *testing.T) {
	requireTools(t)

	indexDir := t.TempDir()
	docs := make(chan fileContent, 1)
	docs <- fileContent{name: "good.go", content: []byte("package good\n// optional_empty_dir_marker\n")}
	close(docs)

	indexedAny, err := indexDocuments(context.Background(), indexDir, "test", t.TempDir(), docs, 1)
	if err != nil {
		t.Fatalf("indexDocuments: %v", err)
	}
	if !indexedAny {
		t.Fatal("expected a valid shard")
	}
	q, err := parseSearchQuery("optional_empty_dir_marker")
	if err != nil {
		t.Fatal(err)
	}
	results, err := executeParsedSearchScopedDirs(context.Background(), []string{indexDir, t.TempDir()}, q, nil)
	if err != nil {
		t.Fatalf("search with empty optional dir: %v", err)
	}
	if len(results) != 1 || results[0].FileName != "good.go" {
		t.Fatalf("expected result from non-empty shard dir only, got %#v", results)
	}
}
