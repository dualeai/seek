package main

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

type fixedRerankScorer struct {
	scores            []float32
	err               error
	closed            *bool
	documents         *[]rerankDocument
	activeQueryTokens int
	queryTruncated    bool
	scoreCalls        atomic.Int32
}

func (*fixedRerankScorer) PackSemanticUnits(
	_ context.Context,
	units []semanticUnit,
) ([]semanticUnit, error) {
	return units, nil
}

func (*fixedRerankScorer) EmbedSemanticUnits(
	context.Context,
	[]semanticUnit,
) ([]semanticUnitEmbedding, error) {
	return nil, errRerankUnavailable
}

func (s *fixedRerankScorer) PrepareSemanticQuery(
	_ context.Context,
	query string,
) (*semanticQueryEmbedding, error) {
	mask := make([]bool, max(1, s.activeQueryTokens))
	for index := range mask {
		mask[index] = true
	}
	return &semanticQueryEmbedding{
		modelQuery: query,
		scoreMask:  mask,
		truncated:  s.queryTruncated,
	}, nil
}

func (s *fixedRerankScorer) ScoresWithSemanticQuery(
	ctx context.Context,
	query *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	batch, err := s.Scores(ctx, query.modelQuery, documents)
	return batch.values, err
}

func (*fixedRerankScorer) SemanticCallCPUs(context.Context) int { return 1 }

func (s *fixedRerankScorer) Scores(
	_ context.Context,
	_ string,
	documents []rerankDocument,
) (rerankScoreBatch, error) {
	s.scoreCalls.Add(1)
	if s.documents != nil {
		*s.documents = append([]rerankDocument(nil), documents...)
	}
	if s.scores == nil {
		return rerankScoreBatch{
			values:            make([]float32, len(documents)),
			activeQueryTokens: max(1, s.activeQueryTokens),
			queryTruncated:    s.queryTruncated,
		}, s.err
	}
	return rerankScoreBatch{
		values:            append([]float32(nil), s.scores...),
		activeQueryTokens: max(1, s.activeQueryTokens),
		queryTruncated:    s.queryTruncated,
	}, s.err
}

func (s *fixedRerankScorer) Close() error {
	if s.closed != nil {
		*s.closed = true
	}
	return nil
}

func TestPlanRerankQuery(t *testing.T) {
	raw, _, err := parseSearchQueryForms("Handle shell completion")
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := planRerankQuery("Handle shell completion", raw)
	if !ok {
		t.Fatal("plain three-term query must be eligible")
	}
	if plan.modelQuery != "Handle shell completion" {
		t.Fatalf("model query=%q", plan.modelQuery)
	}
	if plan.semanticFilter != nil {
		t.Fatal("plain query entered the semantic filter path")
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

func TestRerankQueryGuaranteedTruncatedUsesOnlyCertainBound(t *testing.T) {
	if rerankQueryGuaranteedTruncated(strings.Repeat("long", 1_000)) {
		t.Fatal("one long term is not certainly truncated without tokenization")
	}
	if rerankQueryGuaranteedTruncated(strings.TrimSpace(strings.Repeat(
		"? ",
		rerankQueryMaximumUntruncatedTerms+1,
	))) {
		t.Fatal("punctuation-only terms must use the real tokenizer")
	}
	if rerankQueryGuaranteedTruncated(
		strings.TrimSpace(strings.Repeat("term ", rerankQueryMaximumUntruncatedTerms)),
	) {
		t.Fatal("the maximum possible term count must use the real tokenizer")
	}
	if !rerankQueryGuaranteedTruncated(
		strings.TrimSpace(strings.Repeat("term ", rerankQueryMaximumUntruncatedTerms+1)),
	) {
		t.Fatal("a query above the certain term bound must be truncated")
	}
}

func TestPlanRerankQueryCombinesRepeatedAndContradictoryFilters(t *testing.T) {
	for _, test := range []struct {
		name    string
		pattern string
		path    string
		lang    string
		want    bool
	}{
		{
			name:    "repeated filename filters",
			pattern: "find request file:cmd file:seek parser",
			path:    "cmd/seek/main.go", lang: "Go", want: true,
		},
		{
			name:    "repeated filename filter miss",
			pattern: "find request file:cmd file:seek parser",
			path:    "cmd/main.go", lang: "Go", want: false,
		},
		{
			name:    "contradictory language filters",
			pattern: "find request lang:go lang:python parser",
			path:    "cmd/seek/main.go", lang: "Go", want: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _, err := parseSearchQueryForms(test.pattern)
			if err != nil {
				t.Fatal(err)
			}
			plan, ok := planRerankQuery(test.pattern, raw)
			if !ok || plan.semanticFilter == nil {
				t.Fatal("filtered query is not eligible")
			}
			if got := plan.semanticFilter.matches(test.path, test.lang); got != test.want {
				t.Fatalf("matches=%t, want %t", got, test.want)
			}
		})
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
			plan, ok := planRerankQuery(pattern, raw)
			if !ok {
				t.Fatal("plain description with a programming name must be eligible")
			}
			if plan.modelQuery != pattern {
				t.Fatalf("model query=%q, want %q", plan.modelQuery, pattern)
			}
		})
	}
}

func TestPlanRerankQueryAcceptsFileAndLanguageFilters(t *testing.T) {
	tests := []struct {
		name           string
		pattern        string
		wantModel      string
		allowPath      string
		allowLanguage  string
		rejectPath     string
		rejectLanguage string
	}{
		{
			name:      "language",
			pattern:   "find request parser lang:go",
			wantModel: "find request parser",
			allowPath: "cmd/seek/main.go", allowLanguage: "Go",
			rejectPath: "cmd/seek/main.py", rejectLanguage: "Python",
		},
		{
			name:      "file",
			pattern:   `find request parser file:^cmd/seek/`,
			wantModel: "find request parser",
			allowPath: "cmd/seek/main.go", allowLanguage: "Go",
			rejectPath: "internal/main.go", rejectLanguage: "Go",
		},
		{
			name:      "quoted file",
			pattern:   `find request parser file:"my file"`,
			wantModel: "find request parser",
			allowPath: "docs/my file.go", allowLanguage: "Go",
			rejectPath: "docs/other.go", rejectLanguage: "Go",
		},
		{
			name:      "negative file",
			pattern:   `find request parser -file:_test\.go$`,
			wantModel: "find request parser",
			allowPath: "cmd/seek/main.go", allowLanguage: "Go",
			rejectPath: "cmd/seek/main_test.go", rejectLanguage: "Go",
		},
		{
			name:      "combined and reordered",
			pattern:   `lang:go find file:^cmd/ request -file:_test\.go$ parser`,
			wantModel: "find request parser",
			allowPath: "cmd/seek/main.go", allowLanguage: "Go",
			rejectPath: "cmd/seek/main_test.go", rejectLanguage: "Go",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, _, err := parseSearchQueryForms(test.pattern)
			if err != nil {
				t.Fatal(err)
			}
			plan, ok := planRerankQuery(test.pattern, raw)
			if !ok || plan.semanticFilter == nil {
				t.Fatalf("filtered query is not eligible: %#v", plan)
			}
			if plan.modelQuery != test.wantModel {
				t.Fatalf("model query=%q, want %q", plan.modelQuery, test.wantModel)
			}
			freshRaw, _, err := parseSearchQueryForms(test.pattern)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(raw, freshRaw) {
				t.Fatalf("planning mutated the strict query:\nbefore=%#v\nafter=%#v", freshRaw, raw)
			}
			if !plan.semanticFilter.matches(test.allowPath, test.allowLanguage) {
				t.Fatalf("filter rejected %q (%s)", test.allowPath, test.allowLanguage)
			}
			if plan.semanticFilter.matches(test.rejectPath, test.rejectLanguage) {
				t.Fatalf("filter accepted %q (%s)", test.rejectPath, test.rejectLanguage)
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
			plan, ok := planRerankQuery(pattern, raw)
			if !ok || plan.semanticFilter == nil || plan.modelQuery != "alpha beta" {
				t.Fatalf("normalized query plan=%#v eligible=%t", plan, ok)
			}
		})
	}
}

func TestPlainDescriptionFallbackRejectsExplicitSyntax(t *testing.T) {
	for _, pattern := range []string{
		"find C++ file:[",
		"find C++ [broken",
		"find C++ (broken",
		"find C++ OR handler",
		"find C++ -handler",
		"find C++ foo**",
	} {
		t.Run(pattern, func(t *testing.T) {
			if _, _, err := parseSearchQueryForms(pattern); err == nil {
				t.Fatal("malformed explicit syntax must stay an error")
			}
		})
	}
}

func TestPlanRerankQueryRejectsSyntax(t *testing.T) {
	tests := []string{
		"single",
		`"two words"`,
		"file:go alpha",
		"content:alpha beta",
		"regex:alpha beta",
		"regex:alpha.*beta extra",
		"alpha or beta",
		"alpha -beta",
		"sym:Alpha beta",
		"case:yes Alpha beta",
		"alpha beta lang:definitely-not-a-language",
		"alpha beta -lang:go",
	}
	for _, pattern := range tests {
		t.Run(pattern, func(t *testing.T) {
			raw, _, err := parseSearchQueryForms(pattern)
			if err != nil {
				t.Fatal(err)
			}
			if plan, ok := planRerankQuery(pattern, raw); ok {
				t.Fatalf("query must be ineligible: %#v", plan)
			}
		})
	}
}

func TestRerankCandidateSearchConfig(t *testing.T) {
	if rerankCandidateLimit != semanticModelBatchRows {
		t.Fatalf("candidate limit=%d, want full model batch %d", rerankCandidateLimit, semanticModelBatchRows)
	}
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{name: "display limit below pool", in: 1, want: rerankCandidateLimit},
		{name: "unlimited display", in: 0, want: rerankCandidateLimit},
		{name: "larger display limit", in: 30, want: rerankCandidateLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rerankCandidateSearchConfig(searchConfig{
				opts:              &zoekt.SearchOptions{NumContextLines: 1},
				afterOnly:         true,
				contextMatchLimit: 1,
				contextFileLimit:  tc.in,
			})
			if got.contextFileLimit != tc.want || got.resultFileLimit != tc.want {
				t.Fatalf(
					"candidate file limits=context:%d result:%d, want %d",
					got.contextFileLimit,
					got.resultFileLimit,
					tc.want,
				)
			}
			if got.opts.NumContextLines != searchContextLines ||
				got.afterOnly || got.contextMatchLimit != 0 {
				t.Fatalf("candidate context config=%+v", got)
			}
		})
	}
	got := rerankCandidateSearchConfig(explicitSearchConfig(12, true))
	if got.opts.NumContextLines != 12 || got.afterOnly {
		t.Fatalf("larger user context was not preserved symmetrically: %+v", got)
	}
}

func TestRerankModelSearchConfigKeepsLimitsAndAddsInternalContext(t *testing.T) {
	config := searchConfig{
		opts:              &zoekt.SearchOptions{NumContextLines: 1},
		afterOnly:         true,
		contextMatchLimit: 2,
		contextFileLimit:  7,
		resultFileLimit:   9,
	}
	got := rerankModelSearchConfig(config)
	if got.opts.NumContextLines != searchContextLines || got.afterOnly ||
		got.contextMatchLimit != 2 {
		t.Fatalf("internal context config=%+v", got)
	}
	if got.contextFileLimit != 7 || got.resultFileLimit != 9 {
		t.Fatalf(
			"internal file limits=context:%d result:%d, want 7 and 9",
			got.contextFileLimit,
			got.resultFileLimit,
		)
	}
	if config.opts.NumContextLines != 1 || !config.afterOnly || config.contextMatchLimit != 2 {
		t.Fatalf("internal context conversion mutated its input: %+v", config)
	}
}

func TestTryRerankCorporaCandidateErrorKeepsStrictResults(t *testing.T) {
	strict := []corpusSearchResult{rerankTestResultWithModelContext("strict.go", 10)}
	strictDirty := dirtyFilesByCorpus{"repo": testDirtyFileSet("dirty.go")}
	wantErr := errors.New("candidate failure")
	relaxedQ := &query.Or{Children: []query.Q{
		&query.Substring{Pattern: "alpha", Content: true},
		&query.Substring{Pattern: "beta", Content: true},
	}}
	search := func(
		_ context.Context,
		_ []corpusPlan,
		_ *gitPaths,
		gotQ query.Q,
		gotConfig searchConfig,
	) ([]corpusSearchResult, dirtyFilesByCorpus, error) {
		if !reflect.DeepEqual(gotQ, relaxedQ) {
			t.Fatalf("candidate query=%#v, want %#v", gotQ, relaxedQ)
		}
		if gotConfig.contextFileLimit != rerankCandidateLimit {
			t.Fatalf("candidate context limit=%d, want %d", gotConfig.contextFileLimit, rerankCandidateLimit)
		}
		return nil, nil, wantErr
	}

	got, gotDirty, err := tryRerankCorpora(
		context.Background(),
		strict,
		strictDirty,
		nil,
		nil,
		searchConfig{contextFileLimit: 1},
		rerankQueryPlan{modelQuery: "alpha beta", relaxedQ: relaxedQ},
		(&fixedRerankScorer{}).Scores,
		nil,
		search,
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v, want wrapped candidate error", err)
	}
	if !reflect.DeepEqual(gotDirty, strictDirty) {
		t.Fatalf("fallback changed strict results: results=%#v dirty=%#v", got, gotDirty)
	}
	assertRerankFallbackHidesModelContext(t, got, strict)
	if strict[0].rankOverride != 0 {
		t.Fatal("fallback mutated the strict slice")
	}
}

func TestTryRerankCorporaMissingBackendSkipsCandidateSearch(t *testing.T) {
	strict := []corpusSearchResult{rerankTestResultWithModelContext("strict.go", 10)}
	searchCalls := 0
	got, _, err := tryRerankCorpora(
		context.Background(),
		strict,
		nil,
		nil,
		nil,
		searchConfig{},
		rerankQueryPlan{modelQuery: "alpha beta", relaxedQ: &query.Or{}},
		nil,
		nil,
		func(
			context.Context,
			[]corpusPlan,
			*gitPaths,
			query.Q,
			searchConfig,
		) ([]corpusSearchResult, dirtyFilesByCorpus, error) {
			searchCalls++
			return nil, nil, nil
		},
	)
	if !errors.Is(err, errRerankUnavailable) || searchCalls != 0 {
		t.Fatalf("missing backend: error=%v search calls=%d results=%#v", err, searchCalls, got)
	}
	assertRerankFallbackHidesModelContext(t, got, strict)
}

func TestTryRerankCorporaScorerErrorKeepsStrictResults(t *testing.T) {
	strict := []corpusSearchResult{rerankTestResultWithModelContext("strict.go", 10)}
	strictDirty := dirtyFilesByCorpus{"repo": testDirtyFileSet("dirty.go")}
	relaxed := []corpusSearchResult{
		rerankTestResult("alpha.go", 2),
		rerankTestResult("beta.go", 1),
	}
	wantErr := errors.New("score failure")

	got, gotDirty, err := tryRerankCorpora(
		context.Background(),
		strict,
		strictDirty,
		nil,
		nil,
		searchConfig{},
		rerankQueryPlan{modelQuery: "alpha beta", relaxedQ: &query.Or{}},
		(&fixedRerankScorer{err: wantErr}).Scores,
		nil,
		func(
			context.Context,
			[]corpusPlan,
			*gitPaths,
			query.Q,
			searchConfig,
		) ([]corpusSearchResult, dirtyFilesByCorpus, error) {
			return relaxed, nil, nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("scorer failure: error=%v", err)
	}
	if !reflect.DeepEqual(gotDirty, strictDirty) {
		t.Fatalf("scorer fallback changed strict results: results=%#v dirty=%#v", got, gotDirty)
	}
	assertRerankFallbackHidesModelContext(t, got, strict)
}

func TestRunRerankAcceptanceWithoutStrictResults(t *testing.T) {
	for _, test := range []struct {
		name      string
		scores    []float32
		wantMatch bool
	}{
		{name: "reject", scores: []float32{0, 0}},
		{name: "accept", scores: []float32{1, 0}, wantMatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireTools(t)
			setTestUserCache(t)
			t.Chdir(t.TempDir())
			alpha := t.TempDir()
			beta := t.TempDir()
			writeFileAt(t, alpha, "alpha.go", "package sample\n// alpha only\n")
			writeFileAt(t, beta, "beta.go", "package sample\n// beta only\n")
			policy := newRerankAcceptancePolicy(0.575)
			policy.route = rerankAcceptanceRoute
			var documents []rerankDocument
			scorer := &fixedRerankScorer{scores: test.scores, documents: &documents}

			output, err := captureStdout(t, func() error {
				return runSearchCommand(
					t.Context(),
					"alpha beta",
					[]string{alpha, beta},
					0,
					0,
					defaultSearchConfig(),
					searchRunConfig{
						policy:           defaultSearchPolicy(),
						rerankAcceptance: policy,
						newModel: func(context.Context) (semanticModel, error) {
							return scorer, nil
						},
					},
				)
			})
			if scorer.scoreCalls.Load() != 1 || len(documents) != 2 {
				t.Fatalf(
					"model calls=%d documents=%d, want one call with two documents",
					scorer.scoreCalls.Load(),
					len(documents),
				)
			}
			if test.wantMatch {
				if err != nil || !strings.Contains(output, "alpha.go") ||
					!strings.Contains(output, "beta.go") {
					t.Fatalf("accepted re-rank output=%q error=%v", output, err)
				}
				return
			}
			if !errors.Is(err, errNoMatch) || output != "" {
				t.Fatalf("rejected re-rank output=%q error=%v, want no match", output, err)
			}
		})
	}
}

func TestRunRerankStrictEvidenceAndTruncationContracts(t *testing.T) {
	for _, test := range []struct {
		name          string
		truncated     bool
		wantExpansion bool
	}{
		{name: "strict evidence admits expansion", wantExpansion: true},
		{name: "truncated query stays strict only", truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireTools(t)
			setTestUserCache(t)
			t.Chdir(t.TempDir())
			strict := t.TempDir()
			relaxed := t.TempDir()
			writeFileAt(t, strict, "strict.go", "package sample\n// alpha beta\n")
			writeFileAt(t, relaxed, "relaxed.go", "package sample\n// alpha only\n")
			policy := newRerankAcceptancePolicy(0.575)
			policy.route = rerankAcceptanceRoute
			var documents []rerankDocument
			scorer := &fixedRerankScorer{
				documents:      &documents,
				queryTruncated: test.truncated,
			}

			output, err := captureStdout(t, func() error {
				return runSearchCommand(
					t.Context(),
					"alpha beta",
					[]string{strict, relaxed},
					0,
					0,
					defaultSearchConfig(),
					searchRunConfig{
						policy:           defaultSearchPolicy(),
						rerankAcceptance: policy,
						newModel: func(context.Context) (semanticModel, error) {
							return scorer, nil
						},
					},
				)
			})
			if err != nil || !strings.Contains(output, "strict.go") {
				t.Fatalf("strict result output=%q error=%v", output, err)
			}
			wantScoreCalls, wantDocuments := int32(1), 2
			if test.truncated {
				wantScoreCalls, wantDocuments = 0, 0
			}
			if scorer.scoreCalls.Load() != wantScoreCalls || len(documents) != wantDocuments {
				t.Fatalf(
					"model calls=%d documents=%d, want %d calls with %d documents",
					scorer.scoreCalls.Load(),
					len(documents),
					wantScoreCalls,
					wantDocuments,
				)
			}
			if got := strings.Contains(output, "relaxed.go"); got != test.wantExpansion {
				t.Fatalf("expanded=%t, want %t; output=%q", got, test.wantExpansion, output)
			}
		})
	}
}

func TestRunSearchResolvesAcceptanceOnlyForEligibleQuery(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// alpha beta\n")

	for _, test := range []struct {
		name      string
		pattern   string
		wantCalls int
		wantErr   bool
	}{
		{name: "invalid", pattern: "(", wantErr: true},
		{name: "ineligible", pattern: "alpha"},
		{name: "eligible", pattern: "alpha beta", wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			runConfig := searchRunConfig{
				policy: defaultSearchPolicy(),
				newAcceptancePolicies: func() rerankAcceptancePolicies {
					calls++
					return rerankAcceptancePolicies{
						rerank: alwaysAcceptRerankPolicyForTest(),
						hybrid: alwaysAcceptRerankPolicyForTest(),
					}
				},
			}
			_, err := captureStdout(t, func() error {
				return runSearchCommand(
					t.Context(),
					test.pattern,
					[]string{folder},
					0,
					0,
					defaultSearchConfig(),
					runConfig,
				)
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, want error=%t", err, test.wantErr)
			}
			if calls != test.wantCalls {
				t.Fatalf("acceptance factory calls=%d, want %d", calls, test.wantCalls)
			}
		})
	}
}

func TestRunSearchSkipsModelForGuaranteedTruncatedQuery(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// alpha\n")
	policyCalls := 0
	modelCalls := 0
	pattern := strings.TrimSpace(strings.Repeat(
		"alpha ",
		rerankQueryMaximumUntruncatedTerms+1,
	))

	output, err := captureStdout(t, func() error {
		return runSearchCommand(
			t.Context(),
			pattern,
			[]string{folder},
			0,
			0,
			defaultSearchConfig(),
			searchRunConfig{
				policy: defaultSearchPolicy(),
				newAcceptancePolicies: func() rerankAcceptancePolicies {
					policyCalls++
					return rerankAcceptancePolicies{
						rerank: alwaysAcceptRerankPolicyForTest(),
						hybrid: alwaysAcceptRerankPolicyForTest(),
					}
				},
				newModel: func(context.Context) (semanticModel, error) {
					modelCalls++
					return nil, errors.New("model must not start")
				},
			},
		)
	})
	if err != nil || !strings.Contains(output, "app.go") {
		t.Fatalf("strict output=%q error=%v", output, err)
	}
	if policyCalls != 0 || modelCalls != 0 {
		t.Fatalf("factory calls: policy=%d model=%d, want 0 and 0", policyCalls, modelCalls)
	}
}

func TestRerankCorpusResultsPromotesORCandidateAndAppendsStrictTail(t *testing.T) {
	relaxed := make([]corpusSearchResult, rerankCandidateLimit)
	scores := make([]float32, rerankCandidateLimit)
	for i := range relaxed {
		relaxed[i] = rerankTestResult("candidate-"+string(rune('a'+i))+".go", float64(100-i))
	}
	scores[0] = -1
	scores[1] = 1
	strict := []corpusSearchResult{
		relaxed[0],
		rerankTestResult("strict-only.go", 90),
	}
	scorer := &fixedRerankScorer{scores: scores}

	got, _, err := rerankCorpusResults(
		context.Background(),
		strict,
		nil,
		relaxed,
		nil,
		"alpha beta",
		scorer.Scores,
		alwaysAcceptRerankPolicyForTest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].file.FileName != relaxed[1].file.FileName {
		t.Fatalf("first=%q, want promoted OR rank 2 %q", got[0].file.FileName, relaxed[1].file.FileName)
	}
	if len(got) != rerankCandidateLimit+1 {
		t.Fatalf("result count=%d, want %d", len(got), rerankCandidateLimit+1)
	}
	if got[len(got)-1].file.FileName != "strict-only.go" {
		t.Fatalf("tail=%q, want strict-only.go", got[len(got)-1].file.FileName)
	}
	for i := range got {
		if got[i].rankOverride != i+1 {
			t.Fatalf("rank override at %d=%d", i, got[i].rankOverride)
		}
	}
	if strict[0].rankOverride != 0 || relaxed[0].rankOverride != 0 {
		t.Fatal("re-ranking mutated an input slice")
	}
	output := formatCorpusResultsWithContext(
		got,
		nil,
		1,
		0,
		hideCorpusContext,
		plainPalette,
	)
	if !strings.Contains(output, relaxed[1].file.FileName) ||
		strings.Contains(output, relaxed[0].file.FileName) {
		t.Fatalf("limit 1 did not show the promoted OR candidate:\n%s", output)
	}
}

func TestBuildRerankCandidatesIncludesStrictLeaderWithinLimit(t *testing.T) {
	relaxed := make([]corpusSearchResult, rerankCandidateLimit)
	for i := range relaxed {
		relaxed[i] = rerankTestResult(
			"relaxed-"+string(rune('a'+i))+".go",
			float64(rerankCandidateLimit-i),
		)
	}
	strictLeader := rerankTestResult("strict-leader.go", 100)
	strict := []corpusSearchResult{strictLeader, relaxed[0]}

	candidates := buildRerankCandidates(strict, relaxed)
	if len(candidates) != rerankCandidateLimit {
		t.Fatalf("candidate count=%d, want %d", len(candidates), rerankCandidateLimit)
	}
	if got := candidates[0].result.file.FileName; got != "strict-leader.go" {
		t.Fatalf("first candidate=%q, want strict leader", got)
	}
	if candidates[0].lexicalRank != 1 {
		t.Fatalf("strict leader lexical rank=%d, want 1", candidates[0].lexicalRank)
	}
	if got := candidates[1].result.file.FileName; got != relaxed[0].file.FileName {
		t.Fatalf("second candidate=%q, want %q", got, relaxed[0].file.FileName)
	}
	if candidates[1].lexicalRank != 1 {
		t.Fatalf("relaxed rank-one candidate rank=%d, want 1", candidates[1].lexicalRank)
	}
	for _, candidate := range candidates {
		if candidate.result.file.FileName == relaxed[len(relaxed)-1].file.FileName {
			t.Fatalf("relaxed rank %d must be outside the model limit", len(relaxed))
		}
	}
}

func TestBuildRerankCandidatesHandlesEmptyAndOverlappingStrictResults(t *testing.T) {
	relaxed := []corpusSearchResult{
		rerankTestResult("one.go", 3),
		rerankTestResult("two.go", 2),
		rerankTestResult("three.go", 1),
	}
	withoutStrict := buildRerankCandidates(nil, relaxed)
	if len(withoutStrict) != len(relaxed) {
		t.Fatalf("candidate count without strict results=%d, want %d", len(withoutStrict), len(relaxed))
	}
	for i, candidate := range withoutStrict {
		if candidate.result.file.FileName != relaxed[i].file.FileName ||
			candidate.lexicalRank != i+1 || candidate.preservedStrictLeader {
			t.Fatalf("candidate %d without strict results=%+v", i, candidate)
		}
	}

	strictLeader := rerankTestResult("two.go", 100)
	strictLeader.file.LineMatches[0].Before = []byte("strict context")
	relaxed[1].file.LineMatches[0].Before = []byte("richer relaxed context")
	overlap := buildRerankCandidates([]corpusSearchResult{strictLeader}, relaxed)
	if len(overlap) != len(relaxed) {
		t.Fatalf("overlap candidate count=%d, want %d", len(overlap), len(relaxed))
	}
	if overlap[0].result.file.FileName != "two.go" || overlap[0].lexicalRank != 1 ||
		overlap[0].preservedStrictLeader {
		t.Fatalf("overlapping strict leader=%+v", overlap[0])
	}
	if got := string(overlap[0].result.file.LineMatches[0].Before); got != "richer relaxed context" {
		t.Fatalf("overlapping strict leader context=%q", got)
	}
	if overlap[1].result.file.FileName != "one.go" ||
		overlap[2].result.file.FileName != "three.go" {
		t.Fatalf("relaxed order after overlap=%q, %q", overlap[1].result.file.FileName, overlap[2].result.file.FileName)
	}
}

func TestRerankCorpusResultsScoresStrictLeaderAndKeepsDisplacedRelaxedTail(t *testing.T) {
	relaxed := make([]corpusSearchResult, rerankCandidateLimit)
	for i := range relaxed {
		relaxed[i] = rerankTestResult(
			"relaxed-"+string(rune('a'+i))+".go",
			float64(rerankCandidateLimit-i),
		)
	}
	strictLeader := rerankTestResult("strict-leader.go", 100)
	var documents []rerankDocument

	got, _, err := rerankCorpusResults(
		context.Background(),
		[]corpusSearchResult{strictLeader},
		nil,
		relaxed,
		nil,
		"alpha beta",
		(&fixedRerankScorer{
			scores:    make([]float32, rerankCandidateLimit),
			documents: &documents,
		}).Scores,
		alwaysAcceptRerankPolicyForTest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != rerankCandidateLimit {
		t.Fatalf("scored documents=%d, want %d", len(documents), rerankCandidateLimit)
	}
	if documents[0].Path != strictLeader.file.FileName {
		t.Fatalf("first scored document=%q, want %q", documents[0].Path, strictLeader.file.FileName)
	}
	for _, document := range documents {
		if document.Path == relaxed[len(relaxed)-1].file.FileName {
			t.Fatalf("relaxed rank %d was scored over the strict leader", len(relaxed))
		}
	}
	if len(got) != rerankCandidateLimit+1 {
		t.Fatalf("result count=%d, want %d", len(got), rerankCandidateLimit+1)
	}
	if got[0].file.FileName != relaxed[0].file.FileName ||
		got[1].file.FileName != strictLeader.file.FileName {
		t.Fatalf(
			"leading results=%q, %q; want relaxed leader then strict leader",
			got[0].file.FileName,
			got[1].file.FileName,
		)
	}
	if got[len(got)-1].file.FileName != relaxed[len(relaxed)-1].file.FileName {
		t.Fatalf("unscored tail=%q, want %q", got[len(got)-1].file.FileName, relaxed[len(relaxed)-1].file.FileName)
	}
	for i, result := range got {
		if result.rankOverride != i+1 {
			t.Fatalf("rank override at %d=%d", i, result.rankOverride)
		}
	}
}

func TestRerankCorpusResultsRejectsInvalidScores(t *testing.T) {
	relaxed := []corpusSearchResult{
		rerankTestResult("a.go", 2),
		rerankTestResult("b.go", 1),
	}
	for _, tc := range []struct {
		name   string
		scores []float32
	}{
		{name: "wrong count", scores: []float32{1}},
		{name: "NaN", scores: []float32{float32(math.NaN()), 0}},
		{name: "infinity", scores: []float32{float32(math.Inf(1)), 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := rerankCorpusResults(
				context.Background(), nil, nil, relaxed, nil, "a b",
				(&fixedRerankScorer{scores: tc.scores}).Scores,
				alwaysAcceptRerankPolicyForTest(),
			)
			if err == nil {
				t.Fatal("invalid scores must fail")
			}
		})
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

func TestRerankCorpusResultsKeepsDirtyAndMultiCorpusIdentity(t *testing.T) {
	committed := rerankTestResult("same.go", 10)
	committed.kind = corpusKindGit
	committed.file.Repository = "repo"
	uncommitted := rerankTestResult("same.go", 9)
	uncommitted.kind = corpusKindGit
	uncommitted.file.Repository = repoUncommitted
	other := rerankTestResult("same.go", 8)
	other.corpusID = "other"
	other.file.Repository = "other"
	second := rerankTestResult("second.go", 7)
	relaxedDirty := dirtyFilesByCorpus{
		"repo":  testDirtyFileSet("same.go"),
		"other": testDirtyFileSet("other-dirty.go"),
	}
	strictDirty := dirtyFilesByCorpus{"repo": testDirtyFileSet("strict-dirty.go")}

	got, dirty, err := rerankCorpusResults(
		context.Background(),
		nil,
		strictDirty,
		[]corpusSearchResult{committed, uncommitted, other, second},
		relaxedDirty,
		"alpha beta",
		(&fixedRerankScorer{scores: []float32{3, 2, 1}}).Scores,
		alwaysAcceptRerankPolicyForTest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("result count=%d, want dirty dedup plus both corpora", len(got))
	}
	sameByCorpus := make(map[corpusID]string)
	for _, result := range got {
		if result.file.FileName == "same.go" {
			sameByCorpus[result.corpusID] = result.file.Repository
		}
	}
	if sameByCorpus["repo"] != repoUncommitted || sameByCorpus["other"] != "other" {
		t.Fatalf("same-path results by corpus=%v", sameByCorpus)
	}
	for corpus, name := range map[corpusID]string{
		"repo":  "strict-dirty.go",
		"other": "other-dirty.go",
	} {
		if !dirty[corpus].contains(name) {
			t.Fatalf("merged dirty set lacks %s/%s: %v", corpus, name, dirty)
		}
	}
}

func TestRankCorpusResultsForDisplayHonorsOverride(t *testing.T) {
	low := rerankTestResult("low.go", 1)
	low.rankOverride = 1
	high := rerankTestResult("high.go", 10)
	high.rankOverride = 2
	ranked := rankCorpusResultsForDisplay([]corpusSearchResult{high, low}, nil)
	if ranked[0].file.FileName != "low.go" {
		t.Fatalf("override order=%q then %q", ranked[0].file.FileName, ranked[1].file.FileName)
	}

	low.rankOverride = 0
	high.rankOverride = 0
	ranked = rankCorpusResultsForDisplay([]corpusSearchResult{low, high}, nil)
	if ranked[0].file.FileName != "high.go" {
		t.Fatalf("BM25 order=%q then %q", ranked[0].file.FileName, ranked[1].file.FileName)
	}
}

func TestRunRerankK01FallbackContracts(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package strict\n// alpha beta\n")
	writeFileAt(t, folder, "relaxed.go", "package relaxed\n// alpha\n")

	runQuery := func(pattern string, rerankConfig searchRunConfig) (string, error) {
		return captureStdout(t, func() error {
			return runSearchCommand(
				context.Background(),
				pattern,
				[]string{folder},
				0,
				0,
				defaultSearchConfig(),
				rerankConfig,
			)
		})
	}

	const expected = "## strict.go (Go)\n1 package strict\n2 // alpha beta"
	baseline, err := captureStdout(t, func() error {
		return runWithSearchConfig(
			context.Background(),
			"alpha beta",
			[]string{folder},
			defaultSearchConfig(),
		)
	})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if baseline != expected {
		t.Fatalf("baseline output changed:\nwant=%q\ngot=%q", expected, baseline)
	}

	factoryCalls := 0
	disabled, err := runQuery("alpha beta", searchRunConfig{
		newModel: func(context.Context) (semanticModel, error) {
			factoryCalls++
			return nil, errors.New("must not start")
		},
	})
	if err != nil || disabled != baseline || factoryCalls != 0 {
		t.Fatalf("disabled path: error=%v calls=%d\nwant=%q\ngot=%q", err, factoryCalls, baseline, disabled)
	}

	protectedBaseline, err := runQuery(`"alpha beta"`, searchRunConfig{})
	if err != nil {
		t.Fatalf("protected baseline: %v", err)
	}
	if protectedBaseline != expected {
		t.Fatalf("protected output changed:\nwant=%q\ngot=%q", expected, protectedBaseline)
	}
	protected, err := runQuery(`"alpha beta"`, searchRunConfig{
		policy: defaultSearchPolicy(),
		newModel: func(context.Context) (semanticModel, error) {
			factoryCalls++
			return nil, errors.New("must not start")
		},
	})
	wantIndexCalls := 1
	if err != nil || protected != protectedBaseline || factoryCalls != wantIndexCalls {
		t.Fatalf("protected path: error=%v calls=%d\nwant=%q\ngot=%q", err, factoryCalls, protectedBaseline, protected)
	}

	factoryCalls = 0
	backendFailure, err := runQuery("alpha beta", searchRunConfig{
		policy: defaultSearchPolicy(),
		newModel: func(context.Context) (semanticModel, error) {
			factoryCalls++
			return nil, errors.New("backend failure")
		},
	})
	if err != nil || backendFailure != baseline || factoryCalls != 1 {
		t.Fatalf("backend fallback: error=%v calls=%d\nwant=%q\ngot=%q", err, factoryCalls, baseline, backendFailure)
	}
}

func TestRunRerankClosesPreparedScorerWhenPoolIsTooSmall(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "only.go", "package sample\n// alpha beta\n")

	factoryCalls := 0
	closed := false
	output, err := captureStdout(t, func() error {
		return runSearchCommand(
			context.Background(),
			"alpha beta",
			[]string{folder},
			0,
			0,
			defaultSearchConfig(),
			searchRunConfig{
				policy: defaultSearchPolicy(),
				newModel: func(context.Context) (semanticModel, error) {
					factoryCalls++
					return &fixedRerankScorer{closed: &closed}, nil
				},
			},
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 1 || !closed {
		t.Fatalf("prepared scorer: calls=%d closed=%t", factoryCalls, closed)
	}
	if !strings.Contains(output, "only.go") {
		t.Fatalf("strict result was not kept:\n%s", output)
	}
}

func TestRunRerankDoesNotStartScorerWhenCorpusPlanningFails(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing")

	factoryCalls := 0
	closed := false
	err := runSearchCommand(
		context.Background(),
		"alpha beta",
		[]string{missing},
		0,
		0,
		defaultSearchConfig(),
		searchRunConfig{
			policy: defaultSearchPolicy(),
			newModel: func(context.Context) (semanticModel, error) {
				factoryCalls++
				return &fixedRerankScorer{closed: &closed}, nil
			},
		},
	)
	if err == nil {
		t.Fatal("missing corpus must fail")
	}
	if factoryCalls != 0 || closed {
		t.Fatalf("scorer started before planning: calls=%d closed=%t error=%v", factoryCalls, closed, err)
	}
}

func TestRunRerankUnsupportedQueriesStayByteIdentical(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\nfunc Alpha() {}\n// alpha beta gamma\n")
	writeFileAt(t, folder, "relaxed.go", "package sample\n// alpha only\n")

	for _, pattern := range []string{
		"alpha",
		`"alpha beta"`,
		"file:strict alpha",
		"content:alpha beta",
		"regex:alpha beta",
		"alpha or beta",
		"alpha -missing",
		"case:yes alpha beta",
	} {
		t.Run(pattern, func(t *testing.T) {
			baseline, baselineErr := captureStdout(t, func() error {
				return runWithSearchConfig(
					context.Background(), pattern, []string{folder}, defaultSearchConfig(),
				)
			})
			factoryCalls := 0
			got, gotErr := captureStdout(t, func() error {
				return runSearchCommand(
					context.Background(),
					pattern,
					[]string{folder},
					0,
					0,
					defaultSearchConfig(),
					searchRunConfig{
						policy: defaultSearchPolicy(),
						newModel: func(context.Context) (semanticModel, error) {
							factoryCalls++
							return nil, errors.New("must not start")
						},
					},
				)
			})
			wantIndexCalls := 1
			if baseline != got || factoryCalls != wantIndexCalls ||
				(baselineErr == nil) != (gotErr == nil) ||
				(baselineErr != nil && baselineErr.Error() != gotErr.Error()) {
				t.Fatalf(
					"protected query changed: calls=%d baseline=%q/%v got=%q/%v",
					factoryCalls,
					baseline,
					baselineErr,
					got,
					gotErr,
				)
			}
		})
	}
}

func TestRunRerankWithDirtyFilesAcrossCorpora(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	repoOne := initGitRepo(t, "same.go", "package sample\n// alpha beta committed\n")
	writeFileAt(t, repoOne, "same.go", "package sample\n// alpha dirty only\n")
	writeFileAt(t, repoOne, "untracked.go", "package sample\n// alpha beta untracked\n")
	repoTwo := initGitRepo(t, "same.go", "package sample\n// alpha beta second repository\n")

	var documents []rerankDocument
	output, err := captureStdout(t, func() error {
		return runSearchCommand(
			context.Background(),
			"alpha beta",
			[]string{repoOne, repoTwo},
			0,
			0,
			defaultSearchConfig(),
			searchRunConfig{
				policy:           defaultSearchPolicy(),
				rerankAcceptance: alwaysAcceptRerankPolicyForTest(),
				newModel: func(context.Context) (semanticModel, error) {
					return &fixedRerankScorer{documents: &documents}, nil
				},
			},
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		filepath.Join(repoOne, "same.go"),
		filepath.Join(repoOne, "untracked.go"),
		filepath.Join(repoTwo, "same.go"),
		"[uncommitted]",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("multi-corpus dirty output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "alpha beta committed") {
		t.Fatalf("output leaked the stale committed file:\n%s", output)
	}
	sameDocuments := 0
	dirtyDocument := false
	for _, document := range documents {
		if document.Path == "same.go" {
			sameDocuments++
		}
		if strings.Contains(document.Text, "alpha dirty only") {
			dirtyDocument = true
		}
	}
	if sameDocuments != 2 || !dirtyDocument {
		t.Fatalf("model documents did not keep corpus and dirty content: %+v", documents)
	}
}

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

	var documents []rerankDocument
	output, err := captureStdout(t, func() error {
		return runSearchCommand(
			context.Background(),
			"alpha beta",
			[]string{folder},
			0,
			0,
			explicitSearchConfig(0, false),
			searchRunConfig{
				policy:           defaultSearchPolicy(),
				rerankAcceptance: alwaysAcceptRerankPolicyForTest(),
				newModel: func(context.Context) (semanticModel, error) {
					return &fixedRerankScorer{documents: &documents}, nil
				},
			},
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "hidden before") || strings.Contains(output, "hidden after") {
		t.Fatalf("internal model context reached output:\n%s", output)
	}
	modelSawContext := false
	for _, document := range documents {
		if document.Path == "target.go" &&
			strings.Contains(document.Text, "hidden before three") &&
			strings.Contains(document.Text, "hidden after three") {
			modelSawContext = true
		}
	}
	if !modelSawContext {
		t.Fatalf("model did not receive fixed context: %+v", documents)
	}
}

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

	var documents []rerankDocument
	output, err := captureStdout(t, func() error {
		return runSearchCommand(
			context.Background(),
			"alpha beta",
			[]string{folder},
			0,
			0,
			explicitSearchConfig(0, false),
			searchRunConfig{
				policy: defaultSearchPolicy(),
				newModel: func(context.Context) (semanticModel, error) {
					return &fixedRerankScorer{
						err:       errors.New("score failure"),
						documents: &documents,
					}, nil
				},
			},
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "hidden before") || strings.Contains(output, "hidden after") {
		t.Fatalf("internal model context reached fallback output:\n%s", output)
	}
	if !strings.Contains(output, "alpha beta target") {
		t.Fatalf("fallback lost the strict match:\n%s", output)
	}
	modelSawContext := false
	for _, document := range documents {
		if document.Path == "target.go" &&
			strings.Contains(document.Text, "hidden before three") &&
			strings.Contains(document.Text, "hidden after three") {
			modelSawContext = true
		}
	}
	if !modelSawContext {
		t.Fatalf("failed scorer did not receive fixed context: %+v", documents)
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

func rerankTestResultWithModelContext(name string, score float64) corpusSearchResult {
	result := rerankTestResult(name, score)
	result.file.LineMatches[0].Before = []byte("model-only before\n")
	result.file.LineMatches[0].After = []byte("model-only after\n")
	return result
}

func assertRerankFallbackHidesModelContext(
	t *testing.T,
	got []corpusSearchResult,
	source []corpusSearchResult,
) {
	t.Helper()
	if len(got) != 1 || got[0].file.FileName != "strict.go" ||
		len(got[0].file.LineMatches) != 1 {
		t.Fatalf("fallback results=%#v, want strict.go", got)
	}
	match := got[0].file.LineMatches[0]
	if len(match.Before) != 0 || len(match.After) != 0 {
		t.Fatalf("fallback exposed model context: before=%q after=%q", match.Before, match.After)
	}
	sourceMatch := source[0].file.LineMatches[0]
	if string(sourceMatch.Before) != "model-only before\n" ||
		string(sourceMatch.After) != "model-only after\n" {
		t.Fatal("fallback context conversion mutated its input")
	}
}
