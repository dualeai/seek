//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	ctags "github.com/sourcegraph/go-ctags"
)

func openLateOnTestTokenizer(tb testing.TB) (*lateOnTokenizer, map[int]struct{}) {
	tb.Helper()
	tokenizerJSON, err := decodeLateOnAsset(lateOnCompressedTokenizer)
	if err != nil {
		tb.Fatal(err)
	}
	tokenizer, punctuation, err := newLateOnTokenizer(tokenizerJSON)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := tokenizer.Close(); err != nil {
			tb.Errorf("close tokenizer: %v", err)
		}
	})
	return tokenizer, punctuation
}

func mustEncodeLateOnText(tb testing.TB, tokenizer *lateOnTokenizer, text string) []int {
	tb.Helper()
	ids, err := encodeLateOnText(tokenizer, text)
	if err != nil {
		tb.Fatal(err)
	}
	return ids
}

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
	tokenizer, _ := openLateOnTestTokenizer(t)

	const probe = "[Q] where is the function that parses HTTP请求?"
	want := []int{
		50281, 50368, 2811, 310, 253, 1159, 326, 13328,
		265, 17607, 44673, 43741, 32, 50282,
	}
	if got := mustEncodeLateOnText(t, tokenizer, probe); !reflect.DeepEqual(got, want) {
		t.Fatalf("probe IDs = %v, want %v", got, want)
	}

	upper := mustEncodeLateOnText(t, tokenizer, "[Q] MyHTTPHandler")
	lower := mustEncodeLateOnText(t, tokenizer, "[Q] myhttphandler")
	if reflect.DeepEqual(upper, lower) {
		t.Fatal("tokenizer unexpectedly lowercased the input")
	}
	composed := mustEncodeLateOnText(t, tokenizer, "[Q] café")
	decomposed := mustEncodeLateOnText(t, tokenizer, "[Q] cafe\u0301")
	if !reflect.DeepEqual(composed, decomposed) {
		t.Fatalf("NFC IDs differ: %v and %v", composed, decomposed)
	}

	long := mustEncodeLateOnText(
		t,
		tokenizer,
		lateOnDocumentPrefix+strings.Repeat("identifier ", 300),
	)
	if len(long) != lateOnSequenceLength || long[len(long)-1] != lateOnSEPTokenID {
		t.Fatalf("truncated sequence has length %d and final ID %d", len(long), long[len(long)-1])
	}
}

func TestLateOnTokenizationMasksPunctuationAndPadding(t *testing.T) {
	tokenizer, punctuation := openLateOnTestTokenizer(t)

	query, err := encodeLateOnText(tokenizer, lateOnQueryPrefix+"请求?")
	if err != nil {
		t.Fatal(err)
	}
	queryIDs, queryAttention, queryMask := packLateOnRows([][]int{query}, nil, true)
	documentIDs, documentAttention, documentMask, err := tokenizeLateOnDocuments(
		tokenizer,
		punctuation,
		[]rerankDocument{{Text: "请求;", matchEnd: len("请求")}},
	)
	if err != nil {
		t.Fatal(err)
	}
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
			attention: queryAttention,
			mask:      queryMask[0],
			punct:     32,
			keepPunct: true,
		},
		{
			name:      "document",
			ids:       documentIDs,
			attention: documentAttention,
			mask:      documentMask[0],
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
	tokenizer, _ := openLateOnTestTokenizer(t)

	const lateMatch = "needle"
	const wantMetadata = "path: services/web/src/handler.go\n" +
		"language: Go\n" +
		"symbol: function HandleNeedle"
	markerIDs, _, err := tokenizer.encode(" "+lateMatch, false, false)
	if err != nil || len(markerIDs) != 1 {
		t.Fatalf("tokenizer marker IDs=%v: %v", markerIDs, err)
	}
	markerID := markerIDs[0]
	textPrefix := strings.Repeat("identifier_1234567890 ", 900)
	document := rerankDocument{
		Path:     "workspace/platform/services/web/src/handler.go",
		Language: "Go",
		Symbol:   "function HandleNeedle",
		Text:     textPrefix + lateMatch + "\n",
		matchAt:  len(textPrefix),
		matchEnd: len(textPrefix) + len(lateMatch),
	}
	selected, err := encodeLateOnDocument(tokenizer, document, lateOnSequenceLength)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != lateOnSequenceLength || !slices.Contains(selected, markerID) {
		t.Fatalf("selected IDs do not retain the late match token %d: %v", markerID, selected)
	}
	wantMetadataIDs, _, err := tokenizer.encode(lateOnDocumentPrefix+wantMetadata, true, false)
	if err != nil {
		t.Fatal(err)
	}
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

func TestLateOnTokenizerBoundsPathologicalRows(t *testing.T) {
	tokenizer, _ := openLateOnTestTokenizer(t)
	started := time.Now()
	ids, err := encodeLateOnDocument(
		tokenizer,
		rerankDocument{
			Path:     "generated.go",
			Language: "Go",
			Text:     strings.Repeat("x", maxRerankDocumentBytes),
			matchEnd: maxRerankDocumentBytes,
		},
		lateOnSequenceLength,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != lateOnSequenceLength || ids[len(ids)-1] != lateOnSEPTokenID {
		t.Fatalf("pathological IDs have length %d and final ID %d", len(ids), ids[len(ids)-1])
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("pathological tokenization took %s", elapsed)
	}
}

func TestLateOnTokenizerSanitizesNativeStringInputs(t *testing.T) {
	tokenizer, _ := openLateOnTestTokenizer(t)
	input := "before\x00needle\xffafter"
	sanitized := sanitizeLateOnTokenizerInput(input)
	if strings.ContainsRune(sanitized, '\x00') || !utf8.ValidString(sanitized) {
		t.Fatalf("sanitized input is not a valid native string: %q", sanitized)
	}
	ids, err := encodeLateOnDocument(
		tokenizer,
		rerankDocument{Text: input, matchEnd: len(input)},
		lateOnSequenceLength,
	)
	if err != nil || len(ids) == 0 {
		t.Fatalf("sanitized document IDs=%v: %v", ids, err)
	}
}

func TestPackLateOnSemanticUnitsJoinsOnlyCompleteRows(t *testing.T) {
	tokenizer, _ := openLateOnTestTokenizer(t)
	shortContent := []byte("func Alpha() {}\nfunc Beta() {}\n")
	shortUnits := extractSemanticUnits("sample.go", shortContent, []*ctags.Entry{
		{Name: "Alpha", Kind: "function", Language: "Go", Line: 1},
		{Name: "Beta", Kind: "function", Language: "Go", Line: 2},
	}, nil)
	packed, err := packLateOnSemanticUnits(t.Context(), tokenizer, shortUnits)
	if err != nil {
		t.Fatal(err)
	}
	if len(packed) != 1 || packed[0].kind != semanticUnitPacked ||
		packed[0].symbol != "function Alpha function Beta" {
		t.Fatalf("packed units=%+v", packed)
	}
	if len(packed[0].modelInput) == 0 || len(packed[0].modelInput) > lateOnSequenceLength {
		t.Fatalf("packed model input has %d tokens", len(packed[0].modelInput))
	}
	inputIDs, attention, _, err := tokenizeLateOnSemanticUnits(
		nil,
		map[int]struct{}{},
		packed,
	)
	if err != nil {
		t.Fatal(err)
	}
	for index, id := range packed[0].modelInput {
		if inputIDs[index] != int64(id) || attention[index] != 1 {
			t.Fatalf("cached token %d was not reused", index)
		}
	}
	if packed[0].start != 0 || packed[0].end != uint64(len(shortContent)) ||
		!bytes.Equal(packed[0].text, shortContent) {
		t.Fatalf("packed source span=%d:%d text=%q", packed[0].start, packed[0].end, packed[0].text)
	}
	repeated, err := packLateOnSemanticUnits(t.Context(), tokenizer, shortUnits)
	if err != nil || !reflect.DeepEqual(repeated, packed) {
		t.Fatalf("repeated pack differs: units=%+v err=%v", repeated, err)
	}

	line := strings.Repeat("identifier ", 200) + "\n"
	longContent := []byte(line + line)
	longUnits := extractSemanticUnits("large.go", longContent, []*ctags.Entry{
		{Name: "First", Kind: "function", Language: "Go", Line: 1},
		{Name: "Second", Kind: "function", Language: "Go", Line: 2},
	}, nil)
	notPacked, err := packLateOnSemanticUnits(t.Context(), tokenizer, longUnits)
	if err != nil {
		t.Fatal(err)
	}
	if len(notPacked) != len(longUnits) {
		t.Fatalf("long units=%d, want %d", len(notPacked), len(longUnits))
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := packLateOnSemanticUnits(canceled, tokenizer, shortUnits); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pack error=%v", err)
	}
}

func TestLateOnSemanticProxyDoesNotRepeatStoredSymbol(t *testing.T) {
	tokenizer, punctuation := openLateOnTestTokenizer(t)
	unit := semanticUnit{
		path:     "cmd/seek/search.go",
		language: "Go",
		symbol:   "stored symbol metadata one",
		text:     []byte("func Search() {}\n"),
	}
	leftIDs, leftAttention, _, err := tokenizeLateOnSemanticUnits(
		tokenizer, punctuation, []semanticUnit{unit},
	)
	if err != nil {
		t.Fatal(err)
	}
	unit.symbol = "different stored symbol metadata two"
	rightIDs, rightAttention, _, err := tokenizeLateOnSemanticUnits(
		tokenizer, punctuation, []semanticUnit{unit},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leftIDs, rightIDs) || !reflect.DeepEqual(leftAttention, rightAttention) {
		t.Fatal("stored symbol changed the semantic proxy input")
	}

	documents := []rerankDocument{{Text: string(unit.text), Symbol: "symbol one", matchEnd: len(unit.text)}}
	first, _, _, err := tokenizeLateOnDocuments(tokenizer, punctuation, documents)
	if err != nil {
		t.Fatal(err)
	}
	documents[0].Symbol = "symbol two"
	second, _, _, err := tokenizeLateOnDocuments(tokenizer, punctuation, documents)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(first, second) {
		t.Fatal("final MaxSim document omitted stored symbol metadata")
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
	query := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	query[0] = 1
	query[semanticEmbeddingDimensions+1] = 1
	queryMask := make([]bool, lateOnSequenceLength)
	queryMask[0] = true
	queryMask[1] = true
	documents := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	documents[0] = -1
	documents[1] = -1
	documents[semanticEmbeddingDimensions] = -2
	documents[semanticEmbeddingDimensions+1] = -2
	documentMask := make([]bool, lateOnSequenceLength)
	documentMask[0] = true
	documentMask[1] = true
	scores, err := lateOnMaxSimPrepared(query, queryMask, documents, [][]bool{documentMask})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scores, []float32{-2}) {
		t.Fatalf("scores = %v, want [-2]", scores)
	}
	if _, err := lateOnMaxSimPrepared(query, queryMask, documents[:len(documents)-1], [][]bool{documentMask}); err == nil {
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

func TestLateOnInferenceMatchesReferenceScores(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	scorer, err := newLateOnSemanticModel(context.Background())
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
	scores, err := scoreWithTestSemanticEmbedder(
		context.Background(),
		scorer,
		"find where search results are ranked before output limit",
		documents,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{6.3321176, 4.6452603, 4.445041}
	if len(scores) != len(want) {
		t.Fatalf("scores = %v, want %d scores", scores, len(want))
	}
	for i := range want {
		if math.Abs(float64(scores[i]-want[i])) > 0.01 {
			t.Errorf("scores = %v, want %v", scores, want)
			break
		}
	}
	for i := range scores {
		if i > 0 && scores[i-1] <= scores[i] {
			t.Errorf("rank order = %v, want document order", scores)
		}
	}
}

func TestLateOnEncoderConcurrentOutputsAreIsolated(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	rawModel, err := newLateOnSemanticModel(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rawModel.Close(); err != nil {
			t.Errorf("close model: %v", err)
		}
	})
	model, ok := rawModel.(*lateOnModel)
	if !ok {
		t.Fatalf("model type is %T, want *lateOnModel", rawModel)
	}

	texts := []string{
		"func parseRequest() { validateHeaders() }",
		"func buildIndex() { writeSemanticVectors() }",
		"func searchGraph() { mergeRankedCandidates() }",
		"func closeSnapshot() { releaseMappedFiles() }",
	}
	units := make([]semanticUnit, len(texts))
	for index, source := range texts {
		units[index] = semanticUnit{
			row:      uint64(index),
			path:     "sample.go",
			language: "Go",
			text:     []byte(source),
		}
	}
	tokenizer, err := model.takeTokenizer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	inputIDs, attention, _, tokenizeErr := tokenizeLateOnSemanticUnits(
		tokenizer,
		model.punctuation,
		units,
	)
	model.releaseTokenizer(tokenizer)
	if tokenizeErr != nil {
		t.Fatal(tokenizeErr)
	}
	encoder, err := model.semanticRowEncoder(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	rowValues := lateOnSequenceLength * semanticEmbeddingDimensions
	reference := make([][]float32, len(units))
	if err := encoder.Run(t.Context(), inputIDs, attention, len(units), func(output []float32) error {
		for row := range units {
			start := row * rowValues
			reference[row] = append([]float32(nil), output[start:start+rowValues]...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	ready := make(chan struct{}, len(units))
	release := make(chan struct{})
	concurrent := make([][]float32, len(units))
	errorsByRow := make([]error, len(units))
	var wait sync.WaitGroup
	for row := range units {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			at := row * lateOnSequenceLength
			errorsByRow[row] = encoder.Run(
				t.Context(),
				inputIDs[at:at+lateOnSequenceLength],
				attention[at:at+lateOnSequenceLength],
				1,
				func(output []float32) error {
					concurrent[row] = append([]float32(nil), output...)
					ready <- struct{}{}
					<-release
					return nil
				},
			)
		}()
	}
	close(start)
	timer := time.NewTimer(30 * time.Second)
	for range units {
		select {
		case <-ready:
		case <-timer.C:
			close(release)
			wait.Wait()
			t.Fatal("concurrent encoder calls did not reach their output callbacks")
		}
	}
	if !timer.Stop() {
		<-timer.C
	}
	close(release)
	wait.Wait()

	for row, runErr := range errorsByRow {
		if runErr != nil {
			t.Fatalf("concurrent row %d: %v", row, runErr)
		}
		if len(concurrent[row]) != len(reference[row]) {
			t.Fatalf("concurrent row %d values=%d, want %d", row, len(concurrent[row]), len(reference[row]))
		}
		for value := range reference[row] {
			if math.Abs(float64(concurrent[row][value]-reference[row][value])) > 1e-5 {
				t.Fatalf("concurrent row %d value %d differs: got %g, want %g", row, value, concurrent[row][value], reference[row][value])
			}
		}
	}
}

func TestLateOnInferenceSupportsFullCandidateBatch(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	scorer, err := newLateOnSemanticModel(t.Context())
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
	scores, err := scoreWithTestSemanticEmbedder(
		t.Context(),
		scorer,
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

func TestLateOnModelCanReopen(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	for range 2 {
		scorer, err := newLateOnSemanticModel(context.Background())
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

	lexical := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{"--lexical-only", "alpha beta", folder},
		nil,
	)
	if lexical.code != 0 || lexical.stderr != "" || lexical.stdout == "" {
		t.Fatalf("lexical=%+v", lexical)
	}
	reranked := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{"alpha beta", folder},
		nil,
	)
	if reranked.code != 0 || reranked.stderr != "" || reranked.stdout == "" {
		t.Fatalf("reranked=%+v", reranked)
	}
	if reranked.stdout == lexical.stdout ||
		!strings.Contains(reranked.stdout, "## alpha.go") ||
		!strings.Contains(reranked.stdout, "## beta.go") {
		t.Fatalf("real backend did not publish the OR-only candidates: %+v", reranked)
	}
}
