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

	"github.com/sourcegraph/zoekt"
)

type hybridTestScorer struct {
	prepareCalls   atomic.Int32
	embedCalls     atomic.Int32
	scoreCalls     atomic.Int32
	closeCalls     atomic.Int32
	embedErr       error
	prepareErr     error
	queryTruncated bool
	vectorForUnit  func(semanticUnit) semanticVector
	queryVector    *semanticVector
	mu             sync.Mutex
	documents      []rerankDocument
	embeddedPaths  []string
}

func (*hybridTestScorer) PackSemanticUnits(
	_ context.Context,
	units []semanticUnit,
) ([]semanticUnit, error) {
	return units, nil
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
	if policy.semanticEnabled() {
		// These tests isolate joined retrieval and index behavior. Process tests
		// cover the calibrated production policies.
		runConfig.rerankAcceptance = alwaysAcceptRerankPolicyForTest()
		runConfig.hybridAcceptance = alwaysAcceptRerankPolicyForTest()
	}
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

func (scorer *hybridTestScorer) Scores(
	ctx context.Context,
	query string,
	documents []rerankDocument,
) ([]float32, error) {
	return scoreWithTestSemanticEmbedder(ctx, scorer, query, documents)
}

func assertIndexOnlyModelCalls(t *testing.T, scorer *hybridTestScorer) {
	t.Helper()
	if scorer.embedCalls.Load() == 0 || scorer.prepareCalls.Load() != 0 ||
		scorer.scoreCalls.Load() != 0 {
		t.Fatalf(
			"model calls embed=%d prepare=%d score=%d",
			scorer.embedCalls.Load(),
			scorer.prepareCalls.Load(),
			scorer.scoreCalls.Load(),
		)
	}
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
		truncated:  scorer.queryTruncated,
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

func TestBuildHybridRerankCandidatesProtectsBothBranches(t *testing.T) {
	strict := []corpusSearchResult{rerankTestResult("lex-00.go", 100)}
	relaxed := make([]corpusSearchResult, rerankCandidateLimit)
	semantic := make([]semanticFileCandidate, rerankCandidateLimit)
	for index := range rerankCandidateLimit {
		relaxed[index] = rerankTestResult(
			"lex-"+twoDigitTestNumber(index)+".go",
			float64(rerankCandidateLimit-index),
		)
		semantic[index] = semanticFileCandidate{
			result:   rerankTestResult("sem-"+twoDigitTestNumber(index)+".go", 0),
			document: rerankDocument{Text: "semantic " + twoDigitTestNumber(index)},
		}
	}

	candidates := buildHybridRerankCandidates(strict, relaxed, semantic)
	if len(candidates) != rerankCandidateLimit {
		t.Fatalf("candidate count=%d, want %d", len(candidates), rerankCandidateLimit)
	}
	lexicalCount, semanticCount := 0, 0
	for _, candidate := range candidates {
		switch {
		case strings.HasPrefix(candidate.result.file.FileName, "lex-"):
			lexicalCount++
		case strings.HasPrefix(candidate.result.file.FileName, "sem-"):
			semanticCount++
			if !candidate.hasDocument || candidate.lexicalRank != hybridMissingLexicalRank {
				t.Fatalf("semantic-only candidate=%+v", candidate)
			}
		}
	}
	if lexicalCount != hybridBranchQuota || semanticCount != hybridBranchQuota {
		t.Fatalf("branch counts lexical=%d semantic=%d, want %d each",
			lexicalCount, semanticCount, hybridBranchQuota)
	}
}

func TestBuildHybridRerankCandidatesUsesCheckedUnitForLexicalFile(t *testing.T) {
	lexical := rerankTestResult("shared.go", 10)
	semanticResult := rerankTestResult("shared.go", 0)
	semanticResult.file.LineMatches[0].Line = []byte("semantic evidence\n")
	candidates := buildHybridRerankCandidates(
		nil,
		[]corpusSearchResult{lexical, rerankTestResult("other.go", 9)},
		[]semanticFileCandidate{{
			result:   semanticResult,
			document: rerankDocument{Text: "checked source unit"},
		}},
	)
	if len(candidates) != 2 {
		t.Fatalf("candidate count=%d, want 2", len(candidates))
	}
	shared := candidates[0]
	if shared.result.file.Score != lexical.file.Score ||
		string(shared.result.file.LineMatches[0].Line) != string(lexical.file.LineMatches[0].Line) {
		t.Fatalf("semantic overlap replaced lexical display: %+v", shared.result.file)
	}
	if !shared.hasDocument || shared.document.Text != "checked source unit" || shared.lexicalRank != 1 {
		t.Fatalf("semantic overlap did not replace score input: %+v", shared)
	}
}

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
	selected, err := selectSemanticFileCandidates(
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
	brokenSelected, err := selectSemanticFileCandidates(
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

func TestSelectHybridSemanticCandidatesSkipsUnusedFiles(t *testing.T) {
	plan := corpusPlan{id: "test", kind: corpusKindFolder}
	strict := []zoekt.FileMatch{{FileName: "lex-000.go", Score: 200}}
	relaxed := make([]zoekt.FileMatch, rerankCandidateLimit)
	semantic := make([]semanticFileCandidate, rerankCandidateLimit)
	for index := range rerankCandidateLimit {
		relaxed[index] = zoekt.FileMatch{
			FileName: fmt.Sprintf("lex-%03d.go", index),
			Score:    float64(rerankCandidateLimit - index),
		}
		semantic[index].unit.path = fmt.Sprintf("sem-%03d.go", index)
	}

	selected := selectHybridSemanticCandidates(plan, strict, relaxed, semantic)
	if len(selected) != hybridBranchQuota {
		t.Fatalf("selected semantic files=%d, want %d", len(selected), hybridBranchQuota)
	}
	for index, candidate := range selected {
		want := fmt.Sprintf("sem-%03d.go", index)
		if candidate.unit.path != want {
			t.Fatalf("selected semantic file %d=%q, want %q", index, candidate.unit.path, want)
		}
	}

	for index := range semantic {
		semantic[index].unit.path = relaxed[index].FileName
	}
	selected = selectHybridSemanticCandidates(plan, strict, relaxed, semantic)
	// The last lexical file fills the batch before its semantic duplicate is
	// visited, so that file uses lexical evidence and needs no source read.
	if len(selected) != rerankCandidateLimit-1 {
		t.Fatalf("overlapping semantic files=%d, want %d", len(selected), rerankCandidateLimit-1)
	}
}

func TestRunDefaultJoinedSearchAddsSemanticOnlyFile(t *testing.T) {
	requireTools(t)
	repo := initEmptyGitRepo(t)
	writeFileAt(t, repo, "strict.go", "package sample\n// alpha beta\n")
	for index := range rerankCandidateLimit {
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
	if !joinedGenerationMatches(
		plan.cacheDir, plan.indexDir, stateHash, state.HeadSHA,
	) {
		t.Fatalf("default build did not publish %s", filepath.Join(plan.cacheDir, joinedGenerationFile))
	}
}

func TestRunJoinedAcceptanceRejectionDoesNotUseLexicalFallback(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "alpha.go", "package sample\n// alpha only\n")
	writeFileAt(t, folder, "beta.go", "package sample\n// beta only\n")
	setTestUserCache(t)

	scorer := &hybridTestScorer{}
	output, err := captureStdout(t, func() error {
		return runSearchCommand(
			t.Context(),
			"alpha beta",
			[]string{folder},
			0,
			0,
			defaultSearchConfig(),
			searchRunConfig{
				policy:           defaultSearchPolicy(),
				hybridAcceptance: newRerankAcceptancePolicy(0.6),
				newModel: func(context.Context) (semanticModel, error) {
					return scorer, nil
				},
			},
		)
	})
	if !errors.Is(err, errNoMatch) || output != "" {
		t.Fatalf("joined rejection output=%q error=%v, want no match", output, err)
	}
	if scorer.scoreCalls.Load() != 1 {
		t.Fatalf("joined score calls=%d, want 1", scorer.scoreCalls.Load())
	}
}

func TestRunJoinedTruncatedQuerySkipsExpansionBranches(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha only\n")
	setTestUserCache(t)
	planFolderTestCorpus(t, folder)

	if _, err := runHybridTestSearch(
		t,
		"alpha",
		[]string{folder},
		defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil },
	); err != nil {
		t.Fatal(err)
	}
	scorer := &hybridTestScorer{queryTruncated: true}
	output, err := runHybridTestSearch(
		t,
		"alpha beta",
		[]string{folder},
		defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil },
	)
	if err != nil || !strings.Contains(output, "strict.go") || strings.Contains(output, "relaxed.go") {
		t.Fatalf("truncated joined output=%q error=%v", output, err)
	}
	if scorer.prepareCalls.Load() != 1 || scorer.scoreCalls.Load() != 0 ||
		scorer.embedCalls.Load() != 0 {
		t.Fatalf(
			"model calls: prepare=%d score=%d embed=%d, want 1, 0, 0",
			scorer.prepareCalls.Load(),
			scorer.scoreCalls.Load(),
			scorer.embedCalls.Load(),
		)
	}
}

func TestRunDefaultFolderJoinedSearchAddsSemanticOnlyFile(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	for index := range rerankCandidateLimit {
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
	if !strings.Contains(output, "## semantic.go") ||
		!strings.Contains(output, "SEMANTIC_TARGET") {
		t.Fatalf("joined folder output omitted the semantic-only file:\n%s", output)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !joinedGenerationMatches(plan.cacheDir, plan.indexDir, state, state) {
		t.Fatal("default folder search did not keep a joined generation")
	}
}

func TestRunDefaultJoinedSearchAppliesSemanticFilters(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "allowed/strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "allowed/semantic.go",
		"package sample\n// SEMANTIC_ALLOWED dispatches a request\n")
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
		`alpha beta lang:go file:^allowed/ -file:_test\.go$`,
		[]string{folder},
		defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## allowed/semantic.go") ||
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

func TestRunDefaultFilteredNativeFailureUsesFilteredLexicalRerank(t *testing.T) {
	requireTools(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "allowed/strict.go", "package sample\n// seed alpha beta\n")
	for index := range semanticFilteredExactRows + 8 {
		writeFileAt(
			t,
			folder,
			fmt.Sprintf("allowed/candidate-%03d.go", index),
			"package sample\n// alpha lexical candidate\n",
		)
	}
	writeFileAt(t, folder, "allowed/leak_test.go", "package sample\n// alpha beta TEST_LEAK\n")
	writeFileAt(t, folder, "allowed/leak.py", "# alpha beta LANGUAGE_LEAK\n")
	writeFileAt(t, folder, "blocked/leak.go", "package sample\n// alpha beta PATH_LEAK\n")
	plan := planFolderTestCorpus(t, folder)

	if _, err := runHybridTestSearch(
		t,
		"seed",
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
	generation, err := openSemanticGeneration(semanticDir, state)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := parseSearchQueryForms(
		`alpha beta lang:go file:^allowed/ -file:_test\.go$`,
	)
	if err != nil {
		t.Fatal(err)
	}
	rerankPlan, ok := planRerankQuery("", raw)
	if !ok || rerankPlan.semanticFilter == nil {
		t.Fatal("filtered fallback query is not eligible")
	}
	mask, err := buildSemanticFilterMask(t.Context(), generation, rerankPlan.semanticFilter)
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
	// Damage the graph, then refresh its manifest digest. This bypasses the
	// open-time artifact check so the query reaches the native search error and
	// filtered lexical fallback.
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
	if !joinedGenerationMatches(plan.cacheDir, plan.indexDir, state, state) {
		t.Fatal("fault fixture did not keep the joined generation eligible")
	}

	scorer := &hybridTestScorer{}
	logs := captureTestLogs(t, slog.LevelDebug)
	output, err := runHybridTestSearch(
		t,
		`alpha beta lang:go file:^allowed/ -file:_test\.go$`,
		[]string{folder},
		defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## allowed/strict.go") {
		t.Fatalf("filtered fallback omitted its strict result:\n%s", output)
	}
	for _, leak := range []string{"leak_test.go", "leak.py", "blocked/leak.go", "TEST_LEAK", "LANGUAGE_LEAK", "PATH_LEAK"} {
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
	foundFallbackLog := false
	for _, record := range logs.Records() {
		if record.Message == "Joined search failed; using the lexical path" {
			foundFallbackLog = true
			break
		}
	}
	if !foundFallbackLog {
		t.Fatal("filtered native failure did not enter the lexical fallback")
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
	if !strings.Contains(output, "## semantic.go") ||
		!strings.Contains(output, "SEMANTIC_TARGET") {
		t.Fatalf("single semantic-only result was omitted:\n%s", output)
	}
	if scorer.scoreCalls.Load() != 1 {
		t.Fatalf("final score calls=%d, want 1", scorer.scoreCalls.Load())
	}
}

func TestRunDefaultExactQueryStillBuildsJoinedIndex(t *testing.T) {
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
	if err != nil || !strings.Contains(output, "## app.go") {
		t.Fatalf("exact search: error=%v output=%q", err, output)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !joinedGenerationMatches(plan.cacheDir, plan.indexDir, state, state) {
		t.Fatal("default exact query did not build the joined index")
	}
	assertIndexOnlyModelCalls(t, scorer)
	firstEmbedCalls := scorer.embedCalls.Load()
	warmOutput, err := runHybridTestSearch(t, "alpha", []string{folder}, defaultSearchPolicy(), newModel)
	if err != nil || warmOutput != output {
		t.Fatalf("warm exact search: error=%v output=%q, want %q", err, warmOutput, output)
	}
	if factoryCalls.Load() != 1 || scorer.embedCalls.Load() != firstEmbedCalls {
		t.Fatalf(
			"warm model calls factory=%d embed=%d, want 1 and %d",
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
		output, err := runHybridTestSearch(t, "alpha", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
			return scorer, nil
		})
		if err != nil || !strings.Contains(output, "## app.go") {
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
	if joinedGenerationMatches(plan.cacheDir, plan.indexDir, state, state) {
		t.Fatal("damaged semantic data stayed active")
	}

	repair := &hybridTestScorer{}
	if got := run(repair); got != wantOutput {
		t.Fatalf("repair output=%q, want %q", got, wantOutput)
	}
	if repair.embedCalls.Load() == 0 {
		t.Fatal("repair did not rebuild semantic data")
	}
	generation, err := openSemanticGeneration(
		semanticGenerationDir(plan.indexDir, state),
		state,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = generation.Close() }()
	if generation.usearchErr != nil {
		t.Fatalf("repaired USearch data: %v", generation.usearchErr)
	}
	if !joinedGenerationMatches(plan.cacheDir, plan.indexDir, state, state) {
		t.Fatal("repair did not activate the joined generation")
	}
}

func TestRunDefaultOverCapScopedQueryBuildsJoinedIndex(t *testing.T) {
	requireTools(t)
	repo := initGitRepo(t, "seed.go", "package seed\n")
	inScope := "package small\n// OVERCAP_SEMANTIC_MARKER\n"
	writeTrackedFile(t, repo, filepath.Join("small", "app.go"), inScope)
	writeTrackedFile(
		t,
		repo,
		filepath.Join("big", "huge.go"),
		"package big\n"+strings.Repeat("x", len(inScope)*8),
	)

	oldLimit := gitCorpusIndexedByteLimit
	gitCorpusIndexedByteLimit = int64(len(inScope) + 8)
	t.Cleanup(func() { gitCorpusIndexedByteLimit = oldLimit })

	setTestUserCache(t)
	paths, err := resolveGitPaths(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(repo, "small")
	plans, err := planGitOperands(t.Context(), paths, scope)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plan scoped Git corpus: plans=%d error=%v", len(plans), err)
	}
	plan := plans[0]
	t.Chdir(repo)

	scorer := &hybridTestScorer{}
	output, err := runHybridTestSearch(t, "OVERCAP_SEMANTIC_MARKER", []string{scope}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return scorer, nil
	})
	if err != nil || !strings.Contains(output, "## small/app.go") {
		t.Fatalf("over-cap scoped search: error=%v output=%q", err, output)
	}
	state := mustGitRepoStateIn(t, t.Context(), repo)
	scopedState := repoStateForDirtyScope(state, plan.dirtyScope)
	stateHash := scopedFallbackStateHash(paths, scopedState)
	if !joinedGenerationMatches(
		plan.scopedCacheDir,
		plan.scopedIndexDir,
		stateHash,
		state.HeadSHA,
	) {
		t.Fatal("default over-cap scoped build did not activate a joined generation")
	}
	assertIndexOnlyModelCalls(t, scorer)
	scorer.mu.Lock()
	embeddedPaths := append([]string(nil), scorer.embeddedPaths...)
	scorer.mu.Unlock()
	if len(embeddedPaths) == 0 {
		t.Fatal("scoped semantic build embedded no paths")
	}
	for _, path := range embeddedPaths {
		if path != "small/app.go" {
			t.Fatalf("scoped semantic build embedded %q", path)
		}
	}

	removeJoinedGeneration(plan.scopedCacheDir)
	var repairFactoryCalls atomic.Int32
	repairFuture := newSemanticModelFuture(func(context.Context) (semanticModel, error) {
		repairFactoryCalls.Add(1)
		return &hybridTestScorer{}, nil
	})
	t.Cleanup(func() { _ = repairFuture.Close() })
	repairPlan := plan
	_, repairedState, err := ensureScopedGitCorpusFallback(
		t.Context(),
		&repairPlan,
		paths,
		state,
		searchExecution{policy: defaultSearchPolicy(), model: repairFuture},
	)
	if err != nil || repairedState != corpusSearchable {
		t.Fatalf("repair scoped joined descriptor: state=%d error=%v", repairedState, err)
	}
	if repairFactoryCalls.Load() != 0 {
		t.Fatalf("descriptor repair started the model %d times", repairFactoryCalls.Load())
	}
	if !joinedGenerationMatches(
		plan.scopedCacheDir,
		plan.scopedIndexDir,
		stateHash,
		state.HeadSHA,
	) {
		t.Fatal("scoped repair did not bind the existing semantic generation")
	}
}

func TestRunDefaultRerankFallbackLogsCause(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha\n")
	wantErr := errors.New("test score failure")
	logs := captureTestLogs(t, slog.LevelDebug)

	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return &fixedRerankScorer{err: wantErr}, nil
	})
	if err != nil || !strings.Contains(output, "## strict.go") {
		t.Fatalf("fallback search: error=%v output=%q", err, output)
	}
	assertTestLogError(t, logs, "Re-ranking failed; using BM25", wantErr)
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

	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return &hybridTestScorer{
			embedErr:   wantErr,
			prepareErr: wantErr,
		}, nil
	})
	if err != nil || output != lexicalOutput {
		t.Fatalf("semantic build fallback: error=%v output=%q, want %q", err, output, lexicalOutput)
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
	if !strings.Contains(repairOutput, "## semantic.go") ||
		!strings.Contains(repairOutput, "SEMANTIC_TARGET") {
		t.Fatalf("repaired joined search omitted semantic result:\n%s", repairOutput)
	}
	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if repairScorer.embedCalls.Load() == 0 ||
		!joinedGenerationMatches(plan.cacheDir, plan.indexDir, state, state) {
		t.Fatal("a later healthy run did not repair the joined index")
	}
	generation, err := openSemanticGeneration(
		semanticGenerationDir(plan.indexDir, state),
		state,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = generation.Close() }()
	if generation.usearchErr != nil {
		t.Fatalf("repaired USearch data: %v", generation.usearchErr)
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
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
		return &hybridTestScorer{prepareErr: wantErr}, nil
	})
	if err != nil || output != lexicalOutput {
		t.Fatalf(
			"semantic query fallback: error=%v output=%q, want %q",
			err,
			output,
			lexicalOutput,
		)
	}
	assertTestLogError(t, logs, "Joined search failed; using the lexical path", wantErr)
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
	semanticDirs, err := filepath.Glob(filepath.Join(plan.indexDir, semanticGenerationPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
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
	future := newSemanticModelFuture(func(context.Context) (semanticModel, error) {
		return scorer, nil
	})
	t.Cleanup(func() { _ = future.Close() })
	execution := searchExecution{policy: defaultSearchPolicy(), model: future}

	if state, err := ensureFolderCorpusFreshWithExecution(t.Context(), plan, execution); err != nil {
		t.Fatal(err)
	} else if state != corpusSearchable {
		t.Fatalf("folder state=%d, want searchable", state)
	}
	firstState, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !joinedGenerationMatches(plan.cacheDir, plan.indexDir, firstState, firstState) {
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
	if !joinedGenerationMatches(plan.cacheDir, plan.indexDir, secondState, secondState) {
		t.Fatal("updated folder build did not activate a joined generation")
	}
	semanticDirs, err := filepath.Glob(filepath.Join(plan.indexDir, semanticGenerationPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
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
	execution := searchExecution{policy: lexicalOnlySearchPolicy(), model: future}

	if _, err := ensureFolderCorpusFreshWithExecution(t.Context(), plan, execution); err != nil {
		t.Fatal(err)
	}
	if factoryCalls.Load() != 0 {
		t.Fatalf("model factory calls=%d, want zero", factoryCalls.Load())
	}
	if _, err := os.Lstat(filepath.Join(plan.cacheDir, joinedGenerationFile)); !os.IsNotExist(err) {
		t.Fatalf("lexical-only folder build created a joined descriptor: %v", err)
	}
	semanticDirs, err := filepath.Glob(filepath.Join(plan.indexDir, semanticGenerationPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(semanticDirs) != 0 {
		t.Fatalf("lexical-only folder build created semantic data: %v", semanticDirs)
	}
}

func twoDigitTestNumber(value int) string {
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)})
}
