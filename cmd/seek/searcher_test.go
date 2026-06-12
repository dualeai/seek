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
