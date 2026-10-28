package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	ctags "github.com/sourcegraph/go-ctags"
)

func TestExtractSemanticUnitsCoversSymbolsAndGaps(t *testing.T) {
	content := []byte("package sample\n\n// Alpha docs\nfunc Alpha() {}\n\nfunc Beta() {}\n")
	entries := []*ctags.Entry{
		{Name: "Beta", Line: 6, Language: "Go"},
		// At one line, the first symbol in sort order wins, not the first
		// entry: Zeta comes before Alpha here.
		{Name: "Zeta", Line: 4, Language: "Go"},
		{Name: "Alpha", Line: 4, Language: "Go"},
		{Name: "bad-low", Line: 0},
		{Name: "bad-high", Line: 99},
	}
	units := extractSemanticUnits("sample.go", content, entries, nil)
	if len(units) != 3 {
		t.Fatalf("units=%d, want 3", len(units))
	}
	want := []struct {
		kind   semanticUnitKind
		start  uint64
		end    uint64
		symbol string
	}{
		{kind: semanticUnitGap, start: 0, end: 30},
		{kind: semanticUnitSymbol, start: 30, end: 47, symbol: "Alpha"},
		{kind: semanticUnitSymbol, start: 47, end: 62, symbol: "Beta"},
	}
	for index, expected := range want {
		unit := units[index]
		if unit.kind != expected.kind || unit.start != expected.start || unit.end != expected.end ||
			unit.symbol != expected.symbol || !bytes.Equal(unit.text, content[expected.start:expected.end]) {
			t.Fatalf("unit %d=%+v, want %+v", index, unit, expected)
		}
	}
	for _, unit := range units {
		if unit.parserResult != semanticParserCTags {
			t.Fatalf("parser result=%d, want ctags", unit.parserResult)
		}
	}
}

func TestExtractSemanticUnitsFallbacks(t *testing.T) {
	tests := []struct {
		name       string
		content    []byte
		parseErr   error
		wantResult semanticParserResult
	}{
		{name: "no symbols", content: []byte("plain text\n"), wantResult: semanticParserNoSymbols},
		{name: "parser failed", content: []byte("func F() {}\n"), parseErr: errors.New("ctags stopped"), wantResult: semanticParserFailed},
		{name: "invalid UTF-8", content: []byte{'a', 0xff, 'b'}, wantResult: semanticParserInvalidUTF8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			units := extractSemanticUnits("input", test.content, nil, test.parseErr)
			if len(units) != 1 || units[0].kind != semanticUnitFallback ||
				units[0].parserResult != test.wantResult || units[0].start != 0 ||
				units[0].end != uint64(len(test.content)) ||
				!bytes.Equal(units[0].text, test.content) {
				t.Fatalf("units=%+v", units)
			}
		})
	}
}

// buildSemanticTestUnits builds a semantic generation of files through the
// production build path (buildSemanticGeneration, with ctags) and returns the
// units that the model embeds.
func buildSemanticTestUnits(t *testing.T, files map[string][]byte) []semanticUnit {
	t.Helper()
	requireTools(t)
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
	documents := make(chan fileContent, len(files))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		documents <- makeFileContent(t, name, files[name])
	}
	close(documents)
	if err := buildSemanticGeneration(
		t.Context(), filepath.Join(t.TempDir(), "semantic"), "source-extract", embedder, searchResources{},
		func(context.Context) (<-chan fileContent, func() error) {
			return documents, func() error { return nil }
		},
		&semanticReuseIndex{},
	); err != nil {
		t.Fatal(err)
	}
	return embedded
}

// TestSemanticBuildSkipsBinaryContent builds vectors for a binary file
// and a text file: the model embeds rows of the text file only.
func TestSemanticBuildSkipsBinaryContent(t *testing.T) {
	units := buildSemanticTestUnits(t, map[string][]byte{
		"image.bin": {'p', 'n', 'g', 0, 1, 2, 3},
		"text.go":   []byte("package sample\n"),
	})
	if len(units) == 0 {
		t.Fatal("the text file gave no row")
	}
	for _, unit := range units {
		if unit.path != "text.go" {
			t.Fatalf("row of %q was embedded, want explicit binary skip", unit.path)
		}
	}
}

// TestSemanticBuildSkipsZoektNameOnlyContent builds vectors for files at the
// content limits of Zoekt. Zoekt keeps only the name of a file of 1 or 2
// bytes, and of a file with a NUL byte past its start. These files get no
// rows. A file of 3 bytes gets rows.
func TestSemanticBuildSkipsZoektNameOnlyContent(t *testing.T) {
	units := buildSemanticTestUnits(t, map[string][]byte{
		"one.txt":   []byte("x"),
		"two.txt":   []byte("x\n"),
		"nul.txt":   []byte("text\x00more"),
		"three.txt": []byte("xy\n"),
	})
	paths := map[string]struct{}{}
	for _, unit := range units {
		paths[unit.path] = struct{}{}
	}
	if !maps.Equal(paths, map[string]struct{}{"three.txt": {}}) {
		t.Fatalf("rows of %v, want rows of three.txt only", slices.Sorted(maps.Keys(paths)))
	}
}

// TestSemanticManifestRejectsTinyFileRows validates a manifest of the
// extraction rule that gives rows to files of 1 or 2 bytes
// (ctags-gaps-v6-windows). No shard copy matches such rows, so the check
// rejects that generation, and the next build replaces it.
func TestSemanticManifestRejectsTinyFileRows(t *testing.T) {
	manifest := testLargeSemanticManifest(t, 1)
	if err := validateSemanticManifest(manifest, "head"); err != nil {
		t.Fatalf("a manifest of the current rule fails: %v", err)
	}
	manifest.Extractor = "ctags-gaps-v6-windows"
	if err := validateSemanticManifest(manifest, "head"); err == nil {
		t.Fatal("a generation with rows of files that Zoekt keeps by name only validates")
	}
}

// TestGeneratedFileInVectors checks that a file with the generated-code
// marker gives rows over the whole file, as any source file does.
func TestGeneratedFileInVectors(t *testing.T) {
	content := []byte("// Code generated by fixture. DO NOT EDIT.\npackage generated\nfunc Alpha() {}\n")
	units := buildSemanticTestUnits(t, map[string][]byte{"generated.go": content})
	slices.SortFunc(units, func(left, right semanticUnit) int { return cmp.Compare(left.start, right.start) })
	next := uint64(0)
	for _, unit := range units {
		if unit.path != "generated.go" || unit.start != next {
			t.Fatalf("units=%+v, want the whole generated file", units)
		}
		next = unit.end
	}
	if next != uint64(len(content)) {
		t.Fatalf("rows end at %d, want %d: the whole generated file", next, len(content))
	}
}

func TestExtractSemanticUnitsRejectsPartialParserOutput(t *testing.T) {
	content := []byte("header\nfunc F() {}\n")
	entries := []*ctags.Entry{{Name: "F", Kind: "function", Line: 2}}
	for _, test := range []struct {
		name    string
		content []byte
		err     error
		want    semanticParserResult
	}{
		{name: "parse error", content: content, err: errors.New("partial output"), want: semanticParserFailed},
		{name: "invalid UTF-8", content: append(append([]byte(nil), content...), 0xff), want: semanticParserInvalidUTF8},
	} {
		t.Run(test.name, func(t *testing.T) {
			units := extractSemanticUnits("input.go", test.content, entries, test.err)
			if len(units) != 1 || units[0].kind != semanticUnitFallback ||
				units[0].parserResult != test.want || units[0].symbol != "" ||
				units[0].start != 0 || units[0].end != uint64(len(test.content)) ||
				!bytes.Equal(units[0].text, test.content) {
				t.Fatalf("units=%+v", units)
			}
		})
	}
}

func TestSemanticSymbolMetadataKeepsCtagsContext(t *testing.T) {
	entry := &ctags.Entry{
		Name:       "ServeHTTP",
		Kind:       "method",
		Parent:     "Server",
		ParentKind: "type",
		Signature:  "(w, r)",
	}
	if got, want := semanticSymbolMetadata(entry), "type Server method ServeHTTP (w, r)"; got != want {
		t.Fatalf("metadata=%q, want %q", got, want)
	}
}

func TestExtractSemanticUnitsSplitsLargeLines(t *testing.T) {
	content := bytes.Repeat([]byte{'x'}, semanticUnitMaxBytes*2+17)
	units := extractSemanticUnits("large.txt", content, nil, nil)
	if len(units) != 3 {
		t.Fatalf("units=%d, want 3", len(units))
	}
	for index, unit := range units {
		if len(unit.text) > semanticUnitMaxBytes {
			t.Fatalf("unit bytes=%d, limit=%d", len(unit.text), semanticUnitMaxBytes)
		}
		wantStart := index * semanticUnitMaxBytes
		wantEnd := min(wantStart+semanticUnitMaxBytes, len(content))
		if unit.start != uint64(wantStart) || unit.end != uint64(wantEnd) ||
			!bytes.Equal(unit.text, content[wantStart:wantEnd]) {
			t.Fatalf("unit %d spans %d:%d, want %d:%d", index, unit.start, unit.end, wantStart, wantEnd)
		}
	}
}

func TestExtractSemanticUnitsIsDeterministic(t *testing.T) {
	content := []byte("header\nfunc Z() {}\nfunc A() {}\n")
	left := []*ctags.Entry{{Name: "A", Line: 3}, {Name: "Z", Line: 2}}
	right := []*ctags.Entry{{Name: "Z", Line: 2}, {Name: "A", Line: 3}}
	first := extractSemanticUnits("x.go", content, left, nil)
	second := extractSemanticUnits("x.go", content, right, nil)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("entry order changed units:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func TestSemanticUnitIDEncodingIsStable(t *testing.T) {
	unit := semanticUnit{
		path:         "main.go",
		start:        1,
		end:          2,
		kind:         semanticUnitSymbol,
		parserResult: semanticParserCTags,
		language:     "Go",
		symbol:       "function Main",
	}
	for index := range unit.contentID {
		unit.contentID[index] = byte(index)
	}
	want := "55b3ceda30d4175a7d278d446df5de62895169bc6bc84adc548be8811deda8e3"
	got := makeSemanticUnitID(unit)
	if encoded := hex.EncodeToString(got[:]); encoded != want {
		t.Fatalf("semantic unit ID=%s, want %s", encoded, want)
	}
}
