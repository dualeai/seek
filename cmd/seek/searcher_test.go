package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
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
// does, and reads the node. The read hides the committed copy of a.go,
// because the uncommitted copy wins. With a context file limit of 1, only the
// best file after the merge of the two shards keeps its context lines, in each
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
			if len(selected) != 2 || len(full) != 2 {
				t.Fatalf("result counts selective=%d full=%d, want 2 each", len(selected), len(full))
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

// TestSearchLoadedShardsRecoversAShardPanic checks the two panic classes of a
// shard search: a corrupt-shard panic is index damage that the node read
// repairs, and any other panic is a plain error. The other shard still runs.
func TestSearchLoadedShardsRecoversAShardPanic(t *testing.T) {
	for _, test := range []struct {
		name   string
		value  any
		damage bool
	}{
		{"corrupt shard", fmt.Errorf(`corrupt shard "a.zoekt" while searching repository "r", document 3: %w`, errors.New("reading content")), true},
		{"query type", "type *query.Type", false},
		{"runtime error", recoveredRuntimeError(), true},
		{"error string", fmt.Errorf("index out of range [9] with length 2"), false},
		{"negated case", "type *query.caseQ", false},
		{"corrupt index", "corrupt index: subrepo 127 beyond [[]]", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var healthyRan atomic.Bool
			healthy := &cancellationTestShard{search: func(context.Context) (*zoekt.SearchResult, error) {
				healthyRan.Store(true)
				return &zoekt.SearchResult{Files: []zoekt.FileMatch{{FileName: "ok.go"}}}, nil
			}}
			broken := &cancellationTestShard{search: func(context.Context) (*zoekt.SearchResult, error) {
				panic(test.value)
			}}
			files, err := searchLoadedShards(t.Context(), []zoekt.Searcher{healthy, broken}, &query.Const{Value: true}, defaultSearchConfig(), nil)
			if err == nil || len(files) != 0 || strings.Contains(err.Error(), "shard search panicked") == test.damage {
				t.Fatalf("files=%v err=%v, want no files and the shard panic", files, err)
			}
			if got := errors.Is(err, errShardUnloadable); got != test.damage {
				t.Fatalf("index damage=%v, want %v (err=%v)", got, test.damage, err)
			}
			if !healthyRan.Load() {
				t.Fatal("the healthy shard did not run")
			}
		})
	}
}

// recoveredRuntimeError returns the value of a real Go runtime panic.
func recoveredRuntimeError() (value any) {
	defer func() { value = recover() }()
	var values []int
	index := 9
	_ = values[index]
	return nil
}

// TestZoektShardCallsReturnPanicsAsErrors checks the reads that call a Zoekt
// shard outside searchLoadedShards: the BM25 counts (a List, then a listing)
// and the current-copy listing of an uncommitted shard. A shard whose calls
// panic with a runtime error gives index damage, not a crash.
func TestZoektShardCallsReturnPanicsAsErrors(t *testing.T) {
	broken := &cancellationTestShard{search: func(context.Context) (*zoekt.SearchResult, error) {
		panic(recoveredRuntimeError())
	}}
	// broken.List panics with a nil dereference: its embedded Searcher is nil.
	path := filepath.Join(t.TempDir(), "index", repoUncommitted+"_v16.00000.zoekt")
	writeFixtureFile(t, path, "shard bytes\n")
	record := readShardStatsRecord(filepath.Dir(path))
	if _, err := record.stats(t.Context(), path, broken); !errors.Is(err, errShardUnloadable) {
		t.Fatalf("BM25 counts: err=%v, want index damage", err)
	}
	committed := filepath.Join(filepath.Dir(path), "git-0_v16.00000.zoekt")
	if _, err := currentCopies(t.Context(), []zoekt.Searcher{nil, broken}, []string{committed, path}, nil); !errors.Is(err, errShardUnloadable) {
		t.Fatalf("current-copy listing: err=%v, want index damage", err)
	}
}

// TestCurrentCopiesSkipsTheListingWithNoCommittedShard checks that a node with
// only uncommitted shards gets no clause and lists no shard: no committed copy
// exists to hide.
func TestCurrentCopiesSkipsTheListingWithNoCommittedShard(t *testing.T) {
	listed := &cancellationTestShard{search: func(context.Context) (*zoekt.SearchResult, error) {
		t.Error("the listing ran with no committed shard")
		return &zoekt.SearchResult{}, nil
	}}
	path := filepath.Join(t.TempDir(), repoUncommitted+"_v16.00000.zoekt")
	clause, err := currentCopies(t.Context(), []zoekt.Searcher{listed}, []string{path}, dirtyFileSet{"a.go": {}})
	if err != nil || clause != nil {
		t.Fatalf("clause=%v err=%v, want no clause", clause, err)
	}
}

// TestNullShardMetadataIsNoCrash writes "null" into the .meta file of each
// shard of a Git node: Zoekt then gives a nil repository record with no
// error. The metadata reads drop it, so the next search, the committed head
// check and gc's display read do not stop the process.
func TestNullShardMetadataIsNoCrash(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := initGitRepo(t, "a.go", "package a\n// zqnullmeta\n")
	t.Chdir(repo)
	if _, err := seek(t, "--lexical-only", "zqnullmeta"); err != nil {
		t.Fatal(err)
	}
	cache, err := seekUserCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	shards, _ := filepath.Glob(filepath.Join(cache, "corpora", "*", "index", "*.zoekt"))
	if len(shards) == 0 {
		t.Fatal("no shard")
	}
	for _, shard := range shards {
		writeFixtureFile(t, shard+".meta", "null")
		repos, err := zoektReadMetadata(shard)
		if slices.Contains(repos, nil) {
			t.Fatalf("zoektReadMetadata(%s) = %v, %v: a nil record", shard, repos, err)
		}
		readCorpusDisplayInfo(filepath.Dir(filepath.Dir(shard)))
	}
	_, _ = seek(t, "--lexical-only", "zqnullmeta")
}

// TestDeltaBuilderWithNullShardMetadataIsNoCrash seeds an index, writes
// "null" into the .meta file of its shard, and runs a delta on it. Zoekt's
// delta Builder reads the records of the old shards through the shard-call
// guard: the delta fails as index damage, and the process keeps running.
func TestDeltaBuilderWithNullShardMetadataIsNoCrash(t *testing.T) {
	requireTools(t)
	indexDir := t.TempDir()
	source := t.TempDir()
	seed := make(chan fileContent, 1)
	seed <- makeFileContent(t, "seed.go", []byte("package seed\nfunc Seed() {}\n"))
	close(seed)
	if _, err := indexDocuments(t.Context(), indexDir, "null_meta_repo", source, seed, 1); err != nil {
		t.Fatal(err)
	}
	shards, _ := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if len(shards) == 0 {
		t.Fatal("no shard")
	}
	for _, shard := range shards {
		writeFixtureFile(t, shard+".meta", "null")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("the delta panicked: %v", recovered)
		}
	}()
	changed := []fileContent{makeFileContent(t, "seed.go", []byte("package seed\nfunc Changed() {}\n"))}
	if _, err := indexDeltaDocuments(indexDir, "null_meta_repo", source, changed, 1, []string{"seed.go"}); !errors.Is(err, errShardUnloadable) {
		t.Fatalf("a delta on a shard with a null record gave %v, want index damage", err)
	}
	// The failed Finish leaves no temporary shard in the live directory.
	if temps, _ := filepath.Glob(filepath.Join(indexDir, "*.tmp")); len(temps) != 0 {
		t.Fatalf("the failed delta left %v", temps)
	}
}

// TestFullBuildLaterWindowWithNullRecordIsNoCrash runs a full build whose
// large document opens a second window, a delta Builder, and writes "null"
// into the .meta file of the first window's shard before the input ends. The
// second window's Finish reads that record through the shard-call guard: the
// build fails as index damage, and the process keeps running.
func TestFullBuildLaterWindowWithNullRecordIsNoCrash(t *testing.T) {
	requireTools(t)
	oldShardMax := shardMax
	shardMax = 1024
	t.Cleanup(func() { shardMax = oldShardMax })
	indexDir := t.TempDir()
	source := t.TempDir()
	files := make(chan fileContent)
	go func() {
		// The channel closes after the write, so the last Finish reads it.
		defer close(files)
		files <- makeFileContent(t, "small.go", []byte("package small\nfunc Small() {}\n"))
		files <- makeFileContent(t, "large.txt", []byte(strings.Repeat("large text line\n", 200)))
		shard := filepath.Join(indexDir, "window_repo_v16.00000.zoekt")
		for range 500 {
			if _, err := os.Stat(shard); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		writeFixtureFile(t, shard+".meta", "null")
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("the full build panicked: %v", recovered)
		}
	}()
	if _, err := indexDocuments(t.Context(), indexDir, "window_repo", source, files, 2); !errors.Is(err, errShardUnloadable) {
		t.Fatalf("the build gave %v, want index damage", err)
	}
}
