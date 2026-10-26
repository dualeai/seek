package main

import (
	"errors"
	"fmt"
	"reflect"
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

// TestQuerySupportRowsRoute parses the example of each row next to two
// description words, or alone when the example holds them. The planner must
// give the row its role:
//   - a description adds its text to the model query;
//   - a filter adds no model text and changes the relaxed query;
//   - a modifier leaves the plan of the two words as it is.
//
// The filter rows in strictFilterRows must keep strict ranking.
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
				// These rows make the whole query match nothing or wrap it in
				// a Zoekt result type: strict ranking. Every other filter keeps
				// the two description words on the model routes.
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
				// The "." of row T41 stays in the relaxed query.
				relaxed := strings.ReplaceAll(fmt.Sprint(plan.relaxedQ), "case_", "")
				if !ok || plan.modelQuery != base.modelQuery ||
					len(plan.filters) != len(base.filters) ||
					(row.id != "T41" && relaxed != fmt.Sprint(base.relaxedQ)) {
					t.Fatalf("modifier %q changed the plan: eligible=%t %#v", row.example, ok, plan)
				}
			}
		})
	}
}

// TestPlannerIgnoresSpelling checks that the route follows Zoekt's parsed
// tree. A parse directive changes nothing, and punctuation in a word keeps the
// query on the model routes.
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
		if plan.modelQuery != model {
			t.Errorf("%q: model=%q, want %q", pattern, plan.modelQuery, model)
		}
	}
}

// TestCppFallbackKeepsFilters gives a programming name that Zoekt rejects.
// The filters next to it stay, and the name also parses next to a negation or
// an OR.
func TestCppFallbackKeepsFilters(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "parse.go", "package sample\n// parse C++ request\n")
	writeFileAt(t, folder, "parse.py", "# parse C++ request\n")

	plan := planFor(t, "parse C++ request lang:go")
	if plan.modelQuery != "parse C++ request" || len(plan.filters) == 0 {
		t.Fatalf("plan=%#v, want the name in the model query and the language filter", plan)
	}
	output, err := seek(t, "--lexical-only", "parse C++ request lang:go", folder)
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
		if got := planFor(t, pattern); got.modelQuery != model || len(got.filters) != 0 {
			t.Errorf("%q: model=%q filters=%d, want %q with no filter",
				pattern, got.modelQuery, len(got.filters), model)
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

// TestRegexpThatGoCannotCompileIsAQueryError gives a regexp that nests too
// deeply after Zoekt's simplification: the parse returns a short query error,
// where Zoekt's MustCompile would panic. file:a{1,500} still parses.
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
