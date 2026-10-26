package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	ctags "github.com/sourcegraph/go-ctags"
)

// hybridTestScorer is the test model. It runs no inference. The mode, not the
// text, sets the direction of every unit and the verdict on every document.
// The model input changes only small offsets that keep the rows distinct. A
// test that uses it proves the data flow:
//   - which rows a build writes, keeps, repairs and searches;
//   - which documents the model scores;
//   - which files print.
//
// It cannot prove meaning, the acceptance threshold (tau), the retrieval
// radius, the document cut or rank.
type hybridTestScorer struct {
	prepareCalls atomic.Int32
	embedCalls   atomic.Int32
	scoreCalls   atomic.Int32
	closeCalls   atomic.Int32
	embedErr     error
	prepareErr   error
	scoreErr     error
	mode         payloadMode
	// vectorForUnit, when set, gives the vector of each unit; the mode then
	// sets only the scores.
	vectorForUnit func(semanticUnit) semanticVector
	// queryVector, when set, replaces the first of the two query tokens.
	queryVector *semanticVector
	mu          sync.Mutex
	// documents holds each document that the model scored and embeddedPaths
	// the path of each unit to embed, over all calls, in call order.
	documents     []rerankDocument
	embeddedPaths []string
}

// payloadMode is the verdict of hybridTestScorer on every row and every
// document.
type payloadMode uint8

const (
	// payloadFar, the zero value, puts every unit near axis 1, orthogonal to
	// the query, and scores every document 0: no row is a candidate.
	payloadFar payloadMode = iota
	// payloadAccept puts every unit near the query. It gives every document
	// the kept-token count of the query, a MeanMaxSim of 1. So every row is a
	// candidate, and the model accepts every candidate.
	payloadAccept
	// payloadReject puts every unit near the query and scores every document
	// 0: every row is a candidate, and the model rejects every candidate.
	payloadReject
)

func (scorer *hybridTestScorer) EmbedSemanticUnits(
	_ context.Context,
	units []semanticUnit,
) ([]semanticUnitEmbedding, error) {
	scorer.embedCalls.Add(1)
	scorer.mu.Lock()
	for _, unit := range units {
		scorer.embeddedPaths = append(scorer.embeddedPaths, unit.path)
	}
	scorer.mu.Unlock()
	if scorer.embedErr != nil {
		return nil, scorer.embedErr
	}
	vectors := make([]semanticUnitEmbedding, len(units))
	for index, unit := range units {
		var vector semanticVector
		if scorer.vectorForUnit != nil {
			vector = scorer.vectorForUnit(unit)
		} else {
			vector = payloadUnitVector(unit, scorer.mode)
		}
		for centroid := range semanticCoarseCentroidsPerUnit {
			vectors[index].coarse[centroid] = vector
		}
		for centroid := range semanticFineCentroidsPerUnit {
			vectors[index].fine[centroid] = vector
		}
	}
	return vectors, nil
}

// payloadUnitVector is the vector of unit in mode. In the far mode it is near
// axis 1, orthogonal to the query; in the other modes it is near the query
// axis 0. The offsets come from the model input of the unit, so rows with
// equal model input get equal vectors. The input holds the path header, so
// two copies of one text at two paths get different vectors, as with the real
// model.
func payloadUnitVector(unit semanticUnit, mode payloadMode) semanticVector {
	axis := 0
	if mode == payloadFar {
		axis = 1
	}
	input := make([]byte, 0, 4*len(unit.modelInput))
	for _, token := range unit.modelInput {
		input = binary.LittleEndian.AppendUint32(input, uint32(token))
	}
	return testAxisVector(axis, input)
}

// testAxisVector puts a vector near axis, with small offsets on four more axes
// chosen from a hash of seed. The offsets keep the vectors of different seeds
// distinct, also after USearch's int8 cast, which merges small offsets on few
// axes. USearch's graph can leave keys with one shared vector unreachable
// (https://github.com/unum-cloud/USearch/issues/815). Equal seeds give equal
// vectors. The offsets keep the cosine with the axis above 0.99.
func testAxisVector(axis int, seed []byte) semanticVector {
	first := sha256.Sum256(seed)
	sum := first[:]
	var vector semanticVector
	vector[axis] = 1
	for index, offsets := 0, 0; offsets < 4; index += 3 {
		if index+3 > len(sum) {
			next := sha256.Sum256(sum)
			sum, index = next[:], 0
		}
		offsetAxis := 2 + int(sum[index])%(semanticEmbeddingDimensions-2)
		if vector[offsetAxis] != 0 {
			continue
		}
		vector[offsetAxis] = 0.02 + 0.04*float32(binary.LittleEndian.Uint16(sum[index+1:index+3]))/65535
		offsets++
	}
	if err := normalizeSemanticVector(&vector); err != nil {
		panic(err)
	}
	return vector
}

// PrepareSemanticQuery gives one 128-slot window with two kept tokens on axis
// 0, whatever the query text. Token 0 takes queryVector when the test sets it.
// A zero queryVector leaves token 1, so the preparation passes and the vector
// scan fails, as with the real model (lateOnRowHasUsableToken).
func (scorer *hybridTestScorer) PrepareSemanticQuery(
	_ context.Context,
	query string,
) (*semanticQueryEmbedding, error) {
	scorer.prepareCalls.Add(1)
	if scorer.prepareErr != nil {
		return nil, scorer.prepareErr
	}
	prepared := &semanticQueryEmbedding{
		tokens:     make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions),
		scoreMask:  make([]bool, lateOnSequenceLength),
		modelQuery: query,
	}
	if scorer.queryVector == nil {
		prepared.tokens[0] = 1
	} else {
		copy(prepared.tokens[:semanticEmbeddingDimensions], scorer.queryVector[:])
	}
	prepared.tokens[semanticEmbeddingDimensions] = 1
	prepared.scoreMask[0], prepared.scoreMask[1] = true, true
	return prepared, nil
}

func (scorer *hybridTestScorer) ScoresWithSemanticQuery(
	_ context.Context,
	query *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	scorer.scoreCalls.Add(1)
	if scorer.scoreErr != nil {
		return nil, scorer.scoreErr
	}
	scorer.mu.Lock()
	scorer.documents = append(scorer.documents, documents...)
	scorer.mu.Unlock()
	scores := make([]float32, len(documents))
	if scorer.mode == payloadAccept {
		// A raw score sums one similarity per kept query token, so the
		// kept-token count is a MeanMaxSim of 1. The far and reject modes
		// keep every score 0.
		tokens, err := semanticQueryTokenCount(query)
		if err != nil {
			return nil, err
		}
		for index := range scores {
			scores[index] = float32(tokens)
		}
	}
	return scores, nil
}

func (scorer *hybridTestScorer) Close() error {
	scorer.closeCalls.Add(1)
	return nil
}

func (*hybridTestScorer) SemanticCallCPUs(context.Context) int { return 1 }

// TestSemanticEvidenceReadsTheShardCopy reads the text of semantic hits from
// a Git node's shards. A hit shows the line where its unit starts and its
// context, with no literal fragment. The result and the model document name
// Zoekt's language. A row that does not match its shard copy and a row with
// no document are index damage. A file that Zoekt keeps by name only gives no
// hit.
func TestSemanticEvidenceReadsTheShardCopy(t *testing.T) {
	requireTools(t)
	const script = "#!/bin/sh\n# checked semantic evidence\nserve_request() {\n\techo served\n}\n"
	repo := initGitRepo(t, "serve.sh", script)
	writeFileAt(t, repo, "tiny.txt", "x\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "tiny")
	_, plan := planGitTestCorpus(t, repo)
	if err := openNode(t.Context(), plan, testLexicalSearchExecution(), func(openedNode) error { return nil }); err != nil {
		t.Fatal(err)
	}
	units := extractSemanticUnits("serve.sh", []byte(script),
		[]*ctags.Entry{{Name: "serve_request", Kind: "function", Line: 3, Language: "Sh"}}, nil)
	if len(units) != 2 || units[1].language != "Shell" {
		t.Fatalf("units=%+v, want a gap unit and the function unit, in Zoekt's Shell", units)
	}
	candidate := func(unit semanticUnit) semanticFileCandidate {
		unit.text = nil
		return semanticFileCandidate{unit: unit, proxyScore: 0.75}
	}
	evidence := func(candidates ...semanticFileCandidate) ([]semanticFileCandidate, error) {
		read, readCtx, err := openNodeRead(t.Context(), plan, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer read.close()
		return readSemanticEvidence(readCtx, read, openedNode{plan: plan, repair: &nodeRepair{}}, candidates, 2)
	}

	read, err := evidence(candidate(units[1]))
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 1 {
		t.Fatalf("evidence=%+v, want one hit", read)
	}
	if document := read[0].document; document.Text != "serve_request() {\n\techo served\n}\n" ||
		document.Language != "Shell" || document.Symbol != "function serve_request" {
		t.Fatalf("model document=%+v, want the unit text with Zoekt's language", document)
	}
	file := read[0].result.file
	if file.Language != "Shell" || file.Score != 0.75 || len(file.LineMatches) != 1 {
		t.Fatalf("result=%+v, want one line with Zoekt's language and the proxy score", file)
	}
	match := file.LineMatches[0]
	if len(match.LineFragments) != 0 {
		t.Fatal("semantic evidence must not claim a literal match")
	}
	if string(match.Line) != "serve_request() {\n" || match.LineNumber != 3 ||
		string(match.Before) != "#!/bin/sh\n# checked semantic evidence\n" ||
		string(match.After) != "\techo served\n}\n" {
		t.Fatalf("line=%q number=%d before=%q after=%q", match.Line, match.LineNumber, match.Before, match.After)
	}

	broken := candidate(units[1])
	broken.unit.contentID = semanticContentID(sha256.Sum256([]byte("different")))
	if _, err := evidence(broken); !errors.Is(err, errShardUnloadable) {
		t.Fatalf("a wrong content ID gave err=%v, want index damage", err)
	}
	moved := candidate(units[1])
	moved.unit.start++
	if _, err := evidence(moved); !errors.Is(err, errShardUnloadable) {
		t.Fatalf("a wrong unit identity gave err=%v, want index damage", err)
	}
	gone := candidate(units[0])
	gone.unit.path = "gone.sh"
	if _, err := evidence(gone); !errors.Is(err, errShardUnloadable) {
		t.Fatalf("rows with no document gave err=%v, want index damage", err)
	}
	tiny := extractSemanticUnits("tiny.txt", []byte("x\n"), nil, nil)
	read, err = evidence(candidate(tiny[0]), candidate(units[0]))
	if err != nil || len(read) != 1 || read[0].unit.path != "serve.sh" {
		t.Fatalf("evidence=%+v err=%v, want only serve.sh: Zoekt keeps tiny.txt by name", read, err)
	}
}

// TestVectorHitPrintsZoektLanguage finds a shell file that holds no query
// word, with a model that accepts every row, so the file prints by its vectors
// only. Each embedded unit of the file, its header and its model document name
// Zoekt's language (Shell), not the ctags name (Sh). A strict hit does the
// same.
func TestVectorHitPrintsZoektLanguage(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "serve.sh", "#!/bin/sh\nserve_request() {\n\t# dispatches a request\n\techo served\n}\n")
	model := observeModel(&hybridTestScorer{mode: payloadAccept})
	output, err := seekWith(t, model.factory(), "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(folder, "serve.sh"), filepath.Join(folder, "strict.go")}
	if got := headerPaths(output); !slices.Equal(got, want) ||
		!strings.Contains(output, "## "+filepath.Join(folder, "serve.sh")+" (Shell)\n") {
		t.Fatalf("headers=%v, want %v with Zoekt's language on serve.sh:\n%s", got, want, output)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	var unitLanguages, documentLanguages []string
	for _, unit := range model.units {
		if unit.path == "serve.sh" {
			unitLanguages = append(unitLanguages, unit.language)
		}
	}
	for _, scored := range model.scored {
		if scored.document.Path == "serve.sh" {
			documentLanguages = append(documentLanguages, scored.document.Language)
		}
	}
	for _, languages := range [][]string{unitLanguages, documentLanguages} {
		if len(languages) == 0 || slices.ContainsFunc(languages, func(language string) bool { return language != "Shell" }) {
			t.Fatalf("serve.sh unit languages=%v document languages=%v, want Shell in each",
				unitLanguages, documentLanguages)
		}
	}
}

// TestRunDefaultJoinedSearchAddsSemanticOnlyFile searches a Git node with a
// model that accepts every row. The file with every query word and the file
// with no query word both print. The vector hit joins the strict hit in one
// model session. The build publishes the joined generation of the commit. The
// test proves the data flow, not what the file means.
func TestRunDefaultJoinedSearchAddsSemanticOnlyFile(t *testing.T) {
	requireTools(t)
	repo := initEmptyGitRepo(t)
	writeFileAt(t, repo, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, repo, "semantic.go",
		"package sample\n// dispatches a request to its handler\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-m", "joined search fixture")
	setTestUserCache(t)
	paths, err := resolveGitPaths(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := planGitOperands(t.Context(), paths, canonicalCorpusPath(repo))
	if err != nil || len(plans) != 1 {
		t.Fatalf("plan Git corpus: plans=%d error=%v", len(plans), err)
	}
	plan := plans[0]
	t.Chdir(repo)

	scorer := &hybridTestScorer{mode: payloadAccept}
	model := observeModel(scorer)
	output, err := seekWith(t, model.factory(), "alpha beta")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"semantic.go", "strict.go"}
	if got := headerPaths(output); !slices.Equal(got, want) ||
		!strings.Contains(output, "dispatches a request to its handler") {
		t.Fatalf("headers=%v, want %v with the text of semantic.go:\n%s", got, want, output)
	}
	if model.starts.Load() != 1 || model.prepares.Load() != 1 ||
		model.embeds.Load() == 0 || model.scores.Load() != 1 ||
		model.closes.Load() != 1 {
		t.Fatalf(
			"calls factory=%d prepare=%d embed=%d score=%d close=%d",
			model.starts.Load(), model.prepares.Load(), model.embeds.Load(),
			model.scores.Load(), model.closes.Load(),
		)
	}
	scorer.mu.Lock()
	scored := documentPaths(scorer.documents)
	scorer.mu.Unlock()
	if !slices.Equal(scored, want) {
		t.Fatalf("scored documents=%v, want %v", scored, want)
	}

	state := mustGitRepoStateIn(t, t.Context(), repo)
	stateHash := gitCorpusStateHash(paths, state)
	if !plan.joinedGenerationsMatch(stateHash, state.HeadSHA, "") {
		t.Fatalf("default build did not publish %s", filepath.Join(plan.cacheDir, joinedGenerationFile))
	}
}

// TestRunDefaultFolderJoinedSearchAddsSemanticOnlyFile is the folder form of
// TestRunDefaultJoinedSearchAddsSemanticOnlyFile. With a model that accepts
// every row, the file with no query word prints. The search keeps the joined
// generation of the folder state.
func TestRunDefaultFolderJoinedSearchAddsSemanticOnlyFile(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "semantic.go",
		"package sample\n// dispatches a request to its handler\n")
	plan := planFolderTestCorpus(t, folder)

	output, _, err := stubSearch(t, payloadAccept, "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(folder, "semantic.go"), filepath.Join(folder, "strict.go")}
	if got := headerPaths(output); !slices.Equal(got, want) ||
		!strings.Contains(output, "dispatches a request to its handler") {
		t.Fatalf("headers=%v, want %v with the text of semantic.go:\n%s", got, want, output)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.joinedGenerationsMatch(state, plan.folderSemanticSource(state), "") {
		t.Fatal("default folder search did not keep a joined generation")
	}
}

// TestRunDefaultFilteredNativeFailureRecoversExactly damages the USearch graph
// of a folder, then runs a filtered search with a model that accepts every
// row. The native search fails. The exact recovery over the rows that the
// filters keep finds every kept file and no other. The printed set and the
// scored set are the kept files.
func TestRunDefaultFilteredNativeFailureRecoversExactly(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "allowed/strict.go", "package sample\n// seed alpha beta\n")
	// kept lists the files that the filters of the query below keep. Each
	// candidate has its own text, so no two of them collapse into one result.
	kept := []string{"allowed/strict.go", "allowed/meaning.go"}
	for index := range semanticFilteredExactRows + 8 {
		path := fmt.Sprintf("allowed/candidate-%03d.go", index)
		writeFileAt(t, folder, path, fmt.Sprintf("package sample\n// alpha lexical candidate %03d\n", index))
		kept = append(kept, path)
	}
	writeFileAt(t, folder, "allowed/leak_test.go", "package sample\n// alpha beta TEST_LEAK\n")
	writeFileAt(t, folder, "allowed/leak.py", "# alpha beta LANGUAGE_LEAK\n")
	writeFileAt(t, folder, "blocked/leak.go", "package sample\n// alpha beta PATH_LEAK\n")
	// Two files have no query word, so only vectors find them. Only the exact
	// recovery over the allowed rows can add the allowed one. It must not add
	// the blocked one.
	writeFileAt(t, folder, "allowed/meaning.go", "package sample\n// dispatches a request\n")
	writeFileAt(t, folder, "blocked/meaning.go", "package sample\n// PATH_LEAK dispatches a request\n")
	plan := planFolderTestCorpus(t, folder)

	// The seed query gives the model text, so it builds the generation. The
	// mode sets the vectors that the build writes, so it is the mode of the
	// search below.
	if _, _, err := stubSearch(t, payloadAccept, "seed alpha beta", folder); err != nil {
		t.Fatal(err)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	semanticDir := semanticGenerationDir(plan.indexDir, plan.folderSemanticSource(state))
	generation, err := openSemanticGenerationForSearch(semanticDir, plan.folderSemanticSource(state))
	if err != nil {
		t.Fatal(err)
	}
	// The kept files hold more rows than the direct exact route takes, so the
	// filtered search reads the USearch graph that the test damages below.
	mask, err := buildSemanticFilterMask(t.Context(), generation, selectedFilesKeep(kept...))
	if err != nil {
		t.Fatal(err)
	}
	if mask.mode != semanticFilterPartial || mask.allowed <= semanticFilteredExactRows {
		t.Fatalf("fallback fixture mask=%+v", mask)
	}
	if err := generation.Close(); err != nil {
		t.Fatal(err)
	}

	manifest, err := readSemanticManifest(semanticDir, plan.folderSemanticSource(state))
	if err != nil {
		t.Fatal(err)
	}
	// Damage the graph, then refresh its manifest digest. The shard check
	// before the first native use (validateUSearchShard) then passes, so the
	// query reaches the native search error and the exact-vector recovery.
	shardPath := filepath.Join(semanticDir, manifest.USearchFiles[0].Name)
	shard, err := os.OpenFile(shardPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shard.WriteAt([]byte("damaged"), 0); err != nil {
		_ = shard.Close()
		t.Fatal(err)
	}
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}
	manifest.USearchFiles[0].semanticArtifact, err = inspectSemanticArtifact(shardPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(
		filepath.Join(semanticDir, semanticManifestFile), manifestBytes, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if !plan.joinedGenerationsMatch(state, plan.folderSemanticSource(state), "") {
		t.Fatal("fault fixture did not keep the joined generation eligible")
	}

	logs := captureTestLogs(t, slog.LevelDebug)
	output, scorer, err := stubSearch(t, payloadAccept, `alpha beta lang:go file:allowed/ -file:_test\.go$`, folder)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(kept)
	want := make([]string, 0, len(kept))
	for _, path := range kept {
		want = append(want, filepath.Join(folder, path))
	}
	if got := headerPaths(output); !slices.Equal(got, want) {
		t.Fatalf("headers=%v, want the %d kept files", got, len(want))
	}
	for _, leak := range []string{"leak_test.go", "leak.py", "blocked/leak.go", "blocked/meaning.go", "TEST_LEAK", "LANGUAGE_LEAK", "PATH_LEAK"} {
		if strings.Contains(output, leak) {
			t.Fatalf("filtered fallback contains %q:\n%s", leak, output)
		}
	}
	if scorer.scoreCalls.Load() != 1 {
		t.Fatalf("filtered lexical re-rank calls=%d, want 1", scorer.scoreCalls.Load())
	}
	scorer.mu.Lock()
	scored := documentPaths(scorer.documents)
	scorer.mu.Unlock()
	if !slices.Equal(scored, kept) {
		t.Fatalf("scored documents=%v, want the %d kept files", scored, len(kept))
	}
	// The debug record proves that the damaged graph failed. So
	// allowed/meaning.go, which has no query word, came from the exact
	// recovery.
	foundRecoveryLog := false
	for _, record := range logs.Records() {
		if record.Message == "Filtered USearch search failed; using exact vectors" {
			foundRecoveryLog = true
			break
		}
	}
	if !foundRecoveryLog {
		t.Fatal("filtered native failure did not recover with exact vectors")
	}
}

// TestVectorOnlyFilePrintsOnlyWhenTheModelAcceptsIt searches a folder whose
// one file holds no query word, so only its rows can find it. The mode of the
// test model sets where the rows lie and what the model says. A row far from
// the query is no candidate: the build writes it, and the model scores
// nothing. A row near the query is a candidate: the model scores the document
// of the file. The file prints only when the model accepts it. A search that
// prints nothing exits 1. The test model reads no text, so the test
// proves the path from the rows to the output, not what the file means.
func TestVectorOnlyFilePrintsOnlyWhenTheModelAcceptsIt(t *testing.T) {
	requireTools(t)
	for _, test := range []struct {
		name           string
		mode           payloadMode
		scored, prints bool
	}{
		{"far", payloadFar, false, false},
		{"reject", payloadReject, true, false},
		{"accept", payloadAccept, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			setTestUserCache(t)
			folder := t.TempDir()
			writeFileAt(t, folder, "semantic.go",
				"package sample\n// dispatches a request to its handler\n")
			output, scorer, err := stubSearch(t, test.mode, "alpha beta", folder)
			if scorer.embedCalls.Load() == 0 {
				t.Fatal("the build embedded no row")
			}
			scorer.mu.Lock()
			documents := slices.Clone(scorer.documents)
			scorer.mu.Unlock()
			wantCalls := int32(0)
			if test.scored {
				wantCalls = 1
			}
			if scorer.scoreCalls.Load() != wantCalls ||
				test.scored && (len(documents) != 1 || documents[0].Path != "semantic.go") {
				t.Fatalf("score calls=%d documents=%+v, want %d calls with semantic.go",
					scorer.scoreCalls.Load(), documents, wantCalls)
			}
			if !test.prints {
				if exitCodeForError(err) != 1 || output != "" {
					t.Fatalf("err=%v, want exit status 1 and no output:\n%s", err, output)
				}
				return
			}
			if err != nil || !strings.HasPrefix(output, "## "+filepath.Join(folder, "semantic.go")) ||
				!strings.Contains(output, "dispatches a request to its handler") {
				t.Fatalf("err=%v, want semantic.go with its text:\n%s", err, output)
			}
		})
	}
}

// TestRunDefaultRepairsDamagedSemanticGeneration damages the manifest of the
// semantic generation of a folder. The next search builds the vectors again,
// prints the same output, and activates the joined generation.
func TestRunDefaultRepairsDamagedSemanticGeneration(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// alpha\n")
	plan := planFolderTestCorpus(t, folder)

	run := func(scorer *hybridTestScorer) string {
		t.Helper()
		// The query gives the model text, so it builds the generation that
		// this test then damages and repairs.
		output, err := seekWith(t, modelFactory(scorer), "alpha sample package", folder)
		if err != nil || !strings.Contains(output, "## "+filepath.Join(folder, "app.go")) {
			t.Fatalf("exact search: error=%v output=%q", err, output)
		}
		return output
	}

	first := &hybridTestScorer{}
	wantOutput := run(first)
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(
		semanticGenerationDir(plan.indexDir, plan.folderSemanticSource(state)),
		semanticManifestFile,
	)
	if err := os.WriteFile(manifestPath, []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if plan.joinedGenerationsMatch(state, plan.folderSemanticSource(state), "") {
		t.Fatal("damaged semantic data stayed active")
	}

	repair := &hybridTestScorer{}
	if got := run(repair); got != wantOutput {
		t.Fatalf("repair output=%q, want %q", got, wantOutput)
	}
	if repair.embedCalls.Load() == 0 {
		t.Fatal("repair did not rebuild semantic data")
	}
	requireSearchableGeneration(t, plan, plan.folderSemanticSource(state))
	if !plan.joinedGenerationsMatch(state, plan.folderSemanticSource(state), "") {
		t.Fatal("repair did not activate the joined generation")
	}
}

// TestRunDefaultRerankFallbackLogsCause runs the default search with the real
// model and a score fault. relaxed.go holds only some of the query words. Its
// rows are in the retrieval radius of the real model, so the search sends it
// to the score stage beside strict.go. The search falls back to the strict
// order. It prints the file that holds every query word, drops relaxed.go,
// and writes a debug record with the cause. The real model builds the vectors
// and the query. The fault stops the score stage before the model runs it, so
// the test costs two model batches. The folder has fixed path parts
// (stableModelRoot), so the model reads the same paths in each run.
func TestRunDefaultRerankFallbackLogsCause(t *testing.T) {
	requireTools(t)
	folder := stableModelRoot(t)
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha\n")
	wantErr := errors.New("test score failure")
	logs := captureTestLogs(t, slog.LevelDebug)
	model := observeModel(sharedModel(t))
	model.score = func(context.Context, []rerankDocument) error { return wantErr }

	output, err := seekWith(t, model.factory(), "alpha beta", folder)
	if model.embeds.Load() != 1 || model.prepares.Load() != 1 || model.scores.Load() != 1 {
		t.Fatalf("fallback calls embed=%d prepare=%d score=%d (provider %s)",
			model.embeds.Load(), model.prepares.Load(), model.scores.Load(), sharedModelProvider())
	}
	model.mu.Lock()
	candidates := make([]rerankDocument, 0, len(model.scored))
	for _, scored := range model.scored {
		candidates = append(candidates, scored.document)
	}
	model.mu.Unlock()
	// The fallback can drop relaxed.go only if relaxed.go is a candidate.
	if got, want := documentPaths(candidates), []string{"relaxed.go", "strict.go"}; !slices.Equal(got, want) {
		t.Fatalf("the search sent %v to the score stage, want %v (provider %s)", got, want, sharedModelProvider())
	}
	if err != nil || !strings.Contains(output, "## "+filepath.Join(folder, "strict.go")) ||
		strings.Contains(output, "relaxed.go") {
		t.Fatalf("fallback search: error=%v, want only strict.go:\n%s", err, output)
	}
	assertTestLogError(t, logs, "Model stages failed; using strict order", wantErr)
}

// TestRunDefaultSemanticBuildFailureKeepsLexicalSearchAndCanRepair fails the
// embed call of the first build, with a model that accepts every row. The
// search keeps the lexical output and publishes no joined generation. The next
// search with a healthy model builds the vectors, and every file prints. The
// embed fault is a stub error: the test proves the fallback and the repair,
// not a fault of the real model.
func TestRunDefaultSemanticBuildFailureKeepsLexicalSearchAndCanRepair(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha\n")
	writeFileAt(t, folder, "semantic.go", "package sample\n// dispatches the request\n")
	plan := planFolderTestCorpus(t, folder)
	lexicalOutput, err := seek(t, "--lexical-only", "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("test semantic build failure")
	logs := captureTestLogs(t, slog.LevelDebug)
	scorer := &hybridTestScorer{mode: payloadAccept, embedErr: wantErr}

	output, err := seekWith(t, modelFactory(scorer), "alpha beta", folder)
	// The node gives its strict hits; a relaxed match alone is no candidate.
	if err != nil || output != lexicalOutput {
		t.Fatalf("semantic build fallback: error=%v output=%q, want the lexical output %q", err, output, lexicalOutput)
	}
	// One failed build stops embedding; the query is still prepared and the
	// strict candidates are scored once, and the model is closed once.
	if scorer.embedCalls.Load() != 1 || scorer.prepareCalls.Load() != 1 ||
		scorer.scoreCalls.Load() != 1 || scorer.closeCalls.Load() != 1 {
		t.Fatalf("fallback calls embed=%d prepare=%d score=%d close=%d",
			scorer.embedCalls.Load(), scorer.prepareCalls.Load(), scorer.scoreCalls.Load(), scorer.closeCalls.Load())
	}
	if _, statErr := os.Lstat(filepath.Join(plan.cacheDir, joinedGenerationFile)); !os.IsNotExist(statErr) {
		t.Fatalf("failed semantic build left a joined descriptor: %v", statErr)
	}
	assertTestLogError(
		t,
		logs,
		"Semantic folder index build failed; keeping lexical search",
		wantErr,
	)

	repairOutput, repairScorer, err := stubSearch(t, payloadAccept, "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(folder, "relaxed.go"), filepath.Join(folder, "semantic.go"), filepath.Join(folder, "strict.go"),
	}
	if got := headerPaths(repairOutput); !slices.Equal(got, want) {
		t.Fatalf("repaired joined search headers=%v, want %v:\n%s", got, want, repairOutput)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if repairScorer.embedCalls.Load() == 0 ||
		!plan.joinedGenerationsMatch(state, plan.folderSemanticSource(state), "") {
		t.Fatal("a later healthy run did not repair the joined index")
	}
	requireSearchableGeneration(t, plan, plan.folderSemanticSource(state))
}

// requireSearchableGeneration opens the generation of source as a search
// does, then runs the check that a native search runs before it reads each
// USearch shard.
func requireSearchableGeneration(t *testing.T, plan node, source string) {
	t.Helper()
	generation, err := plan.openJoinedSemanticGeneration(source)
	if err != nil {
		t.Fatalf("open repaired semantic data: %v", err)
	}
	defer func() { _ = generation.Close() }()
	if len(generation.usearch) == 0 {
		t.Fatal("repaired semantic data has no USearch shard")
	}
	for index := range generation.usearch {
		if err := generation.validateUSearchShard(index); err != nil {
			t.Fatalf("repaired USearch shard %d: %v", index, err)
		}
	}
}

// TestRunDefaultSemanticQueryFailureUsesLexicalOutput fails the query
// preparation of the model. The search prints the lexical output and writes
// a debug record with the cause. The build still embeds the folder, and no
// document reaches the score stage.
func TestRunDefaultSemanticQueryFailureUsesLexicalOutput(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha\n")
	planFolderTestCorpus(t, folder)

	// The text index is warm; the failing search builds the vectors.
	if _, err := seek(t, "--lexical-only", "alpha", folder); err != nil {
		t.Fatal(err)
	}
	lexicalOutput, err := seek(t, "--lexical-only", "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("test semantic query failure")
	logs := captureTestLogs(t, slog.LevelDebug)
	scorer := &hybridTestScorer{prepareErr: wantErr}
	output, err := seekWith(t, modelFactory(scorer), "alpha beta", folder)
	if err != nil || output != lexicalOutput {
		t.Fatalf(
			"semantic query fallback: error=%v output=%q, want %q",
			err,
			output,
			lexicalOutput,
		)
	}
	assertTestLogError(t, logs, "Model stages failed; using strict order", wantErr)
	if scorer.embedCalls.Load() != 1 || scorer.prepareCalls.Load() != 1 || scorer.scoreCalls.Load() != 0 {
		t.Fatalf("query failure calls embed=%d prepare=%d score=%d", scorer.embedCalls.Load(), scorer.prepareCalls.Load(), scorer.scoreCalls.Load())
	}
}

// TestDirtyGitSearchUsesCommittedVectors searches a dirty work tree with a
// model that accepts every row. The node builds or reuses the generation of
// its commit, and the clean file with no query word prints through it. The
// dirty file shows its uncommitted bytes, and the next clean search embeds
// nothing new. The dirty bytes reach vectors through the overlay, whose one
// embed call holds only the units of the dirty file.
func TestDirtyGitSearchUsesCommittedVectors(t *testing.T) {
	requireTools(t)
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing-vectors=%t", seeded), func(t *testing.T) {
			repo := initGitRepo(t, "app.go", "package sample\n// alpha beta\n")
			writeFileAt(t, repo, "target.go", "package sample\n// dispatches the request\n")
			gitRunIn(t, repo, "add", ".")
			gitRunIn(t, repo, "commit", "-m", "semantic target")
			setTestUserCache(t)
			accept := modelFactory(&hybridTestScorer{mode: payloadAccept})
			if seeded {
				if _, err := seekWith(t, accept, "alpha beta", repo); err != nil {
					t.Fatal(err)
				}
			}
			want := []string{filepath.Join(repo, "app.go"), filepath.Join(repo, "target.go")}
			writeFileAt(t, repo, "app.go", "package sample\n// alpha beta DIRTY_MARKER\n")
			dirty := observeModel(&hybridTestScorer{mode: payloadAccept})
			// calls holds, for each embed call, the path of each unit, with
			// " (dirty)" when the unit holds the uncommitted bytes.
			var callsMu sync.Mutex
			var calls [][]string
			dirty.embed = func(_ context.Context, units []semanticUnit) error {
				call := make([]string, 0, len(units))
				for _, unit := range units {
					name := unit.path
					if bytes.Contains(unit.text, []byte("DIRTY_MARKER")) {
						name += " (dirty)"
					}
					call = append(call, name)
				}
				callsMu.Lock()
				calls = append(calls, call)
				callsMu.Unlock()
				return nil
			}
			out, err := seekWith(t, dirty.factory(), "alpha beta", repo)
			if got := headerPaths(out); err != nil || !slices.Equal(got, want) ||
				!strings.Contains(out, "DIRTY_MARKER") || !strings.Contains(out, "[uncommitted]") {
				t.Fatalf("dirty headers=%v error=%v, want %v with the dirty bytes:\n%s", got, err, want, out)
			}
			// The overlay embeds the dirty file; the committed base
			// embeds once, before or here.
			wantEmbeds := int32(2)
			if seeded {
				wantEmbeds = 1
			}
			if dirty.embeds.Load() != wantEmbeds || dirty.scores.Load() != 1 {
				t.Fatalf("dirty calls embed=%d score=%d, want %d and 1", dirty.embeds.Load(), dirty.scores.Load(), wantEmbeds)
			}
			callsMu.Lock()
			overlayCalls := 0
			for _, call := range calls {
				if !slices.ContainsFunc(call, func(name string) bool { return strings.HasSuffix(name, " (dirty)") }) {
					continue
				}
				overlayCalls++
				if slices.ContainsFunc(call, func(name string) bool { return name != "app.go (dirty)" }) {
					t.Errorf("overlay embed call=%v, want only the dirty units of app.go", call)
				}
			}
			callsMu.Unlock()
			if overlayCalls != 1 {
				t.Fatalf("embed calls=%v, want one call with the dirty bytes", calls)
			}
			gitRunIn(t, repo, "checkout", "--", "app.go")
			out, clean, err := stubSearch(t, payloadAccept, "alpha beta", repo)
			if got := headerPaths(out); err != nil || !slices.Equal(got, want) || strings.Contains(out, "DIRTY_MARKER") {
				t.Fatalf("clean headers=%v error=%v, want %v with no dirty bytes:\n%s", got, err, want, out)
			}
			if clean.embedCalls.Load() != 0 {
				t.Fatalf("next clean embeds=%d, want 0", clean.embedCalls.Load())
			}
		})
	}
}

// assertTestLogError fails the test unless logs holds a record with message
// whose error attribute wraps want.
func assertTestLogError(
	t *testing.T,
	logs *testLogRecorder,
	message string,
	want error,
) {
	t.Helper()
	for _, record := range logs.Records() {
		if record.Message != message {
			continue
		}
		got, ok := testLogAttrs(record)["error"].(error)
		if !ok || !errors.Is(got, want) {
			t.Fatalf("%s error=%v, want %v", message, got, want)
		}
		return
	}
	t.Fatalf("missing debug record %q", message)
}

// TestDefaultFolderBuildPublishesAndUpdatesJoinedGeneration builds a folder
// with a model, then changes its file and builds again. Each build activates
// the joined generation of its state, and only the current generation
// directory stays.
func TestDefaultFolderBuildPublishesAndUpdatesJoinedGeneration(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// first semantic folder text\n")
	plan := planFolderTestCorpus(t, folder)
	scorer := &hybridTestScorer{}
	execution := testSemanticSearchExecution(t, modelFactory(scorer))

	if state, err := ensureFolderCorpusFreshWithExecution(t.Context(), plan, execution); err != nil {
		t.Fatal(err)
	} else if state != corpusSearchable {
		t.Fatalf("folder state=%d, want searchable", state)
	}
	firstState, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.joinedGenerationsMatch(firstState, plan.folderSemanticSource(firstState), "") {
		t.Fatal("default folder build did not activate a joined generation")
	}

	writeFileAt(t, folder, "app.go", "package sample\n// second semantic folder text with a new length\n")
	if _, err := ensureFolderCorpusFreshWithExecution(t.Context(), plan, execution); err != nil {
		t.Fatal(err)
	}
	secondState, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if secondState == firstState {
		t.Fatal("folder state did not change")
	}
	if !plan.joinedGenerationsMatch(secondState, plan.folderSemanticSource(secondState), "") {
		t.Fatal("updated folder build did not activate a joined generation")
	}
	semanticDirs := semanticGenerationDirs(t, plan.indexDir)
	if len(semanticDirs) != 1 {
		t.Fatalf("semantic generation directories=%v, want one current directory", semanticDirs)
	}
	if scorer.embedCalls.Load() < 2 {
		t.Fatalf("semantic model calls=%d, want a build and an update", scorer.embedCalls.Load())
	}
}

// TestLexicalOnlyFolderBuildStartsNoModelAndWritesNoSemanticData gives the
// folder build a model and asks for vectors, with the lexical-only policy.
// The policy alone keeps the build from starting the model and from writing
// semantic data.
func TestLexicalOnlyFolderBuildStartsNoModelAndWritesNoSemanticData(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// lexical folder text\n")
	plan := planFolderTestCorpus(t, folder)
	var factoryCalls atomic.Int32
	future := newSemanticModelFuture(failingModelFactory(&factoryCalls))
	execution := searchExecution{policy: lexicalOnlySearchPolicy(), model: future, vectors: true}

	if _, err := ensureFolderCorpusFreshWithExecution(t.Context(), plan, execution); err != nil {
		t.Fatal(err)
	}
	if factoryCalls.Load() != 0 {
		t.Fatalf("model factory calls=%d, want zero", factoryCalls.Load())
	}
	if _, err := os.Lstat(filepath.Join(plan.cacheDir, joinedGenerationFile)); !os.IsNotExist(err) {
		t.Fatalf("lexical-only folder build created a joined descriptor: %v", err)
	}
	semanticDirs := semanticGenerationDirs(t, plan.indexDir)
	if len(semanticDirs) != 0 {
		t.Fatalf("lexical-only folder build created semantic data: %v", semanticDirs)
	}
}

// TestRunDefaultFolderJoinedSearchKeepsGuardedFileByName forces the shard
// guard on one large file, with a model that accepts every row, so each unit
// of the file is a candidate. Zoekt keeps only its name, and a vector hit
// shows the text that Zoekt keeps. So the description search prints no vector
// hit of the file and reports no index damage. The name-only notice counts
// it, as for a strict search.
func TestRunDefaultFolderJoinedSearchKeepsGuardedFileByName(t *testing.T) {
	requireTools(t)
	swapGuardLimitsForTest(t)
	t.Chdir(t.TempDir())
	logs := captureTestLogs(t, slog.LevelWarn)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	const needle = "guarded_semantic_needle_marker"
	// Go comment lines keep the file valid source for ctags.
	var guarded strings.Builder
	guarded.WriteString("package sample\n// " + needle + " dispatches a request to its handler\n")
	for _, line := range strings.SplitAfter(string(randomWordText(5, 5<<19, "")), "\n") {
		if line != "" && line != "\n" {
			guarded.WriteString("// " + line)
		}
	}
	writeFileAt(t, folder, "guarded.go", guarded.String())

	output, _, err := stubSearch(t, payloadAccept, "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := headerPaths(output), []string{filepath.Join(folder, "strict.go")}; !slices.Equal(got, want) ||
		!strings.Contains(output, nameOnlyNoticeLine(1)) {
		t.Fatalf("headers=%v, want %v and the name-only notice:\n%s", got, want, output)
	}
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "Index damage") {
			t.Fatalf("the guarded file gave %q", record.Message)
		}
	}

	// The text index holds only the name: the exact term has no match there.
	lexical, err := seek(t, "--lexical-only", needle, folder)
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("lexical search of the guarded file: err=%v, output:\n%s", err, lexical)
	}
}

// TestEditDuringSearchShowsInTheNextSearch edits a tracked file after the
// graph expansion captured the repository, while a warm descriptive search
// runs (the model factory runs after planning). The search reads one capture
// per node, from one git status per node per command, so it shows the
// captured content. The next search captures the edit and shows it.
func TestEditDuringSearchShowsInTheNextSearch(t *testing.T) {
	requireTools(t)
	repo := initGitRepo(t, "a.go", "package sample\n// alpha beta original line\n")
	setTestUserCache(t)
	t.Chdir(repo)
	newModel := modelFactory(&hybridTestScorer{})
	if _, err := seekWith(t, newModel, "alpha beta"); err != nil {
		t.Fatalf("warm-up search: %v", err)
	}
	output, err := seekWith(t, func(context.Context) (semanticModel, error) {
		writeFileAt(t, repo, "a.go", "package sample\n// alpha beta ZQEDITEDDURINGSEARCH\n")
		return &hybridTestScorer{}, nil
	}, "alpha beta")
	if err != nil || !strings.Contains(output, "original line") || strings.Contains(output, "ZQEDITEDDURINGSEARCH") {
		t.Fatalf("the search did not show its capture: err=%v\n%s", err, output)
	}
	output, err = seekWith(t, newModel, "alpha beta")
	if err != nil || !strings.Contains(output, "ZQEDITEDDURINGSEARCH") {
		t.Fatalf("the next search does not show the edit: err=%v\n%s", err, output)
	}
}

// TestFolderFileSavedDuringTheBuildIsATreeChange saves a folder file between
// the two reads of one build: Zoekt indexes the old bytes and the vectors the
// new ones. With a model that accepts every row, the search sees a tree
// change, not index damage. It reads the node again and shows the vector hit
// with the new text, with no damage warning.
func TestFolderFileSavedDuringTheBuildIsATreeChange(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	logs := captureTestLogs(t, slog.LevelDebug)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	before := "package sample\n// dispatches a request\n"
	after := "package sample\n// dispatches a requesT\n"
	writeFileAt(t, folder, "semantic.go", before)
	target := filepath.Join(folder, "semantic.go")
	snapshot := filepath.Join(t.TempDir(), "snapshot.go")
	writeFixtureFile(t, snapshot, before)
	var opens atomic.Int32
	realOpen := folderOpenFile
	folderOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "semantic.go" && opens.Add(1) == 1 {
			// The first read gets the old bytes; the save lands before the
			// second read.
			file, err := realOpen(snapshot)
			if err == nil {
				writeFixtureFile(t, target, after)
			}
			return file, err
		}
		return realOpen(path)
	}
	t.Cleanup(func() { folderOpenFile = realOpen })
	output, _, err := stubSearch(t, payloadAccept, "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{target, filepath.Join(folder, "strict.go")}
	if !slices.Equal(headerPaths(output), want) || !strings.Contains(output, "requesT") {
		for _, record := range logs.Records() {
			t.Logf("log %s: %s", record.Level, record.Message)
		}
		t.Fatalf("want the vector hit with the new text (opens=%d):\n%s", opens.Load(), output)
	}
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "Index damage") {
			t.Fatalf("a save during the build was taken for damage: %s", record.Message)
		}
	}
}

// TestFolderFileWrittenDuringEveryBuildKeepsTheNode writes a folder file
// between the two reads of every build, as a log or an autosave does. The
// model accepts every row, so the file would print if its rows were searched.
// The retry cannot tell that edit from damage, so the file gives no vector
// hit. The node keeps its strict hit, with no damage warning and no skip.
func TestFolderFileWrittenDuringEveryBuildKeepsTheNode(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	logs := captureTestLogs(t, slog.LevelWarn)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "semantic.go", "package sample\n// version 0\n")
	target := filepath.Join(folder, "semantic.go")
	var opens atomic.Int32
	realOpen := folderOpenFile
	folderOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) != "semantic.go" {
			return realOpen(path)
		}
		// Each read gets the bytes of the moment, then the file changes.
		file, err := realOpen(path)
		if err == nil {
			snapshot := filepath.Join(t.TempDir(), "snapshot.go")
			content, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil {
				return nil, readErr
			}
			writeFixtureFile(t, snapshot, string(content))
			writeFixtureFile(t, target, fmt.Sprintf("package sample\n// version %d\n", opens.Add(1)))
			return realOpen(snapshot)
		}
		return nil, err
	}
	t.Cleanup(func() { folderOpenFile = realOpen })
	output, _, err := stubSearch(t, payloadAccept, "alpha beta", folder)
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	if got, want := headerPaths(output), []string{filepath.Join(folder, "strict.go")}; !slices.Equal(got, want) {
		t.Fatalf("headers=%v, want %v: the node keeps its strict hit, and the file written during every build gives no vector hit:\n%s",
			got, want, output)
	}
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "Index damage") || strings.Contains(record.Message, "Skipped a search node") {
			t.Fatalf("a file written during every build gave %q", record.Message)
		}
	}
}

// TestGitDirtyFileWrittenDuringEveryBuildKeepsTheNode writes a dirty file of a
// Git node after each read of it, as a log or an autosave does. Each read of
// the node then finds a new state, also on the retry. The model accepts every
// row, so a vector hit of the file would print. The node keeps its strict hit
// with no skip: the retry serves the text with no vectors.
func TestGitDirtyFileWrittenDuringEveryBuildKeepsTheNode(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := initGitRepo(t, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, repo, "semantic.go", "package sample\n// committed\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "semantic")
	writeFileAt(t, repo, "semantic.go", "package sample\n// version 0\n")
	t.Chdir(repo)
	logs := captureTestLogs(t, slog.LevelWarn)
	var opens atomic.Int32
	realOpen := dirtyOpenFile
	dirtyOpenFile = func(path string) (*os.File, error) {
		file, err := realOpen(path)
		if err == nil && filepath.Base(path) == "semantic.go" {
			writeFileAt(t, repo, "semantic.go",
				fmt.Sprintf("package sample\n// version %d%s\n", opens.Add(1), strings.Repeat("!", int(opens.Load()))))
		}
		return file, err
	}
	t.Cleanup(func() { dirtyOpenFile = realOpen })
	output, _, err := stubSearch(t, payloadAccept, "alpha beta")
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	if got, want := headerPaths(output), []string{"strict.go"}; opens.Load() < 2 || !slices.Equal(got, want) {
		t.Fatalf("headers=%v (opens=%d), want %v: the node keeps its strict hit, and the dirty file gives no vector hit:\n%s",
			got, opens.Load(), want, output)
	}
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "Index damage") || strings.Contains(record.Message, "Skipped a search node") {
			t.Fatalf("a dirty file written during every build gave %q", record.Message)
		}
	}
}
