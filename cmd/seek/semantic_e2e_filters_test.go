package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The tests of this file drive the real search command with the test model
// on query atoms and filters (the querySupport rows). Each description holds
// words that no fixture file holds (zqalpha, zqbeta), so every result of a
// description comes from vector search. The oracle of a filter is the
// one-word text search "zqmark <filter>" (Zoekt's own rule), and a file set
// worked out by hand from the fixture.

// e2eFilterScorer is the test model of this file. It records the text of each
// query that the model prepares and each document that the model scores. With
// windows above 1, it prepares each query as that number of 128-token query
// windows, and each window has one scoring token on the target direction.
type e2eFilterScorer struct {
	*hybridTestScorer
	windows int

	record  sync.Mutex
	queries []string
	scored  []rerankDocument
}

func (scorer *e2eFilterScorer) PrepareSemanticQuery(ctx context.Context, query string) (*semanticQueryEmbedding, error) {
	scorer.record.Lock()
	scorer.queries = append(scorer.queries, query)
	scorer.record.Unlock()
	if scorer.windows <= 1 {
		return scorer.hybridTestScorer.PrepareSemanticQuery(ctx, query)
	}
	scorer.prepareCalls.Add(1)
	prepared := &semanticQueryEmbedding{
		tokens:     make([]float32, scorer.windows*lateOnSequenceLength*semanticEmbeddingDimensions),
		scoreMask:  make([]bool, scorer.windows*lateOnSequenceLength),
		modelQuery: query,
	}
	for window := range scorer.windows {
		token := window*lateOnSequenceLength + 1
		prepared.tokens[token*semanticEmbeddingDimensions] = 1
		prepared.scoreMask[token] = true
	}
	return prepared, nil
}

func (scorer *e2eFilterScorer) ScoresWithSemanticQuery(
	ctx context.Context,
	prepared *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	scorer.record.Lock()
	scorer.scored = append(scorer.scored, documents...)
	scorer.record.Unlock()
	return scorer.hybridTestScorer.ScoresWithSemanticQuery(ctx, prepared, documents)
}

// modelQueries returns the text of each query that the model prepared.
func (scorer *e2eFilterScorer) modelQueries() []string {
	scorer.record.Lock()
	defer scorer.record.Unlock()
	return slices.Clone(scorer.queries)
}

// scoredNames returns the base name of each document that the model scored,
// from every model call of the search.
func (scorer *e2eFilterScorer) scoredNames() []string {
	scorer.record.Lock()
	defer scorer.record.Unlock()
	names := make([]string, 0, len(scorer.scored))
	for _, document := range scorer.scored {
		names = append(names, filepath.Base(document.Path))
	}
	// The hybrid scorer keeps the documents of its last call: they must be
	// in the record too.
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	for _, document := range scorer.documents {
		if !slices.Contains(names, filepath.Base(document.Path)) {
			names = append(names, "missing-from-record:"+document.Path)
		}
	}
	return names
}

// e2eFilterRun is the outcome of one search command.
type e2eFilterRun struct {
	output string
	err    error
	scorer *e2eFilterScorer
	// factory counts the calls of the model factory.
	factory int32
}

// e2eFilterSearch runs query on operands through the real search command,
// with the default policy and the test model.
func e2eFilterSearch(t *testing.T, windows int, query string, operands ...string) e2eFilterRun {
	t.Helper()
	scorer := &e2eFilterScorer{hybridTestScorer: &hybridTestScorer{}, windows: windows}
	var factory atomic.Int32
	output, err := runHybridTestSearch(t, query, operands, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		factory.Add(1)
		return scorer, nil
	})
	return e2eFilterRun{output: output, err: err, scorer: scorer, factory: factory.Load()}
}

// e2eFilterNames returns the sorted base name of each result header.
func e2eFilterNames(output string) []string {
	paths := headerPaths(output)
	names := make([]string, len(paths))
	for index, path := range paths {
		names[index] = filepath.Base(path)
	}
	slices.Sort(names)
	return names
}

// e2eFilterRow is one filter and the live files that it keeps, worked out by
// hand from the fixture. An empty want keeps no file.
type e2eFilterRow struct {
	filter string
	want   []string
}

// e2eFilterExpect checks one filter from the current folder on ".". The text
// search "zqmark <filter>" must keep the hand set. The description
// "zqalpha zqbeta <filter>" must keep the same files through vector search,
// and the model must score no other file. It returns the description output.
func e2eFilterExpect(t *testing.T, row e2eFilterRow) e2eFilterRun {
	t.Helper()
	want := slices.Sorted(slices.Values(row.want))
	text := e2eFilterSearch(t, 1, "zqmark "+row.filter, ".")
	if got := e2eFilterNames(text.output); !slices.Equal(got, want) ||
		(len(want) == 0) != errors.Is(text.err, errNoMatch) {
		t.Fatalf("text oracle %q kept %v (err=%v), want %v:\n%s", row.filter, got, text.err, want, text.output)
	}
	vector := e2eFilterSearch(t, 1, "zqalpha zqbeta "+row.filter, ".")
	switch {
	case len(want) == 0:
		if !errors.Is(vector.err, errNoMatch) || vector.output != "" {
			t.Errorf("description with %q: err=%v, want no match:\n%s", row.filter, vector.err, vector.output)
		}
	case vector.err != nil:
		t.Errorf("description with %q: %v\n%s", row.filter, vector.err, vector.output)
	default:
		if got := e2eFilterNames(vector.output); !slices.Equal(got, want) {
			t.Errorf("vector search with %q kept %v, want the text oracle %v:\n%s", row.filter, got, want, vector.output)
		}
		if vector.scorer.prepareCalls.Load() == 0 {
			t.Errorf("description with %q prepared no model query: it took the strict route", row.filter)
		}
	}
	for _, name := range vector.scorer.scoredNames() {
		if !slices.Contains(want, name) {
			t.Errorf("the model scored %s, which %q drops", name, row.filter)
		}
	}
	return vector
}

// e2eFilterCheckTable runs each row as a subtest. The first row runs cold: it
// builds the vectors of every node, and a warm search must print the same and
// embed nothing.
func e2eFilterCheckTable(t *testing.T, rows []e2eFilterRow) {
	t.Helper()
	for index, row := range rows {
		t.Run(row.filter, func(t *testing.T) {
			cold := e2eFilterExpect(t, row)
			if index != 0 {
				return
			}
			if cold.scorer.embedCalls.Load() == 0 {
				t.Fatalf("the first description embedded nothing: it was not cold")
			}
			warm := e2eFilterSearch(t, 1, "zqalpha zqbeta "+row.filter, ".")
			if warm.err != nil || warm.output != cold.output || warm.scorer.embedCalls.Load() != 0 {
				t.Fatalf("warm search differs (err=%v embeds=%d):\ncold:\n%s\nwarm:\n%s",
					warm.err, warm.scorer.embedCalls.Load(), cold.output, warm.output)
			}
		})
	}
}

// e2eFilterContentTree writes the fixture of the content, symbol, branch and
// repository filters, and makes it the current folder. The tree has three
// nodes:
//
//   - repo, a Git node with zoekt.archived=1: clean.go is committed and
//     clean; mod.go is modified, and its work-tree copy changes every
//     attribute of its committed copy; gone.go, which has every attribute, is
//     deleted from the work tree; new.go is untracked.
//   - the tree itself, a folder node with loose/loose.go.
//   - other, a Git node with zoekt.name=zqnamed: other.go is committed.
//
// Each live file holds SEMANTIC_TARGET and zqmark. It returns the cache home.
func e2eFilterContentTree(t *testing.T) string {
	t.Helper()
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "clean.go", "package repo\n// zqmark SEMANTIC_TARGET zqkeep ZqCase\nfunc ZqHeld() {}\n")
	writeFixtureFile(t, filepath.Join(repo, "mod.go"), "package repo\n// zqmark SEMANTIC_TARGET zqkeep\nfunc ZqHeld() {}\n")
	writeFixtureFile(t, filepath.Join(repo, "gone.go"),
		"package repo\n// zqmark SEMANTIC_TARGET zqkeep zqdrop ZqCase zqcase ZQCASE\nfunc ZqHeld() {}\n\nfunc ZqFreed() {}\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "content")
	gitRunIn(t, repo, "config", "zoekt.archived", "1")
	writeFixtureFile(t, filepath.Join(repo, "mod.go"), "package repo\n// zqmark SEMANTIC_TARGET zqdrop zqcase\nfunc ZqFreed() {}\n")
	if err := os.Remove(filepath.Join(repo, "gone.go")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "new.go"), "package repo\n// zqmark SEMANTIC_TARGET zqdrop ZQCASE\nfunc ZqFreed() {}\n")
	writeFixtureFile(t, f.path("tree/loose/loose.go"), "package loose\n// zqmark SEMANTIC_TARGET zqkeep zqcase\nfunc ZqHeld() {}\n")
	other := f.GitRepo("tree/other", "other.go", "package other\n// zqmark SEMANTIC_TARGET zqdrop ZqCase\nfunc ZqFreed() {}\n")
	gitRunIn(t, other, "config", "zoekt.name", "zqnamed")
	home := setTestUserCache(t)
	t.Chdir(f.path("tree"))
	return home
}

// e2eFilterPathTree writes the fixture of the path filters and the
// description atoms, and makes it the current folder. The tree has three
// nodes:
//
//   - repo, a Git node: ZqUp/clean.go is committed and clean;
//     zqlow/mod_test.go is modified; zqlow/gone.go is deleted from the work
//     tree; ZqUp/new.py is untracked.
//   - the tree itself, a folder node with loose/zqlow/loose.go,
//     loose/ZqUp/loose_test.py, and loose/zqbare, which has no dot in its
//     name or content.
//   - other, a Git node: zqup/other.go is committed.
//
// Each live file holds SEMANTIC_TARGET and zqmark. It returns the cache home.
func e2eFilterPathTree(t *testing.T) string {
	t.Helper()
	f := newGraphFixture(t)
	for _, dir := range []string{"tree/repo/ZqUp", "tree/other/zqup"} {
		// GitRepo writes its first file without its folder.
		if err := os.MkdirAll(f.path(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo := f.GitRepo("tree/repo", "ZqUp/clean.go", "package repo\n// zqmark SEMANTIC_TARGET\n")
	writeFixtureFile(t, filepath.Join(repo, "zqlow/mod_test.go"), "package zqlow\n// zqmark SEMANTIC_TARGET committed\n")
	writeFixtureFile(t, filepath.Join(repo, "zqlow/gone.go"), "package zqlow\n// zqmark SEMANTIC_TARGET deleted\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "paths")
	writeFixtureFile(t, filepath.Join(repo, "zqlow/mod_test.go"), "package zqlow\n// zqmark SEMANTIC_TARGET modified\n")
	if err := os.Remove(filepath.Join(repo, "zqlow/gone.go")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "ZqUp/new.py"), "# zqmark SEMANTIC_TARGET untracked\n")
	writeFixtureFile(t, f.path("tree/loose/zqlow/loose.go"), "package zqlow\n// zqmark SEMANTIC_TARGET folder\n")
	writeFixtureFile(t, f.path("tree/loose/ZqUp/loose_test.py"), "# zqmark SEMANTIC_TARGET folder\n")
	writeFixtureFile(t, f.path("tree/loose/zqbare"), "zqmark SEMANTIC_TARGET folder\n")
	f.GitRepo("tree/other", "zqup/other.go", "package zqup\n// zqmark SEMANTIC_TARGET other\n")
	home := setTestUserCache(t)
	t.Chdir(f.path("tree"))
	return home
}

// e2eFilterPathLive is every live file of e2eFilterPathTree.
var e2eFilterPathLive = []string{"clean.go", "mod_test.go", "new.py", "loose.go", "loose_test.py", "other.go", "zqbare"}

// TestE2EFilterContentSymbolBranchRepo runs descriptions with content:,
// -content:, -word, case directives, sym:, -sym:, branch:, repo:, -repo:,
// archived:, fork:, public:, meta.<field>:, or groups and -( ... ) groups. The
// fixture has a Git node with a dirty overlay, a folder node, and a second
// Git node. A modified file changes every attribute, so the vector search
// must read the filter on its work-tree copy, as the text oracle does.
func TestE2EFilterContentSymbolBranchRepo(t *testing.T) {
	requireTools(t)
	e2eFilterContentTree(t)
	all := []string{"clean.go", "mod.go", "new.go", "loose.go", "other.go"}
	e2eFilterCheckTable(t, []e2eFilterRow{
		{"content:zqdrop", []string{"mod.go", "new.go", "other.go"}},
		{"c:zqdrop", []string{"mod.go", "new.go", "other.go"}},
		{"content:zqkeep", []string{"clean.go", "loose.go"}},
		{"-content:zqkeep", []string{"mod.go", "new.go", "other.go"}},
		{"-c:zqkeep", []string{"mod.go", "new.go", "other.go"}},
		{"-content:zqdrop", []string{"clean.go", "loose.go"}},
		{"-zqkeep", []string{"mod.go", "new.go", "other.go"}},
		{"-zqdrop", []string{"clean.go", "loose.go"}},
		// Smart case: a capital letter makes the atom case-sensitive.
		{"content:ZqCase", []string{"clean.go", "other.go"}},
		{"case:auto content:ZqCase", []string{"clean.go", "other.go"}},
		{"content:zqcase", all},
		{"case:yes content:zqcase", []string{"mod.go", "loose.go"}},
		{"case:yes content:ZQCASE", []string{"new.go"}},
		{"case:no content:ZqCase", all},
		{"sym:ZqFreed", []string{"mod.go", "new.go", "other.go"}},
		{"sym:ZqHeld", []string{"clean.go", "loose.go"}},
		{"-sym:ZqHeld", []string{"mod.go", "new.go", "other.go"}},
		{"-sym:ZqFreed", []string{"clean.go", "loose.go"}},
		// Committed Git files carry HEAD; work-tree and folder files carry no
		// branch.
		{"branch:HEAD", []string{"clean.go", "other.go"}},
		{"-branch:HEAD", []string{"mod.go", "new.go", "loose.go"}},
		{"-b:HEAD", []string{"mod.go", "new.go", "loose.go"}},
		{"branch:nomatch", nil},
		// Work-tree files are in Seek's internal repository "uncommitted".
		{"repo:uncommitted", []string{"mod.go", "new.go"}},
		{"-repo:uncommitted", []string{"clean.go", "loose.go", "other.go"}},
		{"repo:zqnamed", []string{"other.go"}},
		{"r:zqnamed", []string{"other.go"}},
		{"-repo:zqnamed", []string{"clean.go", "mod.go", "new.go", "loose.go"}},
		{"-r:zqnamed", []string{"clean.go", "mod.go", "new.go", "loose.go"}},
		// Only the committed shard of repo reads zoekt.archived=1.
		{"archived:yes", []string{"clean.go"}},
		{"archived:no", []string{"mod.go", "new.go", "loose.go", "other.go"}},
		{"fork:yes", nil},
		{"fork:no", all},
		{"public:yes", nil},
		{"public:no", all},
		// Seek sets no repository metadata.
		{"meta.owner:x", nil},
		{"-meta.owner:x", all},
		{"(content:zqdrop or file:loose)", []string{"mod.go", "new.go", "loose.go", "other.go"}},
		{"(repo:uncommitted or repo:zqnamed)", []string{"mod.go", "new.go", "other.go"}},
		{"-(content:zqkeep or file:other)", []string{"mod.go", "new.go"}},
		{"-(file:repo/ or sym:ZqHeld)", []string{"other.go"}},
	})
}

// TestE2EFilterPathFilters runs descriptions with file:, -file:, and their f:
// aliases, as substrings and regular expressions, with Zoekt's case rules,
// and with -lang:, or groups, and -( ... ) groups. The patterns read the
// header path from the search root. The fixture has a Git node with a dirty
// overlay, a folder node, and a second Git node.
func TestE2EFilterPathFilters(t *testing.T) {
	requireTools(t)
	e2eFilterPathTree(t)
	e2eFilterCheckTable(t, []e2eFilterRow{
		{"file:ZqUp", []string{"clean.go", "new.py", "loose_test.py"}},
		{"f:ZqUp", []string{"clean.go", "new.py", "loose_test.py"}},
		{"file:zqup", []string{"clean.go", "new.py", "loose_test.py", "other.go"}},
		{"case:yes file:zqup", []string{"other.go"}},
		{"case:no file:ZqUp", []string{"clean.go", "new.py", "loose_test.py", "other.go"}},
		{"case:auto file:ZqUp", []string{"clean.go", "new.py", "loose_test.py"}},
		{"file:ZQUP", nil},
		// A pattern of fewer than three runes.
		{"file:Up", []string{"clean.go", "new.py", "loose_test.py"}},
		{"file:up", []string{"clean.go", "new.py", "loose_test.py", "other.go"}},
		{"-file:ZqUp", []string{"mod_test.go", "loose.go", "other.go", "zqbare"}},
		{"-f:zqup", []string{"mod_test.go", "loose.go", "zqbare"}},
		{`file:_test\.go$`, []string{"mod_test.go"}},
		{`file:\.py$`, []string{"new.py", "loose_test.py"}},
		{"-file:_test", []string{"clean.go", "new.py", "loose.go", "other.go", "zqbare"}},
		{`-file:\.go$`, []string{"new.py", "loose_test.py", "zqbare"}},
		{"file:^repo/", []string{"clean.go", "mod_test.go", "new.py"}},
		{"file:^Repo/", nil},
		{"case:no file:^REPO/", []string{"clean.go", "mod_test.go", "new.py"}},
		{"file:^(loose|other)/", []string{"loose.go", "loose_test.py", "other.go", "zqbare"}},
		{"-lang:go file:ZqUp", []string{"new.py", "loose_test.py"}},
		{"-lang:python -file:_test", []string{"clean.go", "loose.go", "other.go", "zqbare"}},
		{"-lang:go -file:loose", []string{"new.py"}},
		{"file:zqlow -lang:python", []string{"mod_test.go", "loose.go"}},
		{`(file:zqlow or file:\.py$)`, []string{"mod_test.go", "loose.go", "new.py", "loose_test.py"}},
		{"(file:other or -file:loose)", []string{"clean.go", "mod_test.go", "new.py", "other.go"}},
		{"-(file:ZqUp or lang:python)", []string{"mod_test.go", "loose.go", "other.go", "zqbare"}},
	})
}

// TestE2EFilterDescriptionAtomsReachTheModel runs descriptions in each
// description form: a quoted phrase, a bare regular expression token, regex:,
// a backslash escape, smart case, programming names, an unknown prefix, a
// plain OR, a token with no letter or digit, and parentheses. Each query takes
// the vector route on every node kind. The model reads the hand-written text,
// and the results are the files that the filters keep.
func TestE2EFilterDescriptionAtomsReachTheModel(t *testing.T) {
	requireTools(t)
	e2eFilterPathTree(t)
	cases := []struct {
		query string
		model string
		want  []string
	}{
		{`"zqalpha zqbeta" zqgamma`, "zqalpha zqbeta zqgamma", e2eFilterPathLive},
		{"zqalph. zqbeta", "zqalph. zqbeta", e2eFilterPathLive},
		{"regex:zqalpha zqbeta", "zqalpha zqbeta", e2eFilterPathLive},
		{`zq\.alpha zqbeta`, "zq.alpha zqbeta", e2eFilterPathLive},
		{"Zqalpha Zqbeta", "Zqalpha Zqbeta", e2eFilterPathLive},
		{"zqparse C++", "zqparse C++", e2eFilterPathLive},
		{"zqparse C++ lang:go", "zqparse C++", []string{"clean.go", "mod_test.go", "loose.go", "other.go"}},
		// Zoekt rejects X[]: Seek quotes the name and parses again.
		{"zqindex X[] zqbeta", "zqindex X[] zqbeta", e2eFilterPathLive},
		// Zoekt accepts a::b() as a regular expression: the model reads the
		// token text (rows T02 and T31).
		{"zqcall a::b() zqbeta", "zqcall a::b() zqbeta", e2eFilterPathLive},
		// l: is no Zoekt prefix: the Python files stay.
		{"zqalpha l:go", "zqalpha l:go", e2eFilterPathLive},
		{"zqalpha OR zqbeta", "zqalpha OR zqbeta", e2eFilterPathLive},
		// A token with no letter or digit is no filter of vector search:
		// zqbare has no dot and stays.
		{"zqalpha zqbeta .", "zqalpha zqbeta", e2eFilterPathLive},
		{`zqalpha zqbeta \.`, "zqalpha zqbeta", e2eFilterPathLive},
		{"(zqalpha zqbeta) -file:_test", "zqalpha zqbeta", []string{"clean.go", "new.py", "loose.go", "other.go", "zqbare"}},
	}
	for index, test := range cases {
		t.Run(test.query, func(t *testing.T) {
			run := e2eFilterSearch(t, 1, test.query, ".")
			if run.err != nil {
				t.Fatalf("%v\n%s", run.err, run.output)
			}
			if index == 0 && run.scorer.embedCalls.Load() == 0 {
				t.Fatal("the first description embedded nothing: it was not cold")
			}
			if got := run.scorer.modelQueries(); !slices.Equal(got, []string{test.model}) {
				t.Errorf("the model read %q, want %q", got, test.model)
			}
			want := slices.Sorted(slices.Values(test.want))
			if got := e2eFilterNames(run.output); !slices.Equal(got, want) {
				t.Errorf("vector search kept %v, want %v:\n%s", got, want, run.output)
			}
			for _, name := range run.scorer.scoredNames() {
				if !slices.Contains(want, name) {
					t.Errorf("the model scored %s, which the query drops", name)
				}
			}
			if index == 0 {
				warm := e2eFilterSearch(t, 1, test.query, ".")
				if warm.err != nil || warm.output != run.output || warm.scorer.embedCalls.Load() != 0 {
					t.Fatalf("warm search differs (err=%v):\ncold:\n%s\nwarm:\n%s", warm.err, run.output, warm.output)
				}
			}
		})
	}
}

// TestE2EFilterStrictFormsDoNoVectorWork runs the query forms that keep
// strict ranking on a fresh cache: one description atom, with and without
// filters, a lone quoted phrase, an or between descriptions, an or that binds
// the whole query, and filters alone. No query starts the model or writes
// vectors, and each prints what --lexical-only prints.
func TestE2EFilterStrictFormsDoNoVectorWork(t *testing.T) {
	requireTools(t)
	home := e2eFilterPathTree(t)
	cases := []struct {
		query string
		// want holds the files that the strict search keeps; nil keeps none.
		want []string
	}{
		{"zqalpha", nil},
		{"zqalpha lang:go -file:loose", nil},
		{`"zqalpha zqbeta"`, nil},
		{"zqalpha or zqbeta", nil},
		{"(zqalpha or zqbeta) lang:go", nil},
		// The or binds the whole query: (zqalpha zqbeta file:zqlow) or
		// file:other.
		{"zqalpha zqbeta file:zqlow or file:other", []string{"other.go"}},
		{"zqmark -lang:go", []string{"new.py", "loose_test.py", "zqbare"}},
		{"lang:go -file:_test", []string{"clean.go", "loose.go", "other.go"}},
		{"file:ZqUp", []string{"clean.go", "new.py", "loose_test.py"}},
		{"-file:zqup -lang:go", []string{"zqbare"}},
	}
	for _, test := range cases {
		t.Run(test.query, func(t *testing.T) {
			run := e2eFilterSearch(t, 1, test.query, ".")
			if run.factory != 0 || run.scorer.prepareCalls.Load() != 0 || run.scorer.embedCalls.Load() != 0 {
				t.Fatalf("the model started: factory=%d prepares=%d embeds=%d",
					run.factory, run.scorer.prepareCalls.Load(), run.scorer.embedCalls.Load())
			}
			want := slices.Sorted(slices.Values(test.want))
			if got := e2eFilterNames(run.output); !slices.Equal(got, want) ||
				(len(want) == 0) != errors.Is(run.err, errNoMatch) {
				t.Fatalf("strict search kept %v (err=%v), want %v:\n%s", got, run.err, want, run.output)
			}
			// The root command runs the query with --lexical-only, as the CLI
			// does. The "--" keeps a query that starts with "-" as an argument.
			root := newQuietRootCmd()
			root.SetArgs([]string{"--lexical-only", "--", test.query, "."})
			lexical, lexicalErr := captureStdout(t, func() error { return root.ExecuteContext(t.Context()) })
			if lexical != run.output || errors.Is(lexicalErr, errNoMatch) != errors.Is(run.err, errNoMatch) {
				t.Fatalf("--lexical-only prints another answer (err=%v):\n%s\nwant:\n%s", lexicalErr, lexical, run.output)
			}
		})
	}
	if dirs := e2eFilterSemanticDirs(t, home); len(dirs) != 0 {
		t.Fatalf("strict queries wrote vector generations: %v", dirs)
	}
}

// e2eFilterSemanticDirs lists each semantic generation directory below root.
func e2eFilterSemanticDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), semanticGenerationPrefix) {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// TestE2EFilterLongQueryKeepsEveryTerm runs a description of 130 words with a
// -file: filter. The model reads every word in order. The test model
// prepares the query as three query windows; a file passes the radius only
// when retrieval reads every window. The results are the files that the
// filter keeps, on every node kind.
func TestE2EFilterLongQueryKeepsEveryTerm(t *testing.T) {
	requireTools(t)
	e2eFilterPathTree(t)
	words := make([]string, 130)
	for index := range words {
		words[index] = fmt.Sprintf("zqw%03d", index)
	}
	description := strings.Join(words, " ")
	run := e2eFilterSearch(t, 3, description+" -file:_test", ".")
	if run.err != nil {
		t.Fatalf("%v\n%s", run.err, run.output)
	}
	if got := run.scorer.modelQueries(); len(got) != 1 || got[0] != description {
		t.Fatalf("the model read %q, want the 130 words", got)
	}
	want := []string{"clean.go", "loose.go", "new.py", "other.go", "zqbare"}
	if got := e2eFilterNames(run.output); !slices.Equal(got, want) {
		t.Fatalf("vector search kept %v, want %v:\n%s", got, want, run.output)
	}
	for _, name := range run.scorer.scoredNames() {
		if !slices.Contains(want, name) {
			t.Fatalf("the model scored %s, which -file:_test drops", name)
		}
	}
}

// TestE2EFilterTypeFilePrintsNamesOnly runs descriptions with type:file and
// its spellings, alone and with filters. Vector search finds each file, and
// the search prints only the name line of each file, as the text oracle does.
// A negated -type:file fails the search of each node: Seek warns once for
// each node and finds nothing.
func TestE2EFilterTypeFilePrintsNamesOnly(t *testing.T) {
	requireTools(t)
	e2eFilterContentTree(t)
	for _, row := range []e2eFilterRow{
		{"type:file", []string{"clean.go", "mod.go", "new.go", "loose.go", "other.go"}},
		{"t:file content:zqdrop", []string{"mod.go", "new.go", "other.go"}},
		{"type:filename -branch:HEAD", []string{"mod.go", "new.go", "loose.go"}},
		{"t:filename repo:zqnamed", []string{"other.go"}},
	} {
		t.Run(row.filter, func(t *testing.T) {
			run := e2eFilterExpect(t, row)
			header := ""
			for line := range strings.Lines(run.output) {
				line = strings.TrimSuffix(line, "\n")
				switch {
				case line == "":
				case strings.HasPrefix(line, "## "):
					header = strings.Fields(strings.TrimPrefix(line, "## "))[0]
				case line != "0 "+header:
					t.Fatalf("line %q is no name line of %s:\n%s", line, header, run.output)
				}
			}
		})
	}
	t.Run("-type:file", func(t *testing.T) {
		logs := captureTestLogs(t, slog.LevelWarn)
		run := e2eFilterSearch(t, 1, "zqalpha zqbeta -type:file", ".")
		if !errors.Is(run.err, errNoMatch) || run.output != "" {
			t.Fatalf("err=%v, want no match:\n%s", run.err, run.output)
		}
		if names := run.scorer.scoredNames(); len(names) != 0 {
			t.Fatalf("the model scored %v", names)
		}
		roots := map[string]bool{}
		for _, record := range logs.Records() {
			if record.Message == "Skipped a search node; its results are missing" {
				roots[fmt.Sprint(testLogAttrs(record)["root"])] = true
			}
		}
		if len(roots) != 3 {
			t.Fatalf("warned for the nodes %v, want the three nodes", roots)
		}
	})
}

// TestE2EFilterUpstreamPanicForms runs descriptions with type:filematch,
// type:repo, their t: spellings, and a negated -case: in the real process.
// The support table documents an upstream Zoekt bug: the per-shard searcher
// panics on these atoms, so the process stops with exit code 2 and prints no
// result. A concurrent shard read can then fault, so the crash text is a
// panic or a fault, and both name Zoekt's per-shard searcher. The type: rows
// keep strict ranking: the crash comes before any vector work.
func TestE2EFilterUpstreamPanicForms(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.go", "package sample\n// zqmark SEMANTIC_TARGET\n")
	for _, test := range []struct {
		query  string
		strict bool
	}{
		{"zqalpha zqbeta type:filematch", true},
		{"zqalpha zqbeta t:filematch", true},
		{"zqalpha zqbeta type:repo", true},
		{"zqalpha zqbeta t:repo", true},
		// The support table gives -case: no route.
		{"-case:yes zqalpha zqbeta", false},
	} {
		t.Run(test.query, func(t *testing.T) {
			cache := t.TempDir()
			result := runCLIProcessWithCache(t, cache, folder, []string{test.query, folder}, nil)
			if result.code != 2 || result.stdout != "" ||
				!strings.Contains(result.stderr, "zoekt/index.(*indexData).newMatchTree") {
				t.Fatalf("code=%d stdout=%q, want the crash of Zoekt's per-shard searcher:\n%s",
					result.code, result.stdout, result.stderr)
			}
			if !test.strict {
				return
			}
			// The model route builds the vectors of a node before it reads the
			// node, so a crash on that route leaves a vector generation.
			if dirs := e2eFilterSemanticDirs(t, cache); len(dirs) != 0 {
				t.Fatalf("the search wrote vector generations: %v", dirs)
			}
		})
	}
}
