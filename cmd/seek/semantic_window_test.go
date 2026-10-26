package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	ctags "github.com/sourcegraph/go-ctags"
)

// longSemanticFunction returns a Go file of exactly size bytes whose second
// line starts one long function.
func longSemanticFunction(size int) []byte {
	var body strings.Builder
	body.WriteString("package sample\nfunc Long() {\n")
	for line := 0; body.Len() < size-64; line++ {
		fmt.Fprintf(&body, "\tvalue%d = compute(value%d, \"window body line\")\n", line, line+1)
	}
	body.WriteString("}\n")
	content := body.String()
	return []byte(content + "//" + strings.Repeat("x", size-len(content)-3) + "\n")
}

// TestLongUnitEmbedsEveryWindow builds a 15,191-byte function: it gives at
// least three rows, the rows cover every byte of the file in order, each model
// input fits one model row, and the model embeds every row.
func TestLongUnitEmbedsEveryWindow(t *testing.T) {
	requireTools(t)
	content := longSemanticFunction(15_191)
	if len(content) != 15_191 {
		t.Fatalf("fixture has %d bytes", len(content))
	}
	var mu sync.Mutex
	var embedded []semanticUnit
	embedder := semanticBuildTestEmbedder{embed: func(_ context.Context, units []semanticUnit) ([]semanticUnitEmbedding, error) {
		mu.Lock()
		embedded = append(embedded, units...)
		mu.Unlock()
		embeddings := make([]semanticUnitEmbedding, len(units))
		for index := range embeddings {
			embeddings[index] = testSemanticUnitEmbedding(semanticVector{0: 1})
		}
		return embeddings, nil
	}}
	documents := make(chan fileContent, 1)
	documents <- makeFileContent(t, "long.go", content)
	close(documents)
	manifest, err := buildSemanticGeneration(
		t.Context(), filepath.Join(t.TempDir(), "semantic"), "source-long", embedder, searchResources{},
		func(context.Context) (<-chan fileContent, func() error) {
			return documents, func() error { return nil }
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Rows < 3 || len(embedded) != int(manifest.Rows) {
		t.Fatalf("rows=%d embedded=%d, want more than 2 and every row", manifest.Rows, len(embedded))
	}
	rows := map[uint64]semanticUnit{}
	for _, unit := range embedded {
		rows[unit.row] = unit
	}
	next := uint64(0)
	for row := range manifest.Rows {
		unit, ok := rows[row]
		if !ok || unit.start != next || unit.end <= unit.start {
			t.Fatalf("row %d spans %d:%d, want a start at %d", row, unit.start, unit.end, next)
		}
		if n := len(unit.modelInput); n == 0 || n > lateOnSequenceLength ||
			unit.modelInput[0] != lateOnCLSTokenID || unit.modelInput[n-1] != lateOnSEPTokenID {
			t.Fatalf("row %d model input has %d tokens or an invalid layout", row, n)
		}
		next = unit.end
	}
	if next != uint64(len(content)) {
		t.Fatalf("rows end at %d, want %d", next, len(content))
	}
}

// TestWindowsRepeatTheHeaderAndKeepTheirUnit splits one unit: every window
// starts with the same path and language header, and keeps the unit's kind
// and symbol: a window holds one unit only. Each window ID matches its span.
func TestWindowsRepeatTheHeaderAndKeepTheirUnit(t *testing.T) {
	content := longSemanticFunction(6_000)
	units := extractSemanticUnits("pkg/long.go", content, []*ctags.Entry{
		{Name: "Long", Kind: "function", Language: "Go", Line: 2},
	}, nil)
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	rows, err := windowSemanticUnits(t.Context(), tokenizer, units, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) <= len(units) {
		t.Fatalf("rows=%d for %d units, want windows", len(rows), len(units))
	}
	header := map[string][]int{}
	for _, row := range rows {
		if row.kind == semanticUnitSymbol && row.symbol == "" {
			t.Fatalf("window lost its symbol: %+v", row)
		}
		key := fmt.Sprint(row.kind, row.symbol)
		prefix := row.modelInput[:min(8, len(row.modelInput))]
		if want, ok := header[key]; ok && fmt.Sprint(want) != fmt.Sprint(prefix) {
			t.Fatalf("window header %v differs from %v", prefix, want)
		}
		header[key] = prefix
		if row.id != makeSemanticUnitID(row) {
			t.Fatalf("window id does not match its span")
		}
	}
}

// TestUnitScoreIsBestWindow gives one file three window rows: the file's
// candidate is its best window, with that window's score.
func TestUnitScoreIsBestWindow(t *testing.T) {
	generation := &semanticGeneration{rows: []semanticUnit{
		{row: 0, path: "a.go", start: 0, end: 10},
		{row: 1, path: "a.go", start: 10, end: 20},
		{row: 2, path: "a.go", start: 20, end: 30},
		{row: 3, path: "b.go", start: 0, end: 10},
	}}
	hits := []semanticHit{{row: 1, score: 0.9}, {row: 3, score: 0.5}, {row: 2, score: 0.2}, {row: 0, score: 0.1}}
	candidates, err := semanticHitFiles(generation, hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].unit.row != 1 || candidates[0].proxyScore != 0.9 {
		t.Fatalf("candidates=%+v, want a.go through its window 1 at 0.9", candidates)
	}
}

// TestWindowsEndAtWordStartsAndKeepRunes splits a long ASCII unit and a long
// CJK and emoji unit: every inner window edge sits at white space, no window
// splits a rune, and the final scorer's document of each window fits one
// model row with no truncation. Each unit body is one line of words that take
// several tokens each, so no window has a line start to end at and the word
// start rule sets every edge.
func TestWindowsEndAtWordStartsAndKeepRunes(t *testing.T) {
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	words := strings.ReplaceAll(string(randomWordText(7, 6_000, "end")), "\n", " ")
	for name, content := range map[string][]byte{
		"ascii": []byte("package sample\nfunc Long() {\n\tdoc := \"" + words + "\"\n}\n"),
		"cjk":   []byte("package sample\nfunc Long() {\n\tdoc := \"" + strings.Repeat("请求处理 🚀 处理请求的函数 説明 ", 200) + "\"\n}\n"),
	} {
		t.Run(name, func(t *testing.T) {
			units := extractSemanticUnits("pkg/long.go", content, []*ctags.Entry{
				{Name: "Long", Kind: "function", Language: "Go", Line: 2},
			}, nil)
			rows, err := windowSemanticUnits(t.Context(), tokenizer, units, "")
			if err != nil {
				t.Fatal(err)
			}
			// The fixtures hold no other white-space bytes.
			isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' }
			edges := 0
			for index, row := range rows {
				if !utf8.Valid(row.text) {
					t.Fatalf("window %d splits a rune", index)
				}
				if index+1 < len(rows) && rows[index+1].start == row.end && row.symbol == rows[index+1].symbol {
					edges++
					edge := int(row.end)
					if !isSpace(content[edge-1]) && !isSpace(content[edge]) {
						t.Fatalf("window %d ends inside a word: %q|%q", index, content[max(0, edge-8):edge], content[edge:min(len(content), edge+8)])
					}
				}
				document := rerankDocument{Path: row.path, Language: row.language, Symbol: row.symbol,
					Text: string(row.text), matchEnd: len(row.text)}
				ids, err := encodeLateOnDocument(tokenizer, document, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				if len(ids) > lateOnSequenceLength {
					t.Fatalf("window %d gives the final scorer %d tokens, more than one row", index, len(ids))
				}
			}
			if edges == 0 {
				t.Fatalf("rows=%d have no inner window edge", len(rows))
			}
		})
	}
}

// TestWindowsStayInsideTheirUnit checks a file of several units: every row
// lies inside one unit and keeps its kind and symbol.
func TestWindowsStayInsideTheirUnit(t *testing.T) {
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	content := append([]byte("// header gap\n"), longSemanticFunction(3_000)[len("package sample\n"):]...)
	content = append(content, []byte("func Short() {}\n")...)
	units := extractSemanticUnits("pkg/three.go", content, []*ctags.Entry{
		{Name: "Long", Kind: "function", Language: "Go", Line: 2},
		{Name: "Short", Kind: "function", Language: "Go", Line: strings.Count(string(content), "\n")},
	}, nil)
	rows, err := windowSemanticUnits(t.Context(), tokenizer, units, "")
	if err != nil {
		t.Fatal(err)
	}
	for index, row := range rows {
		if !slices.ContainsFunc(units, func(unit semanticUnit) bool {
			return unit.start <= row.start && row.end <= unit.end && unit.kind == row.kind && unit.symbol == row.symbol
		}) {
			t.Fatalf("row %d (%d:%d) spans two units", index, row.start, row.end)
		}
	}
}

// TestRowKeyHoldsTheModelPrefix checks that the key is not a hash of the
// token IDs alone: a model or codec change gives other keys.
func TestRowKeyHoldsTheModelPrefix(t *testing.T) {
	input := []int{lateOnCLSTokenID, 100, 200, lateOnSEPTokenID}
	var plain []byte
	for _, id := range input {
		plain = binary.LittleEndian.AppendUint32(plain, uint32(id))
	}
	if makeSemanticRowKey(input) == semanticRowKey(sha256.Sum256(plain)) {
		t.Fatal("the row key ignores the model compatibility prefix")
	}
}

// TestWindowHeaderKeepsAQuarter gives a unit a symbol of 1,900 bytes: the
// header takes a quarter of the content slots, so every window still holds
// body tokens.
func TestWindowHeaderKeepsAQuarter(t *testing.T) {
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	unit := extractSemanticUnits("pkg/long.go", longSemanticFunction(4_000), nil, nil)[0]
	unit.symbol = strings.Repeat("verylongsymbolname ", 100)
	rows, err := windowSemanticUnits(t.Context(), tokenizer, []semanticUnit{unit}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 {
		t.Fatalf("rows=%d, want windows", len(rows))
	}
	// Every window starts with CLS, [D] and the same header. Only the first
	// window body starts with "package", so the common prefix of all rows is
	// CLS, [D] and the header.
	common := len(rows[0].modelInput)
	for _, row := range rows[1:] {
		n := 0
		for n < min(common, len(row.modelInput)) && row.modelInput[n] == rows[0].modelInput[n] {
			n++
		}
		common = n
	}
	// A row has 128 slots less CLS, [D] and SEP; the symbol alone needs more
	// than a quarter of them.
	contentSlots := lateOnSequenceLength - 3
	if header := common - 2; header != contentSlots/4 {
		t.Fatalf("header has %d tokens, want a quarter of %d content slots", header, contentSlots)
	}
	for index, row := range rows {
		if len(row.modelInput) > lateOnSequenceLength || len(row.modelInput) <= common+1 {
			t.Fatalf("window %d has %d tokens after a %d-token prefix", index, len(row.modelInput), common)
		}
	}
}

// TestSanitizedTextMapsBackToRawBytes checks the offset map of invalid UTF-8
// and NUL: a tokenizer offset after U+FFFD maps back to the raw byte.
func TestSanitizedTextMapsBackToRawBytes(t *testing.T) {
	text, raw := sanitizedSemanticText([]byte("a\xff\xfeb\x00c"))
	if text != "a\uFFFDb\uFFFDc" {
		t.Fatalf("text=%q", text)
	}
	for sanitized, want := range map[int]int{0: 0, 1: 1, 4: 3, 5: 4, 8: 5, len(text): 6} {
		if got := raw(sanitized); got != want {
			t.Errorf("offset %d maps to %d, want %d", sanitized, got, want)
		}
	}
}

// TestTokenizerPoolReusesTokenizers checks the shared pool: a released
// tokenizer serves the next take, and two takes at once get two.
func TestTokenizerPoolReusesTokenizers(t *testing.T) {
	first, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two takes got one tokenizer")
	}
	lateOnTokenizers.release(second)
	again, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again != second {
		t.Fatal("a released tokenizer did not serve the next take")
	}
	lateOnTokenizers.release(again)
	lateOnTokenizers.release(first)
}

// TestEmbedUsesTheWindowModelInput checks that the model encodes a row from
// its window token IDs, not from its text.
func TestEmbedUsesTheWindowModelInput(t *testing.T) {
	input := []int{lateOnCLSTokenID, lateOnDocumentPrefixID, 1000, 1001, lateOnSEPTokenID}
	ids, _, _, err := tokenizeLateOnSemanticUnits(nil, nil, []semanticUnit{{text: []byte("other text"), modelInput: input}})
	if err != nil {
		t.Fatal(err)
	}
	for index, id := range input {
		if ids[index] != int64(id) {
			t.Fatalf("input id %d is %d, want %d", index, ids[index], id)
		}
	}
}

// TestWindowHeaderMatchesScorerDocument checks that the final scorer reads
// the lines that the window header of a file's rows holds, for a folder node
// (model path base, absolute) and for a Git node (none): the vectors and the
// final score then describe the same model input. The folder header names the
// last 4 parts of the absolute path.
func TestWindowHeaderMatchesScorerDocument(t *testing.T) {
	unit := semanticUnit{path: "notes/c.go", language: "Go", symbol: "func Handle"}
	for _, base := range []string{"/Users/someone/Dev", ""} {
		document := rerankDocument{Path: unit.path, Language: unit.language, Symbol: unit.symbol, Text: "body", modelBase: base}
		serialized, metadataEnd, _, _ := serializeLateOnDocumentWithMatch(document)
		if header := semanticWindowHeader(unit, base); serialized[:metadataEnd] != header {
			t.Fatalf("base %q: the scorer reads %q, the window header is %q", base, serialized[:metadataEnd], header)
		}
	}
	if header := semanticWindowHeader(unit, "/Users/someone/Dev"); !strings.HasPrefix(header, "path: someone/Dev/notes/c.go\n") {
		t.Fatalf("folder header %q, want the last 4 parts of the absolute path", header)
	}
}

// TestScorerDocumentsNameTheModelPathBase searches a folder with a strict hit
// and a vector-only hit, and a Git repository: every document that the final
// scorer reads names the base that the rows' windows name, the folder's
// absolute path, and none for the Git node. The test model accepts every row,
// so semantic.go, which holds no query word, reaches the scorer from its rows.
func TestScorerDocumentsNameTheModelPathBase(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "semantic.go", "package sample\n// dispatches a request\n")
	repo := initGitRepo(t, "strict.go", "package sample\n// alpha beta\n")
	for operand, want := range map[string]string{
		folder: filepath.ToSlash(canonicalCorpusPath(folder)),
		repo:   "",
	} {
		_, scorer, err := stubSearch(t, payloadAccept, "alpha beta", operand)
		if err != nil {
			t.Fatal(err)
		}
		scorer.mu.Lock()
		documents := slices.Clone(scorer.documents)
		scorer.mu.Unlock()
		paths := map[string]bool{}
		for _, document := range documents {
			paths[document.Path] = true
			if document.modelBase != want {
				t.Fatalf("%s: the scorer document of %s names base %q, want %q", operand, document.Path, document.modelBase, want)
			}
		}
		if operand == folder && (!paths["strict.go"] || !paths["semantic.go"]) {
			t.Fatalf("the scorer read %v, want the strict and the vector hit", paths)
		}
	}
}

// TestWindowHeaderMarksACutPath windows units whose header passes the token
// share of the metadata in the path line or after it, and a unit whose header
// fits: only a cut inside the path line marks the windows.
func TestWindowHeaderMarksACutPath(t *testing.T) {
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	long := "kubernetes_cluster_autoscaler_cloudprovider_integration/aws_elastic_kubernetes_service_managed_nodegroup_provider/autoscaling_group_instance_template_cache/one.go"
	for _, row := range []struct {
		path, symbol string
		want         bool
	}{
		{path: long, want: true},
		{path: "pkg/a.go", symbol: strings.Repeat("very_long_symbol_name_", 20), want: false},
		{path: "pkg/a.go", symbol: "Run", want: false},
	} {
		unit := semanticUnit{path: row.path, language: "Go", symbol: row.symbol, text: []byte("package x\n\nfunc Run() int {\n\treturn 1\n}\n")}
		windows, err := semanticUnitWindows(tokenizer, unit, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, window := range windows {
			if window.pathCut != row.want {
				t.Fatalf("path %.30s, symbol %.20s: pathCut %v, want %v", row.path, row.symbol, window.pathCut, row.want)
			}
		}
	}
}

// TestEqualPathLineKeysAreMarkedCut windows the same content under a name and
// under that name with ".orig", for names of 1 to 80 bytes below a long
// folder, so the path line crosses the header's token share. When the two
// keys are equal, the header did not hold the whole path line, and both rows
// must mark the cut; the boundary is a line that fills the share up to its
// newline. Each row marks the cut exactly when its path line through the
// newline passes the share. The lengths must give equal keys and uncut names.
func TestEqualPathLineKeysAreMarkedCut(t *testing.T) {
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	key := func(path string) (semanticRowKey, bool) {
		t.Helper()
		unit := semanticUnit{path: path, language: "Go", text: []byte("package x\n\nfunc Run() int {\n\treturn 1\n}\n")}
		windows, err := semanticUnitWindows(tokenizer, unit, "")
		if err != nil {
			t.Fatal(err)
		}
		return makeSemanticRowKey(windows[0].input), windows[0].pathCut
	}
	// lineTokens counts the header tokens of the path line through its
	// newline, which the pre-tokenizer splits from the next line: the line is
	// whole when they fit the header's share of the content tokens.
	lineTokens := func(name string) (int, int) {
		t.Helper()
		ids, _, err := tokenizer.encode(lateOnDocumentPrefix+"path: "+name+"\n", true, true)
		if err != nil {
			t.Fatal(err)
		}
		prefixEnd := slices.Index(ids, lateOnDocumentPrefixID) + 1
		return len(ids) - 1 - prefixEnd, (lateOnSequenceLength - prefixEnd - 1) / 4
	}
	equal, uncut := 0, 0
	for length := 1; length <= 80; length++ {
		name := "cluster_autoscaler_cloud/elastic_service_nodegroup/instance_template/" + strings.Repeat("ab", length/2) + strings.Repeat("c", length%2) + ".go"
		first, firstCut := key(name)
		second, secondCut := key(name + ".orig")
		if tokens, share := lineTokens(name); firstCut != (tokens > share) {
			t.Fatalf("%d bytes: path line of %d tokens for a share of %d, cut %v", length, tokens, share, firstCut)
		}
		switch {
		case first == second:
			equal++
			if !firstCut || !secondCut {
				t.Fatalf("%d bytes: equal keys, cut %v and %v, want both cut", length, firstCut, secondCut)
			}
		case !firstCut:
			uncut++
		}
	}
	if equal == 0 || uncut == 0 {
		t.Fatalf("names with equal keys %d, uncut names %d: the lengths do not cross the share", equal, uncut)
	}
}

// TestFoldedTailsAreNotPlain windows pairs of tails that the tokenizer gives
// one key, as the reuse rule expects (semanticPlainTail): NFC maps the Kelvin
// sign to "K", and [MASK] strips the white space before it. In each pair, one
// tail is not plain.
func TestFoldedTailsAreNotPlain(t *testing.T) {
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	key := func(path, language, text string) semanticRowKey {
		t.Helper()
		windows, err := semanticUnitWindows(tokenizer, semanticUnit{path: path, language: language, text: []byte(text)}, "")
		if err != nil {
			t.Fatal(err)
		}
		return makeSemanticRowKey(windows[0].input)
	}
	for _, pair := range []struct{ first, second, language, text string }{
		{first: "dir/K.go", second: "dir/\u212a.go", language: "Go", text: "package x\n"},
		{first: "dir/x[MASK].go", second: "dir/x [MASK].go", language: "Go", text: "package x\n"},
		// With no language line, a text that starts with [MASK] strips the
		// newline and the spaces that end the path line.
		{first: "dir/notes", second: "dir/notes ", text: "[MASK] x"},
	} {
		if key(pair.first, pair.language, pair.text) != key(pair.second, pair.language, pair.text) {
			t.Fatalf("%q and %q: keys differ, want one key", pair.first, pair.second)
		}
		if semanticPlainTail(pair.first) && semanticPlainTail(pair.second) {
			t.Fatalf("%q and %q share a key, but both are plain", pair.first, pair.second)
		}
	}
}
