package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The tests of this file drive runSearchCommand with descriptions whose words
// (zqalpha zqbeta) no file holds, so every result comes from vector search.
// They read the semantic route of each node from the "Searched semantic
// index" debug record.

// e2eRouteMarker puts a row near the query: the model maps a row whose text
// holds it to a vector near the first query axis.
const e2eRouteMarker = "SEMANTIC_TARGET"

// e2eRouteWindowWords is the number of query words that one 128-token query
// window holds in this file's model. A longer query has a second window.
const e2eRouteWindowWords = 125

// e2eRouteModel is the test model of this file. Each row gets its own unit
// vectors from its path, start, and text, so no two rows share a vector. A
// row whose text holds SEMANTIC_TARGET is near query axis 0, with a cosine
// from 0.85 to 0.95. Any other row is orthogonal to both query axes. Text
// markers change a near row:
//   - E2E_FINE_dddd sets the cosine of the fine vectors to dddd/10000; the
//     coarse vectors keep their cosine;
//   - E2E_AXIS1 puts the coarse vectors near query axis 1, the first fine
//     vector near axis 0, and the other fine vectors near axis 1;
//   - E2E_MODEL_ddd sets the model score of the document to ddd; a document
//     with SEMANTIC_TARGET and no such marker scores 100, any other 0.
//
// A query of more than e2eRouteWindowWords words has two 128-token windows,
// as the real model makes for a long query. The scoring token of window 0 is
// on axis 0, and the scoring token of window 1 is on axis 1.
type e2eRouteModel struct {
	mu        sync.Mutex
	documents []rerankDocument
}

func (*e2eRouteModel) EmbedSemanticUnits(_ context.Context, units []semanticUnit) ([]semanticUnitEmbedding, error) {
	embeddings := make([]semanticUnitEmbedding, len(units))
	for index, unit := range units {
		embedding, err := e2eRouteEmbedding(unit)
		if err != nil {
			return nil, err
		}
		embeddings[index] = embedding
	}
	return embeddings, nil
}

func (*e2eRouteModel) PrepareSemanticQuery(_ context.Context, query string) (*semanticQueryEmbedding, error) {
	windows := 1
	if len(strings.Fields(query)) > e2eRouteWindowWords {
		windows = 2
	}
	prepared := &semanticQueryEmbedding{
		tokens:     make([]float32, windows*lateOnSequenceLength*semanticEmbeddingDimensions),
		scoreMask:  make([]bool, windows*lateOnSequenceLength),
		modelQuery: query,
	}
	for window := range windows {
		token := window * lateOnSequenceLength
		prepared.tokens[token*semanticEmbeddingDimensions+window] = 1
		prepared.scoreMask[token] = true
	}
	return prepared, nil
}

func (model *e2eRouteModel) ScoresWithSemanticQuery(
	_ context.Context,
	_ *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	model.mu.Lock()
	model.documents = append(model.documents, documents...)
	model.mu.Unlock()
	scores := make([]float32, len(documents))
	for index, document := range documents {
		if value, ok := e2eRouteNumber(document.Text, "E2E_MODEL_"); ok {
			scores[index] = float32(value)
		} else if strings.Contains(document.Text, e2eRouteMarker) {
			scores[index] = 100
		}
	}
	return scores, nil
}

func (*e2eRouteModel) SemanticCallCPUs(context.Context) int { return 1 }

func (*e2eRouteModel) Close() error { return nil }

// e2eRouteNumber reads the decimal digits after prefix in text.
func e2eRouteNumber(text, prefix string) (float64, bool) {
	_, rest, ok := strings.Cut(text, prefix)
	if !ok {
		return 0, false
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	value, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return float64(value), true
}

// e2eRouteEmbedding gives one row its coarse and fine vectors (see
// e2eRouteModel).
func e2eRouteEmbedding(unit semanticUnit) (semanticUnitEmbedding, error) {
	hash := fnv.New64a()
	_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%s", unit.path, unit.start, unit.text)
	seed := hash.Sum64()
	text := string(unit.text)
	axis, coarseCosine, fineCosine := -1, 0.0, 0.0
	if strings.Contains(text, e2eRouteMarker) {
		axis = 0
		coarseCosine = 0.85 + 0.1*float64(seed%1000)/1000
		fineCosine = coarseCosine
		if value, ok := e2eRouteNumber(text, "E2E_FINE_"); ok {
			fineCosine = value / 10000
		}
		if strings.Contains(text, "E2E_AXIS1") {
			axis = 1
		}
	}
	var embedding semanticUnitEmbedding
	for centroid := range embedding.coarse {
		vector, err := e2eRouteVector(seed, centroid, axis, coarseCosine)
		if err != nil {
			return semanticUnitEmbedding{}, err
		}
		embedding.coarse[centroid] = vector
	}
	for centroid := range embedding.fine {
		fineAxis := axis
		if axis == 1 && centroid == 0 {
			fineAxis = 0
		}
		vector, err := e2eRouteVector(seed, 100+centroid, fineAxis, fineCosine)
		if err != nil {
			return semanticUnitEmbedding{}, err
		}
		embedding.fine[centroid] = vector
	}
	return embedding, nil
}

// e2eRouteVector returns a unit vector whose cosine with query axis axis is
// cosine, and whose other part is seeded noise on the axes after 1. With axis
// -1 the vector is noise only: orthogonal to both query axes. The noise keeps
// every vector distinct after USearch's int8 cast.
func e2eRouteVector(seed uint64, stream, axis int, cosine float64) (semanticVector, error) {
	random := rand.New(rand.NewPCG(seed, uint64(stream)))
	var noise [semanticEmbeddingDimensions]float64
	var norm float64
	for dimension := 2; dimension < semanticEmbeddingDimensions; dimension++ {
		noise[dimension] = random.NormFloat64()
		norm += noise[dimension] * noise[dimension]
	}
	scale := math.Sqrt(1-cosine*cosine) / math.Sqrt(norm)
	var vector semanticVector
	for dimension := 2; dimension < semanticEmbeddingDimensions; dimension++ {
		vector[dimension] = float32(noise[dimension] * scale)
	}
	if axis >= 0 {
		vector[axis] = float32(cosine)
	}
	return vector, normalizeSemanticVector(&vector)
}

// e2eRouteRun is one search command: its output and its error. backends
// holds the sorted semantic backends of its debug records. saturated counts
// the USearch shards that gave all of their allowed rows. fallback tells
// that a USearch call failed over to exact vectors. scored holds the
// node-relative paths that the model scored.
type e2eRouteRun struct {
	output    string
	err       error
	backends  []string
	saturated int64
	fallback  bool
	scored    []string
}

// e2eRouteSearch runs query on operands with a new e2eRouteModel and reads
// the route from the debug log.
func e2eRouteSearch(t *testing.T, query string, operands ...string) e2eRouteRun {
	t.Helper()
	return e2eRouteSearchWith(t, defaultSearchPolicy(), query, operands...)
}

// e2eRouteSearchWith runs e2eRouteSearch with policy.
func e2eRouteSearchWith(t *testing.T, policy searchPolicy, query string, operands ...string) e2eRouteRun {
	t.Helper()
	logs := captureTestLogs(t, slog.LevelDebug)
	model := &e2eRouteModel{}
	output, err := runHybridTestSearch(t, query, operands, policy,
		func(context.Context) (semanticModel, error) { return model, nil })
	run := e2eRouteRun{output: output, err: err}
	for _, record := range logs.Records() {
		switch record.Message {
		case "Searched semantic index":
			attrs := testLogAttrs(record)
			run.backends = append(run.backends, fmt.Sprint(attrs["backend"]))
			if saturated, ok := attrs["saturated_shards"].(int64); ok {
				run.saturated += saturated
			}
		case "Filtered USearch search failed; using exact vectors",
			"USearch semantic search failed; using exact vectors":
			run.fallback = true
		}
	}
	slices.Sort(run.backends)
	model.mu.Lock()
	for _, document := range model.documents {
		run.scored = append(run.scored, document.Path)
	}
	model.mu.Unlock()
	return run
}

// e2eRouteOracle runs the one-word text search zqnear with filters on
// operands. Zoekt's own rule then gives the near files that the filters and
// the scope keep. The oracle search must read no vector and score nothing.
func e2eRouteOracle(t *testing.T, filters string, operands ...string) []string {
	t.Helper()
	run := e2eRouteSearchWith(t, lexicalOnlySearchPolicy(), strings.TrimSpace("zqnear "+filters), operands...)
	if run.err != nil && !errors.Is(run.err, errNoMatch) {
		t.Fatalf("text oracle %q: %v", filters, run.err)
	}
	if len(run.backends) != 0 || len(run.scored) != 0 {
		t.Fatalf("text oracle %q read vectors %v or scored %v", filters, run.backends, run.scored)
	}
	return headerPaths(run.output)
}

// e2eRouteOrder returns the file path of each result header in print order.
func e2eRouteOrder(output string) []string {
	var paths []string
	for line := range strings.Lines(output) {
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			paths = append(paths, strings.Fields(rest)[0])
		}
	}
	return paths
}

// e2eRouteJoin joins root and each slash path rel, and sorts the result.
func e2eRouteJoin(root string, rels ...string) []string {
	paths := make([]string, len(rels))
	for index, rel := range rels {
		paths[index] = filepath.Join(root, filepath.FromSlash(rel))
	}
	slices.Sort(paths)
	return paths
}

// e2eRouteCount counts the paths that contain fragment.
func e2eRouteCount(paths []string, fragment string) int {
	count := 0
	for _, path := range paths {
		if strings.Contains(path, fragment) {
			count++
		}
	}
	return count
}

// e2eRouteCase is one description search. backends lists the sorted
// semantic backends, one for each node that reads base rows. exact routes
// must return every file of the oracle; an approximate route must return
// some of them and no other file. deepIn names a path fragment. The output
// must show more files with that fragment than one USearch call returns:
// semanticUSearchVectorsPerQuery keys for each token and shard.
type e2eRouteCase struct {
	name     string
	filters  string
	operands []string
	backends []string
	exact    bool
	deepIn   string
}

// e2eRouteVerify checks one vector search against want. It checks the route,
// that no USearch call failed over to exact vectors, and the result set. The
// model must score only files that the output shows. It returns the result
// paths.
func e2eRouteVerify(t *testing.T, run e2eRouteRun, want []string, backends []string, exact bool) []string {
	t.Helper()
	if len(want) == 0 {
		if !errors.Is(run.err, errNoMatch) || run.output != "" {
			t.Fatalf("want no match: err=%v\n%s", run.err, run.output)
		}
	} else if run.err != nil {
		t.Fatalf("description: %v\n%s", run.err, run.output)
	}
	if run.fallback {
		t.Fatalf("a USearch call failed over to exact vectors (backends %v)", run.backends)
	}
	if !slices.Equal(run.backends, backends) {
		t.Fatalf("semantic backends %v, want %v", run.backends, backends)
	}
	got := headerPaths(run.output)
	t.Logf("backends %v: %d files, %d expected (exact route: %t)", run.backends, len(got), len(want), exact)
	if exact {
		if !slices.Equal(got, want) {
			t.Fatalf("exact route found %d files, want the %d files of the oracle\ngot:  %v\nwant: %v",
				len(got), len(want), got, want)
		}
	} else {
		if len(want) > 0 && len(got) == 0 {
			t.Fatalf("approximate route found no file; the oracle has %d", len(want))
		}
		for _, path := range got {
			if _, found := slices.BinarySearch(want, path); !found {
				t.Fatalf("approximate route found %s, which the oracle drops", path)
			}
		}
	}
	for _, scored := range run.scored {
		suffix := string(filepath.Separator) + filepath.FromSlash(scored)
		if !slices.ContainsFunc(got, func(path string) bool { return strings.HasSuffix(path, suffix) }) {
			t.Fatalf("the model scored %s, which the output does not show", scored)
		}
	}
	return got
}

// e2eRouteCheck runs the text oracle and the description of test, and
// checks the description against the oracle.
func e2eRouteCheck(t *testing.T, test e2eRouteCase) e2eRouteRun {
	t.Helper()
	oracle := e2eRouteOracle(t, test.filters, test.operands...)
	run := e2eRouteSearch(t, strings.TrimSpace("zqalpha zqbeta "+test.filters), test.operands...)
	got := e2eRouteVerify(t, run, oracle, test.backends, test.exact)
	if test.deepIn != "" {
		if count := e2eRouteCount(got, test.deepIn); count <= semanticUSearchVectorsPerQuery {
			t.Fatalf("found %d files in %q, want more than the %d keys of one USearch call (oracle %d files)",
				count, test.deepIn, semanticUSearchVectorsPerQuery, e2eRouteCount(oracle, test.deepIn))
		}
	}
	return run
}

// e2eRouteColdWarm runs query again: the warm search must print what the
// cold search printed.
func e2eRouteColdWarm(t *testing.T, cold e2eRouteRun, query string, operands ...string) {
	t.Helper()
	warm := e2eRouteSearch(t, query, operands...)
	if warm.err != nil || warm.output != cold.output {
		t.Fatalf("warm search differs (err=%v):\ncold:\n%s\nwarm:\n%s", warm.err, cold.output, warm.output)
	}
}

// e2eRouteBulk returns a Go file of funcs functions with no marker: funcs+1
// rows far from the query.
func e2eRouteBulk(pkg string, file, funcs int) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "package %s\n\n", pkg)
	for function := range funcs {
		fmt.Fprintf(&builder, "// %s %d %d plain filler\nfunc F%d_%d() int { return %d }\n\n",
			pkg, file, function, file, function, function)
	}
	return builder.String()
}

// e2eRouteRepo builds and commits the Git node of the route tests at
// tree/repo. Each small file is one row; zqnear marks the near files for
// the text oracle:
//
//	few/f00.go-f19.go    20 near Go rows
//	few/far.go            1 far Go row
//	dense/d000-d299.py  300 near Python rows
//	tools/t0.rb-t7.rb     8 near Ruby rows
//	filler/g0.go-g3.go 1204 far Go rows
//	README.md             1 far row
//
// The 1,534 rows fill one USearch shard. A partial selection of up to 256
// rows takes the direct exact route. Up to 384 rows (a quarter of the shard,
// plus one), it takes the shard-exact route. Above, it takes the filtered
// USearch route.
func e2eRouteRepo(t *testing.T, f *graphFixture) string {
	t.Helper()
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	for index := range 20 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("few/f%02d.go", index)),
			fmt.Sprintf("package few\n// zqnear SEMANTIC_TARGET few %02d\n", index))
	}
	writeFixtureFile(t, filepath.Join(repo, "few/far.go"), "package few\n// plain few far\n")
	for index := range 300 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("dense/d%03d.py", index)),
			fmt.Sprintf("# zqnear SEMANTIC_TARGET dense %03d\n", index))
	}
	for index := range 8 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("tools/t%d.rb", index)),
			fmt.Sprintf("# zqnear SEMANTIC_TARGET tool %d\n", index))
	}
	for index := range 4 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("filler/g%d.go", index)), e2eRouteBulk("filler", index, 300))
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "routes")
	return repo
}

// TestE2ERouteMaskKindsTakeEveryRoute runs each kind of mask on the committed
// rows of a Git node: file:, -file:, lang:, a path-operand scope, and a
// combination. Each kind takes the direct exact route, the shard-exact route,
// and the filtered USearch route. It also runs the unfiltered routes of the
// Git node and a folder node. Then it runs a filter that keeps every row, one
// that keeps no row, and two nodes together. A one-word text search with the
// same filters is the oracle. Exact routes must return every oracle file. The
// USearch routes must return oracle files only. Where the oracle has more
// files than one USearch call returns, they must return more too. The model
// scores only files that show.
func TestE2ERouteMaskKindsTakeEveryRoute(t *testing.T) {
	fixTestUSearchShardRows(t, semanticUSearchMaxShardRows)
	requireTools(t)
	f := newGraphFixture(t)
	repo := e2eRouteRepo(t, f)
	loose := f.path("tree/loose")
	for index := range 6 {
		writeFixtureFile(t, filepath.Join(loose, fmt.Sprintf("l%d.go", index)),
			fmt.Sprintf("package loose\n// zqnear SEMANTIC_TARGET loose %d\n", index))
	}
	writeFixtureFile(t, filepath.Join(loose, "plain.go"), "package loose\n// plain loose far\n")
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	in := func(rels ...string) []string { return e2eRouteJoin(repo, rels...) }
	cases := []e2eRouteCase{
		{"file/filtered-exact", "file:few/", []string{repo}, []string{"filtered-exact"}, true, ""},
		{"file/filtered-shard-exact", "file:dense/", []string{repo}, []string{"filtered-shard-exact"}, true, ""},
		{"negated-file/filtered-usearch", "-file:few/", []string{repo}, []string{"filtered-usearch"}, false, "/"},
		{"lang/filtered-exact", "lang:ruby", []string{repo}, []string{"filtered-exact"}, true, ""},
		{"lang/filtered-shard-exact", "lang:python", []string{repo}, []string{"filtered-shard-exact"}, true, ""},
		{"lang/filtered-usearch", "lang:go", []string{repo}, []string{"filtered-usearch"}, false, ""},
		{"scope/filtered-exact", "", in("few"), []string{"filtered-exact"}, true, ""},
		{"scope/filtered-shard-exact", "", in("dense"), []string{"filtered-shard-exact"}, true, ""},
		{"scope/filtered-usearch", "", in("dense", "filler", "tools"), []string{"filtered-usearch"}, false, "/"},
		{"combination/filtered-exact", "lang:go -file:f0", in("few"), []string{"filtered-exact"}, true, ""},
		{"combination/filtered-shard-exact", "lang:python -file:d29", in("dense", "tools"), []string{"filtered-shard-exact"}, true, ""},
		{"combination/filtered-usearch", "lang:go -file:f0", in("few", "filler"), []string{"filtered-usearch"}, false, ""},
		{"no-mask/usearch", "", []string{repo}, []string{"usearch"}, false, "/"},
		{"filter-keeps-every-row/usearch", "-file:nomatchzq", []string{repo}, []string{"usearch"}, false, "/"},
		{"filter-keeps-no-row", "file:nomatchzq", []string{repo}, nil, true, ""},
		{"folder/no-mask/exact", "", []string{loose}, []string{"exact"}, true, ""},
		{"folder/filter-keeps-every-row/exact", "lang:go", []string{loose}, []string{"exact"}, true, ""},
		{"folder/file/filtered-exact", "file:l[0-2]", []string{loose}, []string{"filtered-exact"}, true, ""},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			run := e2eRouteCheck(t, test)
			if index == 0 {
				// The first description built the vectors.
				e2eRouteColdWarm(t, run, "zqalpha zqbeta "+test.filters, test.operands...)
			}
		})
	}

	// Each node runs its own mask and route, and the two-node result is the
	// union of the one-node results.
	t.Run("two-nodes", func(t *testing.T) {
		const filters = "-file:few/"
		test := e2eRouteCase{
			"two-nodes", filters, []string{repo, loose},
			[]string{"exact", "filtered-usearch"}, false, "/",
		}
		run := e2eRouteCheck(t, test)
		var union []string
		for _, operand := range test.operands {
			single := e2eRouteSearch(t, "zqalpha zqbeta "+filters, operand)
			if single.err != nil {
				t.Fatalf("one node %s: %v", operand, single.err)
			}
			union = append(union, headerPaths(single.output)...)
		}
		slices.Sort(union)
		if got := headerPaths(run.output); !slices.Equal(got, union) {
			t.Fatalf("two nodes found %d files, the one-node searches %d:\ngot:   %v\nunion: %v",
				len(got), len(union), got, union)
		}
	})
}

// TestE2ERouteSaturatedShardGivesEveryAllowedRow searches the route
// repository. Its one USearch shard holds 1,312 keys near the query, and 1,232
// of them stay when -file:few/ drops the few/ files. The query token still
// reaches the bound after 1,024 keys, so the shard saturates and every
// allowed row goes to the exact fine scores. The output must then hold every
// file of the oracle and no other file, with and without the filter.
func TestE2ERouteSaturatedShardGivesEveryAllowedRow(t *testing.T) {
	fixTestUSearchShardRows(t, semanticUSearchMaxShardRows)
	requireTools(t)
	f := newGraphFixture(t)
	repo := e2eRouteRepo(t, f)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	for _, test := range []e2eRouteCase{
		{"no-mask/usearch", "", []string{repo}, []string{"usearch"}, true, "/"},
		{"negated-file/filtered-usearch", "-file:few/", []string{repo}, []string{"filtered-usearch"}, true, "/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if run := e2eRouteCheck(t, test); run.saturated != 1 {
				t.Fatalf("saturated shards=%d, want the one shard", run.saturated)
			}
		})
	}
}

// TestE2ERouteDirtyExclusionTakesEveryRoute makes a Git node dirty in three
// steps. In each step the dirty-file exclusion of the base rows alone takes
// another route: the filtered USearch route, the shard-exact route, and the
// direct exact route. The dirty files are: a near file edited to far text, a
// deleted near file, a near file with new near text, and a far file edited
// to near text. Two untracked near files are dirty too. The one-word text
// search on the work tree is the oracle. The committed rows of an edited or
// deleted file never show, and the model never scores them. The overlay is
// exact, so each dirty near file always shows.
func TestE2ERouteDirtyExclusionTakesEveryRoute(t *testing.T) {
	fixTestUSearchShardRows(t, semanticUSearchMaxShardRows)
	requireTools(t)
	f := newGraphFixture(t)
	repo := e2eRouteRepo(t, f)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	if run := e2eRouteSearch(t, "zqalpha zqbeta", repo); run.err != nil || !slices.Equal(run.backends, []string{"usearch"}) {
		t.Fatalf("clean search: err=%v backends=%v", run.err, run.backends)
	}

	writeFixtureFile(t, filepath.Join(repo, "few/f00.go"), "package few\n// plain edited far\n")
	if err := os.Remove(filepath.Join(repo, "few/f01.go")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "few/f02.go"), "package few\n// zqnear SEMANTIC_TARGET few 02 edited\n")
	writeFixtureFile(t, filepath.Join(repo, "few/far.go"), "package few\n// zqnear SEMANTIC_TARGET few far edited\n")
	writeFixtureFile(t, filepath.Join(repo, "few/u00.go"), "package few\n// zqnear SEMANTIC_TARGET few untracked\n")
	writeFixtureFile(t, filepath.Join(repo, "tools/t8.rb"), "# zqnear SEMANTIC_TARGET tool untracked\n")
	gone := []string{"few/f00.go", "few/f01.go"}
	overlay := e2eRouteJoin(repo, "few/f02.go", "few/far.go", "few/u00.go", "tools/t8.rb")
	check := func(t *testing.T, test e2eRouteCase) e2eRouteRun {
		t.Helper()
		run := e2eRouteCheck(t, test)
		got := headerPaths(run.output)
		for _, rel := range gone {
			if slices.Contains(got, filepath.Join(repo, rel)) || slices.Contains(run.scored, rel) {
				t.Fatalf("the committed rows of %s leaked: scored %v\n%s", rel, run.scored, run.output)
			}
		}
		oracle := e2eRouteOracle(t, test.filters, test.operands...)
		for _, path := range overlay {
			if slices.Contains(oracle, path) && !slices.Contains(got, path) {
				t.Fatalf("the exact overlay lost %s:\n%s", path, run.output)
			}
		}
		return run
	}

	// Four committed files are dirty: 1,530 base rows stay.
	for index, test := range []e2eRouteCase{
		{"few-dirty/filtered-usearch", "", []string{repo}, []string{"filtered-usearch"}, false, "/"},
		{"few-dirty-and-file/filtered-exact", "file:few/", []string{repo}, []string{"filtered-exact"}, true, ""},
		{"few-dirty-scope-and-lang/filtered-exact", "lang:ruby", e2eRouteJoin(repo, "tools"), []string{"filtered-exact"}, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := check(t, test)
			if index == 0 {
				// The first dirty search built the overlay.
				e2eRouteColdWarm(t, run, "zqalpha zqbeta", repo)
			}
		})
	}

	// The four filler files are dirty too: 326 base rows stay.
	for index := range 4 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("filler/g%d.go", index)),
			e2eRouteBulk("filler", index, 300)+"// plain edited filler\n")
	}
	t.Run("filler-dirty/filtered-shard-exact", func(t *testing.T) {
		check(t, e2eRouteCase{"", "", []string{repo}, []string{"filtered-shard-exact"}, true, ""})
	})

	// 80 dense files are deleted too: 246 base rows stay.
	for index := range 80 {
		if err := os.Remove(filepath.Join(repo, fmt.Sprintf("dense/d%03d.py", index))); err != nil {
			t.Fatal(err)
		}
	}
	gone = append(gone, "dense/d000.py", "dense/d079.py")
	t.Run("dense-deleted/filtered-exact", func(t *testing.T) {
		check(t, e2eRouteCase{"", "", []string{repo}, []string{"filtered-exact"}, true, ""})
	})
}

// e2eRouteShards reads the one semantic generation in the test's cache. It
// returns its USearch shard count and, for each directory of its rows, the
// shards that hold those rows. It checks the fixture, not a result.
func e2eRouteShards(t *testing.T) (int, map[string][]int) {
	t.Helper()
	root, err := seekUserCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := filepath.Glob(filepath.Join(root, "corpora", "*", "index", semanticGenerationPrefix+"*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("semantic generations %v (err=%v), want one", dirs, err)
	}
	manifest, err := readSemanticManifestUnchecked(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	generation, err := openGenerationAt(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = generation.Close() }()
	shards := make(map[string][]int)
	for _, row := range generation.rows {
		shard := slices.IndexFunc(manifest.USearchFiles, func(file semanticUSearchShard) bool {
			return row.row >= file.Start && row.row < file.Start+file.Rows
		})
		dir := path.Dir(row.path)
		if !slices.Contains(shards[dir], shard) {
			shards[dir] = append(shards[dir], shard)
		}
	}
	return len(manifest.USearchFiles), shards
}

// TestE2ERouteFilterInSomeUSearchShards builds a Git node of 4,952 rows in
// two USearch shards of at most semanticUSearchMaxShardRows rows. The near
// files of a/anear lie in shard 0, and those of b/near in shard 1. Each
// filter allows rows in some shards only. So the shard plan skips a shard,
// searches a whole shard with no bitmap, scores a shard directly, or uses the
// bitmap. A one-word text search with the same filter is the oracle. Shard 1
// holds more near files than one USearch call returns, so its results show
// the deepening.
func TestE2ERouteFilterInSomeUSearchShards(t *testing.T) {
	fixTestUSearchShardRows(t, semanticUSearchMaxShardRows)
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	for index := range 240 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("a/anear/n%03d.go", index)),
			fmt.Sprintf("package anear\n// zqnear SEMANTIC_TARGET a near %03d\n", index))
	}
	for index := range 10 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("a/bulk%02d.go", index)), e2eRouteBulk("a", index, 400))
	}
	for index := range 300 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("b/near/m%03d.go", index)),
			fmt.Sprintf("package near\n// zqnear SEMANTIC_TARGET b near %03d\n", index))
	}
	writeFixtureFile(t, filepath.Join(repo, "b/zfar/z0.go"), e2eRouteBulk("zfar", 0, 400))
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "shards")
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	// Rows: README 1, a/anear 240, a/bulk 4,010 (401 for each file), b/near
	// 300, b/zfar 401. The cut falls after a/bulk08.go, the first file after
	// which shard 0 holds 3,840 rows (15/16 of 4,096); a file of 401 rows is
	// always an anchor. So shard 0 holds rows 0-3,849; shard 1 holds
	// a/bulk09.go, b/near, and b/zfar: 1,102 rows.
	cold := e2eRouteCheck(t, e2eRouteCase{"", "", []string{repo}, []string{"usearch"}, false, "/b/near/"})
	e2eRouteColdWarm(t, cold, "zqalpha zqbeta", repo)
	count, shards := e2eRouteShards(t)
	if count != 2 || !slices.Equal(shards["a/anear"], []int{0}) || !slices.Equal(shards["b/near"], []int{1}) {
		t.Fatalf("fixture has %d shards and shard map %v, want a/anear in shard 0 and b/near in shard 1", count, shards)
	}
	for _, test := range []e2eRouteCase{
		// Shard 0 has no allowed row; 300 of shard 1's 1,102 rows take the
		// bitmap search.
		{"second-shard-only/filtered-usearch", "file:b/near/", []string{repo}, []string{"filtered-usearch"}, false, "/b/near/"},
		// 120 rows of shard 0 and 150 rows of shard 1: each at most a
		// quarter of its shard, so both score directly.
		{"both-shards-direct/filtered-shard-exact", `file:near/ file:[02468]\.go$`, []string{repo}, []string{"filtered-shard-exact"}, true, ""},
		// Shard 0 scores its 240 rows directly; shard 1 takes the bitmap
		// search.
		{"direct-and-bitmap/filtered-usearch", "file:near/", []string{repo}, []string{"filtered-usearch"}, false, "/b/near/"},
		// Shard 0 is whole, so its search has no bitmap; shard 1 takes the
		// bitmap search for the 401 rows of a/bulk09.go.
		{"whole-first-shard/filtered-usearch", "-file:b/", []string{repo}, []string{"filtered-usearch"}, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			e2eRouteCheck(t, test)
		})
	}
}

// TestE2ERouteRadiusBoundsEveryRoute gives a folder node and a Git node files
// just inside the retrieval radius (fine MeanMaxSim 0.4860) and just outside
// it (0.4825). All have near coarse vectors, so USearch returns each of them,
// and only the radius tells them apart. The Git node has an inside and an
// outside file in its committed rows and in its untracked overlay. The model
// would accept every one of them. On every route the inside files show, and
// the outside files never reach the model. The expected files come from the
// fixture.
func TestE2ERouteRadiusBoundsEveryRoute(t *testing.T) {
	fixTestUSearchShardRows(t, semanticUSearchMaxShardRows)
	requireTools(t)
	f := newGraphFixture(t)
	fold := f.path("tree/fold")
	writeFixtureFile(t, filepath.Join(fold, "radius/in.go"), "package radius\n// SEMANTIC_TARGET E2E_FINE_4860 inside\n")
	writeFixtureFile(t, filepath.Join(fold, "radius/out.go"), "package radius\n// SEMANTIC_TARGET E2E_FINE_4825 outside\n")
	writeFixtureFile(t, filepath.Join(fold, "filler/big.go"), e2eRouteBulk("filler", 0, 300))
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "radius/cin.go"), "package radius\n// SEMANTIC_TARGET E2E_FINE_4860 committed inside\n")
	writeFixtureFile(t, filepath.Join(repo, "radius/cout.go"), "package radius\n// SEMANTIC_TARGET E2E_FINE_4825 committed outside\n")
	writeFixtureFile(t, filepath.Join(repo, "filler/big.go"), e2eRouteBulk("filler", 1, 300))
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "radius")
	writeFixtureFile(t, filepath.Join(repo, "radius/uin.go"), "package radius\n// SEMANTIC_TARGET E2E_FINE_4860 untracked inside\n")
	writeFixtureFile(t, filepath.Join(repo, "radius/uout.go"), "package radius\n// SEMANTIC_TARGET E2E_FINE_4825 untracked outside\n")
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	if semanticRetrievalRadius <= 0.4825 || semanticRetrievalRadius >= 0.4860 {
		t.Fatalf("the fixture cosines do not bound the radius %v", semanticRetrievalRadius)
	}
	inFold := e2eRouteJoin(fold, "radius/in.go")
	inRepo := e2eRouteJoin(repo, "radius/cin.go", "radius/uin.go")
	both := append(slices.Clone(inFold), inRepo...)
	slices.Sort(both)
	// The untracked files of the Git node have no base rows, so its mask
	// keeps every base row and the node takes an unfiltered route.
	for index, test := range []struct {
		name     string
		filters  string
		operands []string
		backends []string
		want     []string
	}{
		{"folder/usearch", "", []string{fold}, []string{"usearch"}, inFold},
		{"folder/filtered-exact", "file:radius/", []string{fold}, []string{"filtered-exact"}, inFold},
		{"git/usearch", "", []string{repo}, []string{"usearch"}, inRepo},
		{"git/filtered-exact", "file:radius/", []string{repo}, []string{"filtered-exact"}, inRepo},
		{"two-nodes/usearch", "", []string{fold, repo}, []string{"usearch", "usearch"}, both},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := strings.TrimSpace("zqalpha zqbeta " + test.filters)
			run := e2eRouteSearch(t, query, test.operands...)
			e2eRouteVerify(t, run, test.want, test.backends, true)
			for _, scored := range run.scored {
				if strings.Contains(scored, "out.go") {
					t.Fatalf("the model scored %s, which is outside the radius", scored)
				}
			}
			if index == 0 {
				e2eRouteColdWarm(t, run, query, test.operands...)
			}
		})
	}
}

// TestE2ERouteLongQueryWindowsOnEveryRoute runs a 130-word description. The
// model gives it two query windows, whose scoring tokens lie on two axes. The
// w/b files have their coarse vectors on the second window's axis and a fine
// vector on each axis. So only the second window's token finds them in
// USearch, and their MeanMaxSim over both tokens is in the radius. The w/s
// files lie on the first axis only: their MeanMaxSim over two tokens is about
// half, outside the radius. On every route the long query returns w/b files
// only. The USearch routes need the deepening for the second token. A
// two-word description on the exact route returns both kinds. The expected
// files come from the fixture.
func TestE2ERouteLongQueryWindowsOnEveryRoute(t *testing.T) {
	fixTestUSearchShardRows(t, semanticUSearchMaxShardRows)
	requireTools(t)
	folder := t.TempDir()
	for index := range 300 {
		writeFileAt(t, folder, fmt.Sprintf("w/b%03d.go", index),
			fmt.Sprintf("package w\n// SEMANTIC_TARGET E2E_AXIS1 both windows %03d\n", index))
	}
	for index := range 20 {
		writeFileAt(t, folder, fmt.Sprintf("w/s%02d.go", index),
			fmt.Sprintf("package w\n// SEMANTIC_TARGET first window only %02d\n", index))
	}
	writeFileAt(t, folder, "x/plain.go", "package x\n// plain far\n")
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	words := make([]string, e2eRouteWindowWords+5)
	for index := range words {
		words[index] = fmt.Sprintf("zqw%03d", index)
	}
	long := strings.Join(words, " ")
	var both, first []string
	for index := range 100 {
		both = append(both, fmt.Sprintf("w/b%03d.go", index))
	}
	for index := range 10 {
		first = append(first, fmt.Sprintf("w/s%02d.go", index))
	}
	for index, test := range []struct {
		name     string
		query    string
		backends []string
		exact    bool
		want     []string
	}{
		{"long/usearch", long, []string{"usearch"}, false, nil},
		{"long/filtered-usearch", long + " file:w/", []string{"filtered-usearch"}, false, nil},
		{"long/filtered-exact", long + ` file:w/[bs]0`, []string{"filtered-exact"}, true, e2eRouteJoin(folder, both...)},
		{"short/filtered-exact", `zqalpha zqbeta file:w/[bs]0`, []string{"filtered-exact"}, true,
			e2eRouteJoin(folder, append(slices.Clone(both), first...)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := e2eRouteSearch(t, test.query, folder)
			want := test.want
			if !test.exact {
				// Every w/b file is a candidate; the approximate route must
				// return some of them, and more than one USearch call
				// returns.
				want = nil
				for index := range 300 {
					want = append(want, filepath.Join(folder, fmt.Sprintf("w/b%03d.go", index)))
				}
				slices.Sort(want)
			}
			got := e2eRouteVerify(t, run, want, test.backends, test.exact)
			if !test.exact && len(got) <= semanticUSearchVectorsPerQuery {
				t.Fatalf("found %d files, want more than the %d keys of one USearch call", len(got), semanticUSearchVectorsPerQuery)
			}
			if index == 0 {
				// The first description built the vectors.
				e2eRouteColdWarm(t, run, test.query, folder)
			}
		})
	}
}

// TestE2ERouteModelScoresOrderVectorHits finds files only through vectors and
// gives the model scores that order them the other way. The README says
// that the model ranks the files that vectors add. acceptFiles fuses the
// lexical rank (strict files, then relaxed) with the model rank. A file with
// no lexical match has no lexical evidence. So the output must follow the
// model scores when the vector scores order the files the other way. It must
// also follow them when all vector scores are equal. The expected order comes
// from the fixture.
func TestE2ERouteModelScoresOrderVectorHits(t *testing.T) {
	fixTestUSearchShardRows(t, semanticUSearchMaxShardRows)
	requireTools(t)
	folder := t.TempDir()
	for index, file := range []struct{ name, fine, model string }{
		{"rank/r1.go", "9500", "070"},
		{"rank/r2.go", "9000", "080"},
		{"rank/r3.go", "8500", "090"},
		{"rank/r4.go", "8000", "100"},
		{"tie/a.go", "9000", "070"},
		{"tie/b.go", "9000", "080"},
		{"tie/c.go", "9000", "090"},
		{"tie/d.go", "9000", "100"},
	} {
		writeFileAt(t, folder, file.name, fmt.Sprintf(
			"package p\n// SEMANTIC_TARGET E2E_FINE_%s E2E_MODEL_%s file %d\n", file.fine, file.model, index))
	}
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		name, filter string
		want         []string
	}{
		{"vector-order-reversed", "file:rank/", []string{"rank/r4.go", "rank/r3.go", "rank/r2.go", "rank/r1.go"}},
		{"vector-scores-equal", "file:tie/", []string{"tie/d.go", "tie/c.go", "tie/b.go", "tie/a.go"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := e2eRouteSearch(t, "zqalpha zqbeta "+test.filter, folder)
			e2eRouteVerify(t, run, e2eRouteJoin(folder, test.want...), []string{"filtered-exact"}, true)
			var got []string
			for _, path := range e2eRouteOrder(run.output) {
				rel, err := filepath.Rel(folder, path)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, filepath.ToSlash(rel))
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("print order %v, want the model order %v:\n%s", got, test.want, run.output)
			}
		})
	}
}
