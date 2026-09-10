package main

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

type fixedRerankScorer struct {
	scores    []float32
	err       error
	closed    *bool
	documents *[]rerankDocument
}

func (s *fixedRerankScorer) Scores(
	_ context.Context,
	_ string,
	documents []rerankDocument,
) ([]float32, error) {
	if s.documents != nil {
		*s.documents = append([]rerankDocument(nil), documents...)
	}
	if s.scores == nil {
		return make([]float32, len(documents)), s.err
	}
	return append([]float32(nil), s.scores...), s.err
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
		func(context.Context) (rerankScorer, error) {
			return &fixedRerankScorer{}, nil
		},
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

func TestTryRerankCorporaScorerErrorClosesAndKeepsStrictResults(t *testing.T) {
	strict := []corpusSearchResult{rerankTestResultWithModelContext("strict.go", 10)}
	strictDirty := dirtyFilesByCorpus{"repo": testDirtyFileSet("dirty.go")}
	relaxed := []corpusSearchResult{
		rerankTestResult("alpha.go", 2),
		rerankTestResult("beta.go", 1),
	}
	wantErr := errors.New("score failure")
	closed := false

	got, gotDirty, err := tryRerankCorpora(
		context.Background(),
		strict,
		strictDirty,
		nil,
		nil,
		searchConfig{},
		rerankQueryPlan{modelQuery: "alpha beta", relaxedQ: &query.Or{}},
		func(context.Context) (rerankScorer, error) {
			return &fixedRerankScorer{err: wantErr, closed: &closed}, nil
		},
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
	if !errors.Is(err, wantErr) || !closed {
		t.Fatalf("scorer failure: error=%v closed=%t", err, closed)
	}
	if !reflect.DeepEqual(gotDirty, strictDirty) {
		t.Fatalf("scorer fallback changed strict results: results=%#v dirty=%#v", got, gotDirty)
	}
	assertRerankFallbackHidesModelContext(t, got, strict)
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
	closed := false
	newScorer := func(context.Context) (rerankScorer, error) {
		return &fixedRerankScorer{scores: scores, closed: &closed}, nil
	}

	got, _, err := rerankCorpusResults(
		context.Background(),
		strict,
		nil,
		relaxed,
		nil,
		"alpha beta",
		newScorer,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("scorer was not closed")
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
		func(context.Context) (rerankScorer, error) {
			return &fixedRerankScorer{
				scores:    make([]float32, rerankCandidateLimit),
				documents: &documents,
			}, nil
		},
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
				func(context.Context) (rerankScorer, error) {
					return &fixedRerankScorer{scores: tc.scores}, nil
				},
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
		func(context.Context) (rerankScorer, error) {
			return &fixedRerankScorer{scores: []float32{3, 2, 1}}, nil
		},
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

	runQuery := func(pattern string, rerankConfig rerankRunConfig) (string, error) {
		return captureStdout(t, func() error {
			return runWithRerankConfig(
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
	disabled, err := runQuery("alpha beta", rerankRunConfig{
		newScorer: func(context.Context) (rerankScorer, error) {
			factoryCalls++
			return nil, errors.New("must not start")
		},
	})
	if err != nil || disabled != baseline || factoryCalls != 0 {
		t.Fatalf("disabled path: error=%v calls=%d\nwant=%q\ngot=%q", err, factoryCalls, baseline, disabled)
	}

	protectedBaseline, err := runQuery(`"alpha beta"`, rerankRunConfig{})
	if err != nil {
		t.Fatalf("protected baseline: %v", err)
	}
	if protectedBaseline != expected {
		t.Fatalf("protected output changed:\nwant=%q\ngot=%q", expected, protectedBaseline)
	}
	protected, err := runQuery(`"alpha beta"`, rerankRunConfig{
		enabled: true,
		newScorer: func(context.Context) (rerankScorer, error) {
			factoryCalls++
			return nil, errors.New("must not start")
		},
	})
	if err != nil || protected != protectedBaseline || factoryCalls != 0 {
		t.Fatalf("protected path: error=%v calls=%d\nwant=%q\ngot=%q", err, factoryCalls, protectedBaseline, protected)
	}

	backendFailure, err := runQuery("alpha beta", rerankRunConfig{
		enabled: true,
		newScorer: func(context.Context) (rerankScorer, error) {
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
		return runWithRerankConfig(
			context.Background(),
			"alpha beta",
			[]string{folder},
			0,
			0,
			defaultSearchConfig(),
			rerankRunConfig{
				enabled: true,
				newScorer: func(context.Context) (rerankScorer, error) {
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

func TestRunRerankClosesPreparedScorerWhenCorpusPlanningFails(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing")

	factoryCalls := 0
	closed := false
	err := runWithRerankConfig(
		context.Background(),
		"alpha beta",
		[]string{missing},
		0,
		0,
		defaultSearchConfig(),
		rerankRunConfig{
			enabled: true,
			newScorer: func(context.Context) (rerankScorer, error) {
				factoryCalls++
				return &fixedRerankScorer{closed: &closed}, nil
			},
		},
	)
	if err == nil {
		t.Fatal("missing corpus must fail")
	}
	if factoryCalls != 1 || !closed {
		t.Fatalf("prepared scorer: calls=%d closed=%t error=%v", factoryCalls, closed, err)
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
				return runWithRerankConfig(
					context.Background(),
					pattern,
					[]string{folder},
					0,
					0,
					defaultSearchConfig(),
					rerankRunConfig{
						enabled: true,
						newScorer: func(context.Context) (rerankScorer, error) {
							factoryCalls++
							return nil, errors.New("must not start")
						},
					},
				)
			})
			if baseline != got || factoryCalls != 0 ||
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
		return runWithRerankConfig(
			context.Background(),
			"alpha beta",
			[]string{repoOne, repoTwo},
			0,
			0,
			defaultSearchConfig(),
			rerankRunConfig{
				enabled: true,
				newScorer: func(context.Context) (rerankScorer, error) {
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
		return runWithRerankConfig(
			context.Background(),
			"alpha beta",
			[]string{folder},
			0,
			0,
			explicitSearchConfig(0, false),
			rerankRunConfig{
				enabled: true,
				newScorer: func(context.Context) (rerankScorer, error) {
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
		return runWithRerankConfig(
			context.Background(),
			"alpha beta",
			[]string{folder},
			0,
			0,
			explicitSearchConfig(0, false),
			rerankRunConfig{
				enabled: true,
				newScorer: func(context.Context) (rerankScorer, error) {
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
