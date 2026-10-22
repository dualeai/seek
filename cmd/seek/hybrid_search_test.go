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
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	ctags "github.com/sourcegraph/go-ctags"
)

type hybridTestScorer struct {
	prepareCalls  atomic.Int32
	embedCalls    atomic.Int32
	scoreCalls    atomic.Int32
	closeCalls    atomic.Int32
	embedErr      error
	prepareErr    error
	scoreErr      error
	vectorForUnit func(semanticUnit) semanticVector
	queryVector   *semanticVector
	mu            sync.Mutex
	documents     []rerankDocument
	embeddedPaths []string
}

func runHybridTestSearch(
	t *testing.T,
	queryText string,
	paths []string,
	policy searchPolicy,
	newModel semanticModelFactory,
) (string, error) {
	t.Helper()
	runConfig := searchRunConfig{policy: policy, newModel: newModel}
	return captureStdout(t, func() error {
		return runSearchCommand(
			t.Context(),
			queryText,
			paths,
			0,
			0,
			defaultSearchConfig(),
			runConfig,
		)
	})
}

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
			vector = hybridTestUnitVector(unit)
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

// hybridTestUnitVector puts a SEMANTIC_TARGET unit near the query axis 0 and
// any other unit near axis 1. Small offsets on four more axes, chosen from the
// unit text, keep the vectors of units with different text distinct, also
// after USearch's int8 cast: USearch's graph can leave keys with one shared
// vector unreachable (https://github.com/unum-cloud/USearch/issues/815).
// Units with equal text keep equal vectors, as with the real model. The
// offsets keep a target's cosine with the query above 0.99.
func hybridTestUnitVector(unit semanticUnit) semanticVector {
	axis := 1
	if bytes.Contains(unit.text, []byte("SEMANTIC_TARGET")) {
		axis = 0
	}
	first := sha256.Sum256(unit.text)
	sum := first[:]
	// Four offset axes keep the vectors distinct also after USearch's int8
	// cast, which merges small offsets on few axes.
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
	prepared.scoreMask[0] = true
	return prepared, nil
}

func (scorer *hybridTestScorer) ScoresWithSemanticQuery(
	_ context.Context,
	_ *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	scorer.scoreCalls.Add(1)
	if scorer.scoreErr != nil {
		return nil, scorer.scoreErr
	}
	scorer.mu.Lock()
	scorer.documents = append([]rerankDocument(nil), documents...)
	scorer.mu.Unlock()
	scores := make([]float32, len(documents))
	for index, document := range documents {
		if strings.Contains(document.Text, "SEMANTIC_TARGET") {
			scores[index] = 100
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
// a Git node's shards. A hit shows the line where its unit starts, its context
// and no literal fragment, with Zoekt's language in the result and in the
// model document. A row that does not match its shard copy and a row with no
// document are index damage. A file that Zoekt keeps by name only gives no
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

// TestVectorHitPrintsZoektLanguage finds a shell function by its vector only.
// The embedded unit, the header and the model document all name Zoekt's
// language (Shell), as for a strict hit, not the ctags name (Sh).
func TestVectorHitPrintsZoektLanguage(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "serve.sh", "#!/bin/sh\nserve_request() {\n\t# SEMANTIC_TARGET dispatches a request\n\techo served\n}\n")
	var mu sync.Mutex
	var unitLanguages []string
	scorer := &hybridTestScorer{vectorForUnit: func(unit semanticUnit) semanticVector {
		if bytes.Contains(unit.text, []byte("SEMANTIC_TARGET")) {
			mu.Lock()
			unitLanguages = append(unitLanguages, unit.language)
			mu.Unlock()
		}
		return hybridTestUnitVector(unit)
	}}
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil })
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(unitLanguages, []string{"Shell"}) {
		t.Fatalf("unit languages=%v, want the one unit of Shell", unitLanguages)
	}
	if !strings.Contains(output, "## "+filepath.Join(folder, "serve.sh")+" (Shell)\n") {
		t.Fatalf("the vector hit does not print Zoekt's language:\n%s", output)
	}
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	for _, document := range scorer.documents {
		if document.Path == "serve.sh" && document.Language != "Shell" {
			t.Fatalf("model document language=%q, want Shell", document.Language)
		}
	}
}

// topPathScorer is the hybrid test model, but it gives the document of the
// file top the score of a SEMANTIC_TARGET document.
type topPathScorer struct {
	*hybridTestScorer
	top string
}

func (scorer topPathScorer) ScoresWithSemanticQuery(
	ctx context.Context,
	query *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	scores, err := scorer.hybridTestScorer.ScoresWithSemanticQuery(ctx, query, documents)
	if err != nil {
		return nil, err
	}
	for index, document := range documents {
		if document.Path == scorer.top {
			scores[index] = 100
		}
	}
	return scores, nil
}

// TestTinyFileGetsNoVectorHit gives a 2-byte file the query vector and the
// top model score. Zoekt keeps the file by name only, so it gets no rows and
// prints no vector hit.
func TestTinyFileGetsNoVectorHit(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "semantic.go", "package sample\n// SEMANTIC_TARGET dispatches a request\n")
	writeFileAt(t, folder, "tiny.txt", "x\n")
	scorer := &hybridTestScorer{vectorForUnit: func(unit semanticUnit) semanticVector {
		if unit.path == "tiny.txt" {
			return hybridTestUnitVector(semanticUnit{text: []byte("SEMANTIC_TARGET tiny")})
		}
		return hybridTestUnitVector(unit)
	}}
	model := topPathScorer{hybridTestScorer: scorer, top: "tiny.txt"}
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return model, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, filepath.Join(folder, "semantic.go")) || strings.Contains(output, "tiny.txt") {
		t.Fatalf("want the semantic file and no tiny file:\n%s", output)
	}
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	if slices.Contains(scorer.embeddedPaths, "tiny.txt") {
		t.Fatalf("embedded %v, want no row of tiny.txt", scorer.embeddedPaths)
	}
}

func TestRunDefaultJoinedSearchAddsSemanticOnlyFile(t *testing.T) {
	requireTools(t)
	repo := initEmptyGitRepo(t)
	writeFileAt(t, repo, "strict.go", "package sample\n// alpha beta\n")
	for index := range semanticModelBatchRows {
		writeFileAt(t, repo, fmt.Sprintf("lex-%02d.go", index),
			"package sample\n// alpha lexical candidate\n")
	}
	writeFileAt(t, repo, "semantic.go",
		"package sample\n// SEMANTIC_TARGET dispatches a request to its handler\n")
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

	scorer := &hybridTestScorer{}
	var factoryCalls atomic.Int32
	output, err := runHybridTestSearch(t, "alpha beta", nil, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		factoryCalls.Add(1)
		return scorer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## semantic.go") ||
		!strings.Contains(output, "SEMANTIC_TARGET") {
		t.Fatalf("joined output omitted the semantic-only file:\n%s", output)
	}
	if factoryCalls.Load() != 1 || scorer.prepareCalls.Load() != 1 ||
		scorer.embedCalls.Load() == 0 || scorer.scoreCalls.Load() != 1 ||
		scorer.closeCalls.Load() != 1 {
		t.Fatalf(
			"calls factory=%d prepare=%d embed=%d score=%d close=%d",
			factoryCalls.Load(), scorer.prepareCalls.Load(), scorer.embedCalls.Load(),
			scorer.scoreCalls.Load(), scorer.closeCalls.Load(),
		)
	}
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	foundTargetDocument := false
	for _, document := range scorer.documents {
		if strings.Contains(document.Text, "SEMANTIC_TARGET") {
			foundTargetDocument = true
			break
		}
	}
	if !foundTargetDocument {
		t.Fatal("final MaxSim input omitted the checked semantic unit")
	}

	state := mustGitRepoStateIn(t, t.Context(), repo)
	stateHash := gitCorpusStateHash(paths, state)
	if !plan.joinedGenerationsMatch(stateHash, state.HeadSHA, "") {
		t.Fatalf("default build did not publish %s", filepath.Join(plan.cacheDir, joinedGenerationFile))
	}
}

func TestRunDefaultFolderJoinedSearchAddsSemanticOnlyFile(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	for index := range semanticModelBatchRows {
		writeFileAt(t, folder, fmt.Sprintf("lex-%02d.go", index),
			"package sample\n// alpha lexical candidate\n")
	}
	writeFileAt(t, folder, "semantic.go",
		"package sample\n// SEMANTIC_TARGET dispatches a request to its handler\n")
	plan := planFolderTestCorpus(t, folder)

	scorer := &hybridTestScorer{}
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return scorer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## "+filepath.Join(folder, "semantic.go")) ||
		!strings.Contains(output, "SEMANTIC_TARGET") {
		t.Fatalf("joined folder output omitted the semantic-only file:\n%s", output)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.joinedGenerationsMatch(state, plan.folderSemanticSource(state), "") {
		t.Fatal("default folder search did not keep a joined generation")
	}
}

func TestRunDefaultJoinedSearchAppliesSemanticFilters(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "allowed/strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "allowed/semantic.go",
		"package sample\n// SEMANTIC_TARGET SEMANTIC_ALLOWED dispatches a request\n")
	writeFileAt(t, folder, "allowed/semantic_test.go",
		"package sample\n// FILTERED_TEST_TARGET\n")
	writeFileAt(t, folder, "allowed/semantic.py", "# FILTERED_LANGUAGE_TARGET\n")
	writeFileAt(t, folder, "blocked/semantic.go",
		"package sample\n// FILTERED_PATH_TARGET\n")
	planFolderTestCorpus(t, folder)

	queryVector := semanticVector{0: 1}
	scorer := &hybridTestScorer{
		queryVector: &queryVector,
		vectorForUnit: func(unit semanticUnit) semanticVector {
			if unit.path == "allowed/semantic.go" {
				return semanticVector{0: 0.9, 1: float32(math.Sqrt(0.19))}
			}
			if unit.path != "allowed/strict.go" {
				return semanticVector{0: 1}
			}
			return semanticVector{1: 1}
		},
	}
	output, err := runHybridTestSearch(
		t,
		`alpha beta lang:go file:allowed/ -file:_test\.go$`,
		[]string{folder},
		defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## "+filepath.Join(folder, "allowed/semantic.go")) ||
		!strings.Contains(output, "SEMANTIC_ALLOWED") {
		t.Fatalf("filtered joined output omitted the allowed semantic file:\n%s", output)
	}
	for _, blocked := range []string{
		"allowed/semantic_test.go",
		"allowed/semantic.py",
		"blocked/semantic.go",
		"FILTERED_TEST_TARGET",
		"FILTERED_LANGUAGE_TARGET",
		"FILTERED_PATH_TARGET",
	} {
		if strings.Contains(output, blocked) {
			t.Fatalf("filtered joined output contains %q:\n%s", blocked, output)
		}
	}
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	for _, document := range scorer.documents {
		if !strings.HasPrefix(document.Path, "allowed/") ||
			document.Language != "Go" || strings.HasSuffix(document.Path, "_test.go") {
			t.Fatalf("model received filtered document: %+v", document)
		}
	}
}

func TestRunDefaultFilteredNativeFailureRecoversExactly(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "allowed/strict.go", "package sample\n// seed alpha beta\n")
	// kept lists the files that the filters of the query below keep.
	kept := []string{"allowed/strict.go", "allowed/meaning.go"}
	for index := range semanticFilteredExactRows + 8 {
		path := fmt.Sprintf("allowed/candidate-%03d.go", index)
		writeFileAt(t, folder, path, "package sample\n// alpha lexical candidate\n")
		kept = append(kept, path)
	}
	writeFileAt(t, folder, "allowed/leak_test.go", "package sample\n// alpha beta TEST_LEAK\n")
	writeFileAt(t, folder, "allowed/leak.py", "# alpha beta LANGUAGE_LEAK\n")
	writeFileAt(t, folder, "blocked/leak.go", "package sample\n// alpha beta PATH_LEAK\n")
	// Two files with no query word that only vectors find: only the exact
	// recovery over the allowed rows can add the allowed one, and it must not
	// add the blocked one.
	writeFileAt(t, folder, "allowed/meaning.go", "package sample\n// SEMANTIC_TARGET dispatches a request\n")
	writeFileAt(t, folder, "blocked/meaning.go", "package sample\n// SEMANTIC_TARGET SEMANTIC_PATH_LEAK\n")
	plan := planFolderTestCorpus(t, folder)

	// The seed query gives the model text, so it builds the generation.
	if _, err := runHybridTestSearch(
		t,
		"seed alpha beta",
		[]string{folder},
		defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil },
	); err != nil {
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

	scorer := &hybridTestScorer{}
	logs := captureTestLogs(t, slog.LevelDebug)
	output, err := runHybridTestSearch(
		t,
		`alpha beta lang:go file:allowed/ -file:_test\.go$`,
		[]string{folder},
		defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## "+filepath.Join(folder, "allowed/strict.go")) {
		t.Fatalf("filtered fallback omitted its strict result:\n%s", output)
	}
	if !strings.Contains(output, "## "+filepath.Join(folder, "allowed/meaning.go")) {
		t.Fatalf("exact recovery omitted the allowed semantic-only file:\n%s", output)
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
	for _, document := range scorer.documents {
		if !strings.HasPrefix(document.Path, "allowed/") ||
			document.Language != "Go" || strings.HasSuffix(document.Path, "_test.go") {
			scorer.mu.Unlock()
			t.Fatalf("fallback model received filtered document: %+v", document)
		}
	}
	scorer.mu.Unlock()
	// The --verbose record proves that the damaged graph failed, so the
	// semantic-only file above came from the exact recovery.
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

func TestRunDefaultJoinedSearchReturnsOneSemanticOnlyFile(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "semantic.go",
		"package sample\n// SEMANTIC_TARGET dispatches a request to its handler\n")
	planFolderTestCorpus(t, folder)

	scorer := &hybridTestScorer{}
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return scorer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## "+filepath.Join(folder, "semantic.go")) ||
		!strings.Contains(output, "SEMANTIC_TARGET") {
		t.Fatalf("single semantic-only result was omitted:\n%s", output)
	}
	if scorer.scoreCalls.Load() != 1 {
		t.Fatalf("final score calls=%d, want 1", scorer.scoreCalls.Load())
	}
}

// TestRunDefaultNegationQuerySkipsJoinedIndex checks that a query of
// negations only, which gives the model no text, builds no vector generation
// and starts no model, and that a warm search gives the same lexical answer.
func TestRunDefaultNegationQuerySkipsJoinedIndex(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// alpha\n")
	plan := planFolderTestCorpus(t, folder)

	scorer := &hybridTestScorer{}
	var factoryCalls atomic.Int32
	newModel := func(context.Context) (semanticModel, error) {
		factoryCalls.Add(1)
		return scorer, nil
	}
	output, err := runHybridTestSearch(t, "-file:missing", []string{folder}, defaultSearchPolicy(), newModel)
	if err != nil || !strings.Contains(output, "## "+filepath.Join(folder, "app.go")) {
		t.Fatalf("negation search: error=%v output=%q", err, output)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if plan.joinedGenerationsMatch(state, plan.folderSemanticSource(state), "") {
		t.Fatal("a negation query built a joined index that no route can read")
	}
	if got := scorer.embedCalls.Load(); got != 0 {
		t.Fatalf("a negation query embedded %d times", got)
	}
	firstEmbedCalls := scorer.embedCalls.Load()
	warmOutput, err := runHybridTestSearch(t, "-file:missing", []string{folder}, defaultSearchPolicy(), newModel)
	if err != nil || warmOutput != output {
		t.Fatalf("warm negation search: error=%v output=%q, want %q", err, warmOutput, output)
	}
	if factoryCalls.Load() != 0 || scorer.embedCalls.Load() != firstEmbedCalls {
		t.Fatalf(
			"warm model calls factory=%d embed=%d, want 0 and %d",
			factoryCalls.Load(),
			scorer.embedCalls.Load(),
			firstEmbedCalls,
		)
	}
}

func TestRunDefaultRepairsDamagedSemanticGeneration(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// alpha\n")
	plan := planFolderTestCorpus(t, folder)

	run := func(scorer *hybridTestScorer) string {
		t.Helper()
		// The query gives the model text, so it builds the generation that
		// this test then damages and repairs.
		output, err := runHybridTestSearch(t, "alpha sample package", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
			return scorer, nil
		})
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

func TestRunDefaultRerankFallbackLogsCause(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha\n")
	wantErr := errors.New("test score failure")
	logs := captureTestLogs(t, slog.LevelDebug)
	scorer := &hybridTestScorer{scoreErr: wantErr}

	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return scorer, nil
	})
	if err != nil || !strings.Contains(output, "## "+filepath.Join(folder, "strict.go")) {
		t.Fatalf("fallback search: error=%v output=%q", err, output)
	}
	assertTestLogError(t, logs, "Model stages failed; using strict order", wantErr)
	if scorer.embedCalls.Load() != 1 || scorer.prepareCalls.Load() != 1 || scorer.scoreCalls.Load() != 1 {
		t.Fatalf("fallback calls embed=%d prepare=%d score=%d", scorer.embedCalls.Load(), scorer.prepareCalls.Load(), scorer.scoreCalls.Load())
	}
}

func TestRunDefaultSemanticBuildFailureKeepsLexicalSearchAndCanRepair(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha\n")
	writeFileAt(t, folder, "semantic.go", "package sample\n// SEMANTIC_TARGET dispatches the request\n")
	plan := planFolderTestCorpus(t, folder)
	lexicalOutput, err := runHybridTestSearch(t, "alpha beta", []string{folder}, lexicalOnlySearchPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("test semantic build failure")
	logs := captureTestLogs(t, slog.LevelDebug)
	scorer := &hybridTestScorer{embedErr: wantErr}

	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return scorer, nil
	})
	// The node gives its strict hits; a relaxed match alone is no candidate.
	if err != nil || !strings.Contains(output, "## "+filepath.Join(folder, "strict.go")) || strings.Contains(output, "## "+filepath.Join(folder, "relaxed.go")) {
		t.Fatalf("semantic build fallback: error=%v output=%q; lexical control=%q", err, output, lexicalOutput)
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

	repairScorer := &hybridTestScorer{}
	repairOutput, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return repairScorer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(repairOutput, "## "+filepath.Join(folder, "semantic.go")) ||
		!strings.Contains(repairOutput, "SEMANTIC_TARGET") {
		t.Fatalf("repaired joined search omitted semantic result:\n%s", repairOutput)
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

func TestRunDefaultSemanticQueryFailureUsesLexicalOutput(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha\n")
	planFolderTestCorpus(t, folder)

	// The text index is warm; the failing search builds the vectors.
	if _, err := runHybridTestSearch(t, "alpha", []string{folder}, lexicalOnlySearchPolicy(), nil); err != nil {
		t.Fatal(err)
	}
	lexicalOutput, err := runHybridTestSearch(t, "alpha beta", []string{folder}, lexicalOnlySearchPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("test semantic query failure")
	logs := captureTestLogs(t, slog.LevelDebug)
	scorer := &hybridTestScorer{prepareErr: wantErr}
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return scorer, nil
	})
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

// TestDirtyGitSearchUsesCommittedVectors searches a dirty work tree: the node
// builds or reuses the generation of its commit, semantic retrieval finds a
// clean file through it, the dirty file shows its uncommitted bytes, and the
// next clean search embeds nothing new. The dirty bytes reach vectors through
// the overlay.
func TestDirtyGitSearchUsesCommittedVectors(t *testing.T) {
	requireTools(t)
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing-vectors=%t", seeded), func(t *testing.T) {
			repo := initGitRepo(t, "app.go", "package sample\n// alpha beta\n")
			writeFileAt(t, repo, "target.go", "package sample\n// SEMANTIC_TARGET dispatches the request\n")
			gitRunIn(t, repo, "add", ".")
			gitRunIn(t, repo, "commit", "-m", "semantic target")
			setTestUserCache(t)
			if seeded {
				if _, err := runHybridTestSearch(t, "alpha beta", []string{repo}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
					return &hybridTestScorer{}, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			writeFileAt(t, repo, "app.go", "package sample\n// alpha beta DIRTY_MARKER\n")
			dirty := &hybridTestScorer{}
			out, err := runHybridTestSearch(t, "alpha beta", []string{repo}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
				return dirty, nil
			})
			if err != nil || !strings.Contains(out, "DIRTY_MARKER") || !strings.Contains(out, "[uncommitted]") ||
				!strings.Contains(out, "## "+filepath.Join(repo, "target.go")) {
				t.Fatalf("dirty output=%q error=%v, want the dirty file and the semantic target", out, err)
			}
			// The overlay embeds the dirty file; the committed base
			// embeds once, before or here.
			wantEmbeds := int32(2)
			if seeded {
				wantEmbeds = 1
			}
			if dirty.embedCalls.Load() != wantEmbeds || dirty.scoreCalls.Load() != 1 {
				t.Fatalf("dirty calls embed=%d score=%d, want %d and 1", dirty.embedCalls.Load(), dirty.scoreCalls.Load(), wantEmbeds)
			}
			gitRunIn(t, repo, "checkout", "--", "app.go")
			clean := &hybridTestScorer{}
			out, err = runHybridTestSearch(t, "alpha beta", []string{repo}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
				return clean, nil
			})
			if err != nil || !strings.Contains(out, "## "+filepath.Join(repo, "app.go")) || strings.Contains(out, "DIRTY_MARKER") {
				t.Fatalf("clean output=%q error=%v", out, err)
			}
			if clean.embedCalls.Load() != 0 {
				t.Fatalf("next clean embeds=%d, want 0", clean.embedCalls.Load())
			}
		})
	}
}

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

func TestRunLexicalOnlyBuildsNoSemanticDataAndStartsNoModel(t *testing.T) {
	requireTools(t)
	repo := initGitRepo(t, "app.go", "package sample\n// alpha beta\n")
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

	var factoryCalls atomic.Int32
	output, err := runHybridTestSearch(t, "alpha beta", nil, lexicalOnlySearchPolicy(), func(context.Context) (semanticModel, error) {
		factoryCalls.Add(1)
		return nil, fmt.Errorf("model must not start")
	})
	if err != nil || !strings.Contains(output, "## app.go") {
		t.Fatalf("lexical search: error=%v output=%q", err, output)
	}
	if factoryCalls.Load() != 0 {
		t.Fatalf("model factory calls=%d, want 0", factoryCalls.Load())
	}
	if _, err := os.Lstat(filepath.Join(plan.cacheDir, joinedGenerationFile)); !os.IsNotExist(err) {
		t.Fatalf("lexical-only search created a joined descriptor: %v", err)
	}
	semanticDirs := semanticGenerationDirs(t, plan.indexDir)
	if len(semanticDirs) != 0 {
		t.Fatalf("lexical-only search created semantic data: %v", semanticDirs)
	}
}

func TestDefaultFolderBuildPublishesAndUpdatesJoinedGeneration(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// first semantic folder text\n")
	plan := planFolderTestCorpus(t, folder)
	scorer := &hybridTestScorer{}
	execution := testSemanticSearchExecution(t, func(context.Context) (semanticModel, error) {
		return scorer, nil
	})

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

func TestLexicalOnlyFolderBuildStartsNoModelAndWritesNoSemanticData(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// lexical folder text\n")
	plan := planFolderTestCorpus(t, folder)
	var factoryCalls atomic.Int32
	future := newSemanticModelFuture(func(context.Context) (semanticModel, error) {
		factoryCalls.Add(1)
		return nil, fmt.Errorf("model must not start")
	})
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
// guard on one large file. Zoekt keeps only its name, and a vector hit shows
// the text that Zoekt keeps, so the description search prints no vector hit of
// the file. The name-only notice counts it, as for a strict search.
func TestRunDefaultFolderJoinedSearchKeepsGuardedFileByName(t *testing.T) {
	requireTools(t)
	if testing.Short() {
		t.Skip("indexes a 3 MiB file")
	}
	swapGuardLimitsForTest(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	for index := range semanticModelBatchRows {
		writeFileAt(t, folder, fmt.Sprintf("lex-%02d.go", index),
			"package sample\n// alpha lexical candidate\n")
	}
	const needle = "guarded_semantic_needle_marker"
	// Go comment lines keep the file valid source for ctags. Every line carries
	// the text that the test scorer targets, so each of the file's units is a
	// semantic match and the test does not depend on how a long file ranks.
	var guarded strings.Builder
	guarded.WriteString("package sample\n// " + needle + " SEMANTIC_TARGET dispatches a request to its handler\n")
	for _, line := range strings.SplitAfter(string(randomWordText(5, 5<<19, "")), "\n") {
		if line != "" && line != "\n" {
			guarded.WriteString("// SEMANTIC_TARGET " + line)
		}
	}
	writeFileAt(t, folder, "guarded.go", guarded.String())

	scorer := &hybridTestScorer{}
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return scorer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "guarded.go") || !strings.Contains(output, nameOnlyNoticeLine(1)) {
		t.Fatalf("want no vector hit of the guarded file and the name-only notice:\n%s", output)
	}

	// The text index holds only the name: the exact term has no match there.
	lexical, err := runHybridTestSearch(t, needle, []string{folder}, lexicalOnlySearchPolicy(), nil)
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("lexical search of the guarded file: err=%v, output:\n%s", err, lexical)
	}
}

// TestEditDuringSearchShowsInTheNextSearch edits a tracked file after the
// graph expansion captured the repository, while a warm descriptive search
// runs (the model factory runs after planning). The search reads one capture
// per node (one git status per node per command), so it shows the captured
// content; the next search captures the edit and shows it.
func TestEditDuringSearchShowsInTheNextSearch(t *testing.T) {
	requireTools(t)
	repo := initGitRepo(t, "a.go", "package sample\n// alpha beta original line\n")
	setTestUserCache(t)
	t.Chdir(repo)
	newModel := func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil }
	if _, err := runHybridTestSearch(t, "alpha beta", nil, defaultSearchPolicy(), newModel); err != nil {
		t.Fatalf("warm-up search: %v", err)
	}
	output, err := runHybridTestSearch(t, "alpha beta", nil, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		writeFileAt(t, repo, "a.go", "package sample\n// alpha beta ZQEDITEDDURINGSEARCH\n")
		return &hybridTestScorer{}, nil
	})
	if err != nil || !strings.Contains(output, "original line") || strings.Contains(output, "ZQEDITEDDURINGSEARCH") {
		t.Fatalf("the search did not show its capture: err=%v\n%s", err, output)
	}
	output, err = runHybridTestSearch(t, "alpha beta", nil, defaultSearchPolicy(), newModel)
	if err != nil || !strings.Contains(output, "ZQEDITEDDURINGSEARCH") {
		t.Fatalf("the next search does not show the edit: err=%v\n%s", err, output)
	}
}

// TestFolderFileSavedDuringTheBuildIsATreeChange saves a folder file between
// the two reads of one build: Zoekt indexes the old bytes and the vectors the
// new ones. The search sees a tree change, not index damage: it reads the
// node again and shows the vector hit with the new text, with no damage
// warning.
func TestFolderFileSavedDuringTheBuildIsATreeChange(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	logs := captureTestLogs(t, slog.LevelDebug)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	before := "package sample\n// SEMANTIC_TARGET dispatches a request\n"
	after := "package sample\n// SEMANTIC_TARGET dispatches a requesT\n"
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
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "semantic.go") || !strings.Contains(output, "requesT") {
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
// retry cannot tell that edit from damage, so the file gives no vector hit,
// and the node keeps its strict hit with no damage warning and no skip.
func TestFolderFileWrittenDuringEveryBuildKeepsTheNode(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	logs := captureTestLogs(t, slog.LevelWarn)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "semantic.go", "package sample\n// SEMANTIC_TARGET version 0\n")
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
			writeFixtureFile(t, target, fmt.Sprintf("package sample\n// SEMANTIC_TARGET version %d\n", opens.Add(1)))
			return realOpen(snapshot)
		}
		return nil, err
	}
	t.Cleanup(func() { folderOpenFile = realOpen })
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil })
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	if !strings.Contains(output, "strict.go") {
		t.Fatalf("the node lost its strict hit:\n%s", output)
	}
	if strings.Contains(output, "semantic.go") {
		t.Fatalf("the file written during every build gave a vector hit:\n%s", output)
	}
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "Index damage") || strings.Contains(record.Message, "Skipped a search node") {
			t.Fatalf("a file written during every build gave %q", record.Message)
		}
	}
}

// TestGitDirtyFileWrittenDuringEveryBuildKeepsTheNode writes a dirty file of a
// Git node after each read of it, as a log or an autosave does. Each read of
// the node then finds a new state, also on the retry. The node keeps its
// strict hit with no skip: the retry serves the text with no vectors.
func TestGitDirtyFileWrittenDuringEveryBuildKeepsTheNode(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := initGitRepo(t, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, repo, "semantic.go", "package sample\n// SEMANTIC_TARGET committed\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "semantic")
	writeFileAt(t, repo, "semantic.go", "package sample\n// SEMANTIC_TARGET version 0\n")
	t.Chdir(repo)
	logs := captureTestLogs(t, slog.LevelWarn)
	var opens atomic.Int32
	realOpen := dirtyOpenFile
	dirtyOpenFile = func(path string) (*os.File, error) {
		file, err := realOpen(path)
		if err == nil && filepath.Base(path) == "semantic.go" {
			writeFileAt(t, repo, "semantic.go",
				fmt.Sprintf("package sample\n// SEMANTIC_TARGET version %d%s\n", opens.Add(1), strings.Repeat("!", int(opens.Load()))))
		}
		return file, err
	}
	t.Cleanup(func() { dirtyOpenFile = realOpen })
	output, err := runHybridTestSearch(t, "alpha beta", nil, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil })
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	if opens.Load() < 2 || !strings.Contains(output, "strict.go") {
		t.Fatalf("the node lost its strict hit (opens=%d):\n%s", opens.Load(), output)
	}
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "Index damage") || strings.Contains(record.Message, "Skipped a search node") {
			t.Fatalf("a dirty file written during every build gave %q", record.Message)
		}
	}
}
