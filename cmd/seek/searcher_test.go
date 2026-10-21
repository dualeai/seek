package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
)

func TestExplicitSearchConfigCopiesDefaults(t *testing.T) {
	config := explicitSearchConfig(360, true)

	if config.opts.NumContextLines != 360 || !config.afterOnly {
		t.Fatalf("unexpected context config: %+v", config)
	}
	if searchOpts.NumContextLines != searchContextLines {
		t.Fatalf("explicitSearchConfig changed the package defaults: %+v", searchOpts)
	}
}

func TestCloneFileMatchesCopiesContextOnlyForDisplayedLines(t *testing.T) {
	context := []byte("context\n")
	input := []zoekt.FileMatch{{
		FileName: "many.go",
		LineMatches: []zoekt.LineMatch{
			{LineNumber: 30, Line: []byte("third\n"), Before: context, After: context, Score: 3},
			{LineNumber: 10, Line: []byte("first\n"), Before: context, After: context, Score: 2},
			{LineNumber: 20, Line: []byte("second\n"), Before: context, After: context, Score: 1},
		},
	}}

	config := defaultSearchConfig()
	config.contextMatchLimit = 2
	cloned := cloneFileMatches(input, config)
	if got := len(cloned[0].LineMatches); got != 3 {
		t.Fatalf("line matches=%d, want 3", got)
	}
	for i, match := range cloned[0].LineMatches {
		wantContext := i < 2
		if got := len(match.Before) > 0 && len(match.After) > 0; got != wantContext {
			t.Fatalf("match %d context=%t, want %t", i, got, wantContext)
		}
	}

	displayed, hidden := normalizeLineMatches(cloned[0].LineMatches, 2)
	if hidden != 1 || len(displayed) != 2 {
		t.Fatalf("displayed=%d hidden=%d, want 2 and 1", len(displayed), hidden)
	}
	for _, match := range displayed {
		if len(match.Before) == 0 || len(match.After) == 0 {
			t.Fatalf("displayed line %d lost context", match.LineNumber)
		}
	}
	config.afterOnly = true
	afterOnly := cloneFileMatches(input, config)
	for i, match := range afterOnly[0].LineMatches {
		if len(match.Before) != 0 {
			t.Fatalf("after-only match %d copied before context", i)
		}
		if got, want := len(match.After) > 0, i < 2; got != want {
			t.Fatalf("after-only match %d after context=%t, want %t", i, got, want)
		}
	}

	input[0].LineMatches[0].Before[0] = 'X'
	if cloned[0].LineMatches[0].Before[0] == 'X' {
		t.Fatal("displayed context aliases shard memory")
	}
	if afterOnly[0].LineMatches[0].After[0] == 'X' {
		t.Fatal("after-only context aliases shard memory")
	}
}

func TestCloneFileMatchesCopiesContextOnlyForPossibleDisplayedFiles(t *testing.T) {
	newFile := func(name, repository string, score float64) zoekt.FileMatch {
		return zoekt.FileMatch{
			FileName:   name,
			Repository: repository,
			Language:   "Go",
			Score:      score,
			LineMatches: []zoekt.LineMatch{{
				LineNumber: 1,
				Line:       []byte(name + "\n"),
				Before:     []byte("before " + name + "\n"),
				After:      []byte("after " + name + "\n"),
			}},
		}
	}

	// A node read gives one current copy per file.
	input := []zoekt.FileMatch{
		newFile("low.go", "repo", 1),
		newFile("dirty.go", repoUncommitted, 90),
		newFile("b.go", "repo", 50),
		newFile("c.go", "repo", 40),
		newFile("top.go", "repo", 100),
	}
	full := cloneFileMatches(input, defaultSearchConfig())
	config := defaultSearchConfig()
	config.contextFileLimit = 2
	selected := cloneFileMatches(input, config)

	for i, file := range selected {
		gotContext := len(file.LineMatches[0].Before) > 0 && len(file.LineMatches[0].After) > 0
		wantContext := i == 1 || i == 4
		if gotContext != wantContext {
			t.Fatalf("file %d %s context=%t, want %t", i, file.FileName, gotContext, wantContext)
		}
		if len(file.LineMatches[0].Line) == 0 {
			t.Fatalf("file %d %s lost match metadata", i, file.FileName)
		}
	}

	plan := node{id: corpusID("repo"), kind: corpusKindGit}
	want := formatCorpusResultsWithContext(wrapCorpusResults(plan, full), 2, 0, hideCorpusContext, plainPalette)
	got := formatCorpusResultsWithContext(wrapCorpusResults(plan, selected), 2, 0, hideCorpusContext, plainPalette)
	if got != want {
		t.Fatalf("selective context changed output:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	input[4].LineMatches[0].Before[0] = 'X'
	if selected[4].LineMatches[0].Before[0] == 'X' {
		t.Fatal("selected file context aliases shard memory")
	}
}

func TestDisplayedContextFilesUsesFormatterTieOrder(t *testing.T) {
	files := []zoekt.FileMatch{
		{FileName: "z.go", Score: 10},
		{FileName: "a.go", Score: 10},
	}
	config := defaultSearchConfig()
	config.contextFileLimit = 1

	selected := displayedContextFiles(files, config)
	if _, ok := selected[1]; !ok {
		t.Fatalf("selected indexes=%v, want a.go", selected)
	}
	if _, ok := selected[0]; ok {
		t.Fatalf("selected indexes=%v, did not want z.go", selected)
	}
}

func TestPerCorpusContextLimitPreservesGlobalTopFiles(t *testing.T) {
	file := func(name string, score float64) zoekt.FileMatch {
		return zoekt.FileMatch{
			FileName: name,
			Score:    score,
			LineMatches: []zoekt.LineMatch{{
				LineNumber: 1,
				Line:       []byte(name + "\n"),
				Before:     []byte("before\n"),
				After:      []byte("after\n"),
			}},
		}
	}
	planA := node{id: corpusID("a"), kind: corpusKindFolder, displayRoot: "/a"}
	planB := node{id: corpusID("b"), kind: corpusKindFolder, displayRoot: "/b"}
	filesA := []zoekt.FileMatch{file("first.go", 100), file("second.go", 99), file("third.go", 98)}
	filesB := []zoekt.FileMatch{file("other.go", 1)}

	config := defaultSearchConfig()
	config.contextFileLimit = 2
	selective := append(
		wrapCorpusResults(planA, cloneFileMatches(filesA, config)),
		wrapCorpusResults(planB, cloneFileMatches(filesB, config))...,
	)
	full := append(
		wrapCorpusResults(planA, cloneFileMatches(filesA, defaultSearchConfig())),
		wrapCorpusResults(planB, cloneFileMatches(filesB, defaultSearchConfig()))...,
	)
	want := formatCorpusResultsWithContext(full, 2, 0, showCorpusContext, plainPalette)
	got := formatCorpusResultsWithContext(selective, 2, 0, showCorpusContext, plainPalette)
	if got != want {
		t.Fatalf("per-corpus context selection changed global output:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestSearchPlannedCorpusParsed_ContextModes reads one node through the
// production node read and checks that each search config gives its context
// lines.
func TestSearchPlannedCorpusParsed_ContextModes(t *testing.T) {
	requireTools(t)

	var content strings.Builder
	content.WriteString("package sample\n")
	for i := range 20 {
		fmt.Fprintf(&content, "// before %03d\n", i)
	}
	content.WriteString("// CONTEXT_MODE_MARKER\n")
	for i := range 400 {
		fmt.Fprintf(&content, "// after %03d\n", i)
	}

	indexDir := t.TempDir()
	docs := make(chan fileContent, 1)
	docs <- fileContent{name: "context.go", content: []byte(content.String())}
	close(docs)
	indexedAny, err := indexDocuments(t.Context(), indexDir, "test", t.TempDir(), docs, 1)
	if err != nil {
		t.Fatalf("indexDocuments: %v", err)
	}
	if !indexedAny {
		t.Fatal("expected a valid shard")
	}
	q, err := parseSearchQuery("CONTEXT_MODE_MARKER")
	if err != nil {
		t.Fatal(err)
	}
	plan := node{cacheDir: t.TempDir(), indexDir: indexDir}

	cases := []struct {
		name       string
		config     searchConfig
		wantBefore int
		wantAfter  int
	}{
		{name: "default", config: defaultSearchConfig(), wantBefore: 3, wantAfter: 3},
		{name: "symmetric 12", config: explicitSearchConfig(12, false), wantBefore: 12, wantAfter: 12},
		{name: "after only 360", config: explicitSearchConfig(360, true), wantAfter: 360},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := searchPlannedCorpusParsed(t.Context(), plan, nil, q, tc.config)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(files) != 1 || len(files[0].LineMatches) != 1 {
				t.Fatalf("expected one line match, got %#v", files)
			}
			match := files[0].LineMatches[0]
			if got := countContextLines(match.Before); got != tc.wantBefore {
				t.Fatalf("before lines=%d, want %d", got, tc.wantBefore)
			}
			if got := countContextLines(match.After); got != tc.wantAfter {
				t.Fatalf("after lines=%d, want %d", got, tc.wantAfter)
			}
		})
	}
}

// TestSearchPlannedCorpusParsed_ContextFileSelectionAfterShardMerge puts a
// committed and an uncommitted shard in one index directory, as a Git node
// does, and reads the node. With a context file limit of 1, only the best
// file after the merge of the two shards keeps its context lines, in each
// shard order.
func TestSearchPlannedCorpusParsed_ContextFileSelectionAfterShardMerge(t *testing.T) {
	requireTools(t)

	const content = "package p\n// before\n// TWO_SHARD_CONTEXT_MARKER\n// after\n"
	// indexShard writes the one shard of repository into indexDir.
	indexShard := func(indexDir, repository string, names ...string) {
		t.Helper()
		docs := make(chan fileContent, len(names))
		for _, name := range names {
			docs <- fileContent{name: name, content: []byte(content)}
		}
		close(docs)
		indexedAny, err := indexDocuments(t.Context(), indexDir, repository, t.TempDir(), docs, 1)
		if err != nil {
			t.Fatalf("index %s shard: %v", repository, err)
		}
		if !indexedAny {
			t.Fatalf("expected %s shard", repository)
		}
	}

	q, err := parseSearchQuery("TWO_SHARD_CONTEXT_MARKER")
	if err != nil {
		t.Fatal(err)
	}

	// The node lists its shards by file name, and a shard name starts with
	// its repository name: "repo" sorts before "uncommitted", and "zrepo"
	// after it.
	orders := []struct {
		name      string
		committed string
	}{
		{name: "committed first", committed: "repo"},
		{name: "uncommitted first", committed: "zrepo"},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			plan := node{id: corpusID("repo"), kind: corpusKindGit, cacheDir: t.TempDir(), indexDir: t.TempDir()}
			indexShard(plan.indexDir, order.committed, "a.go", "z.go")
			indexShard(plan.indexDir, repoUncommitted, "a.go")
			shards, err := familyShardNames(plan.indexDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(shards) != 2 {
				t.Fatalf("shards=%v, want one committed and one uncommitted shard", shards)
			}
			if first := filepath.Base(shards[0]); strings.HasPrefix(first, repoUncommitted) != (order.committed == "zrepo") {
				t.Fatalf("first shard %s, want the %s", first, order.name)
			}

			config := explicitSearchConfig(1, false)
			config.contextFileLimit = 1
			selected, err := searchPlannedCorpusParsed(t.Context(), plan, nil, q, config)
			if err != nil {
				t.Fatalf("selective search: %v", err)
			}

			fullConfig := config
			fullConfig.contextFileLimit = 0
			full, err := searchPlannedCorpusParsed(t.Context(), plan, nil, q, fullConfig)
			if err != nil {
				t.Fatalf("full search: %v", err)
			}
			if len(selected) != 3 || len(full) != 3 {
				t.Fatalf("result counts selective=%d full=%d, want 3 each", len(selected), len(full))
			}

			type resultKey struct {
				name       string
				repository string
			}
			withContext := make(map[resultKey]bool, len(selected))
			for _, file := range selected {
				if len(file.LineMatches) != 1 {
					t.Fatalf("%s in %s line matches=%d, want 1", file.FileName, file.Repository, len(file.LineMatches))
				}
				match := file.LineMatches[0]
				if len(match.Line) == 0 || match.LineNumber == 0 || len(match.LineFragments) == 0 {
					t.Fatalf("%s in %s lost match metadata", file.FileName, file.Repository)
				}
				withContext[resultKey{name: file.FileName, repository: file.Repository}] =
					len(match.Before) > 0 && len(match.After) > 0
			}

			wantContext := map[resultKey]bool{
				{name: "a.go", repository: order.committed}: false,
				{name: "a.go", repository: repoUncommitted}: true,
				{name: "z.go", repository: order.committed}: false,
			}
			if !maps.Equal(withContext, wantContext) {
				t.Fatalf("files with context=%v, want %v", withContext, wantContext)
			}

			want := formatCorpusResultsWithContext(
				wrapCorpusResults(plan, full), 1, 0, hideCorpusContext, plainPalette,
			)
			got := formatCorpusResultsWithContext(
				wrapCorpusResults(plan, selected), 1, 0, hideCorpusContext, plainPalette,
			)
			if got != want {
				t.Fatalf("selective context changed merged-shard output:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			for _, text := range []string{"a.go", "[uncommitted]", "… 1 more file (showing 1 of 2)"} {
				if !strings.Contains(got, text) {
					t.Fatalf("output does not contain %q:\n%s", text, got)
				}
			}
		})
	}
}

// TestNodeRead_FailsClosedOnPartialCorruptShard reads a node whose index
// holds a good shard and a corrupt one: the read returns index damage, which
// the caller repairs, and no partial result.
func TestNodeRead_FailsClosedOnPartialCorruptShard(t *testing.T) {
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

	q, err := parseSearchQuery("load_shards_marker")
	if err != nil {
		t.Fatal(err)
	}
	plan := node{cacheDir: t.TempDir(), indexDir: indexDir}
	results, err := searchPlannedCorpusParsed(context.Background(), plan, nil, q, defaultSearchConfig())
	if err == nil {
		t.Fatal("expected corrupt shard to fail closed")
	}
	if len(results) != 0 {
		t.Fatalf("corrupt shard search should not return partial results: %#v", results)
	}
	var damage *shardDamage
	if !errors.As(err, &damage) || !errors.Is(err, errShardUnloadable) || !strings.Contains(err.Error(), "load shard") {
		t.Fatalf("error=%v, want shard damage that names the shard load", err)
	}
}
