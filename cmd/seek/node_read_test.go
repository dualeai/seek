package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// TestNodeReadSharesOneLoad gives a read one load of two shards. Two searches
// use the same searchers and close none. Two calls of close close each shard
// once. A search after close is a defect, not index damage.
func TestNodeReadSharesOneLoad(t *testing.T) {
	cacheDir := t.TempDir()
	plan := node{corpusStore: corpusStore{cacheDir: cacheDir, indexDir: filepath.Join(cacheDir, "index")}}
	read, readCtx, err := openNodeRead(t.Context(), plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	shard := func(name string) *cancellationTestShard {
		return &cancellationTestShard{search: func(context.Context) (*zoekt.SearchResult, error) {
			return &zoekt.SearchResult{Files: []zoekt.FileMatch{{FileName: name}}}, nil
		}}
	}
	first, second := shard("a.go"), shard("b.go")
	read.load.Do(func() {
		read.loaded = nodeShards{searchers: []zoekt.Searcher{first, second}, paths: []string{"a.zoekt", "b.zoekt"}}
	})
	for range 2 {
		files, err := read.search(readCtx, &query.Const{Value: true}, defaultSearchConfig())
		if err != nil || len(files) != 2 {
			t.Fatalf("files=%v err=%v, want both shards' files", files, err)
		}
	}
	if first.closeCalls.Load() != 0 || second.closeCalls.Load() != 0 {
		t.Fatal("a search closed a shard of the read")
	}
	read.close()
	read.close()
	if first.closeCalls.Load() != 1 || second.closeCalls.Load() != 1 {
		t.Fatalf("close calls=%d/%d, want 1/1", first.closeCalls.Load(), second.closeCalls.Load())
	}
	if _, err := read.search(readCtx, &query.Const{Value: true}, defaultSearchConfig()); !errors.Is(err, errInvariant) || errors.Is(err, errShardUnloadable) {
		t.Fatalf("search after close: err=%v, want errInvariant", err)
	}
}

// TestNodeReadLoadsShardsOnce counts the shard loads of one warm search of a
// dirty Git node, by the debug line of each load. The strict route (no model)
// and the model route each load the node's shards once for all the queries of
// the read. A read with no search loads none. The test model accepts every
// row. So c.go, which holds no query word, is a vector hit with no strict
// match. The model route runs the relaxed query and scores c.go.
func TestNodeReadLoadsShardsOnce(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.go", "package main\n// zqonce alpha committed\n")
	writeTrackedFile(t, repo, "c.go", "package main\n// no word of the query\n")
	// b.go is dirty: the overlay.
	writeFixtureFile(t, filepath.Join(repo, "b.go"), "package main\n// zqonce alpha dirty\n")
	setTestUserCache(t)
	if _, _, err := stubSearch(t, payloadAccept, "zqonce alpha file:go", repo); err != nil {
		t.Fatal(err)
	}
	// shardLoads counts the loads that the logs saw: nodeRead.shards writes
	// one debug line for each load.
	shardLoads := func(logs *testLogRecorder) int {
		loads := 0
		for _, record := range logs.Records() {
			if record.Message == "Loaded the node's shards" {
				loads++
			}
		}
		return loads
	}
	t.Run("strict", func(t *testing.T) {
		// The file: atom makes the strict route also run its file listing
		// (nodeHeaderQuery).
		logs := captureTestLogs(t, slog.LevelDebug)
		out, err := seek(t, "--lexical-only", "zqonce file:go", repo)
		if err != nil || !strings.Contains(out, "b.go") {
			t.Fatalf("err=%v output:\n%s", err, out)
		}
		if got := shardLoads(logs); got != 1 {
			t.Fatalf("shard loads=%d, want 1", got)
		}
	})
	t.Run("description", func(t *testing.T) {
		// The strict query, the name-only count, both scans with the file
		// selection, and the relaxed query.
		logs := captureTestLogs(t, slog.LevelDebug)
		out, scorer, err := stubSearch(t, payloadAccept, "zqonce alpha file:go", repo)
		if err != nil || !strings.Contains(out, "b.go") {
			t.Fatalf("err=%v output:\n%s", err, out)
		}
		if got := shardLoads(logs); got != 1 {
			t.Fatalf("shard loads=%d, want 1", got)
		}
		scorer.mu.Lock()
		scored := slices.ContainsFunc(scorer.documents, func(document rerankDocument) bool { return document.Path == "c.go" })
		scorer.mu.Unlock()
		if !scored {
			t.Fatalf("the model did not score c.go, so the relaxed query did not run:\n%s", out)
		}
	})
	plans, err := planCorpora(t.Context(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	logs := captureTestLogs(t, slog.LevelDebug)
	read, _, err := openNodeRead(t.Context(), plans[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	read.close()
	if got := shardLoads(logs); got != 0 {
		t.Fatalf("a read with no search loaded the shards %d times", got)
	}
}

// TestRemoveDamagedFamilyManifestKeepsANewPublish checks that the repair
// removes the family manifest that a damaged read saw. It keeps the manifest
// in each other case: a peer published it after the read, the read saw none,
// or the node was evicted.
func TestRemoveDamagedFamilyManifestKeepsANewPublish(t *testing.T) {
	newNode := func(t *testing.T) node {
		t.Helper()
		cacheDir := t.TempDir()
		plan := node{corpusStore: corpusStore{cacheDir: cacheDir, indexDir: filepath.Join(cacheDir, "index")}}
		writeFixtureFile(t, filepath.Join(plan.indexDir, "a.zoekt"), "shard")
		writeFixtureFile(t, filepath.Join(cacheDir, lockFile), "")
		if err := writeFamilyManifest(plan.indexDir, []string{"a.zoekt"}); err != nil {
			t.Fatal(err)
		}
		return plan
	}
	// seen is the damage of one read of plan.
	seen := func(t *testing.T, plan node) error {
		t.Helper()
		read, _, err := openNodeRead(t.Context(), plan, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer read.close()
		damage := read.damage(fmt.Errorf("load index: %w", errShardUnloadable))
		if !errors.Is(damage, errShardUnloadable) {
			t.Fatalf("damage=%v, want errShardUnloadable", damage)
		}
		return damage
	}
	manifestExists := func(plan node) bool {
		_, err := os.Stat(filepath.Join(plan.indexDir, familyManifestFile))
		return err == nil
	}
	t.Run("the manifest that the read saw", func(t *testing.T) {
		plan := newNode(t)
		removeDamagedFamilyManifest(t.Context(), plan, seen(t, plan))
		if manifestExists(plan) {
			t.Fatal("the manifest stays")
		}
	})
	t.Run("a manifest published after the read", func(t *testing.T) {
		plan := newNode(t)
		damage := seen(t, plan)
		if err := writeFamilyManifest(plan.indexDir, []string{"a.zoekt"}); err != nil {
			t.Fatal(err)
		}
		removeDamagedFamilyManifest(t.Context(), plan, damage)
		if !manifestExists(plan) {
			t.Fatal("the repair removed the peer's new manifest")
		}
	})
	t.Run("no manifest at the read", func(t *testing.T) {
		plan := newNode(t)
		removeFamilyManifest(plan.indexDir)
		damage := seen(t, plan)
		if err := writeFamilyManifest(plan.indexDir, []string{"a.zoekt"}); err != nil {
			t.Fatal(err)
		}
		removeDamagedFamilyManifest(t.Context(), plan, damage)
		if !manifestExists(plan) {
			t.Fatal("the repair removed a manifest that the read did not see")
		}
	})
	t.Run("an evicted node", func(t *testing.T) {
		plan := newNode(t)
		damage := seen(t, plan)
		if err := os.Remove(filepath.Join(plan.cacheDir, lockFile)); err != nil {
			t.Fatal(err)
		}
		removeDamagedFamilyManifest(t.Context(), plan, damage)
		if !manifestExists(plan) {
			t.Fatal("the repair of an evicted node removed the manifest")
		}
	})
}
