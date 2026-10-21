package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// zoektQueryKeys are the prefixes and the reserved word that the pinned Zoekt
// parser accepts (query/parse.go at the go.mod version). The support table
// must name each of them.
var zoektQueryKeys = []string{
	"archived:", "b:", "branch:", "c:", "case:", "content:", "f:", "file:",
	"fork:", "public:", "r:", "regex:", "repo:", "lang:", "sym:", "t:",
	"type:", "meta.", "or",
}

// TestQuerySupportRowsCoverInventory checks that the table has each row
// T01-T41 once and names every Zoekt key.
func TestQuerySupportRowsCoverInventory(t *testing.T) {
	seen := map[string]bool{}
	for _, row := range querySupport {
		if seen[row.id] {
			t.Errorf("row %s appears twice", row.id)
		}
		seen[row.id] = true
		if row.spelling == "" || row.meaning == "" {
			t.Errorf("row %s has no spelling or meaning", row.id)
		}
		switch row.role {
		case roleDescription, roleFilter, roleModifier:
		default:
			t.Errorf("row %s has role %q", row.id, row.role)
		}
	}
	for id := 1; id <= 41; id++ {
		if !seen[fmt.Sprintf("T%02d", id)] {
			t.Errorf("row T%02d is missing", id)
		}
	}
	for _, key := range zoektQueryKeys {
		found := false
		for _, row := range querySupport {
			for _, word := range strings.FieldsFunc(row.spelling, func(r rune) bool {
				return r == ' ' || r == ',' || r == '(' || r == ')'
			}) {
				if word == key || (strings.HasSuffix(key, ":") || strings.HasSuffix(key, ".")) &&
					strings.HasPrefix(word, key) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("no row names the Zoekt key %q", key)
		}
	}
}

// TestQuerySupportRowsRoute parses each row's example next to two description
// words (or alone, when the example holds them) and checks that the planner
// gives the row's role: a description adds its text to the model query, a
// filter adds no model text, and a modifier leaves the plan of the two words
// unchanged.
func TestQuerySupportRowsRoute(t *testing.T) {
	base := planFor(t, "alpha beta")
	for _, row := range querySupport {
		if row.example == "" {
			continue
		}
		t.Run(row.id, func(t *testing.T) {
			pattern := row.example
			if !strings.Contains(pattern, "alpha beta") {
				pattern += " alpha beta"
			}
			if _, err := query.Parse(pattern); err != nil && row.id != "T31" {
				t.Fatalf("Zoekt rejects %q: %v", pattern, err)
			}
			raw, _, err := parseSearchQueryForms(pattern)
			if err != nil {
				t.Fatal(err)
			}
			plan, ok := planRerankQuery(raw)
			switch row.role {
			case roleDescription:
				if !ok || plan.modelQuery == base.modelQuery ||
					!strings.HasSuffix(plan.modelQuery, " "+base.modelQuery) {
					t.Fatalf("description %q: eligible=%t model=%q", row.example, ok, plan.modelQuery)
				}
			case roleFilter:
				// These rows make the whole query match nothing or wrap it in a
				// Zoekt result type: strict ranking. Every other filter keeps the
				// two description words on the model routes.
				if strictFilterRows[row.id] {
					if ok {
						t.Fatalf("filter %q must keep strict ranking: %q", row.example, plan.modelQuery)
					}
				} else if !ok || plan.modelQuery != base.modelQuery ||
					strings.ReplaceAll(fmt.Sprint(plan.relaxedQ), "case_", "") == fmt.Sprint(base.relaxedQ) {
					// A filter stays in relaxed retrieval.
					t.Fatalf("filter %q: eligible=%t model=%q relaxed=%v",
						row.example, ok, plan.modelQuery, plan.relaxedQ)
				}
			case roleModifier:
				// case:yes sets the case of the atoms; nothing else changes.
				relaxed := strings.ReplaceAll(fmt.Sprint(plan.relaxedQ), "case_", "")
				if !ok || plan.modelQuery != base.modelQuery ||
					!reflect.DeepEqual(plan.semanticFilter, base.semanticFilter) ||
					plan.semanticUnfiltered != base.semanticUnfiltered ||
					(row.id != "T41" && relaxed != fmt.Sprint(base.relaxedQ)) {
					t.Fatalf("modifier %q changed the plan: eligible=%t %#v", row.example, ok, plan)
				}
			}
		})
	}
}

// TestPlannerIgnoresSpelling checks that the route follows Zoekt's parsed tree:
// a parse directive changes nothing, and punctuation in a word keeps the
// query on the model routes with vectors.
func TestPlannerIgnoresSpelling(t *testing.T) {
	plain := planFor(t, "x y")
	directive := planFor(t, "case:auto x y")
	if !reflect.DeepEqual(plain, directive) {
		t.Fatalf("case:auto changed the plan:\nplain=%#v\ncase=%#v", plain, directive)
	}
	for pattern, model := range map[string]string{
		"how is a request line parsed?": "how is a request line parsed?",
		"where is config.yaml loaded":   "where is config.yaml loaded",
		"match foo.*bar names":          "match foo.*bar names",
		"parse request line .":          "parse request line",
	} {
		plan := planFor(t, pattern)
		if plan.modelQuery != model || !plan.semanticRetrievable() {
			t.Errorf("%q: model=%q vectors=%t, want %q with vectors",
				pattern, plan.modelQuery, plan.semanticRetrievable(), model)
		}
	}
}

// TestCppFallbackKeepsFilters checks that a programming name that Zoekt
// rejects does not drop the filters next to it.
func TestCppFallbackKeepsFilters(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "parse.go", "package sample\n// parse C++ request\n")
	writeFileAt(t, folder, "parse.py", "# parse C++ request\n")

	plan := planFor(t, "parse C++ request lang:go")
	if plan.modelQuery != "parse C++ request" || plan.semanticFilter == nil {
		t.Fatalf("plan=%#v, want the name in the model query and the language filter", plan)
	}
	output, err := captureStdout(t, func() error {
		return runSearchCommand(
			context.Background(), "parse C++ request lang:go", []string{folder},
			0, 0, defaultSearchConfig(), searchRunConfig{},
		)
	})
	if err != nil || !strings.Contains(output, "parse.go") || strings.Contains(output, "parse.py") {
		t.Fatalf("output=%q err=%v, want parse.go only", output, err)
	}
	for _, pattern := range []string{"find C++ -handler", "find C++ OR handler"} {
		if _, _, err := parseSearchQueryForms(pattern); err != nil {
			t.Errorf("%q: %v", pattern, err)
		}
	}
	// A namespace that is also a Zoekt prefix stays part of the name, and the
	// separators inside a phrase stay as typed.
	for pattern, model := range map[string]string{
		"how content::WebContents[] works": "how content::WebContents[] works",
		"lang::Foo() C++ value":            "lang::Foo() C++ value",
		"find t::size() C++":               "find t::size() C++",
	} {
		if got := planFor(t, pattern); got.modelQuery != model || got.semanticFilter != nil {
			t.Errorf("%q: model=%q filter=%t, want %q with no filter",
				pattern, got.modelQuery, got.semanticFilter != nil, model)
		}
	}
	raw, _, err := parseSearchQueryForms("\"foo  bar\" C++")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw.String(), `"foo  bar"`) {
		t.Errorf("phrase spacing changed: %s", raw)
	}
}

// TestTypeFilePrintsNamesOnly checks that type:file prints names only on the
// model route as on the strict route.
func TestTypeFilePrintsNamesOnly(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	strict := t.TempDir()
	relaxed := t.TempDir()
	writeFileAt(t, strict, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, relaxed, "relaxed.go", "package sample\n// alpha only\n")
	search := func(config searchRunConfig) string {
		t.Helper()
		output, err := captureStdout(t, func() error {
			return runSearchCommand(
				t.Context(), "type:file alpha beta", []string{strict, relaxed},
				0, 0, defaultSearchConfig(), config,
			)
		})
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	lexical := search(searchRunConfig{})
	scorer := &fixedRerankScorer{}
	model := search(searchRunConfig{
		policy: defaultSearchPolicy(),
		newModel: func(context.Context) (semanticModel, error) {
			return scorer, nil
		},
	})
	if scorer.scoreCalls.Load() != 1 {
		t.Fatalf("model calls=%d, want 1", scorer.scoreCalls.Load())
	}
	// The relaxed-only file is no candidate: both routes print the strict
	// names.
	if model != lexical || strings.Contains(model, "relaxed.go") {
		t.Fatalf("model output differs from the strict names:\nlexical=%q\nmodel=%q", lexical, model)
	}
	if strings.Contains(model, "alpha") || strings.Contains(model, "package") {
		t.Fatalf("model output prints content:\n%s", model)
	}
}

// TestRunRerankDescriptionAtomsStartTheModel checks that a description with
// regex:, case:, a negated word, or a sym:, content:, or -lang: filter takes
// the model routes: it starts the model, and a model failure keeps the strict
// output.
func TestRunRerankDescriptionAtomsStartTheModel(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\nfunc Alpha() {}\n// alpha beta gamma\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha only\n")
	for _, pattern := range []string{
		"regex:alpha beta", "case:yes alpha beta", "alpha beta -missing",
		"alpha beta sym:Alpha", "alpha beta content:gamma", "alpha beta -lang:python",
	} {
		t.Run(pattern, func(t *testing.T) {
			baseline, err := captureStdout(t, func() error {
				return runWithSearchConfig(context.Background(), pattern, []string{folder}, defaultSearchConfig())
			})
			if err != nil {
				t.Fatal(err)
			}
			factoryCalls := 0
			got, err := captureStdout(t, func() error {
				return runSearchCommand(
					context.Background(), pattern, []string{folder}, 0, 0, defaultSearchConfig(),
					searchRunConfig{
						policy: defaultSearchPolicy(),
						newModel: func(context.Context) (semanticModel, error) {
							factoryCalls++
							return nil, errors.New("backend failure")
						},
					},
				)
			})
			if err != nil || got != baseline || factoryCalls == 0 {
				t.Fatalf("calls=%d err=%v\nwant=%q\ngot=%q", factoryCalls, err, baseline, got)
			}
		})
	}
}

// strictFilterRows are the filter rows whose example keeps strict ranking.
var strictFilterRows = map[string]bool{"T11": true, "T18": true, "T19": true}

// planFor returns the model plan of pattern and fails the test when pattern
// is not eligible for the model routes.
func planFor(t *testing.T, pattern string) rerankQueryPlan {
	t.Helper()
	raw, _, err := parseSearchQueryForms(pattern)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := planRerankQuery(raw)
	if !ok {
		t.Fatalf("%q is not eligible", pattern)
	}
	return plan
}

// TestKeepFileNamesOnlyKeepsZoektNameLine keeps Zoekt's name line, with its
// highlight, and gives a content result one name line.
func TestKeepFileNamesOnlyKeepsZoektNameLine(t *testing.T) {
	name := zoekt.LineMatch{Line: []byte("a.go"), FileName: true,
		LineFragments: []zoekt.LineFragmentMatch{{LineOffset: 0, MatchLength: 1}}}
	results := []corpusSearchResult{
		{file: zoekt.FileMatch{FileName: "a.go", LineMatches: []zoekt.LineMatch{{Line: []byte("text")}, name}}},
		{file: zoekt.FileMatch{FileName: "b.go", LineMatches: []zoekt.LineMatch{{Line: []byte("text")}}}},
	}
	keepFileNamesOnly(results)
	if lines := results[0].file.LineMatches; len(lines) != 1 || len(lines[0].LineFragments) != 1 {
		t.Fatalf("a.go lines=%+v, want Zoekt's highlighted name line", lines)
	}
	if lines := results[1].file.LineMatches; len(lines) != 1 || !lines[0].FileName || string(lines[0].Line) != "b.go" {
		t.Fatalf("b.go lines=%+v, want one name line", lines)
	}
}

// TestPunctuationAtomStaysInRelaxedRetrieval keeps "." in the relaxed query
// of a description (row T41).
func TestPunctuationAtomStaysInRelaxedRetrieval(t *testing.T) {
	with, without := planFor(t, "parse request line ."), planFor(t, "parse request line")
	if fmt.Sprint(with.relaxedQ) == fmt.Sprint(without.relaxedQ) {
		t.Fatalf("relaxed query %v lost the punctuation atom", with.relaxedQ)
	}
}

// TestCLIFilterMasksSemanticRetrieval runs a cold description with -lang:go:
// Zoekt applies the filter to the semantic mask, so the node builds vectors
// and semantic retrieval finds only files that the filter keeps.
func TestCLIFilterMasksSemanticRetrieval(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "notes.py", "# SEMANTIC_TARGET dispatches the request\n")
	writeFileAt(t, folder, "target.go", "package sample\n// SEMANTIC_TARGET dispatches the request\n")
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	t.Chdir(t.TempDir())
	output, err := runHybridTestSearch(t, "alpha beta -lang:go", []string{folder}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil })
	if err != nil || !strings.Contains(output, "notes.py") || strings.Contains(output, "target.go") || strings.Contains(output, "strict.go") {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	plans, err := planCorpora(context.Background(), []string{folder})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	if dirs := semanticGenerationDirs(t, plans[0].indexDir); len(dirs) != 1 {
		t.Fatalf("built %d vector generations, want 1", len(dirs))
	}
}

// TestQuerySupportReadmeMatchesTable keeps the README support table equal to
// the Go table, row for row: a change to either fails here.
func TestQuerySupportReadmeMatchesTable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(data), "\n### Support table\n")
	if !ok {
		t.Fatal("README has no support table")
	}
	var rows [][]string
	for line := range strings.Lines(section) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "### ") || strings.HasPrefix(line, "## ") {
			break
		}
		if !strings.HasPrefix(line, "| T") {
			continue
		}
		fields := strings.Split(strings.Trim(line, "|"), " | ")
		for index := range fields {
			fields[index] = strings.TrimSpace(fields[index])
		}
		rows = append(rows, fields)
	}
	if len(rows) != len(querySupport) {
		t.Fatalf("README rows=%d, Go rows=%d", len(rows), len(querySupport))
	}
	for index, row := range querySupport {
		example := ""
		if row.example != "" {
			example = "`" + row.example + "`"
		}
		want := []string{row.id, row.spelling, string(row.role), row.meaning, example}
		if !slices.Equal(rows[index], want) {
			t.Errorf("README row %d=%q, Go row=%q", index+1, rows[index], want)
		}
	}
}

// TestRegexpThatGoCannotCompileIsAQueryError gives a regexp that nests too
// deeply after Zoekt's simplification: the parse returns a query error, where
// Zoekt's MustCompile would panic.
func TestRegexpThatGoCannotCompileIsAQueryError(t *testing.T) {
	for _, pattern := range []string{"a{1,1000}", "file:a{1,501}", "content:a{1,1000}"} {
		_, _, _, err := parseQueryHalves(pattern)
		var syntax *querySyntaxError
		if !errors.As(err, &syntax) || !strings.Contains(err.Error(), "nests too deeply") || len(err.Error()) > 200 {
			t.Fatalf("%s: err=%v, want a short query error", pattern, err)
		}
	}
	if _, _, _, err := parseQueryHalves("file:a{1,500}"); err != nil {
		t.Fatalf("a{1,500}: %v", err)
	}
}
