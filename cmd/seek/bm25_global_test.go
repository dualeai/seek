package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// TestBM25LayoutIndependent searches the same files as one folder node and as
// three folder nodes of unequal file lengths. One global average file length
// gives both the same strict order.
func TestBM25LayoutIndependent(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	for part, lines := range map[string]int{"a": 1, "b": 60, "c": 15} {
		for index := range 6 {
			var body strings.Builder
			body.WriteString("package p\n")
			for range index + 1 {
				body.WriteString("func F() int { return 1 }\n")
			}
			for line := range lines * (index + 1) {
				fmt.Fprintf(&body, "// filler %d\n", line)
			}
			writeFixtureFile(t, f.path(fmt.Sprintf("tree/%s/f%d.go", part, index)), body.String())
		}
	}
	setTestUserCache(t)
	t.Chdir(f.root)
	// type:file ranks by Zoekt's whole-name candidate.
	for _, query := range []string{"case:yes func", "case:yes return", "type:file case:yes func"} {
		_, one := searchHeaders(t, query, "tree")
		_, three := searchHeaders(t, query, "tree/a", "tree/b", "tree/c")
		if len(one) != 18 || !slices.Equal(one, three) {
			t.Fatalf("%s: one node %v\nthree nodes %v", query, one, three)
		}
	}
}

// TestParseZoektBM25Debug reads Zoekt's debug line and refuses a boosted one.
func TestParseZoektBM25Debug(t *testing.T) {
	for debug, want := range map[string][4]any{
		"bm25-score: 1.61 (low-priority: false) <- sum-termFrequencies: 4, length-ratio: 3.53":           {false, 4, 3.53, true},
		"bm25-score: 0.00 (low-priority: true) <- sum-termFrequencies: 0, length-ratio: 0.18":            {true, 0, 0.18, true},
		"bm25-score: 2.00 (low-priority: false) <- sum-termFrequencies: 3, length-ratio: 1.00 (boosted)": {false, 0, 0.0, false},
		"score: 12 (repo-rank: 0, file-rank: 0.5)":                                                       {false, 0, 0.0, false},
	} {
		low, sum, ratio, ok := parseZoektBM25Debug(debug)
		if ok != want[3].(bool) || (ok && (low != want[0].(bool) || sum != want[1].(int) || ratio != want[2].(float64))) {
			t.Errorf("%q: got %t %d %v %t, want %v", debug, low, sum, ratio, ok, want)
		}
	}
}

// TestBM25NameAtomUnicodeName matches a file-name atom in a name whose lower
// case is longer in bytes: no panic, one term.
func TestBM25NameAtomUnicodeName(t *testing.T) {
	atom := compileBM25Query(&query.Substring{Pattern: "zqfoo", FileName: true}).names[0]
	if got := atom.matches("ȺȺȺzqfoo.go"); !slices.Equal(got, []string{"zqfoo"}) {
		t.Fatalf("matches=%q", got)
	}
}

// bm25ContextFixture writes 40 short files to short. To long it writes b1.go,
// 52 bytes with one "foo" between context lines, and b2.go, 200 bytes with
// three. Beside b2.go alone, or with b1.go only, b2.go ranks first; under the
// average of all 42 files, b1.go does.
func bm25ContextFixture(t *testing.T, short, long string) {
	t.Helper()
	for index := range 40 {
		writeFixtureFile(t, filepath.Join(short, fmt.Sprintf("a%02d.txt", index)), "bar\n")
	}
	writeFixtureFile(t, filepath.Join(long, "b1.go"), "zqline1\nzqline2\nzqline3\nfoo\nzqline5\nzqline6\nzqline7\n")
	writeFixtureFile(t, filepath.Join(long, "b2.go"), "foo foo foo\n"+strings.Repeat("x\n", 94))
}

// TestBM25ContextFollowsTheFinalScore runs -n 1 on files that the rescore
// reorders. The printed file keeps its context lines in a command of two
// nodes, and in one Git node with a committed and an uncommitted shard.
func TestBM25ContextFollowsTheFinalScore(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	check := func(name string, operands ...string) {
		t.Helper()
		out, err := seek(t, slices.Concat([]string{"--lexical-only", "-n", "1", "-m", "3", "case:yes foo"}, operands)...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(out, "b1.go") || !strings.Contains(out, "zqline3") {
			t.Fatalf("%s: want b1.go with its context lines:\n%s", name, out)
		}
	}
	root := t.TempDir()
	bm25ContextFixture(t, filepath.Join(root, "a"), filepath.Join(root, "b"))
	check("two nodes", filepath.Join(root, "a"), filepath.Join(root, "b"))

	repo := filepath.Join(t.TempDir(), "repo")
	bm25ContextFixture(t, filepath.Join(repo, "a"), filepath.Join(repo, "b"))
	initEmptyGitRepoNoRemoteAt(t, repo)
	// b1.go shares the committed shard with the short files; b2.go is alone
	// in the uncommitted shard.
	gitRunIn(t, repo, "add", "a", "b/b1.go")
	gitRunIn(t, repo, "commit", "-q", "-m", "fixture")
	check("one Git node", repo)
}

// TestScopeAddsNoScoreTerm searches a Git repository whole and scoped to sub:
// the scope adds no file-name term, so the files of sub keep their order.
func TestScopeAddsNoScoreTerm(t *testing.T) {
	requireTools(t)
	repo := filepath.Join(t.TempDir(), "repo")
	writeFixtureFile(t, filepath.Join(repo, "sub/a.go"), "foo\n"+strings.Repeat("line\n", 10))
	writeFixtureFile(t, filepath.Join(repo, "sub/b.go"), "foo foo\n"+strings.Repeat("line\n", 20))
	for index := range 10 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("other%02d.txt", index)), "bar\n")
	}
	initEmptyGitRepoNoRemoteAt(t, repo)
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "fixture")
	setTestUserCache(t)
	order := func(operand string) []string {
		t.Helper()
		out, err := seek(t, "--lexical-only", "-n", "10", "-m", "3", "case:yes foo", operand)
		if err != nil {
			t.Fatalf("%s: %v", operand, err)
		}
		var files []string
		for _, result := range printedResults(out) {
			files = append(files, filepath.Base(result.path))
		}
		return files
	}
	whole, scoped := order(repo), order(filepath.Join(repo, "sub"))
	if len(whole) != 2 || !slices.Equal(whole, scoped) {
		t.Fatalf("whole %v, scoped %v; want one order", whole, scoped)
	}
}

// TestBM25NameAtomsSkipNegations compiles no file-name atom under a Not, as
// Zoekt gathers none there: the double negation of a scope adds no term.
func TestBM25NameAtomsSkipNegations(t *testing.T) {
	scope, err := query.RegexpQuery("^sub/", false, true)
	if err != nil {
		t.Fatal(err)
	}
	content := &query.Substring{Pattern: "foo", Content: true}
	name := &query.Substring{Pattern: "foo", FileName: true}
	q := query.NewAnd(query.NewOr(content, name), &query.Not{Child: &query.Not{Child: scope}})
	if atoms := compileBM25Query(q).names; len(atoms) != 1 || atoms[0].substring != "foo" {
		t.Fatalf("atoms=%+v, want the one positive file-name atom", atoms)
	}
}

// bm25NodeScores runs pattern as a strict search over the nodes of operands,
// with the command's rescore, and returns each file's score by path.
func bm25NodeScores(t *testing.T, pattern string, operands ...string) map[string]float64 {
	t.Helper()
	scores, _ := bm25NodeSearch(t, pattern, operands...)
	return scores
}

// bm25NodeSearch runs the strict route of runSearchCommand up to the display:
// it searches the nodes of operands with searchNodes and strictNode, then
// rescores the hits. The output hides the scores, so the test reads them
// here. It returns each file's score by path and the command's BM25 state.
// TestBM25LayoutIndependent and TestBM25ContextFollowsTheFinalScore check
// through seek that runSearchCommand runs the same route.
func bm25NodeSearch(t *testing.T, pattern string, operands ...string) (map[string]float64, *bm25Command) {
	t.Helper()
	ctx := withSkippedNodes(context.Background())
	plans, err := planCorpora(ctx, operands)
	if err != nil {
		t.Fatal(err)
	}
	ctx = withIndexWindow(ctx, len(plans))
	qp, err := planQuery(pattern, lexicalOnlySearch)
	if err != nil {
		t.Fatal(err)
	}
	execution := searchExecution{
		policy:    lexicalOnlySearch,
		resources: liveSearchResources(),
		opened:    &openedNodes{},
		// The operands give the headers that file: atoms read, as in a search.
		operands: newDisplayOperands(operands),
	}
	ctx, command := withBM25Command(ctx, len(plans))
	all, _, err := searchNodes(ctx, plans, func(nodeCtx context.Context, plan node) ([]corpusSearchResult, dirtyFileSet, error) {
		return strictNode(nodeCtx, plan, qp, defaultSearchConfig(), execution)
	})
	if err != nil {
		t.Fatalf("%s: %v", pattern, err)
	}
	rescoreStrictResults(all, command)
	scores := map[string]float64{}
	for _, result := range all {
		scores[filepath.Join(result.displayRoot, result.file.FileName)] = result.file.Score
	}
	return scores, command
}

// TestBM25RescoreIsExact searches each query shape as two nodes: each hit must
// get the score that Zoekt gives it in one node of one shard. The rebuild keys
// every candidate as Zoekt does:
//   - regexps share one key;
//   - a regexp equal to substrings takes their keys;
//   - short atoms are regexps.
//
// It also drops overlapping file-name candidates and counts a multi-line
// candidate once.
func TestBM25RescoreIsExact(t *testing.T) {
	requireTools(t)
	root := t.TempDir()
	write := func(rel, body string) { writeFixtureFile(t, filepath.Join(root, rel), body) }
	for index := range 12 {
		write(fmt.Sprintf("a/short%02d.txt", index), "bar\n")
	}
	write("a/foo1.go", "package a\n// foax and fobx and foo\nfoo bar\nfoo\nbar end\n")
	write("a/foo2.go", "package a\n// foax foax\nfoo foo foo\n")
	write("b/foo3.go", "package b\n// fobx foax foo\nfoo\nbar\n"+strings.Repeat("// long filler line for the length\n", 30))
	write("b/foo4.go", "package b\n// foo\nfoo bar foo\nbar\n"+strings.Repeat("// another filler line\n", 12))
	setTestUserCache(t)
	// Use relative operands. file: matches the printed path, and the macOS
	// temporary folder path (/var/folders/...) contains "fo".
	t.Chdir(root)
	for _, pattern := range []string{
		"case:yes foo",
		"case:yes foo bar",
		"case:yes content:fo[a-z]x",
		"case:yes content:foax|fobx",
		"case:yes content:(foax)+",
		"case:yes content:foax|bar",
		"case:yes fo",
		"case:yes foo file:foo",
		"case:yes foo file:fo",
		"case:yes (?s)foo.bar",
		"case:yes content:foo\\nbar",
		"type:file case:yes foo",
	} {
		one := bm25NodeScores(t, pattern, ".")
		two := bm25NodeScores(t, pattern, "a", "b")
		if len(one) == 0 || len(one) != len(two) {
			t.Fatalf("%s: one node %d hits, two nodes %d", pattern, len(one), len(two))
		}
		for path, want := range one {
			if got, ok := two[path]; !ok || math.Abs(got-want) > 1e-9*max(1, math.Abs(want)) {
				t.Errorf("%s: %s scores %v in two nodes, want %v", pattern, path, got, want)
			}
		}
	}
}

// These are Zoekt's scores of the hand-made hits below, worked out by hand
// from its tfScore 2.2f / (1.2(0.25 + 0.75L) + f) (index/score.go). The
// rebuild must match Zoekt, not its own formula.
const (
	// The line "func bar(foo int)\n" at L = 0.18 has one term on a symbol
	// (f = 5) and one not (f = 1). Its score is 11/5.462 + 2.2/1.462.
	bm25SymbolLineScore = 3.51870227879627
	// The line "bar\n" (L = 0.04) with f = 1: 2.2/1.336.
	bm25BarLineScore = 1.6467065868263473
)

// TestBM25LineSplitAmbiguity rebuilds a hit with a line where either of two
// terms can be the symbol, so the line gives two splits. The printed length
// ratio of the file picks the split that Zoekt counted. A ratio that no split
// fits keeps Zoekt's score.
func TestBM25LineSplitAmbiguity(t *testing.T) {
	bq := compileBM25Query(query.NewAnd(
		&query.Substring{Pattern: "bar", Content: true, CaseSensitive: true},
		&query.Substring{Pattern: "foo", Content: true, CaseSensitive: true},
	))
	line := zoekt.LineMatch{
		Line:  []byte("func bar(foo int)\n"),
		Score: bm25SymbolLineScore,
		LineFragments: []zoekt.LineFragmentMatch{
			{LineOffset: 5, Offset: 5, MatchLength: 3},
			{LineOffset: 9, Offset: 9, MatchLength: 3},
		},
	}
	if splits, ok := bq.lineFrequencies(line, nil); !ok || len(splits) != 2 {
		t.Fatalf("splits=%v ok=%t, want both splits", splits, ok)
	}
	// Zoekt counted bar on the symbol: bar 5 + 1, foo 1. At L = 1.37:
	// 13.2/7.533 + 2.2/2.533.
	const ratio, score = 1.37, 2.620825257929461
	file := func(printed float64) zoekt.FileMatch {
		return zoekt.FileMatch{
			FileName: "x.go",
			Score:    score,
			Debug:    fmt.Sprintf("bm25-score: 0 (low-priority: false) <- sum-termFrequencies: 7, length-ratio: %.2f", printed),
			LineMatches: []zoekt.LineMatch{line, {
				Line:          []byte("bar\n"),
				Score:         bm25BarLineScore,
				LineFragments: []zoekt.LineFragmentMatch{{LineOffset: 0, Offset: 18, MatchLength: 3}},
			}},
		}
	}
	match := file(ratio)
	if !bm25Parts(&match, bq, 100) || !strings.HasSuffix(match.Debug, " 1,6") {
		t.Fatalf("debug=%q, want the split that the ratio fits", match.Debug)
	}
	// Under the same average the rescore gives Zoekt's score back.
	if got, ok := bm25Rescore(match.Debug, 100); !ok || math.Abs(got-score) > 1e-9 {
		t.Fatalf("rescore=%v ok=%t, want %v", got, ok, score)
	}
	off := file(ratio + 1)
	if bm25Parts(&off, bq, 100) {
		t.Fatalf("a ratio that no split fits gave %q", off.Debug)
	}
}

// TestBM25MultiLineKeysFollowTheLines gives a candidate that spans two lines,
// with the lines in Zoekt's score order, not in file order. Each piece of the
// candidate takes its key at its own place in the line order. The other
// fragments keep their own keys, and the candidate counts once.
func TestBM25MultiLineKeysFollowTheLines(t *testing.T) {
	bq := compileBM25Query(query.NewAnd(
		&query.Substring{Pattern: "nil\n}", Content: true, CaseSensitive: true},
		&query.Substring{Pattern: "foo", Content: true, CaseSensitive: true},
	))
	// The file is "x foo nil\n} foo\n": "nil" ends line 1, "}" starts line 2.
	lines := []zoekt.LineMatch{
		{Line: []byte("} foo\n"), LineFragments: []zoekt.LineFragmentMatch{
			{LineOffset: 0, Offset: 10, MatchLength: 1},
			{LineOffset: 2, Offset: 12, MatchLength: 3},
		}},
		{Line: []byte("x foo nil\n"), LineFragments: []zoekt.LineFragmentMatch{
			{LineOffset: 2, Offset: 2, MatchLength: 3},
			{LineOffset: 6, Offset: 6, MatchLength: 3},
		}},
	}
	keys, merges, ok := bq.multiLineMerges(lines)
	if want := []string{"nil\n}", "foo", "foo", "nil\n}"}; !ok || !slices.Equal(keys, want) {
		t.Fatalf("keys=%q ok=%t, want %q", keys, ok, want)
	}
	if len(merges) != 1 || merges["nil\n}"] != -1 {
		t.Fatalf("merges=%v, want the candidate counted once", merges)
	}
}

// TestBM25ShardCountsIgnoreTombstones edits a dirty file, so a delta shard
// tombstones it in the older uncommitted shard. A fresh listing of that shard
// still counts the tombstoned file, as Zoekt's average does. A record entry of
// another shard file with the same name and size makes the search list the
// shard again.
func TestBM25ShardCountsIgnoreTombstones(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "seed.go", "package seed\n")
	writeFixtureFile(t, filepath.Join(repo, "xxxxxxxxxx.go"), "package x\n// zqtomb first version\n")
	writeFixtureFile(t, filepath.Join(repo, "y.go"), "package y\n// zqtomb\n")
	setTestUserCache(t)
	search := func() {
		t.Helper()
		if _, headers := searchHeaders(t, "case:yes zqtomb", repo); len(headers) != 2 {
			t.Fatalf("headers=%v", headers)
		}
	}
	plans, err := planCorpora(context.Background(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(plans[0].cacheDir, bm25StatsFile)
	read := func() map[string]shardLengthStats {
		t.Helper()
		data, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatal(err)
		}
		var shards map[string]shardLengthStats
		if err := json.Unmarshal(data, &shards); err != nil {
			t.Fatal(err)
		}
		return shards
	}
	search()
	before := read()
	writeFixtureFile(t, filepath.Join(repo, "xxxxxxxxxx.go"), "package x\n// zqtomb second one\n")
	search()
	metas, _ := filepath.Glob(filepath.Join(plans[0].indexDir, "*.zoekt.meta"))
	tombstoned := ""
	for _, meta := range metas {
		if data, err := os.ReadFile(meta); err == nil && strings.Contains(string(data), "xxxxxxxxxx.go") {
			tombstoned = strings.TrimSuffix(filepath.Base(meta), ".meta")
		}
	}
	if tombstoned == "" {
		t.Fatalf("no shard tombstones the edited file: %v", metas)
	}
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	search()
	after := read()
	want, got := before[tombstoned], after[tombstoned]
	if want.Docs == 0 || got.Docs != want.Docs || got.Content != want.Content {
		t.Fatalf("shard %s: counts %+v after the tombstone, want %+v", tombstoned, got, want)
	}
	// Another shard file with the same name and size: the search lists the
	// shard again when the record gives a bogus count with another inode.
	bogus := after
	entry := bogus[tombstoned]
	entry.Content, entry.Ino = 1, entry.Ino+1
	bogus[tombstoned] = entry
	data, err := json.Marshal(bogus)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	search()
	if again := read()[tombstoned]; again.Content != want.Content {
		t.Fatalf("a record entry of another file was kept: %+v, want content %d", again, want.Content)
	}
}

// TestBM25NameCandidateEdges checks two edges of the file-name candidates.
// When two atoms with different keys match one name range, both keys stay as
// options: Zoekt's unstable sort keeps either, and the ratio picks one. The
// offsets of a non-ASCII name come from the name itself.
func TestBM25NameCandidateEdges(t *testing.T) {
	regexp, err := query.RegexpQuery("fo+", true, true)
	if err != nil {
		t.Fatal(err)
	}
	bq := compileBM25Query(query.NewOr(&query.Substring{Pattern: "foo", FileName: true, CaseSensitive: true}, regexp))
	if lists, ok := bq.nameCandidates("a/foo.go"); !ok || len(lists) != 2 || !slices.ContainsFunc(lists, func(keys []string) bool { return slices.Equal(keys, []string{"foo"}) }) ||
		!slices.ContainsFunc(lists, func(keys []string) bool { return slices.Equal(keys, []string{""}) }) {
		t.Fatalf("a tie between two keys gave %q ok=%t, want both keys as options", lists, ok)
	}
	atom := compileBM25Query(&query.Substring{Pattern: "foo", FileName: true}).names[0]
	found, ok := atom.candidates("ȺȺfooK.go")
	if !ok || len(found) != 1 || found[0].at != len("ȺȺ") {
		t.Fatalf("candidates=%+v ok=%t, want one at byte %d", found, ok, len("ȺȺ"))
	}
}

// TestBM25InPlaceAverageReadsItsOwnShards searches one node of two shards in
// a one-node command: the search scores each hit in place with the average of
// the node's shards. The expected scores are Zoekt's own, from the same files
// in one node of one shard.
func TestBM25InPlaceAverageReadsItsOwnShards(t *testing.T) {
	requireTools(t)
	root := t.TempDir()
	for _, dir := range []string{"split", "whole"} {
		writeFixtureFile(t, filepath.Join(root, dir, "a.go"), "package a\nfoo\n"+strings.Repeat("// filler\n", 20))
		writeFixtureFile(t, filepath.Join(root, dir, "b.go"), "package a\nfoo foo\n")
	}
	setTestUserCache(t)
	shardCount := func(operand string) int {
		t.Helper()
		plans, err := planCorpora(context.Background(), []string{operand})
		if err != nil {
			t.Fatal(err)
		}
		shards, _ := filepath.Glob(filepath.Join(plans[0].indexDir, "*.zoekt"))
		return len(shards)
	}
	exact := map[string]float64{}
	wholeScores, _ := bm25NodeSearch(t, "case:yes foo", filepath.Join(root, "whole"))
	for path, score := range wholeScores {
		exact[filepath.Base(path)] = score
	}
	if shards := shardCount(filepath.Join(root, "whole")); shards != 1 {
		t.Fatalf("whole: %d shards, want 1", shards)
	}
	// whole has one shard, so Zoekt's scores are final and no rescore runs.
	// From here each file of split gets its own shard.
	swapForTest(t, &shardMax, 64)
	split, command := bm25NodeSearch(t, "case:yes foo", filepath.Join(root, "split"))
	if shards := shardCount(filepath.Join(root, "split")); shards < 2 {
		t.Fatalf("split: %d shards, want 2 or more", shards)
	}
	if !command.final.Load() || len(split) != 2 || len(exact) != 2 {
		t.Fatalf("final=%t hits=%d expected=%d, want an in-place rescore of 2 hits", command.final.Load(), len(split), len(exact))
	}
	for path, score := range split {
		name := filepath.Base(path)
		if want, ok := exact[name]; !ok || math.Abs(score-want) > 1e-9*max(1, want) {
			t.Errorf("%s: score %v, want Zoekt's %v", name, score, want)
		}
	}
}

// TestBM25FileAtomTermInAnyLayout searches with explicit file: atoms as one
// node whose headers are its index paths and as two nodes whose headers are
// not. In both, Zoekt scores the atom on the index path. The one node scores
// it directly, and the two nodes through the clause atom-or-not-atom beside
// the header set. So the scores are the same, also for filter-only queries.
func TestBM25FileAtomTermInAnyLayout(t *testing.T) {
	requireTools(t)
	root := t.TempDir()
	write := func(rel, body string) { writeFixtureFile(t, filepath.Join(root, rel), body) }
	for index := range 12 {
		write(fmt.Sprintf("a/short%02d.txt", index), "bar\n")
	}
	write("a/foo1.go", "package a\nfoo\n"+strings.Repeat("// filler\n", 3))
	write("a/foo2.go", "package a\nfoo foo\n"+strings.Repeat("// filler\n", 40))
	write("b/foo3.go", "package b\nfoo\n"+strings.Repeat("// filler\n", 10))
	write("b/foo4.go", "package b\nfoo foo foo\n"+strings.Repeat("// filler\n", 80))
	setTestUserCache(t)
	t.Chdir(root)
	// The or row holds a file: atom in a branch that does not match some
	// hits: Zoekt counts it only where the branch matched.
	for _, pattern := range []string{"case:yes foo file:foo", "case:yes foo file:go", "file:foo", "file:go", "(case:yes bar file:short) or case:yes foo"} {
		one := bm25NodeScores(t, pattern, ".")
		two := bm25NodeScores(t, pattern, "a", "b")
		if len(one) < 4 || len(one) != len(two) {
			t.Fatalf("%s: hits %d and %d", pattern, len(one), len(two))
		}
		for path, want := range one {
			if got, ok := two[path]; !ok || math.Abs(got-want) > 1e-9*max(1, math.Abs(want)) {
				t.Errorf("%s: %s scores %v in two nodes, %v in one", pattern, path, got, want)
			}
		}
	}
}

// TestBM25ManySplitsAndLongLines rebuilds two hits. In the first, each of 13
// lines allows two symbol splits: 8,192 combinations and 14 distinct sets. The
// second has one line with 20,000 matches of one term.
func TestBM25ManySplitsAndLongLines(t *testing.T) {
	bq := compileBM25Query(query.NewAnd(
		&query.Substring{Pattern: "bar", Content: true, CaseSensitive: true},
		&query.Substring{Pattern: "foo", Content: true, CaseSensitive: true},
	))
	text := []byte("func bar(foo int)\n")
	var lines []zoekt.LineMatch
	offset := uint32(0)
	for range 13 {
		lines = append(lines, zoekt.LineMatch{
			Line:  text,
			Score: bm25SymbolLineScore,
			LineFragments: []zoekt.LineFragmentMatch{
				{LineOffset: 5, Offset: offset + 5, MatchLength: 3},
				{LineOffset: 9, Offset: offset + 9, MatchLength: 3},
			},
		})
		offset += uint32(len(text))
	}
	// One more "bar" breaks the symmetry of the two splits.
	lines = append(lines, zoekt.LineMatch{
		Line: []byte("bar\n"), Score: bm25BarLineScore,
		LineFragments: []zoekt.LineFragmentMatch{{LineOffset: 0, Offset: offset, MatchLength: 3}},
	})
	// Zoekt counted bar on the symbol in every line: bar 13*5 + 1, foo 13. At
	// L = 2.41: 145.2/68.469 + 28.6/15.469.
	const ratio = 2.41
	match := zoekt.FileMatch{
		FileName:    "x.go",
		Score:       3.969526755787009,
		Debug:       fmt.Sprintf("bm25-score: 0 (low-priority: false) <- sum-termFrequencies: 79, length-ratio: %.2f", ratio),
		LineMatches: lines,
	}
	if !bm25Parts(&match, bq, 100) || !strings.HasSuffix(match.Debug, " 13,66") {
		t.Fatalf("13 ambiguous lines: debug=%q", match.Debug)
	}

	long := []byte(strings.Repeat("foo ", 20000) + "\n")
	fragments := make([]zoekt.LineFragmentMatch, 20000)
	for index := range fragments {
		fragments[index] = zoekt.LineFragmentMatch{LineOffset: 4 * index, Offset: uint32(4 * index), MatchLength: 3}
	}
	single := compileBM25Query(&query.Substring{Pattern: "foo", Content: true, CaseSensitive: true})
	// The line holds 80,001 bytes (L = 800.01): 44000/20720.309. The file at
	// L = 1.5: 44000/20001.65.
	longMatch := zoekt.FileMatch{
		FileName:    "long.txt",
		Score:       2.199818514972515,
		Debug:       "bm25-score: 0 (low-priority: false) <- sum-termFrequencies: 20000, length-ratio: 1.50",
		LineMatches: []zoekt.LineMatch{{Line: long, Score: 2.1235204552210103, LineFragments: fragments}},
	}
	if !bm25Parts(&longMatch, single, 100) || !strings.HasSuffix(longMatch.Debug, " 20000") {
		t.Fatalf("a long one-term line: debug=%q", longMatch.Debug)
	}
}

// TestBM25FileAtomTermBelowTheCwd searches a Git repository from its root and
// from a subfolder, whose headers drop the subfolder. A file: atom gives the
// files of the subfolder the same scores in both.
func TestBM25FileAtomTermBelowTheCwd(t *testing.T) {
	requireTools(t)
	repo := filepath.Join(t.TempDir(), "repo")
	for index := range 8 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("other/x%02d.txt", index)), "bar\n")
	}
	writeFixtureFile(t, filepath.Join(repo, "sub/foo/foo1.go"), "package a\nfoo\n"+strings.Repeat("// filler\n", 3))
	writeFixtureFile(t, filepath.Join(repo, "sub/foo2.go"), "package a\nfoo foo\n"+strings.Repeat("// filler\n", 40))
	writeFixtureFile(t, filepath.Join(repo, "sub/OWNERS"), "foo\n")
	initEmptyGitRepoNoRemoteAt(t, repo)
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "fixture")
	setTestUserCache(t)
	for _, pattern := range []string{"file:foo", "case:yes foo file:foo"} {
		t.Chdir(repo)
		root := bm25NodeScores(t, pattern, ".")
		t.Chdir(filepath.Join(repo, "sub"))
		below := bm25NodeScores(t, pattern, ".")
		if len(below) == 0 {
			t.Fatalf("%s: no hit below the cwd", pattern)
		}
		for path, got := range below {
			if want, ok := root[path]; !ok || math.Abs(got-want) > 1e-9*max(1, math.Abs(want)) {
				t.Errorf("%s: %s scores %v below the cwd, %v from the root", pattern, path, got, want)
			}
		}
	}
}

// TestEqualScoresSortByName searches eight files with the same text in one
// shard: their true scores are equal, so every run prints them in name order.
func TestEqualScoresSortByName(t *testing.T) {
	requireTools(t)
	root := t.TempDir()
	// Unequal term counts and a long file give each term a different
	// weight, so the sum order changes the last bits.
	text := strings.Repeat("alpha ", 2) + "\n" + strings.Repeat("beta ", 5) + "\n" +
		strings.Repeat("gamma ", 11) + "\n" + strings.Repeat("delta ", 13) + "\nfiller line\n"
	var want []string
	for index := range 8 {
		name := fmt.Sprintf("f%d.txt", index)
		writeFixtureFile(t, filepath.Join(root, name), text)
		want = append(want, name)
	}
	writeFixtureFile(t, filepath.Join(root, "zz_long.txt"), strings.Repeat("x", 3000)+"\n")
	setTestUserCache(t)
	t.Chdir(root)
	for run := range 8 {
		if _, headers := searchHeaders(t, "case:yes alpha beta gamma delta", "."); !slices.Equal(headers, want) {
			t.Fatalf("run %d: %v, want name order", run, headers)
		}
	}
}

// matches returns the term keys of the atom's matches in name.
func (atom bm25NameAtom) matches(name string) []string {
	found, _ := atom.candidates(name)
	keys := make([]string, len(found))
	for index, candidate := range found {
		keys[index] = candidate.key
	}
	return keys
}
