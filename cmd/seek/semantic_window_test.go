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
// and symbol: a window holds one unit only.
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
	rows, err := windowSemanticUnits(t.Context(), tokenizer, units)
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
			rows, err := windowSemanticUnits(t.Context(), tokenizer, units)
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
	rows, err := windowSemanticUnits(t.Context(), tokenizer, units)
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
	rows, err := windowSemanticUnits(t.Context(), tokenizer, []semanticUnit{unit})
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
