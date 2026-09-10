//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const lateOnExtractHelperEnv = "SEEK_TEST_RERANK_EXTRACT"

func TestLateOnRuntimeManifestRejectsIncompleteAssets(t *testing.T) {
	if _, err := lateOnRuntimeBundleFromManifest([]byte(`{}`), []byte{1}); err == nil {
		t.Fatal("incomplete manifest did not fail")
	}
	if _, err := lateOnRuntimeBundleFromManifest(lateOnRuntimeManifestJSON, nil); err == nil {
		t.Fatal("empty compressed runtime did not fail")
	}
	wrongTarget := bytes.Replace(
		lateOnRuntimeManifestJSON,
		[]byte(runtime.GOOS+"-"+runtime.GOARCH),
		[]byte("unsupported-target"),
		1,
	)
	if bytes.Equal(wrongTarget, lateOnRuntimeManifestJSON) {
		t.Fatal("runtime manifest does not contain the current target")
	}
	if _, err := lateOnRuntimeBundleFromManifest(wrongTarget, lateOnCompressedRuntime); err == nil {
		t.Fatal("wrong runtime target did not fail")
	}
}

func TestLateOnTokenizerMatchesPyLate(t *testing.T) {
	tokenizerJSON, err := decodeLateOnAsset(lateOnCompressedTokenizer)
	if err != nil {
		t.Fatal(err)
	}
	tokenizer, _, err := newLateOnTokenizer(tokenizerJSON)
	if err != nil {
		t.Fatal(err)
	}

	const probe = "[Q] where is the function that parses HTTP请求?"
	want := []int{
		50281, 50368, 2811, 310, 253, 1159, 326, 13328,
		265, 17607, 44673, 43741, 32, 50282,
	}
	if got := encodeLateOnText(tokenizer, probe); !reflect.DeepEqual(got, want) {
		t.Fatalf("probe IDs = %v, want %v", got, want)
	}

	upper := encodeLateOnText(tokenizer, "[Q] MyHTTPHandler")
	lower := encodeLateOnText(tokenizer, "[Q] myhttphandler")
	if reflect.DeepEqual(upper, lower) {
		t.Fatal("tokenizer unexpectedly lowercased the input")
	}
	composed := encodeLateOnText(tokenizer, "[Q] café")
	decomposed := encodeLateOnText(tokenizer, "[Q] cafe\u0301")
	if !reflect.DeepEqual(composed, decomposed) {
		t.Fatalf("NFC IDs differ: %v and %v", composed, decomposed)
	}

	long := encodeLateOnText(
		tokenizer,
		lateOnDocumentPrefix+strings.Repeat("identifier ", 300),
	)
	if len(long) != lateOnSequenceLength || long[len(long)-1] != lateOnSEPTokenID {
		t.Fatalf("truncated sequence has length %d and final ID %d", len(long), long[len(long)-1])
	}
}

func TestTokenizeLateOnBatchMasksPunctuationAndPadding(t *testing.T) {
	tokenizerJSON, err := decodeLateOnAsset(lateOnCompressedTokenizer)
	if err != nil {
		t.Fatal(err)
	}
	tokenizer, punctuation, err := newLateOnTokenizer(tokenizerJSON)
	if err != nil {
		t.Fatal(err)
	}

	ids, attention, mask := tokenizeLateOnBatch(
		tokenizer,
		punctuation,
		"请求?",
		[]rerankDocument{{Text: "请求;", matchEnd: len("请求")}},
	)
	queryIDs := ids[:lateOnSequenceLength]
	documentIDs := ids[lateOnSequenceLength:]
	for _, row := range []struct {
		name      string
		ids       []int64
		attention []int64
		mask      []bool
		punct     int64
		keepPunct bool
	}{
		{
			name:      "query",
			ids:       queryIDs,
			attention: attention[:lateOnSequenceLength],
			mask:      mask[0],
			punct:     32,
			keepPunct: true,
		},
		{
			name:      "document",
			ids:       documentIDs,
			attention: attention[lateOnSequenceLength:],
			mask:      mask[1],
			punct:     28,
			keepPunct: false,
		},
	} {
		punctIndex := slices.Index(row.ids, row.punct)
		if punctIndex < 0 || row.mask[punctIndex] != row.keepPunct {
			t.Fatalf("%s punctuation mask = %v at %d", row.name, row.mask, punctIndex)
		}
		paddingIndex := slices.Index(row.attention, 0)
		if paddingIndex < 0 || row.ids[paddingIndex] != lateOnPadTokenID || row.mask[paddingIndex] {
			t.Fatalf("%s padding is IDs=%v attention=%v mask=%v", row.name, row.ids, row.attention, row.mask)
		}
	}
}

func TestEncodeLateOnDocumentKeepsMetadataAndLateMatch(t *testing.T) {
	tokenizerJSON, err := decodeLateOnAsset(lateOnCompressedTokenizer)
	if err != nil {
		t.Fatal(err)
	}
	tokenizer, _, err := newLateOnTokenizer(tokenizerJSON)
	if err != nil {
		t.Fatal(err)
	}

	const lateMatch = "needle"
	const wantMetadata = "path: services/web/src/handler.go\n" +
		"language: Go\n" +
		"symbol: function HandleNeedle"
	markerID, ok := tokenizer.TokenToID("Ġ" + lateMatch)
	if !ok {
		t.Fatalf("tokenizer has no single token for %q", lateMatch)
	}
	textPrefix := strings.Repeat("identifier_1234567890 ", 900)
	document := rerankDocument{
		Path:     "workspace/platform/services/web/src/handler.go",
		Language: "Go",
		Symbol:   "function HandleNeedle",
		Text:     textPrefix + lateMatch + "\n",
		matchAt:  len(textPrefix),
		matchEnd: len(textPrefix) + len(lateMatch),
	}
	selected := encodeLateOnDocument(tokenizer, document, lateOnSequenceLength)
	if len(selected) != lateOnSequenceLength || !slices.Contains(selected, markerID) {
		t.Fatalf("selected IDs do not retain the late match token %d: %v", markerID, selected)
	}
	wantMetadataIDs := tokenizer.Encode(lateOnDocumentPrefix + wantMetadata)
	if len(wantMetadataIDs) == 0 {
		t.Fatal("tokenizer returned no metadata IDs")
	}
	if wantMetadataIDs[len(wantMetadataIDs)-1] == lateOnSEPTokenID {
		wantMetadataIDs = wantMetadataIDs[:len(wantMetadataIDs)-1]
	}
	if !containsTokenSequence(selected, wantMetadataIDs) {
		t.Fatalf("selected IDs do not retain metadata %v: %v", wantMetadataIDs, selected)
	}
}

func containsTokenSequence(values, sequence []int) bool {
	if len(sequence) == 0 {
		return false
	}
	for i := 0; i+len(sequence) <= len(values); i++ {
		if slices.Equal(values[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func TestLateOnMaxSimUsesMasksAndNegativeValues(t *testing.T) {
	// The final zero vector is padding. If MaxSim includes it, the score is 0
	// instead of -2.
	embeddings := []float32{
		1, 0, 0, 1, 0, 0,
		-1, -1, -2, -2, 0, 0,
	}
	masks := [][]bool{{true, true, false}, {true, true, false}}
	scores, err := lateOnMaxSim(embeddings, masks, 2, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scores, []float32{-2}) {
		t.Fatalf("scores = %v, want [-2]", scores)
	}
	if _, err := lateOnMaxSim(embeddings[:len(embeddings)-1], masks, 2, 3, 2); err == nil {
		t.Fatal("short tensor did not fail")
	}
}

func TestLateOnRuntimeCacheLifecycle(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		t.Fatal(err)
	}
	wantRuntime, err := decodeLateOnAsset(bundle.compressed)
	if err != nil {
		t.Fatal(err)
	}
	path, installed, err := ensureLateOnRuntime(bundle)
	if err != nil || !installed {
		t.Fatalf("cold extraction: path=%q installed=%t err=%v", path, installed, err)
	}
	gotRuntime, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(gotRuntime, wantRuntime) {
		t.Fatalf("cold runtime bytes differ: err=%v", err)
	}
	warmPath, installed, err := ensureLateOnRuntime(bundle)
	if err != nil || installed || warmPath != path {
		t.Fatalf("warm extraction: path=%q installed=%t err=%v", warmPath, installed, err)
	}
	corruptRuntime := bytes.Clone(wantRuntime)
	corruptRuntime[len(corruptRuntime)/2] ^= 0xff
	if err := os.WriteFile(path, corruptRuntime, 0o700); err != nil {
		t.Fatal(err)
	}
	repairedPath, installed, err := ensureLateOnRuntime(bundle)
	if err != nil || !installed || repairedPath != path {
		t.Fatalf("repair: path=%q installed=%t err=%v", repairedPath, installed, err)
	}
	gotRuntime, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(gotRuntime, wantRuntime) {
		t.Fatalf("repaired runtime bytes differ: err=%v", err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode = %o, want 700", info.Mode().Perm())
	}
}

func TestLateOnRuntimeCacheAcrossProcesses(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cache)
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, len(commands))
	for i := range commands {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestLateOnRuntimeExtractHelper$")
		commands[i].Stdout = &outputs[i]
		commands[i].Stderr = &outputs[i]
		commands[i].Env = append(
			commands[i].Environ(),
			lateOnExtractHelperEnv+"=1",
		)
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("extract helper: %v\n%s", err, outputs[i].String())
		}
	}
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		t.Fatal(err)
	}
	path, installed, err := ensureLateOnRuntime(bundle)
	if err != nil || installed {
		t.Fatalf("concurrent extraction did not leave a warm runtime: path=%q installed=%t err=%v", path, installed, err)
	}
	wantRuntime, err := decodeLateOnAsset(bundle.compressed)
	if err != nil {
		t.Fatal(err)
	}
	gotRuntime, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(gotRuntime, wantRuntime) {
		t.Fatalf("two-process runtime bytes differ: err=%v", err)
	}
}

func TestLateOnRuntimeExtractHelper(t *testing.T) {
	if os.Getenv(lateOnExtractHelperEnv) != "1" {
		return
	}
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureLateOnRuntime(bundle); err != nil {
		t.Fatal(err)
	}
}

func TestLateOnSessionOptionsEnableX64Precision(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		t.Fatal(err)
	}
	runtimePath, _, err := ensureLateOnRuntime(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureLateOnEnvironment(runtimePath); err != nil {
		t.Fatal(err)
	}
	options, err := newLateOnSessionOptions()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := options.Destroy(); err != nil {
			t.Errorf("destroy session options: %v", err)
		}
	})
	got, err := options.GetSessionConfigEntry(lateOnX64PrecisionKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1" {
		t.Fatalf("%s = %q, want 1", lateOnX64PrecisionKey, got)
	}
}

func TestLateOnInferenceMatchesReferenceScores(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	scorer, err := newLateOnRerankScorer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scorer.Close(); err != nil {
			t.Errorf("close scorer: %v", err)
		}
	}()

	documents := []rerankDocument{
		{
			Path:     "cmd/seek/searcher.go",
			Language: "Go",
			Symbol:   "function executeParsedSearch",
			Text: "func executeParsedSearchScoped(results []corpusSearchResult, limit int) {\n" +
				"    ranked := rankCorpusResultsBM25(results, nil)\n" +
				"    return formatResults(ranked, limit)\n}",
		},
		{
			Path:     "cmd/seek/indexer.go",
			Language: "Go",
			Symbol:   "function buildIndex",
			Text: "func buildIndex(files []string) error {\n" +
				"    for _, file := range files { parse(file) }\n" +
				"    return writeShard()\n}",
		},
		{
			Path:     "docs/install.md",
			Language: "Markdown",
			Text:     "Install seek with Homebrew. Configure Universal Ctags on the PATH.",
		},
	}
	scores, err := scorer.Scores(
		context.Background(),
		"find where search results are ranked before output limit",
		documents,
	)
	if err != nil {
		t.Fatal(err)
	}
	// ONNX Runtime 1.29.0 uses row-wise activation quantization on the tested
	// ARM SME/SME2 path. Its tested generic path quantizes the full activation
	// tensor. The bundled model and tokenizer produce both stable references.
	references := []struct {
		name   string
		scores []float32
	}{
		{name: "tensor-wide", scores: []float32{6.1823421, 5.1173959, 4.8599272}},
		{name: "KleidiAI row-wise", scores: []float32{6.5482426, 4.7140551, 4.5997243}},
	}
	if len(scores) != len(references[0].scores) {
		t.Fatalf("scores = %v, want %d scores", scores, len(references[0].scores))
	}
	matchesReference := false
	for _, reference := range references {
		matches := true
		for i := range reference.scores {
			if math.Abs(float64(scores[i]-reference.scores[i])) > 0.001 {
				matches = false
				break
			}
		}
		if matches {
			matchesReference = true
			break
		}
	}
	if !matchesReference {
		t.Errorf("scores = %v, want one of %v", scores, references)
	}
	for i := range scores {
		if i > 0 && scores[i-1] <= scores[i] {
			t.Errorf("rank order = %v, want document order", scores)
		}
	}
}

func TestLateOnInferenceSupportsFullCandidateBatch(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	scorer, err := newLateOnRerankScorer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := scorer.Close(); err != nil {
			t.Errorf("close scorer: %v", err)
		}
	})

	documents := make([]rerankDocument, rerankCandidateLimit)
	for i := range documents {
		documents[i] = rerankDocument{
			Path:     "cmd/seek/searcher.go",
			Language: "Go",
			Symbol:   "function executeParsedSearch",
			Text:     "func executeParsedSearch() { rankCorpusResultsBM25() }",
		}
	}
	scores, err := scorer.Scores(
		t.Context(),
		"find where search results are ranked",
		documents,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != rerankCandidateLimit {
		t.Fatalf("scores=%d, want %d", len(scores), rerankCandidateLimit)
	}
}

func TestLateOnScorerCanReopen(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	for range 2 {
		scorer, err := newLateOnRerankScorer(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := scorer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLIProcessLateOnReranks(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "alpha.go", "package sample\n// alpha\n")
	writeFileAt(t, folder, "beta.go", "package sample\n// beta\n")
	cache := t.TempDir()

	baseline := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{"alpha beta", folder},
		nil,
	)
	if baseline.code != 0 || baseline.stderr != "" || baseline.stdout == "" {
		t.Fatalf("baseline=%+v", baseline)
	}
	reranked := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{"--rerank", "alpha beta", folder},
		nil,
	)
	if reranked.code != 0 || reranked.stderr != "" || reranked.stdout == "" {
		t.Fatalf("reranked=%+v", reranked)
	}
	if reranked.stdout == baseline.stdout ||
		!strings.Contains(reranked.stdout, "## alpha.go") ||
		!strings.Contains(reranked.stdout, "## beta.go") {
		t.Fatalf("real backend did not publish the OR-only candidates: %+v", reranked)
	}
}
