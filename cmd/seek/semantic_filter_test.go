package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

func semanticFilterForTest(t testing.TB, source string) *semanticFilterPlan {
	t.Helper()
	node, err := query.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compileSemanticFilterPlan([]query.Q{node})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestSemanticFilenameFilterMatchesZoektCaseRules(t *testing.T) {
	tests := []struct {
		name   string
		filter string
		path   string
		want   bool
	}{
		{name: "long folded sigma", filter: "file:σσσ", path: "ΣΣΣ.go", want: true},
		{name: "long final sigma differs", filter: "file:σσσ", path: "ςςς.go", want: false},
		{name: "short regexp fold", filter: "file:σσ", path: "ςς.go", want: true},
		{name: "short Kelvin fold", filter: "file:k", path: "K.go", want: true},
		{name: "smart case", filter: "file:API", path: "api/client.go", want: false},
		{name: "anchored regexp", filter: `file:^cmd/.+\.go$`, path: "cmd/main.go", want: true},
		{name: "anchored regexp miss", filter: `file:^cmd/.+\.go$`, path: "internal/main.go", want: false},
		{name: "negative regexp", filter: `-file:_test\.go$`, path: "main.go", want: true},
		{name: "negative regexp rejects", filter: `-file:_test\.go$`, path: "main_test.go", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := semanticFilterForTest(t, test.filter)
			if got := plan.matches(test.path, "Go"); got != test.want {
				t.Fatalf("matches(%q)=%t, want %t", test.path, got, test.want)
			}
		})
	}
}

func TestSemanticLanguageFilterUsesCanonicalName(t *testing.T) {
	plan := semanticFilterForTest(t, "lang:go")
	if !plan.matches("main.go", "Go") {
		t.Fatal("canonical Go language did not match")
	}
	for _, language := range []string{"go", "Python", ""} {
		if plan.matches("main.go", language) {
			t.Fatalf("noncanonical language %q matched", language)
		}
	}
}

func TestBuildSemanticFilterMask(t *testing.T) {
	rows := []semanticUnit{
		{row: 0, path: "keep/a.go", fileLanguage: "Go"},
		{row: 1, path: "keep/a.go", fileLanguage: "Go"},
		{row: 2, path: "drop/a.go", fileLanguage: "Go"},
		{row: 3, path: "keep/b.go", fileLanguage: "Go"},
		{row: 4, path: "keep/b.go", fileLanguage: "Go"},
	}
	generation := &semanticGeneration{
		manifest: semanticManifest{
			Rows: 5,
			USearchFiles: []semanticUSearchShard{
				{Start: 0, Rows: 3},
				{Start: 3, Rows: 2},
			},
		},
		rows: rows,
	}
	mask, err := buildSemanticFilterMask(t.Context(), generation, semanticFilterForTest(t, "file:^keep/"))
	if err != nil {
		t.Fatal(err)
	}
	if mask.mode != semanticFilterPartial || mask.allowed != 4 || mask.rows != 5 {
		t.Fatalf("mask=%+v", mask)
	}
	if !reflect.DeepEqual(mask.shardAllowed, []uint64{2, 2}) {
		t.Fatalf("shard counts=%v, want [2 2]", mask.shardAllowed)
	}
	if got, want := mask.selectedRows(), []uint64{0, 1, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected rows=%v, want %v", got, want)
	}
	if !reflect.DeepEqual(mask.exactRows, []uint64{0, 1, 3, 4}) {
		t.Fatalf("retained exact rows=%v", mask.exactRows)
	}
	if len(mask.bits) != 1 {
		t.Fatalf("bitmap bytes=%d, want 1", len(mask.bits))
	}

	all, err := buildSemanticFilterMask(t.Context(), generation, semanticFilterForTest(t, "file:go"))
	if err != nil || all.mode != semanticFilterAll || len(all.bits) != 0 || len(all.shardAllowed) != 0 {
		t.Fatalf("all mask=%+v error=%v", all, err)
	}
	none, err := buildSemanticFilterMask(t.Context(), generation, semanticFilterForTest(t, "file:^missing/"))
	if err != nil || none.mode != semanticFilterNone || len(none.bits) != 0 || len(none.shardAllowed) != 0 {
		t.Fatalf("none mask=%+v error=%v", none, err)
	}
}

func TestBuildSemanticFilterMaskHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	generation := &semanticGeneration{
		manifest: semanticManifest{Rows: 1},
		rows:     []semanticUnit{{row: 0, path: "main.go", fileLanguage: "Go"}},
	}
	_, err := buildSemanticFilterMask(ctx, generation, semanticFilterForTest(t, "lang:go"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestBuildSemanticFilterMaskRejectsInvalidGroupedRow(t *testing.T) {
	generation := &semanticGeneration{
		manifest: semanticManifest{Rows: 2},
		rows: []semanticUnit{
			{row: 0, path: "main.go", fileLanguage: "Go"},
			{row: 7, path: "main.go", fileLanguage: "Go"},
		},
	}
	_, err := buildSemanticFilterMask(
		t.Context(), generation, semanticFilterForTest(t, "lang:go"),
	)
	if err == nil {
		t.Fatal("invalid row identity was accepted")
	}
}

func TestSemanticFilterMaskCrossesByteAndShardBoundary(t *testing.T) {
	rows := make([]semanticUnit, 9)
	for row := range rows {
		path := "drop.go"
		if row == 8 {
			path = "keep.go"
		}
		rows[row] = semanticUnit{row: uint64(row), path: path, fileLanguage: "Go"}
	}
	generation := &semanticGeneration{
		manifest: semanticManifest{
			Rows: 9,
			USearchFiles: []semanticUSearchShard{
				{Start: 0, Rows: 8},
				{Start: 8, Rows: 1},
			},
		},
		rows: rows,
	}
	mask, err := buildSemanticFilterMask(t.Context(), generation, semanticFilterForTest(t, "file:^keep"))
	if err != nil {
		t.Fatal(err)
	}
	if mask.mode != semanticFilterPartial || !reflect.DeepEqual(mask.bits, []byte{0, 1}) ||
		!reflect.DeepEqual(mask.shardAllowed, []uint64{0, 1}) {
		t.Fatalf("boundary mask=%+v", mask)
	}
}

func TestSemanticFilterMatchesRealZoektShard(t *testing.T) {
	requireTools(t)
	documents := []fileContent{
		{name: "ΣΣΣ.go", content: []byte("package sample\nvar Sigma = 1\n")},
		{name: "ςςς.go", content: []byte("package sample\nvar FinalSigma = 1\n")},
		{name: "cmd/main.go", content: []byte("package main\nfunc main() {}\n")},
		{name: "cmd/main_test.go", content: []byte("package main\nfunc TestMain() {}\n")},
		{name: "tools/main.py", content: []byte("def main():\n    return 1\n")},
		{name: "script", content: []byte("#!/usr/bin/env python3\nprint('hint')\n")},
		{name: "binary.unknown-seek-test", content: []byte("#!/usr/bin/env python3\x00ignored\n")},
		{
			name:       "forced.py",
			content:    []byte("content must not override a supplied skip decision\n"),
			skipReason: index.SkipReasonTooLarge,
		},
		{name: "docs/my file.go", content: []byte("package docs\nvar Guide = 1\n")},
		{name: string([]byte{'b', 'a', 'd', '/', 0xff, '.', 'g', 'o'}), content: []byte("x")},
	}
	input := make(chan fileContent, len(documents))
	for _, document := range documents {
		input <- document
	}
	close(input)
	indexDir := t.TempDir()
	if indexed, err := indexDocuments(
		t.Context(), indexDir, "filter-test", t.TempDir(), input, 1,
	); err != nil || !indexed {
		t.Fatalf("build Zoekt fixture: indexed=%t error=%v", indexed, err)
	}

	options := indexBuildOptions("", 1)
	options.SetDefaults()
	checker := &index.DocChecker{}
	languages := make(map[string]string, len(documents))
	for _, document := range documents {
		languages[document.name] = semanticDocumentLanguage(checker, options, document)
	}
	allFiles, err := executeParsedShardSearchForTest(
		t.Context(), indexDir, &query.Const{Value: true}, defaultSearchConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	zoektLanguages := make(map[string]string, len(allFiles))
	for _, file := range allFiles {
		zoektLanguages[file.FileName] = file.Language
	}
	for _, document := range documents {
		if got, want := languages[document.name], zoektLanguages[document.name]; got != want {
			t.Errorf("file language for %q=%q, Zoekt=%q", document.name, got, want)
		}
	}
	for path, want := range map[string]string{
		"script":                   "Python",
		"binary.unknown-seek-test": "",
		"forced.py":                "Python",
	} {
		if got := languages[path]; got != want {
			t.Errorf("fixture language for %q=%q, want %q", path, got, want)
		}
	}
	for _, source := range []string{
		"file:σσσ",
		"file:σσ",
		`file:^cmd/.+\.go$`,
		`file:^cmd/[a-z_]+\.go$`,
		`file:^(cmd|tools)/`,
		`file:"my file"`,
		`-file:_test\.go$`,
		`file:^cmd/ -file:_test\.go$`,
		`file:^cmd/ lang:go`,
		`file:^cmd/ lang:python`,
		`file:main file:cmd`,
		"file:�",
		"lang:go",
		"lang:python",
	} {
		t.Run(source, func(t *testing.T) {
			node, err := query.Parse(source)
			if err != nil {
				t.Fatal(err)
			}
			files, err := executeParsedShardSearchForTest(
				t.Context(), indexDir, node, defaultSearchConfig(),
			)
			if err != nil {
				t.Fatal(err)
			}
			zoektMatches := make(map[string]bool, len(files))
			for _, file := range files {
				zoektMatches[file.FileName] = true
			}
			filterNodes := []query.Q{node}
			if and, ok := node.(*query.And); ok {
				filterNodes = and.Children
			}
			plan, err := compileSemanticFilterPlan(filterNodes)
			if err != nil {
				t.Fatal(err)
			}
			for _, document := range documents {
				got := plan.matches(document.name, languages[document.name])
				if got != zoektMatches[document.name] {
					t.Errorf(
						"%s on %q=%t, Zoekt=%t (language %q)",
						source,
						document.name,
						got,
						zoektMatches[document.name],
						languages[document.name],
					)
				}
			}
		})
	}
}
