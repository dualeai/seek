package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

func TestPlanRerankQuery(t *testing.T) {
	raw, _, err := parseSearchQueryForms("Handle shell completion")
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := planRerankQuery(raw)
	if !ok {
		t.Fatal("plain three-term query must be eligible")
	}
	if plan.modelQuery != "Handle shell completion" {
		t.Fatalf("model query=%q", plan.modelQuery)
	}
	if len(plan.filters) != 0 {
		t.Fatal("plain query has a filter")
	}
	or, ok := plan.relaxedQ.(*query.Or)
	if !ok || len(or.Children) != 3 {
		t.Fatalf("relaxed query=%#v, want three-child Or", plan.relaxedQ)
	}
	want := []string{"Handle", "shell", "completion"}
	for i, child := range or.Children {
		term, ok := child.(*query.Substring)
		if !ok {
			t.Fatalf("child %d type=%T", i, child)
		}
		if term.Pattern != want[i] || !term.Content || term.FileName {
			t.Fatalf("child %d=%+v", i, term)
		}
	}
}

func TestPlanRerankQueryAcceptsPlainProgrammingName(t *testing.T) {
	for _, pattern := range []string{
		"how from_json populates a custom C++ type from JSON",
		"how operator[] and at() access and create nested JSON values",
		"find std::vector handling in C++ code",
	} {
		t.Run(pattern, func(t *testing.T) {
			raw, _, err := parseSearchQueryForms(pattern)
			if err != nil {
				t.Fatal(err)
			}
			plan, ok := planRerankQuery(raw)
			if !ok {
				t.Fatal("plain description with a programming name must be eligible")
			}
			if plan.modelQuery != pattern {
				t.Fatalf("model query=%q, want %q", plan.modelQuery, pattern)
			}
		})
	}
}

// TestPlanRerankQueryAcceptsFileAndLanguageFilters plans descriptions with
// positive and negative file: and lang: filters. The model reads only the
// description. The node's semantic row selection (nodeRowSelection) applies
// the kept filters with Zoekt to an indexed folder.
func TestPlanRerankQueryAcceptsFileAndLanguageFilters(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	for _, path := range []string{"cmd/seek/main.go", "internal/main.go", "docs/my file.go", "docs/other.go", "cmd/seek/main_test.go"} {
		writeFileAt(t, folder, path, "package sample\n// request parser\n")
	}
	writeFileAt(t, folder, "cmd/seek/main.py", "# request parser\nprint('parse')\n")
	plans, err := planCorpora(t.Context(), []string{folder})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v, want one folder node", len(plans), err)
	}
	tests := []struct {
		name       string
		pattern    string
		wantModel  string
		allowPath  string
		rejectPath string
	}{
		{
			name:       "language",
			pattern:    "find request parser lang:go",
			wantModel:  "find request parser",
			allowPath:  "cmd/seek/main.go",
			rejectPath: "cmd/seek/main.py",
		},
		{
			name:       "file",
			pattern:    `find request parser file:^cmd/seek/`,
			wantModel:  "find request parser",
			allowPath:  "cmd/seek/main.go",
			rejectPath: "internal/main.go",
		},
		{
			name:       "quoted file",
			pattern:    `find request parser file:"my file"`,
			wantModel:  "find request parser",
			allowPath:  "docs/my file.go",
			rejectPath: "docs/other.go",
		},
		{
			name:       "negative file",
			pattern:    `find request parser -file:_test\.go$`,
			wantModel:  "find request parser",
			allowPath:  "cmd/seek/main.go",
			rejectPath: "cmd/seek/main_test.go",
		},
		{
			name:       "combined and reordered",
			pattern:    `lang:go find file:^cmd/ request -file:_test\.go$ parser`,
			wantModel:  "find request parser",
			allowPath:  "cmd/seek/main.go",
			rejectPath: "cmd/seek/main_test.go",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			qp, err := planQuery(test.pattern, defaultSearchPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if qp.model == nil || len(qp.model.filters) == 0 {
				t.Fatalf("filtered query is not eligible: %#v", qp.model)
			}
			if qp.model.modelQuery != test.wantModel {
				t.Fatalf("model query=%q, want %q", qp.model.modelQuery, test.wantModel)
			}
			freshRaw, _, _, err := parseQueryHalves(test.pattern)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(qp.raw, freshRaw) {
				t.Fatalf("planning mutated the strict query:\nbefore=%#v\nafter=%#v", freshRaw, qp.raw)
			}
			var selected map[string]struct{}
			err = openNode(t.Context(), plans[0], testLexicalSearchExecution(), func(node openedNode) error {
				read, readCtx, err := openNodeRead(t.Context(), node.plan, node.dirty)
				if err != nil {
					return err
				}
				defer read.close()
				selected, err = nodeRowSelection(readCtx, read, qp)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := selected[test.allowPath]; !ok {
				t.Fatalf("filters rejected %q: selected=%v", test.allowPath, selected)
			}
			if _, ok := selected[test.rejectPath]; ok {
				t.Fatalf("filters accepted %q: selected=%v", test.rejectPath, selected)
			}
		})
	}
}

func TestPlanRerankQueryUsesZoektNormalizedTree(t *testing.T) {
	for _, pattern := range []string{
		"alpha beta f:go",
		"case:yes alpha beta file:go",
		"regex:alpha beta file:go",
		"(alpha beta) file:go",
	} {
		t.Run(pattern, func(t *testing.T) {
			raw, _, err := parseSearchQueryForms(pattern)
			if err != nil {
				t.Fatal(err)
			}
			plan, ok := planRerankQuery(raw)
			if !ok || len(plan.filters) == 0 || plan.modelQuery != "alpha beta" {
				t.Fatalf("normalized query plan=%#v eligible=%t", plan, ok)
			}
		})
	}
}

// TestPlainDescriptionFallbackRejectsMalformedSyntax proves that a
// description with malformed explicit syntax stays a parse error.
func TestPlainDescriptionFallbackRejectsMalformedSyntax(t *testing.T) {
	for _, pattern := range []string{
		"find C++ file:[",
		"find C++ [broken",
		"find C++ (broken",
		"find C++ foo**",
	} {
		t.Run(pattern, func(t *testing.T) {
			if _, _, err := parseSearchQueryForms(pattern); err == nil {
				t.Fatal("malformed explicit syntax must stay an error")
			}
		})
	}
}

// TestPlanRerankQueryRejectsSyntax plans queries that give the model no text:
// one that matches nothing, one of a result type other than files, negations
// only, and punctuation only. They keep strict ranking.
func TestPlanRerankQueryRejectsSyntax(t *testing.T) {
	tests := []string{
		"alpha beta lang:definitely-not-a-language",
		"type:repo alpha beta",
		"-file:test",
		"^",
	}
	for _, pattern := range tests {
		t.Run(pattern, func(t *testing.T) {
			raw, _, err := parseSearchQueryForms(pattern)
			if err != nil {
				t.Fatal(err)
			}
			if plan, ok := planRerankQuery(raw); ok {
				t.Fatalf("query must be ineligible: %#v", plan)
			}
		})
	}
}

// TestPlanRerankQueryEveryShapeUsesTheModel plans one word, a phrase, an
// identifier, filters, an or of descriptions and punctuation: each gives the
// model its description atoms in query order, and a query of filters only
// gives it the text of its positive patterns.
func TestPlanRerankQueryEveryShapeUsesTheModel(t *testing.T) {
	for pattern, model := range map[string]string{
		"single":                       "single",
		`"two words"`:                  "two words",
		"ParseRequest":                 "ParseRequest",
		"file:go alpha":                "alpha",
		"content:alpha beta":           "beta",
		"alpha or beta":                "alpha beta",
		"alpha -beta":                  "alpha",
		"sym:Alpha beta":               "beta",
		"alpha ^":                      "alpha",
		"alpha beta (gamma or file:x)": "alpha beta gamma",
		"(alpha or beta) gamma":        "alpha beta gamma",
		`alpha \d+`:                    "alpha",
		"alpha [^a-z]":                 "alpha",
		`alpha \b`:                     "alpha",
		"sym:ParseRequest":             "ParseRequest",
		"file:cmd -file:test":          "cmd",
		"lang:go":                      "Go",
	} {
		t.Run(pattern, func(t *testing.T) {
			if plan := planFor(t, pattern); plan.modelQuery != model {
				t.Fatalf("model text %q, want %q", plan.modelQuery, model)
			}
		})
	}
}

// TestPlanRerankQueryKeepsZoektFiltersOutOfModelText plans descriptions with
// filters other than a positive lang: or file: (a negated language, sym:,
// content:, repo:, and an or group): the model reads only the description,
// and the plan keeps each filter for the per-node Zoekt mask, which applies
// it to semantic retrieval (TestLanguageFilterKeepsTheVectorHitsOfThatLanguage,
// TestPassThroughAtomMasksSemantic).
func TestPlanRerankQueryKeepsZoektFiltersOutOfModelText(t *testing.T) {
	for _, pattern := range []string{
		"alpha beta -lang:go",
		"alpha beta sym:Alpha",
		"alpha beta content:gamma",
		"alpha beta repo:x",
		"alpha beta (file:a or lang:go)",
	} {
		t.Run(pattern, func(t *testing.T) {
			plan := planFor(t, pattern)
			if plan.modelQuery != "alpha beta" || len(plan.filters) == 0 {
				t.Fatalf("model=%q filters=%v, want alpha beta and the filter kept for Zoekt",
					plan.modelQuery, plan.filters)
			}
		})
	}
}

// TestPlanRerankQueryAcceptsRegexAndCaseAtoms proves that regular
// expressions, case modifiers, and quoted phrases give model text.
func TestPlanRerankQueryAcceptsRegexAndCaseAtoms(t *testing.T) {
	for pattern, model := range map[string]string{
		"regex:alpha beta":        "alpha beta",
		"regex:alpha.*beta extra": "alpha.*beta extra",
		"case:yes Alpha beta":     "Alpha beta",
		`"two words" more`:        "two words more",
		// Zoekt removes capture groups; the model text keeps the typed ones.
		"Foo() bar":     "Foo() bar",
		"call a::b() x": "call a::b() x",
		"a(bx|cy)d bar": "a(bx|cy)d bar",
		"(ab)+ bar":     "(ab)+ bar",
		"x(yz)? bar":    "x(yz)? bar",
	} {
		t.Run(pattern, func(t *testing.T) {
			plan := planFor(t, pattern)
			if plan.modelQuery != model {
				t.Fatalf("model=%q, want %q", plan.modelQuery, model)
			}
		})
	}
}

// TestRerankModelSearchConfigClearsDisplayLimitsAndAddsInternalContext proves
// that the model search config has no display limits and the standard context
// on both sides, and that it leaves its input unchanged.
func TestRerankModelSearchConfigClearsDisplayLimitsAndAddsInternalContext(t *testing.T) {
	config := searchConfig{
		opts:              &zoekt.SearchOptions{NumContextLines: 1},
		afterOnly:         true,
		contextMatchLimit: 2,
		contextFileLimit:  7,
	}
	got := rerankModelSearchConfig(config)
	// The model reads three lines of context on each side of a match.
	if got.opts.NumContextLines != 3 || got.afterOnly {
		t.Fatalf("internal context config=%+v", got)
	}
	if got.contextMatchLimit != 0 || got.contextFileLimit != 0 {
		t.Fatalf("internal limits=%+v, want none: the display applies them after the rank", got)
	}
	if config.opts.NumContextLines != 1 || !config.afterOnly || config.contextMatchLimit != 2 {
		t.Fatalf("internal context conversion mutated its input: %+v", config)
	}
}

// TestRunRerankAcceptanceWithoutStrictResults searches two folders where no
// file holds both words: a relaxed match alone is a rank signal, not a
// candidate, so the search has no match and the model scores nothing. The
// build fails on purpose, so no vector exists.
func TestRunRerankAcceptanceWithoutStrictResults(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	alpha := t.TempDir()
	beta := t.TempDir()
	writeFileAt(t, alpha, "alpha.go", "package sample\n// alpha only\n")
	writeFileAt(t, beta, "beta.go", "package sample\n// beta only\n")
	scorer := &hybridTestScorer{embedErr: errRerankUnavailable}
	output, err := seekWith(t, modelFactory(scorer), "alpha beta", alpha, beta)
	if !errors.Is(err, errNoMatch) || output != "" || scorer.scoreCalls.Load() != 0 {
		t.Fatalf("output=%q err=%v model calls=%d, want no match and no model call", output, err, scorer.scoreCalls.Load())
	}
}

// TestRunRerankStrictEvidenceContract searches a strict all-word file and a
// relaxed-only file: the strict file is admitted and model-scored, the
// relaxed-only file is no candidate. The build fails on purpose, so no vector
// exists.
func TestRunRerankStrictEvidenceContract(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	strict := t.TempDir()
	relaxed := t.TempDir()
	writeFileAt(t, strict, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, relaxed, "relaxed.go", "package sample\n// alpha only\n")
	scorer := &hybridTestScorer{embedErr: errRerankUnavailable}
	output, err := seekWith(t, modelFactory(scorer), "alpha beta", strict, relaxed)
	if err != nil || !strings.Contains(output, "strict.go") || strings.Contains(output, "relaxed.go") {
		t.Fatalf("output=%q err=%v, want strict.go only", output, err)
	}
	if scorer.scoreCalls.Load() != 1 || len(scorer.documents) != 1 {
		t.Fatalf("model calls=%d documents=%d, want one call with the strict file", scorer.scoreCalls.Load(), len(scorer.documents))
	}
}

func TestNewRerankDocumentUsesBestMatchAndThreeLineContext(t *testing.T) {
	symbol := &zoekt.Symbol{Sym: "Serve", Kind: "method", Parent: "API"}
	const matchedText = "Serve"
	result := rerankTestResult("api/server.go", 1)
	result.file.Language = "Go"
	result.file.LineMatches = []zoekt.LineMatch{
		{Line: []byte("low\n"), LineNumber: 2, Score: 1},
		{
			Before:     []byte("before-1\nbefore-2\nbefore-3\nbefore-4\n"),
			Line:       []byte("return Serve(request)\n"),
			After:      []byte("after-1\nafter-2\nafter-3\nafter-4\n"),
			LineNumber: 8,
			Score:      3,
			LineFragments: []zoekt.LineFragmentMatch{{
				LineOffset:  len("return "),
				MatchLength: len(matchedText),
				SymbolInfo:  symbol,
			}},
		},
	}
	document := newRerankDocument(result)
	if document.Path != "api/server.go" || document.Language != "Go" {
		t.Fatalf("metadata=%+v", document)
	}
	if document.Symbol != "API method Serve" {
		t.Fatalf("symbol=%q", document.Symbol)
	}
	if document.Text != "before-2\nbefore-3\nbefore-4\nreturn Serve(request)\nafter-1\nafter-2\nafter-3\n" {
		t.Fatalf("text=%q", document.Text)
	}
	if document.matchAt < 0 || document.matchEnd > len(document.Text) ||
		document.matchAt >= document.matchEnd {
		t.Fatalf(
			"match bounds=[%d:%d] are invalid for %d bytes",
			document.matchAt,
			document.matchEnd,
			len(document.Text),
		)
	}
	if got := document.Text[document.matchAt:document.matchEnd]; got != matchedText {
		t.Fatalf("match bounds select %q, want %q", got, matchedText)
	}
}

func TestSerializeLateOnDocumentIncludesCompactMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		wantPath string
	}{
		{
			name:     "long path",
			path:     "platform/services/web/src/handler.go",
			wantPath: "services/web/src/handler.go",
		},
		{
			name:     "short path",
			path:     "src/handler.go",
			wantPath: "src/handler.go",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const matchedText = "Serve"
			const body = "call Serve now\n"
			wantMetadata := "path: " + tc.wantPath +
				"\nlanguage: Go\nsymbol: API method Serve\n"
			serialized, metadataEnd, matchAt, matchEnd := serializeLateOnDocumentWithMatch(
				rerankDocument{
					Path:     tc.path,
					Language: "Go",
					Symbol:   "API method Serve",
					Text:     body,
					matchAt:  len("call "),
					matchEnd: len("call ") + len(matchedText),
				},
			)
			if metadataEnd < 0 || metadataEnd > len(serialized) {
				t.Fatalf("metadata end=%d is invalid for %d bytes", metadataEnd, len(serialized))
			}
			if gotMetadata := serialized[:metadataEnd]; metadataEnd != len(wantMetadata) || gotMetadata != wantMetadata {
				t.Fatalf(
					"metadata end=%d metadata=%q, want end=%d metadata=%q",
					metadataEnd,
					gotMetadata,
					len(wantMetadata),
					wantMetadata,
				)
			}
			if matchAt < 0 || matchAt > matchEnd || matchEnd > len(serialized) {
				t.Fatalf("serialized match bounds [%d:%d] are invalid for %d bytes", matchAt, matchEnd, len(serialized))
			}
			if serialized[matchAt:matchEnd] != matchedText {
				t.Fatalf("serialized match=%q, want %q", serialized[matchAt:matchEnd], matchedText)
			}
		})
	}
}

func TestSerializeLateOnDocumentKeepsMatchWithinByteLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		before string
		match  string
		after  string
	}{
		{
			name:   "large UTF-8 metadata and context",
			path:   strings.Repeat("p", maxRerankDocumentBytes),
			before: strings.Repeat("é", maxRerankDocumentBytes),
			match:  "Serve()",
		},
		{
			name:   "match after the head window",
			before: strings.Repeat("x", maxRerankDocumentBytes+2048),
			match:  "late-semantic-match",
			after:  strings.Repeat("y", 2048),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.before + tc.match + tc.after
			serialized, metadataEnd, matchAt, matchEnd := serializeLateOnDocumentWithMatch(
				rerankDocument{
					Path:     tc.path,
					Language: "Go",
					Text:     text,
					matchAt:  len(tc.before),
					matchEnd: len(tc.before) + len(tc.match),
				},
			)
			if len(serialized) > maxRerankDocumentBytes || !utf8.ValidString(serialized) {
				t.Fatalf("serialized document has %d bytes, valid=%t", len(serialized), utf8.ValidString(serialized))
			}
			if matchAt < 0 || matchAt > matchEnd || matchEnd > len(serialized) {
				t.Fatalf("serialized match bounds [%d:%d] are invalid for %d bytes", matchAt, matchEnd, len(serialized))
			}
			if serialized[matchAt:matchEnd] != tc.match {
				t.Fatalf("serialized match [%d:%d] = %q, want %q", matchAt, matchEnd, serialized[matchAt:matchEnd], tc.match)
			}
			if metadataEnd <= 0 || metadataEnd > matchAt {
				t.Fatalf("metadata end %d is outside (0:%d]", metadataEnd, matchAt)
			}
		})
	}
}

func TestApplyRerankDisplayConfigHidesModelContext(t *testing.T) {
	result := rerankTestResult("context.go", 1)
	result.file.LineMatches[0].Before = []byte("b1\nb2\nb3\nb4\n")
	result.file.LineMatches[0].After = []byte("a1\na2\na3\na4\n")

	hidden := applyRerankDisplayConfig(
		[]corpusSearchResult{result},
		explicitSearchConfig(0, false),
	)
	if len(hidden[0].file.LineMatches[0].Before) != 0 ||
		len(hidden[0].file.LineMatches[0].After) != 0 {
		t.Fatalf("model context reached context-zero output: %#v", hidden[0].file.LineMatches[0])
	}

	afterOnly := explicitSearchConfig(1, true)
	afterOnly.contextMatchLimit = 1
	trimmed := applyRerankDisplayConfig([]corpusSearchResult{result}, afterOnly)
	match := trimmed[0].file.LineMatches[0]
	if len(match.Before) != 0 || string(match.After) != "a1\n" {
		t.Fatalf("after-only context=%q/%q", match.Before, match.After)
	}
	if string(result.file.LineMatches[0].Before) != "b1\nb2\nb3\nb4\n" ||
		string(result.file.LineMatches[0].After) != "a1\na2\na3\na4\n" {
		t.Fatal("display context conversion mutated its input")
	}
}

func TestRankCorpusResultsForDisplayHonorsOverride(t *testing.T) {
	low := rerankTestResult("low.go", 1)
	low.rankOverride = 1
	high := rerankTestResult("high.go", 10)
	high.rankOverride = 2
	ranked := rankCorpusResultsForDisplay([]corpusSearchResult{high, low})
	if ranked[0].file.FileName != "low.go" {
		t.Fatalf("override order=%q then %q", ranked[0].file.FileName, ranked[1].file.FileName)
	}

	low.rankOverride = 0
	high.rankOverride = 0
	ranked = rankCorpusResultsForDisplay([]corpusSearchResult{low, high})
	if ranked[0].file.FileName != "high.go" {
		t.Fatalf("BM25 order=%q then %q", ranked[0].file.FileName, ranked[1].file.FileName)
	}
}

func TestRunRerankDoesNotStartScorerWhenCorpusPlanningFails(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing")

	model := observeModel(&hybridTestScorer{})
	_, err := seekWith(t, model.factory(), "alpha beta", missing)
	if err == nil {
		t.Fatal("missing corpus must fail")
	}
	if model.starts.Load() != 0 || model.closes.Load() != 0 {
		t.Fatalf("scorer started before planning: calls=%d closes=%d error=%v", model.starts.Load(), model.closes.Load(), err)
	}
}

// TestRunRerankFailingModelKeepsStrictOutputForEveryShape searches one word,
// a description, a phrase, regex:, case:, filters, an or and negations with a
// model that cannot start. The factory error is a test value. Each shape
// takes the model route: it asks for the model once and prints the strict
// output byte for byte. A query of negations only gives the model no text, so
// it never asks.
func TestRunRerankFailingModelKeepsStrictOutputForEveryShape(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\nfunc Alpha() {}\n// alpha beta gamma\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha only\n")

	for pattern, wantFactoryCalls := range map[string]int32{
		"alpha":                    1,
		"alpha beta":               1,
		`"alpha beta"`:             1,
		"regex:alpha beta":         1,
		"case:yes alpha beta":      1,
		"file:strict alpha":        1,
		"content:alpha beta":       1,
		"alpha beta content:gamma": 1,
		"alpha beta sym:Alpha":     1,
		"alpha beta -lang:python":  1,
		"alpha or beta":            1,
		"alpha -missing":           1,
		"alpha beta -missing":      1,
		"-file:missing":            0,
	} {
		t.Run(pattern, func(t *testing.T) {
			baseline, err := seek(t, "--lexical-only", pattern, folder)
			if err != nil || baseline == "" {
				t.Fatalf("lexical baseline=%q err=%v, want a strict match", baseline, err)
			}
			var factoryCalls atomic.Int32
			got, err := seekWith(t, failingModelFactory(&factoryCalls), pattern, folder)
			if err != nil || got != baseline || factoryCalls.Load() != wantFactoryCalls {
				t.Fatalf("a failing model changed the output: calls=%d err=%v\nwant=%q\ngot=%q",
					factoryCalls.Load(), err, baseline, got)
			}
		})
	}
}

// TestRunRerankWithDirtyFilesAcrossCorpora searches two Git repositories, one
// with a dirty and an untracked file: the model scores the strict files of
// both, and the committed text of the dirty file never shows. The build fails
// on purpose, so no vector exists.
func TestRunRerankWithDirtyFilesAcrossCorpora(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	repoOne := initGitRepo(t, "same.go", "package sample\n// alpha beta committed\n")
	writeFileAt(t, repoOne, "same.go", "package sample\n// alpha dirty only\n")
	writeFileAt(t, repoOne, "untracked.go", "package sample\n// alpha beta untracked\n")
	repoTwo := initGitRepo(t, "same.go", "package sample\n// alpha beta second repository\n")

	scorer := &hybridTestScorer{embedErr: errRerankUnavailable}
	output, err := seekWith(t, modelFactory(scorer), "alpha beta", repoOne, repoTwo)
	if err != nil {
		t.Fatal(err)
	}
	// The strict files of both nodes show; the dirty same.go of repo one
	// matches only one word, so it is no candidate, and its stale
	// committed bytes never show.
	for _, want := range []string{
		filepath.Join(repoOne, "untracked.go"),
		filepath.Join(repoTwo, "same.go"),
		"[uncommitted]",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("multi-corpus dirty output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "alpha beta committed") || strings.Contains(output, filepath.Join(repoOne, "same.go")) {
		t.Fatalf("output shows the stale or relaxed-only same.go of repo one:\n%s", output)
	}
	paths := make([]string, 0, len(scorer.documents))
	for _, document := range scorer.documents {
		paths = append(paths, document.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"same.go", "untracked.go"}) {
		t.Fatalf("model documents=%v, want the two strict files", paths)
	}
}

// TestRunRerankModelContextStaysOutOfDisplay searches with no context lines:
// the model reads context lines on both sides of the match, and the output
// shows none of them. The build fails on purpose, so no vector exists.
func TestRunRerankModelContextStaysOutOfDisplay(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "target.go", strings.Join([]string{
		"package sample",
		"// hidden before one",
		"// hidden before two",
		"// hidden before three",
		"// alpha beta target",
		"// hidden after one",
		"// hidden after two",
		"// hidden after three",
		"",
	}, "\n"))
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha only\n")

	scorer := &hybridTestScorer{embedErr: errRerankUnavailable}
	output, err := seekWith(t, modelFactory(scorer), "-C", "0", "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "hidden before") || strings.Contains(output, "hidden after") {
		t.Fatalf("internal model context reached output:\n%s", output)
	}
	modelSawContext := false
	for _, document := range scorer.documents {
		if document.Path == "target.go" &&
			strings.Contains(document.Text, "hidden before three") &&
			strings.Contains(document.Text, "hidden after three") {
			modelSawContext = true
		}
	}
	if !modelSawContext {
		t.Fatalf("model did not receive fixed context: %+v", scorer.documents)
	}
}

// TestRunRerankFailureKeepsModelContextOutOfDisplay searches with no context
// lines and a model that fails at the score call: the model reads context
// lines on both sides of the match, and the strict output shows the match and
// none of them. The build fails on purpose, so no vector exists.
func TestRunRerankFailureKeepsModelContextOutOfDisplay(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "target.go", strings.Join([]string{
		"package sample",
		"// hidden before one",
		"// hidden before two",
		"// hidden before three",
		"// alpha beta target",
		"// hidden after one",
		"// hidden after two",
		"// hidden after three",
		"",
	}, "\n"))
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha only\n")

	model := observeModel(&hybridTestScorer{
		embedErr: errRerankUnavailable,
		scoreErr: errors.New("score failure"),
	})
	output, err := seekWith(t, model.factory(), "-C", "0", "alpha beta", folder)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "hidden before") || strings.Contains(output, "hidden after") {
		t.Fatalf("internal model context reached fallback output:\n%s", output)
	}
	if !strings.Contains(output, "alpha beta target") {
		t.Fatalf("fallback lost the strict match:\n%s", output)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	modelSawContext := false
	for _, scored := range model.scored {
		if scored.document.Path == "target.go" &&
			strings.Contains(scored.document.Text, "hidden before three") &&
			strings.Contains(scored.document.Text, "hidden after three") {
			modelSawContext = true
		}
	}
	if !modelSawContext {
		t.Fatalf("failed scorer did not receive fixed context: %+v", model.scored)
	}
}

func rerankTestResult(name string, score float64) corpusSearchResult {
	return corpusSearchResult{
		corpusID: "repo",
		kind:     corpusKindFolder,
		file: zoekt.FileMatch{
			FileName: name,
			Score:    score,
			LineMatches: []zoekt.LineMatch{{
				Line:       []byte("alpha beta\n"),
				LineNumber: 1,
				Score:      score,
			}},
		},
	}
}

// TestPlanRerankQueryFilterTextOutlivesItsMask plans filters whose
// punctuation makes the mask true or narrower: the model still reads the text
// of the filter's positive patterns.
func TestPlanRerankQueryFilterTextOutlivesItsMask(t *testing.T) {
	for _, row := range []struct {
		pattern, model, filters string
	}{
		{pattern: `lang:go or \d+`, model: "Go", filters: "[]"},
		{pattern: `\d+ or lang:go`, model: "Go", filters: "[]"},
		{pattern: `lang:go (lang:python or \d+)`, model: "Go Python", filters: "[lang:Go]"},
	} {
		plan := planFor(t, row.pattern)
		if plan.modelQuery != row.model || fmt.Sprint(plan.filters) != row.filters {
			t.Fatalf("%s: model text %q, mask %v; want %q, %s", row.pattern, plan.modelQuery, plan.filters, row.model, row.filters)
		}
	}
}

// TestPlanRerankQueryOrMaskKeepsOnlyFilters plans queries with a punctuation
// atom or a nested type:file next to their filters: the model text and the
// semantic mask are those of the same query with neither, and so is relaxed
// retrieval, except that it keeps the punctuation of a filter.
func TestPlanRerankQueryOrMaskKeepsOnlyFilters(t *testing.T) {
	for _, row := range []struct {
		pattern, same           string
		relaxedKeepsPunctuation bool
	}{
		{pattern: `(alpha \d+ lang:go) or (lang:python beta)`, same: "(alpha lang:go) or (lang:python beta)"},
		{pattern: "(type:file alpha lang:go) or (lang:python beta)", same: "(alpha lang:go) or (lang:python beta)"},
		{pattern: "((type:file alpha) lang:go) or (lang:python beta)", same: "(alpha lang:go) or (lang:python beta)"},
		{pattern: "lang:go (type:file alpha)", same: "type:file lang:go alpha"},
		{pattern: `(\d+ lang:go) or lang:python`, same: "lang:go or lang:python", relaxedKeepsPunctuation: true},
	} {
		got, want := planFor(t, row.pattern), planFor(t, row.same)
		if got.modelQuery != want.modelQuery {
			t.Fatalf("%s: model text %q, want %q", row.pattern, got.modelQuery, want.modelQuery)
		}
		if fmt.Sprint(got.filters) != fmt.Sprint(want.filters) {
			t.Fatalf("%s: mask %s, want %s", row.pattern, got.filters, want.filters)
		}
		if equal := got.relaxedQ.String() == want.relaxedQ.String(); equal == row.relaxedKeepsPunctuation {
			t.Fatalf("%s: relaxed %s, %s: %s", row.pattern, got.relaxedQ, row.same, want.relaxedQ)
		}
	}
}
