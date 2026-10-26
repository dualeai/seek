//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// openLateOnTestTokenizer opens the bundled tokenizer and returns it with the
// IDs of its punctuation tokens. Cleanup closes it.
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

// mustLateOnQueryIDs returns the token IDs of a query that fits one model
// window, through the production query path (lateOnQueryWindows).
func mustLateOnQueryIDs(tb testing.TB, tokenizer *lateOnTokenizer, modelQuery string) []int {
	tb.Helper()
	windows, err := lateOnQueryWindows(tokenizer, modelQuery)
	if err != nil {
		tb.Fatal(err)
	}
	if len(windows) != 1 {
		tb.Fatalf("query %q gives %d windows, want 1", modelQuery, len(windows))
	}
	return windows[0]
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

	// PyLate encodes "[Q] where is the function that parses HTTP请求?" to
	// these IDs.
	const probe = "where is the function that parses HTTP请求?"
	want := []int{
		50281, 50368, 2811, 310, 253, 1159, 326, 13328,
		265, 17607, 44673, 43741, 32, 50282,
	}
	if got := mustLateOnQueryIDs(t, tokenizer, probe); !reflect.DeepEqual(got, want) {
		t.Fatalf("probe IDs = %v, want %v", got, want)
	}
	upper := mustLateOnQueryIDs(t, tokenizer, "MyHTTPHandler")
	lower := mustLateOnQueryIDs(t, tokenizer, "myhttphandler")
	if reflect.DeepEqual(upper, lower) {
		t.Fatal("tokenizer unexpectedly lowercased the input")
	}
	composed := mustLateOnQueryIDs(t, tokenizer, "café")
	decomposed := mustLateOnQueryIDs(t, tokenizer, "cafe\u0301")
	if !reflect.DeepEqual(composed, decomposed) {
		t.Fatalf("NFC IDs differ: %v and %v", composed, decomposed)
	}

	text := strings.Repeat("identifier ", 300)
	long, err := encodeLateOnDocument(
		tokenizer,
		rerankDocument{Text: text, matchEnd: len(text)},
		lateOnSequenceLength,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(long) != lateOnSequenceLength || long[len(long)-1] != lateOnSEPTokenID {
		t.Fatalf("truncated sequence has length %d and final ID %d", len(long), long[len(long)-1])
	}
	// With an empty match span, the truncation keeps the head
	// (truncateLateOnHead): the first 127 IDs, then SEP.
	ids, spans, err := tokenizer.encode(lateOnDocumentPrefix+text, true, true)
	if err != nil {
		t.Fatal(err)
	}
	head := truncateLateOnDocumentTokens(ids, spans, 0, 0, 0, lateOnSequenceLength)
	if len(head) != lateOnSequenceLength || head[len(head)-1] != lateOnSEPTokenID ||
		!slices.Equal(head[:len(head)-1], ids[:len(head)-1]) {
		t.Fatalf("head truncation has length %d and final ID %d", len(head), head[len(head)-1])
	}
}

// TestLateOnPrepareSemanticQueryUsesWindows prepares a short and a 300-term
// query. The short one is one 128-token window. The long one keeps every token
// in ceil(tokens/room) windows, with no truncation. The long query takes its
// empty-document score from the row after its last window. That score must
// equal the score that the scorer gives an empty document.
func TestLateOnPrepareSemanticQueryUsesWindows(t *testing.T) {
	scorer := sharedModel(t)
	tokenizer, _ := openLateOnTestTokenizer(t)
	// A query starts with the class and the [Q] tokens and ends with the
	// separator (TestLateOnTokenizerMatchesPyLate). So each window has room
	// for 128 - 3 = 125 query tokens. The 300 words encode to 301 tokens (one
	// per word and one for the final space): ceil(301/125) = 3 windows.
	const head = 2
	for _, tc := range []struct {
		query   string
		body    int
		windows int
	}{
		{query: "request authentication flow", body: 3, windows: 1},
		{query: strings.Repeat("identifier ", 300), body: 301, windows: 3},
	} {
		windows, err := lateOnQueryWindows(tokenizer, tc.query)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _, err := tokenizer.encode(lateOnQueryPrefix+tc.query, true, false)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(encoded[:head], []int{50281, 50368}) || len(encoded)-head-1 != tc.body {
			t.Fatalf("query encodes to %d tokens with head %v, want %d query tokens", len(encoded), encoded[:head], tc.body)
		}
		if len(windows) != tc.windows {
			t.Fatalf("%d query tokens: windows=%d, want %d", tc.body, len(windows), tc.windows)
		}
		// The windows repeat the head, end with the separator, fill the model
		// input except the last, and give back the query tokens in order.
		var kept []int
		for index, window := range windows {
			if len(window) > lateOnSequenceLength || !slices.Equal(window[:head], encoded[:head]) ||
				window[len(window)-1] != lateOnSEPTokenID ||
				(index < len(windows)-1 && len(window) != lateOnSequenceLength) {
				t.Fatalf("window %d of %d has %d tokens: not a whole model input", index+1, len(windows), len(window))
			}
			kept = append(kept, window[head:len(window)-1]...)
		}
		if !slices.Equal(kept, encoded[head:len(encoded)-1]) {
			t.Fatalf("windows keep %d of %d query tokens", len(kept), tc.body)
		}
		prepared, err := scorer.PrepareSemanticQuery(t.Context(), tc.query)
		if err != nil {
			t.Fatal(err)
		}
		if len(prepared.scoreMask) != tc.windows*lateOnSequenceLength || !validSemanticQuery(prepared) {
			t.Fatalf("prepared mask=%d tokens=%d, want %d windows", len(prepared.scoreMask), len(prepared.tokens), tc.windows)
		}
		// Each scorer call costs one model batch, so only the long query
		// checks the empty-document score. Its row comes after several
		// windows, and its score sums the tokens of every window.
		if tc.windows == 1 {
			continue
		}
		scores, err := scorer.ScoresWithSemanticQuery(t.Context(), prepared, []rerankDocument{{}})
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(float64(prepared.emptyScore-scores[0])) > 1e-4 {
			t.Fatalf("emptyScore=%g, want the scorer's empty-document score %g", prepared.emptyScore, scores[0])
		}
	}
}

// TestLateOnQueryWindowsStopAtOneModelCall splits queries of 127 and 128
// windows with the bundled tokenizer. PrepareSemanticQuery puts the windows
// and the empty document in one static batch of semanticModelBatchRows rows,
// so 127 windows fit and 128 fail with errLateOnQueryTooLong. Each window has
// room for 125 query tokens, and n words of "identifier " encode to n + 1
// tokens. The test needs no model batch.
func TestLateOnQueryWindowsStopAtOneModelCall(t *testing.T) {
	tokenizer, _ := openLateOnTestTokenizer(t)
	const room = lateOnSequenceLength - 3
	limit := semanticModelBatchRows - 1
	windows, err := lateOnQueryWindows(tokenizer, strings.Repeat("identifier ", limit*room-1))
	if err != nil || len(windows) != limit {
		t.Fatalf("a query of %d windows: windows=%d err=%v, want %d windows", limit, len(windows), err, limit)
	}
	windows, err = lateOnQueryWindows(tokenizer, strings.Repeat("identifier ", limit*room))
	if !errors.Is(err, errLateOnQueryTooLong) || windows != nil {
		t.Fatalf("a query of %d windows: windows=%d err=%v, want errLateOnQueryTooLong", limit+1, len(windows), err)
	}
}

func TestLateOnTokenizationMasksPunctuationAndPadding(t *testing.T) {
	tokenizer, punctuation := openLateOnTestTokenizer(t)

	// PrepareSemanticQuery packs the query windows with the query flag set.
	windows, err := lateOnQueryWindows(tokenizer, "请求?")
	if err != nil {
		t.Fatal(err)
	}
	queryIDs, queryAttention, queryMask := packLateOnRows(windows, nil, true)
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

// TestLateOnTokenizerBoundsPathologicalRows encodes a document of
// maxRerankDocumentBytes with no space, and checks that its IDs fill one
// model input and end with SEP.
func TestLateOnTokenizerBoundsPathologicalRows(t *testing.T) {
	tokenizer, _ := openLateOnTestTokenizer(t)
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
	// A score difference of the selected case fails the test. The process
	// model uses the provider that the run-time check selected on this host,
	// as a user's search does.
	t.Run("selected", func(t *testing.T) {
		lateOnReferenceScoreCheck(t, sharedModel(t), sharedModelProvider, true)
	})
	t.Run(lateOnCPUProviderName, func(t *testing.T) {
		t.Setenv(lateOnProviderEnv, lateOnCPUProviderName)
		lateOnReferenceScoreCheck(t, newPinnedLateOnModel(t), semanticProviderName, true)
	})
	if lateOnAcceleratedProviderName == lateOnCPUProviderName {
		return
	}
	// A pinned accelerated provider skips the run-time check, so this case
	// logs its scores and does not fail. On a host where that provider is
	// wrong, the check demotes it, so the automatic case above still passes.
	// The scores logged here show how wrong the provider is.
	t.Run(lateOnAcceleratedProviderName, func(t *testing.T) {
		t.Setenv(lateOnProviderEnv, lateOnAcceleratedProviderName)
		lateOnReferenceScoreCheck(t, newPinnedLateOnModel(t), semanticProviderName, false)
	})
}

// newPinnedLateOnModel returns a new model in a new cache root, for a test
// that pins a provider. The process model keeps the provider of the process.
// Cleanup closes the new model.
func newPinnedLateOnModel(t *testing.T) *lateOnModel {
	t.Helper()
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	model := newTestLateOnModel(t)
	t.Cleanup(func() {
		if err := model.Close(); err != nil {
			t.Errorf("close model: %v", err)
		}
	})
	return model
}

// lateOnReferenceScoreCheck scores one description against three fixed
// documents. Every provider must give the reference scores (want) within 0.01,
// in the same order. With gate false, a difference in a score or in the order
// is logged, not failed. provider names the provider of scorer after its first
// call. semanticProviderName gives the provider of the last model that built
// an encoder. That is not the process model if another test built one later.
//
// A provider that passes the run-time check in verifyLateOnAcceleratedProvider
// but fails here shows that the check accepts too much. Then make
// lateOnProbeAgreementLimit or lateOnProbeMagnitudeLimit smaller.
func lateOnReferenceScoreCheck(t *testing.T, scorer semanticModel, provider func() string, gate bool) {
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
	t.Logf("provider=%s scores=%v", provider(), scores)
	for i := range want {
		if math.Abs(float64(scores[i]-want[i])) > 0.01 {
			if !gate {
				t.Logf("recorded only: scores = %v, want %v", scores, want)
				return
			}
			t.Errorf("scores = %v, want %v", scores, want)
			break
		}
	}
	for i := range scores {
		if i > 0 && scores[i-1] <= scores[i] {
			if !gate {
				t.Logf("recorded only: rank order = %v", scores)
				return
			}
			t.Errorf("rank order = %v, want document order", scores)
		}
	}
}

func TestLateOnEncoderConcurrentOutputsAreIsolated(t *testing.T) {
	model := sharedModel(t)

	// Two calls that hold their outputs at the same time show the isolation.
	// Each call is one full model batch, so more texts add only time.
	texts := []string{
		"func parseRequest() { validateHeaders() }",
		"func buildIndex() { writeSemanticVectors() }",
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
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	inputIDs, attention, _, tokenizeErr := tokenizeLateOnSemanticUnits(
		tokenizer,
		model.punctuation,
		units,
	)
	lateOnTokenizers.release(tokenizer)
	if tokenizeErr != nil {
		t.Fatal(tokenizeErr)
	}
	encoder, err := model.semanticRowEncoder(t.Context(), false)
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
	// Each call holds its output until all calls have one. The time of a
	// batch changes from host to host, so the test has no timer. An encoder
	// that runs one call at a time blocks here until the -timeout of the test
	// binary, which prints every goroutine.
	for range units {
		<-ready
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
	scorer := sharedModel(t)

	documents := make([]rerankDocument, semanticModelBatchRows)
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
	if len(scores) != semanticModelBatchRows {
		t.Fatalf("scores=%d, want %d", len(scores), semanticModelBatchRows)
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

// TestCLIProcessLateOnStrictEvidenceContract runs a description in a
// process, as a user does. Only strict.go holds each word of the query, so
// --lexical-only prints only it. The real model adds evict.go and lru.go,
// which hold some of the words and the same topic. Their lifts above the
// empty-document score are 0.302 and 0.294 per token, at least 0.044 above
// pipelineTauFile. The folder has a fixed model path (stableModelRoot), so
// the scores do not change from run to run.
func TestCLIProcessLateOnStrictEvidenceContract(t *testing.T) {
	requireTools(t)
	folder := stableModelRoot(t)
	writeFileAt(t, folder, "strict.go", "package sample\n// evict the least recently used entry\n")
	writeFileAt(t, folder, "evict.go", "package sample\n// evict the oldest entry\n")
	writeFileAt(t, folder, "lru.go", "package sample\n// least recently used list\n")
	cache := t.TempDir()
	const query = "evict least recently used"
	strict := filepath.Join(folder, "strict.go")

	lexical := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{"--lexical-only", query, folder},
		nil,
	)
	if got := printedFiles(lexical.stdout); lexical.code != 0 || lexical.stderr != "" ||
		!slices.Equal(got, []string{strict}) {
		t.Fatalf("lexical search printed %v, want only %s: %+v", got, strict, lexical)
	}
	reranked := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{query, folder},
		nil,
	)
	want := []string{filepath.Join(folder, "evict.go"), filepath.Join(folder, "lru.go"), strict}
	if got := printedFiles(reranked.stdout); reranked.code != 0 || reranked.stderr != "" ||
		!slices.Equal(got, want) {
		t.Fatalf("description printed %v, want %v: %+v", got, want, reranked)
	}
}

// TestCLIProcessLateOnAcceptanceBoundary runs a nonsense query and a
// description in a process. The nonsense query shares no word piece with the
// files. Its lift above the empty-document score is under 0 for each file. So
// no candidate gets to the model, and it admits no file. No file holds each
// word of the description, so --lexical-only finds nothing. Only the model
// can add auth.go: its lift is 0.277 per token. colors.go has 0.048 and does
// not print. The folder has a fixed model path (stableModelRoot), so the
// scores do not change from run to run.
func TestCLIProcessLateOnAcceptanceBoundary(t *testing.T) {
	requireTools(t)
	folder := stableModelRoot(t)
	writeFileAt(
		t,
		folder,
		"auth.go",
		"package sample\nfunc authenticateRequest(request Request) error { return validateToken(request.Token) }\n",
	)
	writeFileAt(
		t,
		folder,
		"colors.go",
		"package sample\nfunc terminalColorCode(name string) int { return ansiColors[name] }\n",
	)
	cache := t.TempDir()
	const nonexistent = "zqxvplor blingwazzle nyoomphastic"

	rejected := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{nonexistent, folder},
		nil,
	)
	if rejected.code != 1 || rejected.stdout != "" || rejected.stderr != "" {
		t.Fatalf("nonexistent query=%+v, want silent no match", rejected)
	}

	verbose := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{"--verbose", nonexistent, folder},
		nil,
	)
	if verbose.code != 1 || verbose.stdout != "" {
		t.Fatalf("verbose nonexistent query=%+v, want no match", verbose)
	}
	// The search reads the vector index, but no row is in the retrieval
	// radius. So no candidate gets to the score stage, which logs "Accepted
	// model candidates".
	if !strings.Contains(verbose.stderr, `msg="Searched semantic index"`) ||
		strings.Contains(verbose.stderr, `msg="Accepted model candidates"`) {
		t.Fatalf("verbose nonexistent query: want a vector search and no model candidate:\n%s", verbose.stderr)
	}

	const supportedQuery = "request authentication flow"
	lexical := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{"--lexical-only", supportedQuery, folder},
		nil,
	)
	if lexical.code != 1 || lexical.stdout != "" || lexical.stderr != "" {
		t.Fatalf("positive lexical control=%+v, want silent no match", lexical)
	}

	accepted := runCLIProcessWithCache(
		t,
		cache,
		t.TempDir(),
		[]string{"--verbose", supportedQuery, folder},
		nil,
	)
	auth := filepath.Join(folder, "auth.go")
	if got := printedFiles(accepted.stdout); accepted.code != 0 || !slices.Equal(got, []string{auth}) {
		t.Fatalf("accepted query printed %v, want only %s: %+v", got, auth, accepted)
	}
	if !strings.Contains(accepted.stderr, `msg="Accepted model candidates"`) || strings.Contains(accepted.stderr, "accepted=0") {
		t.Fatalf("acceptance lacks an accepted candidate:\n%s", accepted.stderr)
	}
}

func TestLateOnMaxSimRejectsZeroDocumentVectors(t *testing.T) {
	query := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	query[0] = 1
	queryMask := make([]bool, lateOnSequenceLength)
	queryMask[0] = true
	documents := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	documentMask := make([]bool, lateOnSequenceLength)
	documentMask[0] = true
	documentMask[1] = true
	if _, err := lateOnMaxSimPrepared(query, queryMask, documents, [][]bool{documentMask}); err == nil {
		t.Fatal("all-zero document scored instead of failing")
	}
	documents[semanticEmbeddingDimensions] = 1
	scores, err := lateOnMaxSimPrepared(query, queryMask, documents, [][]bool{documentMask})
	if err != nil {
		t.Fatalf("document with one usable token failed: %v", err)
	}
	if len(scores) != 1 {
		t.Fatalf("scores = %v, want one score", scores)
	}
}

func TestPrepareSemanticQueryReturnsUsableTokens(t *testing.T) {
	// PrepareSemanticQuery checks its own output. A degenerate query row gives
	// every candidate a finite score of zero, and no later check can tell it
	// from a real miss. The test checks real model output, so a provider that
	// starts to return zero rows fails here.
	model := sharedModel(t)
	prepared, err := model.PrepareSemanticQuery(t.Context(), "rank search results before the output limit")
	if err != nil {
		t.Fatal(err)
	}
	if !lateOnRowHasUsableToken(prepared.tokens, 0, prepared.scoreMask) {
		t.Fatal("PrepareSemanticQuery returned a query row with no usable token")
	}
	// An empty query still produces the model prefix tokens, so it must also
	// survive the guard rather than failing the search.
	if _, err := model.PrepareSemanticQuery(t.Context(), ""); err != nil {
		t.Fatalf("an empty query failed the guard: %v", err)
	}
}

func TestLateOnRowUsableTokenScansEveryMaskedToken(t *testing.T) {
	documents := make([]float32, 2*lateOnSequenceLength*semanticEmbeddingDimensions)
	mask := make([]bool, lateOnSequenceLength)
	mask[0] = true
	mask[1] = true
	// Row 0: a non-finite first token must not end the scan, because a later
	// token can still carry the row.
	documents[0] = float32(math.NaN())
	documents[semanticEmbeddingDimensions] = 1
	if !lateOnRowHasUsableToken(documents, 0, mask) {
		t.Fatal("a non-finite first token ended the scan")
	}
	// Row 1 is untouched, so it stays unusable. This also proves the row offset
	// is applied, because row 0 is usable.
	if lateOnRowHasUsableToken(documents, 1, mask) {
		t.Fatal("an empty second row reported a usable token")
	}
	// A token outside the mask must not rescue the row.
	documents[lateOnSequenceLength*semanticEmbeddingDimensions+5*semanticEmbeddingDimensions] = 1
	if lateOnRowHasUsableToken(documents, 1, mask) {
		t.Fatal("an unmasked token rescued the row")
	}
}

func TestLateOnForcedProviderParsesEnvironment(t *testing.T) {
	t.Setenv(lateOnProviderEnv, "")
	if forced, err := lateOnForcedProvider(); forced != "" || err != nil {
		t.Fatalf("empty=%q err=%v, want automatic", forced, err)
	}
	t.Setenv(lateOnProviderEnv, "auto")
	if forced, err := lateOnForcedProvider(); forced != "" || err != nil {
		t.Fatalf("auto=%q err=%v, want automatic", forced, err)
	}
	t.Setenv(lateOnProviderEnv, " "+lateOnCPUProviderName+" ")
	if forced, err := lateOnForcedProvider(); forced != lateOnCPUProviderName || err != nil {
		t.Fatalf("cpu=%q err=%v", forced, err)
	}
	t.Setenv(lateOnProviderEnv, lateOnAcceleratedProviderName)
	if forced, err := lateOnForcedProvider(); forced != lateOnAcceleratedProviderName || err != nil {
		t.Fatalf("accelerated=%q err=%v", forced, err)
	}
	t.Setenv(lateOnProviderEnv, "CPU")
	if forced, err := lateOnForcedProvider(); forced != lateOnCPUProviderName || err != nil {
		t.Fatalf("mixed case=%q err=%v", forced, err)
	}
	t.Setenv(lateOnProviderEnv, "gpu")
	err := func() error { _, err := lateOnForcedProvider(); return err }()
	if err == nil {
		t.Fatal("unknown provider did not fail")
	}
	// The message must name only the providers this build can select, and must
	// not double-quote them.
	message := err.Error()
	if strings.Contains(message, `\"`) {
		t.Fatalf("message escapes its own quotes: %s", message)
	}
	if !strings.Contains(message, strconv.Quote(lateOnCPUProviderName)) ||
		!strings.Contains(message, strconv.Quote("auto")) {
		t.Fatalf("message does not list the accepted values: %s", message)
	}
	if !lateOnProviderHasAccelerator && strings.Count(message, strconv.Quote(lateOnCPUProviderName)) != 1 {
		t.Fatalf("message repeats one provider on a single-provider build: %s", message)
	}
}

func TestLateOnProbeRowsCoverActiveTokens(t *testing.T) {
	ids, attention := lateOnProbeRows()
	if len(ids) != semanticModelBatchRows*lateOnSequenceLength || len(attention) != len(ids) {
		t.Fatalf("ids=%d attention=%d, want %d", len(ids), len(attention), semanticModelBatchRows*lateOnSequenceLength)
	}
	// The probe needs the same rows on each call. Both providers must get the
	// same input, and a cached verdict must describe the input of a later
	// process.
	again, againAttention := lateOnProbeRows()
	if !reflect.DeepEqual(ids, again) || !reflect.DeepEqual(attention, againAttention) {
		t.Fatal("the probe row is not stable between calls")
	}
	// Every attended token must be one that the model can embed, and no other
	// token is attended. Otherwise the check compares the two providers on
	// padding.
	for row := range semanticModelBatchRows {
		base := row * lateOnSequenceLength
		for token := range lateOnSequenceLength {
			attended := attention[base+token] == 1
			if attended != (token < lateOnProbeActiveTokens) {
				t.Fatalf("row %d attention[%d]=%d outside the active span", row, token, attention[base+token])
			}
			if !attended && ids[base+token] != lateOnPadTokenID {
				t.Fatalf("row %d ids[%d]=%d is not padding", row, token, ids[base+token])
			}
			if attended && ids[base+token] == lateOnPadTokenID {
				t.Fatalf("row %d ids[%d] is padding inside the active span", row, token)
			}
		}
	}
	// The batch must carry different content in each row. Identical rows would
	// pay for a full batch of inference and sample one row of it.
	distinct := map[string]bool{}
	for row := range semanticModelBatchRows {
		base := row * lateOnSequenceLength
		distinct[fmt.Sprint(ids[base:base+lateOnProbeActiveTokens])] = true
	}
	if len(distinct) != semanticModelBatchRows {
		t.Fatalf("the probe batch holds %d distinct rows, want %d", len(distinct), semanticModelBatchRows)
	}
}

func TestLateOnOutputsAgreeSeparatesFaultFromNoise(t *testing.T) {
	batch := semanticModelBatchRows * lateOnSequenceLength * semanticEmbeddingDimensions
	reference := make([]float32, batch)
	for row := range semanticModelBatchRows {
		for token := range lateOnProbeActiveTokens {
			offset := (row*lateOnSequenceLength + token) * semanticEmbeddingDimensions
			for dimension := range semanticEmbeddingDimensions {
				reference[offset+dimension] = float32(1 + (row+token+dimension)%7)
			}
		}
	}
	if agree, why := lateOnOutputsAgree(reference, reference); !agree {
		t.Fatalf("identical outputs disagreed: %s", why)
	}

	// A uniform shrink keeps every cosine at 1 but lowers every score by the
	// same factor, which is the shape of the macOS 15 fault. It must be
	// rejected.
	scaled := make([]float32, batch)
	for index, value := range reference {
		scaled[index] = value * 0.87
	}
	if agree, _ := lateOnOutputsAgree(scaled, reference); agree {
		t.Fatal("a uniform 13 percent shrink agreed")
	}

	// A shrink inside the measured provider spread must still agree.
	nudged := make([]float32, batch)
	for index, value := range reference {
		nudged[index] = value * 0.999
	}
	if agree, why := lateOnOutputsAgree(nudged, reference); !agree {
		t.Fatalf("a 0.1 percent scale disagreed: %s", why)
	}

	// A zero token vector is the shape the macOS 15 fault takes. Put the
	// fault in the last row, so a check that inspects only the first row fails
	// this test.
	lastRow := (semanticModelBatchRows - 1) * lateOnSequenceLength
	zeroed := append([]float32(nil), reference...)
	for dimension := range semanticEmbeddingDimensions {
		zeroed[(lastRow+35)*semanticEmbeddingDimensions+dimension] = 0
	}
	if agree, _ := lateOnOutputsAgree(zeroed, reference); agree {
		t.Fatal("a zero token vector agreed")
	}

	// A direction change past the limit must be rejected.
	turned := append([]float32(nil), reference...)
	turnedAt := (lastRow + 7) * semanticEmbeddingDimensions
	turned[turnedAt] = -turned[turnedAt]
	if agree, _ := lateOnOutputsAgree(turned, reference); agree {
		t.Fatal("a turned token agreed")
	}

	// Noise well inside the measured provider spread must be accepted.
	noisy := append([]float32(nil), reference...)
	for index := range noisy {
		noisy[index] *= 1 + float32(index%3)*1e-6
	}
	if agree, why := lateOnOutputsAgree(noisy, reference); !agree {
		t.Fatalf("measured-scale noise disagreed: %s", why)
	}

	if agree, _ := lateOnOutputsAgree(reference[:10], reference); agree {
		t.Fatal("a short output agreed")
	}
}

func TestSemanticProviderNameIsSettledAfterConstruction(t *testing.T) {
	// The fallback logs print this value. A real construction must leave a
	// settled provider, not an intent.
	// An atomic.Value cannot hold nil again, so a value that was not set
	// comes back as an empty string, which names no provider in use.
	previous := lateOnSelectedProvider.Load()
	t.Cleanup(func() { lateOnSelectedProvider.Store(cmp.Or[any](previous, "")) })
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	model, err := newLateOnSemanticModel(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := model.Close(); err != nil {
			t.Errorf("close model: %v", err)
		}
	})
	if _, err := model.PrepareSemanticQuery(t.Context(), "rank results before the output limit"); err != nil {
		t.Fatal(err)
	}
	name := semanticProviderName()
	if strings.HasPrefix(name, "selecting:") {
		t.Fatalf("provider name is still an intent after a successful call: %s", name)
	}
	if name != lateOnCPUProviderName && name != lateOnAcceleratedProviderName {
		t.Fatalf("provider name = %q, want a known provider", name)
	}
}

func TestDegenerateScoringErrorCarriesTheProviderSentinel(t *testing.T) {
	// The search must tell a provider fault from any other scoring failure.
	// Otherwise a wrong provider on a built index drops every search to text
	// ranking and never records itself (recordFault).
	query := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	query[0] = 1
	queryMask := make([]bool, lateOnSequenceLength)
	queryMask[0] = true
	documents := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	documentMask := make([]bool, lateOnSequenceLength)
	documentMask[0] = true
	_, err := lateOnMaxSimPrepared(query, queryMask, documents, [][]bool{documentMask})
	if !errors.Is(err, errDegenerateSemanticVector) {
		t.Fatalf("error = %v, want the degenerate-output sentinel", err)
	}
	// A shape failure is not a provider fault and must not be reported as one.
	_, shapeErr := lateOnMaxSimPrepared(query[:10], queryMask, documents, [][]bool{documentMask})
	if errors.Is(shapeErr, errDegenerateSemanticVector) {
		t.Fatalf("a shape error claimed a provider fault: %v", shapeErr)
	}
}

// TestScoresWithSemanticQueryBatchesAboveModelWidth scores 300 documents in
// one call. The scorer runs ceil(300/128) = 3 encoder batches. It returns one
// score per document, equal to the scores of smaller calls.
func TestScoresWithSemanticQueryBatchesAboveModelWidth(t *testing.T) {
	scorer := sharedModel(t)
	documents := make([]rerankDocument, 300)
	for index := range documents {
		text := fmt.Sprintf("func rank%d() { rankCorpusResults(%d) }", index, index)
		documents[index] = rerankDocument{Path: fmt.Sprintf("pkg/f%03d.go", index), Language: "Go", Text: text, matchEnd: len(text)}
	}
	prepared, err := scorer.PrepareSemanticQuery(t.Context(), "find where search results are ranked")
	if err != nil {
		t.Fatal(err)
	}
	all, err := scorer.ScoresWithSemanticQuery(t.Context(), prepared, documents)
	if err != nil || len(all) != len(documents) {
		t.Fatalf("scores=%d err=%v, want %d", len(all), err, len(documents))
	}
	for start := 0; start < len(documents); start += 100 {
		part, err := scorer.ScoresWithSemanticQuery(t.Context(), prepared, documents[start:start+100])
		if err != nil {
			t.Fatal(err)
		}
		for offset, score := range part {
			if diff := math.Abs(float64(score - all[start+offset])); diff > 1e-3 {
				t.Fatalf("document %d scored %v in one call and %v alone", start+offset, all[start+offset], score)
			}
		}
	}
}

// newTestLateOnModel returns a new LateOn model; the caller closes it.
func newTestLateOnModel(t *testing.T) *lateOnModel {
	t.Helper()
	model, err := newLateOnSemanticModel(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	lateOn, ok := model.(*lateOnModel)
	if !ok {
		t.Fatalf("model type is %T, want *lateOnModel", model)
	}
	return lateOn
}

// heldBuildContext is the context of an encoder build that a test holds.
// newLateOnRowEncoder and newLateOnCPUBuildEncoder read the context error
// first, in the sync.Once of the model. The first Err call closes started and
// waits until release closes. So the build stays in its Once for that time.
type heldBuildContext struct {
	context.Context
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (c *heldBuildContext) Err() error {
	c.once.Do(func() {
		close(c.started)
		<-c.release
	})
	return c.Context.Err()
}

// closeDuringBuild starts a call of model that builds an encoder for the
// index build, and holds the build at its start (heldBuildContext). It starts
// Close while the build is held, then lets the build go on. The call passed
// the closed check before Close, so it must get its encoder. Close must wait
// for the build and close what it made.
func closeDuringBuild(t *testing.T, model *lateOnModel) {
	t.Helper()
	ctx := &heldBuildContext{Context: t.Context(), started: make(chan struct{}), release: make(chan struct{})}
	built := make(chan error, 1)
	go func() {
		encoder, err := model.semanticRowEncoder(ctx, false)
		if err == nil && encoder == nil {
			err = errors.New("no encoder and no error")
		}
		built <- err
	}()
	select {
	case <-ctx.started:
	case err := <-built:
		t.Fatalf("the build ended before it read its context, so the test cannot hold it: err=%v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- model.Close() }()
	// Close sets closed first. Wait for it, so Close starts before the build
	// goes on.
	for !model.closed.Load() {
		runtime.Gosched()
	}
	close(ctx.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-built; err != nil {
		t.Fatalf("a call that started before Close: %v", err)
	}
}

// TestLateOnModelCloseAtEachStage closes a model at four stages:
//   - before its first call;
//   - after its first call;
//   - while the first call builds the encoder;
//   - while a later call builds the second CPU session (closeDuringBuild).
//
// Close must close each encoder that a build made, also the encoder of a
// build that runs now. A call after Close gets the closed error. -race sees
// no write of an encoder field after its read.
func TestLateOnModelCloseAtEachStage(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	t.Run("close first", func(t *testing.T) {
		model := newTestLateOnModel(t)
		if err := model.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := model.semanticRowEncoder(t.Context(), false); !errors.Is(err, errLateOnModelClosed) {
			t.Fatalf("call after Close: err=%v, want errLateOnModelClosed", err)
		}
	})
	t.Run("close after use", func(t *testing.T) {
		model := newTestLateOnModel(t)
		if _, err := model.semanticRowEncoder(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if err := model.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := model.semanticRowEncoder(t.Context(), false); !errors.Is(err, errLateOnModelClosed) {
			t.Fatalf("call after Close: err=%v, want errLateOnModelClosed", err)
		}
	})
	t.Run("close during the first build", func(t *testing.T) {
		model := newTestLateOnModel(t)
		closeDuringBuild(t, model)
		if model.encoder == nil || model.encoder.session != nil {
			t.Fatal("Close did not close the encoder of the build in flight")
		}
	})
	t.Run("close during the build session", func(t *testing.T) {
		t.Setenv(lateOnProviderEnv, lateOnCPUProviderName)
		model := newTestLateOnModel(t)
		if _, err := model.semanticRowEncoder(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if !model.encoder.interactive {
			t.Fatal("the first CPU session is not sized for one call")
		}
		closeDuringBuild(t, model)
		if model.build == nil || model.build.session != nil {
			t.Fatal("Close did not close the build session in flight")
		}
	})
}

// TestLateOnSessionThreadsFollowTheFirstUse pins the CPU provider. A model
// whose first call is the index build gets the build's thread count, and a
// later query makes no second session. A model whose first call is a query
// gets the interactive count, and a scoring call uses that session. A later
// index build gets a session of its own with the build's count. On 1 or 2
// CPUs both counts are 1, so the test sets GOMAXPROCS to 8 for its run. Then
// the build count is round(sqrt(8/2)) = 2 and the interactive count is
// round(8*2/3) = 5, on each host.
func TestLateOnSessionThreadsFollowTheFirstUse(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	t.Setenv(lateOnProviderEnv, lateOnCPUProviderName)
	previous := runtime.GOMAXPROCS(8)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	const build, interactive = 2, 5
	newModel := func(t *testing.T) *lateOnModel {
		t.Helper()
		model := newTestLateOnModel(t)
		t.Cleanup(func() { _ = model.Close() })
		return model
	}
	t.Run("build first", func(t *testing.T) {
		model := newModel(t)
		if got := model.SemanticCallCPUs(t.Context()); got != build {
			t.Fatalf("build call CPUs=%d, want %d", got, build)
		}
		if _, err := model.PrepareSemanticQuery(t.Context(), "parse the request"); err != nil {
			t.Fatal(err)
		}
		if model.build != nil {
			t.Fatal("a query after the build made a second session")
		}
	})
	t.Run("query first", func(t *testing.T) {
		model := newModel(t)
		if _, err := model.PrepareSemanticQuery(t.Context(), "parse the request"); err != nil {
			t.Fatal(err)
		}
		if model.encoder.callCPUs != interactive {
			t.Fatalf("query session threads=%d, want %d", model.encoder.callCPUs, interactive)
		}
		// Scoring is one call too: it uses the query session.
		if _, err := scoreWithTestSemanticEmbedder(t.Context(), model, "parse the request", lateOnBenchmarkDocuments(4)); err != nil {
			t.Fatal(err)
		}
		if model.build != nil {
			t.Fatal("a scoring call made the build session")
		}
		if got := model.SemanticCallCPUs(t.Context()); got != build {
			t.Fatalf("build call CPUs after a query=%d, want %d (a session of its own)", got, build)
		}
		if model.build == nil || model.build == model.encoder {
			t.Fatal("the build did not get a session of its own")
		}
	})
}
