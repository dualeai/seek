package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

// guardShardReport returns the number of shard files above shardFormatLimit
// and the stored content of each document, by name, across all shards.
func guardShardReport(t *testing.T, dir string) (int, map[string][]string) {
	t.Helper()
	shards, err := filepath.Glob(filepath.Join(dir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	wrapped := 0
	contents := map[string][]string{}
	for _, shard := range shards {
		info, err := os.Stat(shard)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > shardFormatLimit {
			wrapped++
		}
		searcher, err := openShard(shard)
		if err != nil {
			t.Fatalf("open %s: %v", shard, err)
		}
		result, err := searcher.Search(context.Background(), &query.Const{Value: true}, &zoekt.SearchOptions{Whole: true})
		if err != nil {
			searcher.Close()
			t.Fatal(err)
		}
		// Copy the content before Close unmaps the shard.
		for _, file := range result.Files {
			contents[file.FileName] = append(contents[file.FileName], string(file.Content))
		}
		searcher.Close()
	}
	return wrapped, contents
}

// guardDocs returns a document that Zoekt skips at Add (NUL bytes, so it does
// not flush), one that fits, and one that wraps under a 60 MiB limit, in the
// given order of names.
func guardDocs(order ...string) []fileContent {
	documents := map[string]fileContent{
		"bin.dat":   {name: "bin.dat", content: make([]byte, 12<<20)},
		"fits.txt":  {name: "fits.txt", content: randomWordText(3, 12<<20, "guard_fits_needle")},
		"wraps.txt": {name: "wraps.txt", content: randomWordText(4, 20<<20, "guard_wraps_needle")},
	}
	docs := make([]fileContent, len(order))
	for i, name := range order {
		docs[i] = documents[name]
	}
	return docs
}

func expectGuardedShards(t *testing.T, dir string) {
	t.Helper()
	wrapped, contents := guardShardReport(t, dir)
	if wrapped != 0 {
		t.Fatalf("%d shard files above the format limit", wrapped)
	}
	if fits := contents["fits.txt"]; len(fits) != 1 || !strings.Contains(fits[0], "guard_fits_needle") {
		t.Fatalf("fits.txt stored %d times, want once with content", len(fits))
	}
	if wraps := contents["wraps.txt"]; len(wraps) != 1 || !strings.HasPrefix(wraps[0], "NOT-INDEXED: ") {
		t.Fatalf("wraps.txt stored %d times, want once as a name-only marker", len(wraps))
	}
	if bin := contents["bin.dat"]; len(bin) != 1 {
		t.Fatalf("bin.dat stored %d times, want once", len(bin))
	}
}

func TestGuardWrappedShards_SkippedDocumentDoesNotShiftMapping(t *testing.T) {
	requireTools(t)
	if testing.Short() {
		t.Skip("indexes 44 MiB")
	}
	orders := map[string][]string{
		// A skipped document first must not shift the mapping.
		"skipped first": {"bin.dat", "fits.txt", "wraps.txt"},
		// A fitting document after a wrapped one is added again with content.
		"fits after wraps": {"wraps.txt", "fits.txt", "bin.dat"},
	}
	builders := map[string]func(dir string, docs []fileContent) error{
		"delta default shard size": func(dir string, docs []fileContent) error {
			_, err := indexDeltaDocuments(dir, "guard", "", docs, 0, nil)
			return err
		},
		"delta folder shard size": func(dir string, docs []fileContent) error {
			_, err := indexDeltaDocuments(dir, "guard", "", docs, folderDeltaShardMax, nil)
			return err
		},
		"windows with parallelism 3": func(dir string, docs []fileContent) error {
			ch := make(chan fileContent, len(docs))
			for _, doc := range docs {
				ch <- doc
			}
			close(ch)
			_, err := indexDocuments(context.Background(), dir, "guard", "", ch, 3)
			return err
		},
	}
	swapShardFormatLimitForTest(t, 60<<20)
	for name, order := range orders {
		for builderName, build := range builders {
			t.Run(name+"/"+builderName, func(t *testing.T) {
				dir := t.TempDir()
				if err := build(dir, guardDocs(order...)); err != nil {
					t.Fatal(err)
				}
				expectGuardedShards(t, dir)
			})
		}
	}
}

// TestIsolatesShard_MatchesZoektFlush checks isolatesShard against Zoekt
// itself: each document is added to an empty Builder before a small one, and
// Zoekt's shards show whether the document filled a shard alone.
func TestIsolatesShard_MatchesZoektFlush(t *testing.T) {
	requireTools(t)
	const shardMax = 1024
	text := func(size int) []byte { return []byte(strings.Repeat("a", size)) }
	cases := []struct {
		name string
		doc  index.Document
	}{
		{"name and content at ShardMax", index.Document{Name: "a", Content: text(shardMax - 1)}},
		{"name takes the size past ShardMax", index.Document{Name: "a", Content: text(shardMax)}},
		{"large text", index.Document{Name: "large.txt", Content: text(4 * shardMax)}},
		{"large NUL content", index.Document{Name: "large.bin", Content: append([]byte{0}, text(4*shardMax)...)}},
		{"large content with a skip reason", index.Document{Name: "skip.txt", Content: text(4 * shardMax), SkipReason: index.SkipReasonTooLarge}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := indexBuildOptions(t.TempDir(), 1)
			opts.ShardMax = shardMax
			opts.RepositoryDescription = zoekt.Repository{Name: "isolate"}
			builder, err := index.NewBuilder(opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, doc := range []index.Document{tc.doc, {Name: "next.txt", Content: []byte("next document\n")}} {
				if err := builder.Add(doc); err != nil {
					t.Fatal(err)
				}
			}
			if err := builder.Finish(); err != nil {
				t.Fatal(err)
			}
			shards := opts.FindAllShards()
			searcher, err := openShard(shards[0])
			if err != nil {
				t.Fatal(err)
			}
			result, err := searcher.Search(context.Background(), &query.Const{Value: true}, &zoekt.SearchOptions{})
			searcher.Close()
			if err != nil {
				t.Fatal(err)
			}
			alone := len(result.Files) == 1 && result.Files[0].FileName == tc.doc.Name
			if got := isolatesShard(tc.doc, opts); got != alone {
				t.Fatalf("isolatesShard=%v, but Zoekt put the document alone in shard 0: %v", got, alone)
			}
		})
	}
}
