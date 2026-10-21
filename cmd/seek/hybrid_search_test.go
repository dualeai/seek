package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
		vector := semanticVector{1: 1}
		if scorer.vectorForUnit != nil {
			vector = scorer.vectorForUnit(unit)
		} else if bytes.Contains(unit.text, []byte("SEMANTIC_TARGET")) {
			vector = semanticVector{0: 1}
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

func TestMaterializeSemanticCandidatesChecksCommittedBytes(t *testing.T) {
	requireTools(t)
	const content = "package sample\n\nfunc ServeRequest() {\n\t// checked semantic evidence\n}\n"
	repo := initGitRepo(t, "app.go", content)
	paths, plan := planGitTestCorpus(t, repo)
	state := mustGitRepoStateIn(t, context.Background(), repo)
	units := extractSemanticUnits("app.go", []byte(content), nil, nil)
	if len(units) != 1 {
		t.Fatalf("unit count=%d, want 1", len(units))
	}
	units[0].row = 0
	units[0].text = nil
	generation := &semanticGeneration{rows: units}
	selected, err := semanticHitFiles(
		generation,
		[]semanticHit{{row: 0, score: 0.75}},
	)
	if err != nil {
		t.Fatal(err)
	}

	candidates, err := materializeSemanticCandidates(
		t.Context(),
		plan,
		paths,
		state.HeadSHA,
		selected,
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].document.Text != content {
		t.Fatalf("materialized candidate=%+v", candidates)
	}
	match := candidates[0].result.file.LineMatches[0]
	if len(match.LineFragments) != 0 {
		t.Fatal("semantic evidence must not claim a literal match")
	}
	if !bytes.Equal(match.Line, []byte("package sample\n")) || match.LineNumber != 1 {
		t.Fatalf("semantic evidence line=%q number=%d", match.Line, match.LineNumber)
	}

	broken := &semanticGeneration{rows: append([]semanticUnit(nil), generation.rows...)}
	broken.rows[0].contentID = semanticContentID(sha256.Sum256([]byte("different")))
	brokenSelected, err := semanticHitFiles(
		broken,
		[]semanticHit{{row: 0, score: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializeSemanticCandidates(
		t.Context(), plan, paths, state.HeadSHA, brokenSelected, 0,
	); err == nil {
		t.Fatal("wrong content ID must reject semantic evidence")
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
	if !plan.joinedGenerationsMatch(state, state, "") {
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

	// A re-rank eligible seed builds the generation; a one-word query does not.
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
	semanticDir := semanticGenerationDir(plan.indexDir, state)
	generation, err := openSemanticGenerationForSearch(semanticDir, state)
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

	manifest, err := readSemanticManifest(semanticDir, state)
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
	if !plan.joinedGenerationsMatch(state, state, "") {
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

// TestRunDefaultExactQuerySkipsJoinedIndex checks that a one-word or exact
// query, which is not re-rank eligible, builds no vector generation and starts
// no model, and that a warm search gives the same lexical answer.
func TestRunDefaultExactQuerySkipsJoinedIndex(t *testing.T) {
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
	output, err := runHybridTestSearch(t, "alpha", []string{folder}, defaultSearchPolicy(), newModel)
	if err != nil || !strings.Contains(output, "## "+filepath.Join(folder, "app.go")) {
		t.Fatalf("exact search: error=%v output=%q", err, output)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if plan.joinedGenerationsMatch(state, state, "") {
		t.Fatal("an exact query built a joined index that no route can read")
	}
	if got := scorer.embedCalls.Load(); got != 0 {
		t.Fatalf("an exact query embedded %d times", got)
	}
	firstEmbedCalls := scorer.embedCalls.Load()
	warmOutput, err := runHybridTestSearch(t, "alpha", []string{folder}, defaultSearchPolicy(), newModel)
	if err != nil || warmOutput != output {
		t.Fatalf("warm exact search: error=%v output=%q, want %q", err, warmOutput, output)
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
		// A multi-word description is re-rank eligible, so it builds the
		// generation this test then damages and repairs.
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
		semanticGenerationDir(plan.indexDir, state),
		semanticManifestFile,
	)
	if err := os.WriteFile(manifestPath, []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if plan.joinedGenerationsMatch(state, state, "") {
		t.Fatal("damaged semantic data stayed active")
	}

	repair := &hybridTestScorer{}
	if got := run(repair); got != wantOutput {
		t.Fatalf("repair output=%q, want %q", got, wantOutput)
	}
	if repair.embedCalls.Load() == 0 {
		t.Fatal("repair did not rebuild semantic data")
	}
	requireSearchableGeneration(t, plan, state)
	if !plan.joinedGenerationsMatch(state, state, "") {
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
		!plan.joinedGenerationsMatch(state, state, "") {
		t.Fatal("a later healthy run did not repair the joined index")
	}
	requireSearchableGeneration(t, plan, state)
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

	if _, err := runHybridTestSearch(t, "alpha", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return &hybridTestScorer{}, nil
	}); err != nil {
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
	if !plan.joinedGenerationsMatch(firstState, firstState, "") {
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
	if !plan.joinedGenerationsMatch(secondState, secondState, "") {
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

// TestRunDefaultFolderJoinedSearchFindsGuardedFileByMeaning forces the shard
// guard on one large file. Its text index keeps only the name, but the
// meaning-based index reads the file itself and has no shard format limit, so a
// descriptive search must still show the file's semantic evidence.
func TestRunDefaultFolderJoinedSearchFindsGuardedFileByMeaning(t *testing.T) {
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
	if !strings.Contains(output, "## "+filepath.Join(folder, "guarded.go")) || !strings.Contains(output, "SEMANTIC_TARGET") {
		t.Fatalf("joined output omitted the guarded file's semantic evidence:\n%s", output)
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
