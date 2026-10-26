package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The tests of this file drive the root command with the test model on query
// atoms and filters (the querySupport rows). Each description holds words
// that no fixture file holds (zqalpha, zqbeta), so every result of a
// description comes from vector search. The test model accepts every row
// (payloadAccept), so a description prints every live file that its filters
// keep. The tests prove the filters, the routes and the text that the model
// reads; they prove nothing about meaning, tau, the retrieval radius or rank.
// The oracle of a filter is the one-word text search "zqmark <filter>" with
// --lexical-only (Zoekt's own rule), and a file set worked out by hand from
// the fixture.

// e2eFilterRun is the outcome of one search command, with the record of its
// test model in accept mode. A test with it proves which files the filters
// keep and which query text the model reads. It cannot prove meaning, tau,
// the retrieval radius or rank.
type e2eFilterRun struct {
	output string
	err    error
	// model records the calls of the test model, and its starts count the
	// calls of the factory.
	model *observedModel
}

// e2eFilterSearch runs query on operands through the root command, with the
// test model in accept mode. It does not use stubSearch, because these tests
// read the records of observedModel: the query texts, the scored documents and
// the factory starts. The "--" keeps a query that starts with "-" as the query.
func e2eFilterSearch(t *testing.T, query string, operands ...string) e2eFilterRun {
	t.Helper()
	model := observeModel(&hybridTestScorer{mode: payloadAccept})
	output, err := seekWith(t, model.factory(), slices.Concat([]string{"--", query}, operands)...)
	return e2eFilterRun{output: output, err: err, model: model}
}

// modelQueries returns the text of each query that the model prepared.
func (run e2eFilterRun) modelQueries() []string {
	run.model.mu.Lock()
	defer run.model.mu.Unlock()
	return slices.Clone(run.model.queries)
}

// scoredNames returns the base name of each distinct path that the search
// sent to the model, over all score calls (scoredPaths).
func (run e2eFilterRun) scoredNames() []string {
	paths, _ := run.model.scoredPaths()
	names := make([]string, len(paths))
	for index, path := range paths {
		names[index] = filepath.Base(path)
	}
	return names
}

// e2eFilterText runs query on operands through the root command with
// --lexical-only. The "--" keeps a query that starts with "-" as an argument.
func e2eFilterText(t *testing.T, query string, operands ...string) (string, error) {
	t.Helper()
	return seek(t, slices.Concat([]string{"--lexical-only", "--", query}, operands)...)
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
// "zqalpha zqbeta <filter>" must print the same files through vector search,
// and the model must score no other file. It returns the description output.
func e2eFilterExpect(t *testing.T, row e2eFilterRow) e2eFilterRun {
	t.Helper()
	want := slices.Sorted(slices.Values(row.want))
	text, textErr := e2eFilterText(t, "zqmark "+row.filter, ".")
	if got := e2eFilterNames(text); !slices.Equal(got, want) || (len(want) == 0) != errors.Is(textErr, errNoMatch) {
		t.Fatalf("text oracle %q kept %v (err=%v), want %v:\n%s", row.filter, got, textErr, want, text)
	}
	vector := e2eFilterSearch(t, "zqalpha zqbeta "+row.filter, ".")
	switch {
	case len(want) == 0:
		if !errors.Is(vector.err, errNoMatch) || vector.output != "" {
			t.Errorf("description with %q: err=%v, want no match:\n%s", row.filter, vector.err, vector.output)
		}
	case vector.err != nil:
		t.Errorf("description with %q: %v\n%s", row.filter, vector.err, vector.output)
	default:
		if got := e2eFilterNames(vector.output); !slices.Equal(got, want) {
			t.Errorf("vector search with %q printed %v, want the text oracle %v:\n%s", row.filter, got, want, vector.output)
		}
		if vector.model.prepares.Load() == 0 {
			t.Errorf("description with %q prepared no model query: it took the strict route", row.filter)
		}
	}
	for _, name := range vector.scoredNames() {
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
			if cold.model.embeds.Load() == 0 {
				t.Fatalf("the first description embedded nothing: it was not cold")
			}
			warm := e2eFilterSearch(t, "zqalpha zqbeta "+row.filter, ".")
			if warm.err != nil || warm.output != cold.output || warm.model.embeds.Load() != 0 {
				t.Fatalf("warm search differs (err=%v embeds=%d):\ncold:\n%s\nwarm:\n%s",
					warm.err, warm.model.embeds.Load(), cold.output, warm.output)
			}
		})
	}
}

// e2eFilterContentTree writes the fixture of the content, symbol, branch and
// repository filters, and makes it the current folder. The tree has three
// nodes:
//
//   - repo, a Git node with zoekt.archived=1, which Seek does not read:
//     clean.go is committed and clean; mod.go is modified, and its work-tree
//     copy changes every attribute of its committed copy; gone.go, which has
//     every attribute, is deleted from the work tree; new.go is untracked.
//   - the tree itself, a folder node with loose/loose.go.
//   - other, a Git node with zoekt.name=zqnamed, which Seek does not read:
//     other.go is committed in the repository with the opaque name.
//
// Each live file holds zqmark. It returns the cache home.
func e2eFilterContentTree(t *testing.T) string {
	t.Helper()
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "clean.go", "package repo\n// zqmark zqkeep ZqCase\nfunc ZqHeld() {}\n")
	writeFixtureFile(t, filepath.Join(repo, "mod.go"), "package repo\n// zqmark zqkeep\nfunc ZqHeld() {}\n")
	writeFixtureFile(t, filepath.Join(repo, "gone.go"),
		"package repo\n// zqmark zqkeep zqdrop ZqCase zqcase ZQCASE\nfunc ZqHeld() {}\n\nfunc ZqFreed() {}\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "content")
	gitRunIn(t, repo, "config", "zoekt.archived", "1")
	writeFixtureFile(t, filepath.Join(repo, "mod.go"), "package repo\n// zqmark zqdrop zqcase\nfunc ZqFreed() {}\n")
	if err := os.Remove(filepath.Join(repo, "gone.go")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "new.go"), "package repo\n// zqmark zqdrop ZQCASE\nfunc ZqFreed() {}\n")
	writeFixtureFile(t, f.path("tree/loose/loose.go"), "package loose\n// zqmark zqkeep zqcase\nfunc ZqHeld() {}\n")
	other := f.GitRepo("tree/other", "other.go", "package other\n// zqmark zqdrop ZqCase\nfunc ZqFreed() {}\n")
	gitRunIn(t, other, "config", "zoekt.name", "zqnamed")
	home := setTestUserCache(t)
	t.Chdir(f.path("tree"))
	return home
}

// e2eFilterOtherRepo returns the opaque repository name of the other node of
// e2eFilterContentTree, from the tree folder. repo: matches only this name;
// Seek does not read zoekt.name.
func e2eFilterOtherRepo(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return fallbackGitRepositoryName(filepath.Join(cwd, "other"))
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
// Each live file holds zqmark. It returns the cache home.
func e2eFilterPathTree(t *testing.T) string {
	t.Helper()
	f := newGraphFixture(t)
	for _, dir := range []string{"tree/repo/ZqUp", "tree/other/zqup"} {
		// GitRepo writes its first file without its folder.
		if err := os.MkdirAll(f.path(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo := f.GitRepo("tree/repo", "ZqUp/clean.go", "package repo\n// zqmark\n")
	writeFixtureFile(t, filepath.Join(repo, "zqlow/mod_test.go"), "package zqlow\n// zqmark committed\n")
	writeFixtureFile(t, filepath.Join(repo, "zqlow/gone.go"), "package zqlow\n// zqmark deleted\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "paths")
	writeFixtureFile(t, filepath.Join(repo, "zqlow/mod_test.go"), "package zqlow\n// zqmark modified\n")
	if err := os.Remove(filepath.Join(repo, "zqlow/gone.go")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "ZqUp/new.py"), "# zqmark untracked\n")
	writeFixtureFile(t, f.path("tree/loose/zqlow/loose.go"), "package zqlow\n// zqmark folder\n")
	writeFixtureFile(t, f.path("tree/loose/ZqUp/loose_test.py"), "# zqmark folder\n")
	writeFixtureFile(t, f.path("tree/loose/zqbare"), "zqmark folder\n")
	f.GitRepo("tree/other", "zqup/other.go", "package zqup\n// zqmark other\n")
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
	other := e2eFilterOtherRepo(t)
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
		{"repo:" + other, []string{"other.go"}},
		{"r:" + other, []string{"other.go"}},
		{"-repo:" + other, []string{"clean.go", "mod.go", "new.go", "loose.go"}},
		{"-r:" + other, []string{"clean.go", "mod.go", "new.go", "loose.go"}},
		{"repo:zqnamed", nil},
		// No shard reads zoekt.archived=1.
		{"archived:yes", nil},
		{"archived:no", all},
		{"fork:yes", nil},
		{"fork:no", all},
		{"public:yes", nil},
		{"public:no", all},
		// Seek sets no repository metadata.
		{"meta.owner:x", nil},
		{"-meta.owner:x", all},
		{"(content:zqdrop or file:loose)", []string{"mod.go", "new.go", "loose.go", "other.go"}},
		{"(repo:uncommitted or repo:" + other + ")", []string{"mod.go", "new.go", "other.go"}},
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
// and with a model that accepts every row, the results are the files that the
// filters keep.
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
			run := e2eFilterSearch(t, test.query, ".")
			if run.err != nil {
				t.Fatalf("%v\n%s", run.err, run.output)
			}
			if index == 0 && run.model.embeds.Load() == 0 {
				t.Fatal("the first description embedded nothing: it was not cold")
			}
			if got := run.modelQueries(); !slices.Equal(got, []string{test.model}) {
				t.Errorf("the model read %q, want %q", got, test.model)
			}
			want := slices.Sorted(slices.Values(test.want))
			if got := e2eFilterNames(run.output); !slices.Equal(got, want) {
				t.Errorf("vector search printed %v, want %v:\n%s", got, want, run.output)
			}
			for _, name := range run.scoredNames() {
				if !slices.Contains(want, name) {
					t.Errorf("the model scored %s, which the query drops", name)
				}
			}
			if index == 0 {
				warm := e2eFilterSearch(t, test.query, ".")
				if warm.err != nil || warm.output != run.output || warm.model.embeds.Load() != 0 {
					t.Fatalf("warm search differs (err=%v):\ncold:\n%s\nwarm:\n%s", warm.err, run.output, warm.output)
				}
			}
		})
	}
}

// TestE2EFilterEveryFormUsesVectors runs the query forms on a fresh cache.
// A query of negations only gives the model no text: it starts no model,
// writes no vectors, and prints what --lexical-only prints. Every other form
// (one description atom, with and without filters, a lone quoted phrase, an or
// between descriptions, an or that binds the whole query, and filters alone)
// prepares one model query. With a model that accepts every row, it prints
// every live file that its filters keep, and so every file of the strict
// search.
func TestE2EFilterEveryFormUsesVectors(t *testing.T) {
	requireTools(t)
	home := e2eFilterPathTree(t)
	t.Run("-file:zqup -lang:go", func(t *testing.T) {
		run := e2eFilterSearch(t, "-file:zqup -lang:go", ".")
		if run.model.starts.Load() != 0 || run.model.prepares.Load() != 0 || run.model.embeds.Load() != 0 {
			t.Fatalf("the model started: factory=%d prepares=%d embeds=%d",
				run.model.starts.Load(), run.model.prepares.Load(), run.model.embeds.Load())
		}
		if got := e2eFilterNames(run.output); !slices.Equal(got, []string{"zqbare"}) {
			t.Fatalf("strict search kept %v (err=%v), want zqbare:\n%s", got, run.err, run.output)
		}
		text, textErr := e2eFilterText(t, "-file:zqup -lang:go", ".")
		if text != run.output || errors.Is(textErr, errNoMatch) != errors.Is(run.err, errNoMatch) {
			t.Fatalf("--lexical-only prints another answer (err=%v):\n%s\nwant:\n%s", textErr, text, run.output)
		}
		if dirs := e2eFilterSemanticDirs(t, home); len(dirs) != 0 {
			t.Fatalf("a query of negations only wrote vector generations: %v", dirs)
		}
	})
	for _, test := range []struct {
		query string
		// want holds the files that the strict search keeps; nil keeps none.
		want []string
		// filters is the filter part of the query; the search prints every
		// live file that it keeps. Empty: the query has no filter, and the
		// search prints every live file.
		filters string
	}{
		{"zqalpha", nil, ""},
		{"zqalpha lang:go -file:loose", nil, "lang:go -file:loose"},
		{`"zqalpha zqbeta"`, nil, ""},
		{"zqalpha or zqbeta", nil, ""},
		{"(zqalpha or zqbeta) lang:go", nil, "lang:go"},
		// The or binds the whole query: (zqalpha zqbeta file:zqlow) or
		// file:other.
		{"zqalpha zqbeta file:zqlow or file:other", []string{"other.go"}, "file:zqlow or file:other"},
		{"zqmark -lang:go", []string{"new.py", "loose_test.py", "zqbare"}, "-lang:go"},
		{"lang:go -file:_test", []string{"clean.go", "loose.go", "other.go"}, "lang:go -file:_test"},
		{"file:ZqUp", []string{"clean.go", "new.py", "loose_test.py"}, "file:ZqUp"},
	} {
		t.Run(test.query, func(t *testing.T) {
			run := e2eFilterSearch(t, test.query, ".")
			if run.model.prepares.Load() != 1 {
				t.Fatalf("model queries=%d, want 1", run.model.prepares.Load())
			}
			text, textErr := e2eFilterText(t, test.query, ".")
			want := slices.Sorted(slices.Values(test.want))
			if strict := e2eFilterNames(text); !slices.Equal(strict, want) || (len(want) == 0) != errors.Is(textErr, errNoMatch) {
				t.Fatalf("strict search kept %v (err=%v), want %v:\n%s", strict, textErr, want, text)
			}
			got := e2eFilterNames(run.output)
			for _, name := range want {
				if !slices.Contains(got, name) {
					t.Fatalf("the model route dropped the strict file %s:\n%s", name, run.output)
				}
			}
			allowed := slices.Sorted(slices.Values(e2eFilterPathLive))
			if test.filters != "" {
				allowedText, _ := e2eFilterText(t, test.filters, ".")
				allowed = e2eFilterNames(allowedText)
			}
			if !slices.Equal(got, allowed) {
				t.Fatalf("the model route printed %v, want every live file that %q keeps %v:\n%s",
					got, test.filters, allowed, run.output)
			}
		})
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
// -file: filter. The model reads the 130 words once, in order, with no filter
// text, and with a model that accepts every row, the results are the files
// that the filter keeps, on every node kind. The test proves that a long
// description reaches the model whole and that its filters stay out; it
// proves nothing about how a long real query ranks files.
func TestE2EFilterLongQueryKeepsEveryTerm(t *testing.T) {
	requireTools(t)
	e2eFilterPathTree(t)
	words := make([]string, 130)
	for index := range words {
		words[index] = fmt.Sprintf("zqw%03d", index)
	}
	description := strings.Join(words, " ")
	run := e2eFilterSearch(t, description+" -file:_test", ".")
	if run.err != nil {
		t.Fatalf("%v\n%s", run.err, run.output)
	}
	if got := run.modelQueries(); len(got) != 1 || got[0] != description {
		t.Fatalf("the model read %q, want the 130 words", got)
	}
	want := []string{"clean.go", "loose.go", "new.py", "other.go", "zqbare"}
	if got := e2eFilterNames(run.output); !slices.Equal(got, want) {
		t.Fatalf("vector search printed %v, want %v:\n%s", got, want, run.output)
	}
	for _, name := range run.scoredNames() {
		if !slices.Contains(want, name) {
			t.Fatalf("the model scored %s, which -file:_test drops", name)
		}
	}
}

// TestE2EFilterTypeFilePrintsNamesOnly runs descriptions with type:file and
// its spellings, alone and with filters. Vector search, with a model that
// accepts every row, finds each file that the filters keep, and the search
// prints only the name line of each file, as the text oracle does.
// A negated -type:file fails the search of each node: Seek warns once for
// each node and finds nothing.
func TestE2EFilterTypeFilePrintsNamesOnly(t *testing.T) {
	requireTools(t)
	e2eFilterContentTree(t)
	for _, row := range []e2eFilterRow{
		{"type:file", []string{"clean.go", "mod.go", "new.go", "loose.go", "other.go"}},
		{"t:file content:zqdrop", []string{"mod.go", "new.go", "other.go"}},
		{"type:filename -branch:HEAD", []string{"mod.go", "new.go", "loose.go"}},
		{"t:filename repo:" + e2eFilterOtherRepo(t), []string{"other.go"}},
		{"t:filename repo:zqnamed", nil},
	} {
		t.Run(row.filter, func(t *testing.T) {
			run := e2eFilterExpect(t, row)
			if before, _, _ := strings.Cut(run.output, "## "); strings.TrimSpace(before) != "" {
				t.Fatalf("%q prints before the first result:\n%s", before, run.output)
			}
			for _, result := range printedResults(run.output) {
				for _, line := range result.body {
					if line != "" && line != "0 "+result.path {
						t.Fatalf("line %q is no name line of %s:\n%s", line, result.path, run.output)
					}
				}
			}
		})
	}
	t.Run("-type:file", func(t *testing.T) {
		logs := captureTestLogs(t, slog.LevelWarn)
		run := e2eFilterSearch(t, "zqalpha zqbeta -type:file", ".")
		if !errors.Is(run.err, errNoMatch) || run.output != "" {
			t.Fatalf("err=%v, want no match:\n%s", run.err, run.output)
		}
		if names := run.scoredNames(); len(names) != 0 {
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
// Zoekt's per-shard searcher panics on these atoms (an upstream bug). Seek
// recovers the panic at the shard: the node is skipped with one warning that
// names the panic, and with no other node the search prints nothing and ends
// with exit code 1. The type: rows keep strict ranking, so the search writes
// no vectors.
func TestE2EFilterUpstreamPanicForms(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.go", "package sample\n// zqmark\n")
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
			if result.code != 1 || result.stdout != "" ||
				strings.Count(result.stderr, "Skipped a search node") != 1 ||
				!strings.Contains(result.stderr, "shard search panicked") {
				t.Fatalf("code=%d stdout=%q, want one skipped-node warning for the shard panic:\n%s",
					result.code, result.stdout, result.stderr)
			}
			if !test.strict {
				return
			}
			// The model route builds the vectors of a node before it reads the
			// node, so a panic on that route leaves a vector generation.
			if dirs := e2eFilterSemanticDirs(t, cache); len(dirs) != 0 {
				t.Fatalf("the search wrote vector generations: %v", dirs)
			}
		})
	}
}
