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
	"testing"
)

// The tests of this file run the root command with descriptions whose words
// (zqalpha zqbeta) no file holds, so every result comes from vector search.
// They read the semantic route of each node from the "Searched semantic
// index" debug record.
//
// The route tests (mask kinds, saturated shard, dirty exclusion, shard plan)
// use hybridTestScorer in its accept mode: every row is near the query, and
// the model accepts every candidate. So each route must print exactly the
// files that the filters and the scope keep. Every fixture file holds zqrow,
// so the lexical-only search "zqrow <filters>" gives these files: the oracle.
// The route tests prove which rows each route reads. They prove nothing about
// meaning, the acceptance threshold, the radius or rank. With every row near
// the query, each shard that a USearch route of these tests searches allows
// more than 1,024 near coarse vectors, so the shard saturates: all of its
// allowed rows go to the exact fine scores. The radius test shows the radius
// cut.
//
// The build sizes the USearch shards from the row count and GOMAXPROCS, as
// for a user. A test whose routes need a shard plan sets the CPU count that
// gives it (setTestUSearchShardCPUs). A generation of fewer than 512 rows
// gets shards of 256 rows on every host.
//
// The geometry tests (radius, long query, model order) use e2eRouteModel,
// whose cosines and scores the fixture text chooses.

// e2eRouteNear puts a row of e2eRouteModel near query axis 0.
const e2eRouteNear = "E2E_NEAR"

// e2eRouteEmptyScore is the empty-document score of each scoring token of a
// query of e2eRouteModel. A row near the query has a cosine of at least 0.85,
// so it stays within the radius above this score; an orthogonal row does not.
const e2eRouteEmptyScore = 0.5

// e2eRouteModel is the chosen-geometry model of the radius, long-query and
// model-order tests. The text of a row chooses its cosines, and the text of a
// document chooses its score. So the tests prove the arithmetic of the radius
// cut, the query windows and the fusion rule; they prove nothing about
// meaning, about where real vectors put a file, or about tau. Each row gets
// its own unit vectors from its path, start, and text, so no two rows share a
// vector. A row whose text holds E2E_NEAR is near query axis 0, with a cosine
// from 0.85 to 0.95. Any other row is orthogonal to both query axes. Text
// markers change a near row:
//   - E2E_FINE_dddd sets the cosine of the fine vectors to dddd/10000; the
//     coarse vectors keep their cosine;
//   - E2E_AXIS1 puts the coarse vectors near query axis 1, the first fine
//     vector near axis 0, and the other fine vectors near axis 1;
//   - E2E_MODEL_ddd sets the model score of the document to ddd; a document
//     with E2E_NEAR and no such marker scores 100, any other 0.
//
// The query has one scoring token for each 128-token window that the bundled
// tokenizer gives it (lateOnQueryWindows), as with the real model. The token
// of window 0 is on axis 0, and the tokens of the other windows are on axis 1.
// The query has an empty-document score of e2eRouteEmptyScore per scoring
// token, so the retrieval floor and the acceptance lift both subtract it.
type e2eRouteModel struct {
	tokenizer *lateOnTokenizer
}

// newE2ERouteModel returns an e2eRouteModel with the bundled tokenizer.
func newE2ERouteModel(t *testing.T) *e2eRouteModel {
	t.Helper()
	tokenizer, _ := openLateOnTestTokenizer(t)
	return &e2eRouteModel{tokenizer: tokenizer}
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

func (model *e2eRouteModel) PrepareSemanticQuery(_ context.Context, query string) (*semanticQueryEmbedding, error) {
	queryWindows, err := lateOnQueryWindows(model.tokenizer, query)
	if err != nil {
		return nil, err
	}
	windows := len(queryWindows)
	prepared := &semanticQueryEmbedding{
		tokens:     make([]float32, windows*lateOnSequenceLength*semanticEmbeddingDimensions),
		scoreMask:  make([]bool, windows*lateOnSequenceLength),
		emptyScore: e2eRouteEmptyScore * float32(windows),
		modelQuery: query,
	}
	for window := range windows {
		token := window * lateOnSequenceLength
		prepared.tokens[token*semanticEmbeddingDimensions+min(window, 1)] = 1
		prepared.scoreMask[token] = true
	}
	return prepared, nil
}

func (*e2eRouteModel) ScoresWithSemanticQuery(
	_ context.Context,
	_ *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	scores := make([]float32, len(documents))
	for index, document := range documents {
		if value, ok := e2eRouteNumber(document.Text, "E2E_MODEL_"); ok {
			scores[index] = float32(value)
		} else if strings.Contains(document.Text, e2eRouteNear) {
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
	if strings.Contains(text, e2eRouteNear) {
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
// documents that the model scored, with node-relative paths.
type e2eRouteRun struct {
	output    string
	err       error
	backends  []string
	saturated int64
	fallback  bool
	scored    []rerankDocument
}

// e2eRouteSearch runs query on operands through the root command with model
// and reads the route from the debug log.
func e2eRouteSearch(t *testing.T, model semanticModel, query string, operands ...string) e2eRouteRun {
	t.Helper()
	logs := captureTestLogs(t, slog.LevelDebug)
	observed := observeModel(model)
	output, err := seekWith(t, observed.factory(), append([]string{query}, operands...)...)
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
	observed.mu.Lock()
	for _, score := range observed.scored {
		run.scored = append(run.scored, score.document)
	}
	observed.mu.Unlock()
	return run
}

// e2eRouteOracle runs the lexical-only search "zqrow <filters>" on operands.
// Every fixture file of the route tests holds zqrow, so Zoekt's own rule gives
// the files that the filters and the scope keep. The lexical-only search gets
// no model, so it reads no vector.
func e2eRouteOracle(t *testing.T, filters string, operands ...string) []string {
	t.Helper()
	args := append([]string{"--lexical-only", strings.TrimSpace("zqrow " + filters)}, operands...)
	output, err := seek(t, args...)
	if err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("text oracle %q: %v", filters, err)
	}
	return headerPaths(output)
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

// e2eRouteCase is one description search of a route test. backends lists the
// sorted semantic backends, one for each node that reads base rows.
type e2eRouteCase struct {
	name     string
	filters  string
	operands []string
	backends []string
}

// e2eRouteVerify checks one vector search against want. It checks the route,
// that no USearch call failed over to exact vectors, and that the result set
// equals want. The model must score only files that the output shows. It
// returns the result paths.
func e2eRouteVerify(t *testing.T, run e2eRouteRun, want, backends []string) []string {
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
	t.Logf("backends %v, saturated shards %d: %d files", run.backends, run.saturated, len(got))
	if !slices.Equal(got, want) {
		t.Fatalf("backends %v found %d files, want %d\ngot:  %v\nwant: %v",
			run.backends, len(got), len(want), got, want)
	}
	for _, scored := range run.scored {
		suffix := string(filepath.Separator) + filepath.FromSlash(scored.Path)
		if !slices.ContainsFunc(got, func(path string) bool { return strings.HasSuffix(path, suffix) }) {
			t.Fatalf("the model scored %s, which the output does not show", scored.Path)
		}
	}
	return got
}

// e2eRouteCheck runs the text oracle and the description of test with a model
// that accepts every row, and checks that the description prints the files of
// the oracle. The oracle of a route that reads rows must hold a file, so that
// two empty results cannot pass.
func e2eRouteCheck(t *testing.T, test e2eRouteCase) e2eRouteRun {
	t.Helper()
	oracle := e2eRouteOracle(t, test.filters, test.operands...)
	if len(oracle) == 0 && len(test.backends) > 0 {
		t.Fatalf("the text oracle %q finds no file on a route that reads rows", test.filters)
	}
	run := e2eRouteSearch(t, &hybridTestScorer{mode: payloadAccept}, strings.TrimSpace("zqalpha zqbeta "+test.filters), test.operands...)
	e2eRouteVerify(t, run, oracle, test.backends)
	return run
}

// e2eRouteColdWarm runs query again with model: the warm search must print
// what the cold search printed.
func e2eRouteColdWarm(t *testing.T, model semanticModel, cold e2eRouteRun, query string, operands ...string) {
	t.Helper()
	warm := e2eRouteSearch(t, model, query, operands...)
	if warm.err != nil || warm.output != cold.output {
		t.Fatalf("warm search differs (err=%v):\ncold:\n%s\nwarm:\n%s", warm.err, cold.output, warm.output)
	}
}

// e2eRouteBulk returns a Go file of funcs functions: funcs+2 rows (its first
// line, its package clause, and one row for each function). Its first line
// holds zqrow, and its first line and each function comment hold the version
// mark zqv1.
func e2eRouteBulk(pkg string, file, funcs int) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "// zqrow %s %d zqv1\npackage %s\n\n", pkg, file, pkg)
	for function := range funcs {
		fmt.Fprintf(&builder, "// %s %d %d filler zqv1\nfunc F%d_%d() int { return %d }\n\n",
			pkg, file, function, file, function, function)
	}
	return builder.String()
}

// e2eRouteRepo builds and commits the Git node of the route tests at
// tree/repo. Each small file is one row. Every file holds zqrow for the text
// oracle and the version mark zqv1. The rows come in path order:
//
//	README.md             1 row
//	dense/d000-d299.py  300 Python rows
//	few/extra.go          1 Go row
//	few/f00.go-f19.go    20 Go rows
//	filler/g0.go-g3.go 1808 Go rows (452 in each file)
//	tools/t0.rb-t7.rb     8 Ruby rows
//
// It sets GOMAXPROCS (setTestUSearchShardCPUs) so that the 2,138 rows get
// shards of 2,048 rows. The cut gives two shards: shard 0 holds README.md,
// dense/, few/, and filler/g0-g2 (1,678 rows), and shard 1 holds filler/g3.go
// and tools/ (460 rows). e2eRouteRepoShards checks the row count of each shard
// and the folders of its rows: with the rows in path order, the 452 filler
// rows of shard 1 are the rows of filler/g3.go. A partial selection of up to
// 256 rows takes the direct exact route. When each shard that it reads allows
// at most a quarter of its rows (plus one), or at most 64 rows, it takes the
// shard-exact route. Otherwise it takes the filtered USearch route.
func e2eRouteRepo(t *testing.T, f *graphFixture) string {
	t.Helper()
	setTestUSearchShardCPUs(t, 2_138, 2_048)
	repo := f.GitRepo("tree/repo", "README.md", "zqrow readme zqv1\n")
	for index := range 20 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("few/f%02d.go", index)),
			fmt.Sprintf("package few\n// zqrow few %02d zqv1\n", index))
	}
	writeFixtureFile(t, filepath.Join(repo, "few/extra.go"), "package few\n// zqrow few extra zqv1\n")
	for index := range 300 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("dense/d%03d.py", index)),
			fmt.Sprintf("# zqrow dense %03d zqv1\n", index))
	}
	for index := range 8 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("tools/t%d.rb", index)),
			fmt.Sprintf("# zqrow tool %d zqv1\n", index))
	}
	for index := range 4 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("filler/g%d.go", index)), e2eRouteBulk("filler", index, 450))
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "routes")
	return repo
}

// e2eRouteRepoShards checks the shards of the e2eRouteRepo build, the one
// semantic generation of the test's cache, and returns their count: 1,678 and
// 460 rows, README.md, dense/ and few/ in shard 0, filler/ in both, and tools/
// in shard 1.
func e2eRouteRepoShards(t *testing.T) int {
	t.Helper()
	rows, shards := e2eRouteShards(t)
	if !slices.Equal(rows, []uint64{1_678, 460}) || !slices.Equal(shards["."], []int{0}) ||
		!slices.Equal(shards["dense"], []int{0}) || !slices.Equal(shards["few"], []int{0}) ||
		!slices.Equal(shards["filler"], []int{0, 1}) || !slices.Equal(shards["tools"], []int{1}) {
		t.Fatalf("the route repository has shards of %v rows and shard map %v, want 1,678 and 460 rows, "+
			"README.md, dense/ and few/ in shard 0, filler/ in both, and tools/ in shard 1", rows, shards)
	}
	return len(rows)
}

// TestE2ERouteMaskKindsTakeEveryRoute runs each kind of mask on the committed
// rows of a Git node: file:, -file:, lang:, a path-operand scope, and a
// combination. Each kind takes the direct exact route, the shard-exact route,
// and the filtered USearch route. It also runs the unfiltered routes of the
// Git node and a folder node. Then it runs a filter that keeps every row, one
// that keeps no row, and two nodes together. The model accepts every row, so
// each route must print exactly the files of the oracle, the lexical-only
// search with the same filters. The model scores only files that show, and
// the two-node result is the union of the one-node results.
func TestE2ERouteMaskKindsTakeEveryRoute(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := e2eRouteRepo(t, f)
	loose := f.path("tree/loose")
	for index := range 6 {
		writeFixtureFile(t, filepath.Join(loose, fmt.Sprintf("l%d.go", index)),
			fmt.Sprintf("package loose\n// zqrow loose %d\n", index))
	}
	writeFixtureFile(t, filepath.Join(loose, "extra.go"), "package loose\n// zqrow loose extra\n")
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	in := func(rels ...string) []string { return e2eRouteJoin(repo, rels...) }
	cases := []e2eRouteCase{
		{"file/filtered-exact", "file:few/", []string{repo}, []string{"filtered-exact"}},
		{"file/filtered-shard-exact", "file:dense/", []string{repo}, []string{"filtered-shard-exact"}},
		{"negated-file/filtered-usearch", "-file:few/", []string{repo}, []string{"filtered-usearch"}},
		{"lang/filtered-exact", "lang:ruby", []string{repo}, []string{"filtered-exact"}},
		{"lang/filtered-shard-exact", "lang:python", []string{repo}, []string{"filtered-shard-exact"}},
		{"lang/filtered-usearch", "lang:go", []string{repo}, []string{"filtered-usearch"}},
		{"scope/filtered-exact", "", in("few"), []string{"filtered-exact"}},
		{"scope/filtered-shard-exact", "", in("dense"), []string{"filtered-shard-exact"}},
		{"scope/filtered-usearch", "", in("dense", "filler", "tools"), []string{"filtered-usearch"}},
		{"combination/filtered-exact", "lang:go -file:f0", in("few"), []string{"filtered-exact"}},
		{"combination/filtered-shard-exact", "lang:python -file:d29", in("dense", "tools"), []string{"filtered-shard-exact"}},
		{"combination/filtered-usearch", "lang:go -file:f0", in("few", "filler"), []string{"filtered-usearch"}},
		{"no-mask/usearch", "", []string{repo}, []string{"usearch"}},
		{"filter-keeps-every-row/usearch", "-file:nomatchzq", []string{repo}, []string{"usearch"}},
		{"filter-keeps-no-row", "file:nomatchzq", []string{repo}, nil},
		{"folder/no-mask/exact", "", []string{loose}, []string{"exact"}},
		{"folder/filter-keeps-every-row/exact", "lang:go", []string{loose}, []string{"exact"}},
		{"folder/file/filtered-exact", "file:l[0-2]", []string{loose}, []string{"filtered-exact"}},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			run := e2eRouteCheck(t, test)
			if index == 0 {
				// The first description built the vectors.
				e2eRouteColdWarm(t, &hybridTestScorer{mode: payloadAccept}, run, "zqalpha zqbeta "+test.filters, test.operands...)
				e2eRouteRepoShards(t)
			}
		})
	}

	// Each node runs its own mask and route, and the two-node result is the
	// union of the one-node results.
	t.Run("two-nodes", func(t *testing.T) {
		const filters = "-file:few/"
		test := e2eRouteCase{"two-nodes", filters, []string{repo, loose}, []string{"exact", "filtered-usearch"}}
		run := e2eRouteCheck(t, test)
		var union []string
		for _, operand := range test.operands {
			single := e2eRouteSearch(t, &hybridTestScorer{mode: payloadAccept}, "zqalpha zqbeta "+filters, operand)
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
// repository with a model that puts every row near the query. Its two USearch
// shards hold 6,712 and 1,840 near keys, and 6,628 keys of shard 0 stay when
// -file:few/ drops the few/ files. The query tokens still reach the deepen
// bound after 1,024 keys, so each shard saturates and every allowed row goes
// to the exact fine scores. The output must then hold every file of the
// oracle and no other file, with and without the filter.
func TestE2ERouteSaturatedShardGivesEveryAllowedRow(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := e2eRouteRepo(t, f)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	for _, test := range []e2eRouteCase{
		{"no-mask/usearch", "", []string{repo}, []string{"usearch"}},
		{"negated-file/filtered-usearch", "-file:few/", []string{repo}, []string{"filtered-usearch"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := e2eRouteCheck(t, test)
			if count := e2eRouteRepoShards(t); run.saturated != int64(count) {
				t.Fatalf("saturated shards=%d, want all %d shards", run.saturated, count)
			}
		})
	}
}

// TestE2ERouteDirtyExclusionTakesEveryRoute makes a Git node dirty in three
// steps. In each step the dirty-file exclusion of the base rows alone takes
// another route: the filtered USearch route, the shard-exact route, and the
// direct exact route. The committed rows hold the version mark zqv1, and the
// edited and untracked files zqv2. The dirty files are three edited files, a
// deleted file, and two untracked files; then the filler files are edited and
// 80 dense files are deleted. The model accepts every row. At each step the
// output must equal the lexical-only search on the work tree. The model never
// scores a deleted file or the zqv1 text of an edited file, and no edited file
// shows a zqv1 line.
func TestE2ERouteDirtyExclusionTakesEveryRoute(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := e2eRouteRepo(t, f)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	if run := e2eRouteSearch(t, &hybridTestScorer{mode: payloadAccept}, "zqalpha zqbeta", repo); run.err != nil || !slices.Equal(run.backends, []string{"usearch"}) {
		t.Fatalf("clean search: err=%v backends=%v", run.err, run.backends)
	}
	e2eRouteRepoShards(t)

	writeFixtureFile(t, filepath.Join(repo, "few/f00.go"), "package few\n// zqrow few 00 zqv2\n")
	if err := os.Remove(filepath.Join(repo, "few/f01.go")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "few/f02.go"), "package few\n// zqrow few 02 zqv2\n")
	writeFixtureFile(t, filepath.Join(repo, "few/extra.go"), "package few\n// zqrow few extra zqv2\n")
	writeFixtureFile(t, filepath.Join(repo, "few/u00.go"), "package few\n// zqrow few untracked zqv2\n")
	writeFixtureFile(t, filepath.Join(repo, "tools/t8.rb"), "# zqrow tool untracked zqv2\n")
	edited := []string{"few/f00.go", "few/f02.go", "few/extra.go"}
	gone := []string{"few/f01.go"}
	check := func(t *testing.T, test e2eRouteCase) e2eRouteRun {
		t.Helper()
		run := e2eRouteCheck(t, test)
		for _, document := range run.scored {
			if slices.Contains(gone, document.Path) ||
				slices.Contains(edited, document.Path) && strings.Contains(document.Text, "zqv1") {
				t.Fatalf("the model scored the committed rows of %s:\n%s", document.Path, document.Text)
			}
		}
		editedPaths := e2eRouteJoin(repo, edited...)
		for _, result := range printedResults(run.output) {
			if !slices.Contains(editedPaths, result.path) {
				continue
			}
			for _, line := range result.body {
				if strings.Contains(line, "zqv1") {
					t.Fatalf("%s shows its committed line %q:\n%s", result.path, line, run.output)
				}
			}
		}
		return run
	}

	// Four committed files are dirty: 2,134 base rows stay.
	for index, test := range []e2eRouteCase{
		{"few-dirty/filtered-usearch", "", []string{repo}, []string{"filtered-usearch"}},
		{"few-dirty-and-file/filtered-exact", "file:few/", []string{repo}, []string{"filtered-exact"}},
		{"few-dirty-scope-and-lang/filtered-exact", "lang:ruby", e2eRouteJoin(repo, "tools"), []string{"filtered-exact"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := check(t, test)
			if index == 0 {
				// The first dirty search built the overlay.
				e2eRouteColdWarm(t, &hybridTestScorer{mode: payloadAccept}, run, "zqalpha zqbeta", repo)
			}
		})
	}

	// The four filler files are dirty too: 326 base rows stay.
	for index := range 4 {
		rel := fmt.Sprintf("filler/g%d.go", index)
		writeFixtureFile(t, filepath.Join(repo, rel), strings.ReplaceAll(e2eRouteBulk("filler", index, 300), "zqv1", "zqv2"))
		edited = append(edited, rel)
	}
	t.Run("filler-dirty/filtered-shard-exact", func(t *testing.T) {
		check(t, e2eRouteCase{"", "", []string{repo}, []string{"filtered-shard-exact"}})
	})

	// 80 dense files are deleted too: 246 base rows stay.
	for index := range 80 {
		rel := fmt.Sprintf("dense/d%03d.py", index)
		if err := os.Remove(filepath.Join(repo, rel)); err != nil {
			t.Fatal(err)
		}
		gone = append(gone, rel)
	}
	t.Run("dense-deleted/filtered-exact", func(t *testing.T) {
		check(t, e2eRouteCase{"", "", []string{repo}, []string{"filtered-exact"}})
	})
}

// e2eRouteShards reads the one semantic generation in the test's cache. It
// returns the row count of each of its USearch shards and, for each directory
// of its rows, the shards that hold those rows. It checks the fixture, not a
// result.
func e2eRouteShards(t *testing.T) ([]uint64, map[string][]int) {
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
	rows := make([]uint64, len(manifest.USearchFiles))
	for index, file := range manifest.USearchFiles {
		rows[index] = file.Rows
	}
	return rows, shards
}

// TestE2ERouteFilterInSomeUSearchShards builds a Git node of 4,963 rows on
// one CPU, which gives shards of semanticUSearchMaxShardRows rows: two shards.
// The files of a/anear lie in shard 0, and those of b/near in shard 1. Each
// filter allows rows in some shards only. So the shard plan skips a shard,
// searches a whole shard with no bitmap, scores a shard directly, or uses the
// bitmap. The model accepts every row, so each route must print exactly the
// files of the lexical-only search with the same filter.
func TestE2ERouteFilterInSomeUSearchShards(t *testing.T) {
	requireTools(t)
	setTestUSearchShardCPUs(t, 4_963, semanticUSearchMaxShardRows)
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "README.md", "zqrow readme\n")
	for index := range 240 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("a/anear/n%03d.go", index)),
			fmt.Sprintf("package anear\n// zqrow a near %03d\n", index))
	}
	for index := range 10 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("a/bulk%02d.go", index)), e2eRouteBulk("a", index, 400))
	}
	for index := range 300 {
		writeFixtureFile(t, filepath.Join(repo, fmt.Sprintf("b/near/m%03d.go", index)),
			fmt.Sprintf("package near\n// zqrow b near %03d\n", index))
	}
	writeFixtureFile(t, filepath.Join(repo, "b/zfar/z0.go"), e2eRouteBulk("zfar", 0, 400))
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "shards")
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	// Rows: README 1, a/anear 240, a/bulk 4,020 (402 for each file), b/near
	// 300, b/zfar 402. The cut falls after a/bulk08.go, the first file after
	// which shard 0 holds 3,840 rows (15/16 of 4,096); a file of 402 rows is
	// always an anchor. So shard 0 holds rows 0-3,858; shard 1 holds
	// a/bulk09.go, b/near, and b/zfar: 1,104 rows.
	cold := e2eRouteCheck(t, e2eRouteCase{"", "", []string{repo}, []string{"usearch"}})
	e2eRouteColdWarm(t, &hybridTestScorer{mode: payloadAccept}, cold, "zqalpha zqbeta", repo)
	rows, shards := e2eRouteShards(t)
	if len(rows) != 2 || !slices.Equal(shards["a/anear"], []int{0}) || !slices.Equal(shards["b/near"], []int{1}) {
		t.Fatalf("fixture has %d shards and shard map %v, want a/anear in shard 0 and b/near in shard 1", len(rows), shards)
	}
	for _, test := range []e2eRouteCase{
		// Shard 0 has no allowed row; 300 of shard 1's 1,104 rows take the
		// bitmap search.
		{"second-shard-only/filtered-usearch", "file:b/near/", []string{repo}, []string{"filtered-usearch"}},
		// 120 rows of shard 0 and 150 rows of shard 1: each at most a
		// quarter of its shard, so both score directly.
		{"both-shards-direct/filtered-shard-exact", `file:near/ file:[02468]\.go$`, []string{repo}, []string{"filtered-shard-exact"}},
		// Shard 0 scores its 240 rows directly; shard 1 takes the bitmap
		// search.
		{"direct-and-bitmap/filtered-usearch", "file:near/", []string{repo}, []string{"filtered-usearch"}},
		// Shard 0 is whole, so its search has no bitmap; shard 1 takes the
		// bitmap search for the 402 rows of a/bulk09.go.
		{"whole-first-shard/filtered-usearch", "-file:b/", []string{repo}, []string{"filtered-usearch"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e2eRouteCheck(t, test)
		})
	}
}

// TestE2ERouteRadiusBoundsEveryRoute gives a folder node and a Git node files
// just inside the retrieval radius (a fine lift 0.0018 above it) and just
// outside it (0.0018 under it). The lift is the fine cosine minus the
// empty-document score of the one query token, so a scan that leaves out that
// score takes the outside files too. All have near coarse vectors, so USearch
// returns each of them; only the radius tells them apart. The Git node has an
// inside and an outside file in its committed rows and in its untracked
// overlay. The model would accept every one of them. On every route the
// inside files show, and the outside files never reach the model. The cosines
// are chosen. The test proves the radius cut on each route; it proves neither
// where real vectors put a file nor that tau or delta are right.
func TestE2ERouteRadiusBoundsEveryRoute(t *testing.T) {
	requireTools(t)
	const margin = 0.0018
	// floor is the fine cosine at the radius for a query of one token.
	floor := e2eRouteEmptyScore + float64(semanticRetrievalRadius)
	// The index stores the fine vectors as binary16. Its step at the floor
	// must be under the margin, so that the cast keeps each file on its side.
	code := semanticF16FromFloat32(float32(floor))
	if step := semanticF16ToFloat32(code+1) - semanticF16ToFloat32(code); float64(step) >= margin {
		t.Fatalf("the binary16 step %v at the fine cosine %.4f is not under the margin %v", step, floor, margin)
	}
	marker := func(lift float64) string {
		return fmt.Sprintf("%s E2E_FINE_%04d", e2eRouteNear, int(math.Round((floor+lift)*10000)))
	}
	inside, outside := marker(margin), marker(-margin)
	f := newGraphFixture(t)
	fold := f.path("tree/fold")
	writeFixtureFile(t, filepath.Join(fold, "radius/in.go"), "package radius\n// "+inside+" inside\n")
	writeFixtureFile(t, filepath.Join(fold, "radius/out.go"), "package radius\n// "+outside+" outside\n")
	writeFixtureFile(t, filepath.Join(fold, "filler/big.go"), e2eRouteBulk("filler", 0, 300))
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "radius/cin.go"), "package radius\n// "+inside+" committed inside\n")
	writeFixtureFile(t, filepath.Join(repo, "radius/cout.go"), "package radius\n// "+outside+" committed outside\n")
	writeFixtureFile(t, filepath.Join(repo, "filler/big.go"), e2eRouteBulk("filler", 1, 300))
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "radius")
	writeFixtureFile(t, filepath.Join(repo, "radius/uin.go"), "package radius\n// "+inside+" untracked inside\n")
	writeFixtureFile(t, filepath.Join(repo, "radius/uout.go"), "package radius\n// "+outside+" untracked outside\n")
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	model := newE2ERouteModel(t)
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
			run := e2eRouteSearch(t, model, query, test.operands...)
			e2eRouteVerify(t, run, test.want, test.backends)
			for _, scored := range run.scored {
				if strings.Contains(scored.Path, "out.go") {
					t.Fatalf("the model scored %s, which is outside the radius", scored.Path)
				}
			}
			if index == 0 {
				e2eRouteColdWarm(t, model, run, query, test.operands...)
			}
		})
	}
}

// TestE2ERouteLongQueryWindowsOnEveryRoute runs a 130-word description. The
// bundled tokenizer splits it into W query windows (5), and e2eRouteModel gives
// each window one scoring token: window 0 on the first axis, the other windows
// on the second. The w/b files have their coarse vectors on the second axis
// and a fine vector on each axis, so only the tokens of windows 1 and up find
// them in USearch. The w/s files lie on the first axis only. Each row has the
// fine cosine c, chosen so that c/W < floor < c, with floor the
// empty-document score plus the radius per token. Over the W tokens a w/b row
// has a fine MeanMaxSim of about c, in the radius, and a w/s row about c/W,
// outside it. On every route the long query returns every w/b file and no
// other file. A two-word description has one window, and on the exact route
// it returns both kinds. The cosines are chosen; only the window count comes
// from the tokenizer.
func TestE2ERouteLongQueryWindowsOnEveryRoute(t *testing.T) {
	requireTools(t)
	model := newE2ERouteModel(t)
	words := make([]string, 130)
	for index := range words {
		words[index] = fmt.Sprintf("zqw%03d", index)
	}
	long := strings.Join(words, " ")
	plan, err := planQuery(long, defaultSearchPolicy())
	if err != nil || plan.model == nil {
		t.Fatalf("plan the long query: model %v, err %v", plan.model, err)
	}
	queryWindows, err := lateOnQueryWindows(model.tokenizer, plan.model.modelQuery)
	if err != nil {
		t.Fatal(err)
	}
	windows := len(queryWindows)
	t.Logf("the long query has %d windows", windows)
	if windows < 2 {
		t.Fatalf("the long query has %d windows, want 2 or more", windows)
	}
	floor := e2eRouteEmptyScore + float64(semanticRetrievalRadius)
	high := min(1, float64(windows)*floor)
	if high-floor < 0.01 {
		t.Fatalf("no fine cosine c gives c/%d < %.4f < c with a margin", windows, floor)
	}
	fine := fmt.Sprintf("E2E_FINE_%04d", int(math.Round((floor+high)/2*10000)))

	folder := t.TempDir()
	for index := range 300 {
		writeFileAt(t, folder, fmt.Sprintf("w/b%03d.go", index),
			fmt.Sprintf("package w\n// %s %s E2E_AXIS1 both windows %03d\n", e2eRouteNear, fine, index))
	}
	for index := range 20 {
		writeFileAt(t, folder, fmt.Sprintf("w/s%02d.go", index),
			fmt.Sprintf("package w\n// %s %s first window only %02d\n", e2eRouteNear, fine, index))
	}
	writeFileAt(t, folder, "x/plain.go", "package x\n// plain far\n")
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	var every, both, first []string
	for index := range 300 {
		every = append(every, fmt.Sprintf("w/b%03d.go", index))
	}
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
		want     []string
	}{
		{"long/usearch", long, []string{"usearch"}, e2eRouteJoin(folder, every...)},
		{"long/filtered-usearch", long + " file:w/", []string{"filtered-usearch"}, e2eRouteJoin(folder, every...)},
		{"long/filtered-exact", long + ` file:w/[bs]0`, []string{"filtered-exact"}, e2eRouteJoin(folder, both...)},
		{"short/filtered-exact", `zqalpha zqbeta file:w/[bs]0`, []string{"filtered-exact"},
			e2eRouteJoin(folder, append(slices.Clone(both), first...)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := e2eRouteSearch(t, model, test.query, folder)
			e2eRouteVerify(t, run, test.want, test.backends)
			if index == 0 {
				// The first description built the vectors.
				e2eRouteColdWarm(t, model, run, test.query, folder)
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
// from the fixture. The scores are chosen. The test proves the fusion rule
// for vector-only hits; it proves nothing about the real model's ranking.
func TestE2ERouteModelScoresOrderVectorHits(t *testing.T) {
	requireTools(t)
	model := newE2ERouteModel(t)
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
			"package p\n// %s E2E_FINE_%s E2E_MODEL_%s file %d\n", e2eRouteNear, file.fine, file.model, index))
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
			run := e2eRouteSearch(t, model, "zqalpha zqbeta "+test.filter, folder)
			e2eRouteVerify(t, run, e2eRouteJoin(folder, test.want...), []string{"filtered-exact"})
			var got []string
			for _, result := range printedResults(run.output) {
				rel, err := filepath.Rel(folder, result.path)
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
