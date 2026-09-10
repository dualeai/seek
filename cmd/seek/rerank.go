package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

const (
	rerankCandidateLimit     = 20
	rerankRRFConstant        = 60.0
	rerankLexicalWeight      = 2.0
	rerankPathComponentLimit = 4
	// maxRerankDocumentBytes bounds work before the backend packs model tokens.
	maxRerankDocumentBytes = 16 << 10
)

var (
	errRerankUnavailable = errors.New("re-ranker unavailable")
	errRerankNotUseful   = errors.New("not enough candidates to re-rank")
)

type rerankQueryPlan struct {
	modelQuery string
	relaxedQ   query.Q
}

type rerankDocument struct {
	Path     string
	Language string
	Symbol   string
	Text     string
	// matchAt and matchEnd are byte offsets in Text.
	matchAt  int
	matchEnd int
}

type rerankScorer interface {
	Scores(ctx context.Context, query string, documents []rerankDocument) ([]float32, error)
	Close() error
}

type rerankScorerFactory func(ctx context.Context) (rerankScorer, error)

type rerankRunConfig struct {
	enabled   bool
	newScorer rerankScorerFactory
}

type rerankSearchFunc func(
	context.Context,
	[]corpusPlan,
	*gitPaths,
	query.Q,
	searchConfig,
) ([]corpusSearchResult, dirtyFilesByCorpus, error)

type rerankCandidate struct {
	result      corpusSearchResult
	lexicalRank int
	// preservedStrictLeader is true when the strict leader was absent from
	// the relaxed candidate set and was added as a separate candidate.
	preservedStrictLeader bool
}

// planRerankQuery accepts only a plain multi-word AND query. Any syntax node
// outside that form keeps the existing BM25 path.
func planRerankQuery(pattern string, raw query.Q) (rerankQueryPlan, bool) {
	and, ok := raw.(*query.And)
	if !ok || len(and.Children) < 2 {
		return rerankQueryPlan{}, false
	}
	originalTerms := strings.Fields(pattern)
	if len(originalTerms) != len(and.Children) {
		return rerankQueryPlan{}, false
	}

	terms := make([]string, 0, len(and.Children))
	children := make([]query.Q, 0, len(and.Children))
	for i, child := range and.Children {
		substring, ok := child.(*query.Substring)
		if !ok || substring.FileName || substring.Content ||
			substring.Pattern == "" ||
			strings.IndexFunc(substring.Pattern, unicode.IsSpace) >= 0 ||
			originalTerms[i] != substring.Pattern {
			return rerankQueryPlan{}, false
		}
		term := *substring
		term.FileName = false
		term.Content = true
		terms = append(terms, term.Pattern)
		children = append(children, &term)
	}

	return rerankQueryPlan{
		modelQuery: strings.Join(terms, " "),
		relaxedQ:   query.Simplify(query.NewOr(children...)),
	}, true
}

func rerankCandidateSearchConfig(config searchConfig) searchConfig {
	out := rerankModelSearchConfig(config)
	out.contextMatchLimit = 0
	out.contextFileLimit = rerankCandidateLimit
	out.resultFileLimit = rerankCandidateLimit
	return out
}

// rerankModelSearchConfig collects at least the standard symmetric context for
// model input. The original config must be applied again before display.
func rerankModelSearchConfig(config searchConfig) searchConfig {
	out := config
	opts := searchOpts
	if config.opts != nil {
		opts = *config.opts
	}
	if opts.NumContextLines < searchContextLines {
		opts.NumContextLines = searchContextLines
	}
	out.opts = &opts
	out.afterOnly = false
	return out
}

// tryRerankCorpora returns re-ranked results on success. On each failure, it
// returns normal all-term results with the user's display context restored.
func tryRerankCorpora(
	ctx context.Context,
	strictResults []corpusSearchResult,
	strictDirty dirtyFilesByCorpus,
	plans []corpusPlan,
	paths *gitPaths,
	config searchConfig,
	plan rerankQueryPlan,
	newScorer rerankScorerFactory,
	search rerankSearchFunc,
) ([]corpusSearchResult, dirtyFilesByCorpus, error) {
	if newScorer == nil {
		return applyRerankDisplayConfig(strictResults, config), strictDirty, errRerankUnavailable
	}
	relaxedResults, relaxedDirty, err := search(
		ctx,
		plans,
		paths,
		plan.relaxedQ,
		rerankCandidateSearchConfig(config),
	)
	if err != nil {
		return applyRerankDisplayConfig(strictResults, config), strictDirty,
			fmt.Errorf("collect re-rank candidates: %w", err)
	}

	reranked, mergedDirty, err := rerankCorpusResults(
		ctx,
		strictResults,
		strictDirty,
		relaxedResults,
		relaxedDirty,
		plan.modelQuery,
		newScorer,
	)
	if err != nil {
		return applyRerankDisplayConfig(strictResults, config), strictDirty, err
	}
	return applyRerankDisplayConfig(reranked, config), mergedDirty, nil
}

// rerankCorpusResults scores at most 20 files from the normal all-term leader
// and the relaxed any-term BM25 order. Weighted reciprocal rank fusion gives
// lexical rank twice the model-rank weight. A strict-only injected leader
// cannot take first place. Unscored relaxed and normal tails remain available.
func rerankCorpusResults(
	ctx context.Context,
	strictResults []corpusSearchResult,
	strictDirty dirtyFilesByCorpus,
	relaxedResults []corpusSearchResult,
	relaxedDirty dirtyFilesByCorpus,
	modelQuery string,
	newScorer rerankScorerFactory,
) ([]corpusSearchResult, dirtyFilesByCorpus, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	strictRanked := rankCorpusResultsBM25(strictResults, strictDirty)
	relaxedRanked := rankCorpusResultsBM25(relaxedResults, relaxedDirty)
	if len(relaxedRanked) > rerankCandidateLimit {
		relaxedRanked = relaxedRanked[:rerankCandidateLimit]
	}
	candidates := buildRerankCandidates(strictRanked, relaxedRanked)
	if len(candidates) < 2 {
		return nil, nil, errRerankNotUseful
	}
	if newScorer == nil {
		return nil, nil, errRerankUnavailable
	}

	documents := make([]rerankDocument, len(candidates))
	for i := range candidates {
		documents[i] = newRerankDocument(candidates[i].result)
	}

	scorer, err := newScorer(ctx)
	if err != nil {
		return nil, nil, err
	}
	scores, scoreErr := scorer.Scores(ctx, modelQuery, documents)
	closeErr := scorer.Close()
	if scoreErr != nil {
		return nil, nil, scoreErr
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	if len(scores) != len(candidates) {
		return nil, nil, fmt.Errorf(
			"invalid semantic score count: got %d, want %d",
			len(scores),
			len(candidates),
		)
	}
	for _, score := range scores {
		if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
			return nil, nil, fmt.Errorf("invalid semantic score")
		}
	}

	semanticOrder := make([]int, len(scores))
	for i := range semanticOrder {
		semanticOrder[i] = i
	}
	sort.SliceStable(semanticOrder, func(i, j int) bool {
		return scores[semanticOrder[i]] > scores[semanticOrder[j]]
	})
	semanticRanks := make([]int, len(scores))
	for rank, index := range semanticOrder {
		semanticRanks[index] = rank + 1
	}
	type fusedCandidate struct {
		result                corpusSearchResult
		score                 float64
		preservedStrictLeader bool
	}
	fused := make([]fusedCandidate, len(candidates))
	for i, candidate := range candidates {
		semanticRank := semanticRanks[i]
		fused[i] = fusedCandidate{
			result:                candidate.result,
			preservedStrictLeader: candidate.preservedStrictLeader,
			score: rerankLexicalWeight/(rerankRRFConstant+float64(candidate.lexicalRank)) +
				1/(rerankRRFConstant+float64(semanticRank)),
		}
	}
	sort.SliceStable(fused, func(i, j int) bool {
		return fused[i].score > fused[j].score
	})
	// A strict-only leader adds all-term lexical evidence, but one such file must
	// not replace the best result selected from both relaxed and semantic
	// evidence. It can still rank second or rise naturally when it was already
	// part of the relaxed candidate set.
	if fused[0].preservedStrictLeader {
		fused[0], fused[1] = fused[1], fused[0]
	}

	out := make([]corpusSearchResult, 0, len(fused)+len(strictRanked))
	seen := make(map[corpusResultKey]struct{}, len(fused)+len(strictRanked))
	for _, candidate := range fused {
		result := candidate.result
		result.rankOverride = len(out) + 1
		out = append(out, result)
		seen[corpusResultKey{corpusID: result.corpusID, fileName: result.file.FileName}] = struct{}{}
	}
	// A strict-only leader can displace the last relaxed candidate from a full
	// model set. Keep any such file in the final result set as an unscored tail.
	for _, result := range relaxedRanked {
		key := corpusResultKey{corpusID: result.corpusID, fileName: result.file.FileName}
		if _, ok := seen[key]; ok {
			continue
		}
		result.rankOverride = len(out) + 1
		out = append(out, result)
		seen[key] = struct{}{}
	}
	for _, result := range strictRanked {
		key := corpusResultKey{corpusID: result.corpusID, fileName: result.file.FileName}
		if _, ok := seen[key]; ok {
			continue
		}
		result.rankOverride = len(out) + 1
		out = append(out, result)
		seen[key] = struct{}{}
	}

	return out, mergeDirtyFilesByCorpus(strictDirty, relaxedDirty), nil
}

// buildRerankCandidates adds strict BM25 rank one when present, then fills the
// fixed model set in relaxed BM25 order with stable file deduplication.
func buildRerankCandidates(
	strictRanked []corpusSearchResult,
	relaxedRanked []corpusSearchResult,
) []rerankCandidate {
	candidates := make([]rerankCandidate, 0, rerankCandidateLimit)
	seen := make(map[corpusResultKey]struct{}, rerankCandidateLimit)
	add := func(result corpusSearchResult, lexicalRank int, preservedStrictLeader bool) {
		if len(candidates) >= rerankCandidateLimit {
			return
		}
		key := corpusResultKey{corpusID: result.corpusID, fileName: result.file.FileName}
		if _, ok := seen[key]; ok {
			return
		}
		candidates = append(candidates, rerankCandidate{
			result:                result,
			lexicalRank:           lexicalRank,
			preservedStrictLeader: preservedStrictLeader,
		})
		seen[key] = struct{}{}
	}

	if len(strictRanked) > 0 {
		strictLeader := strictRanked[0]
		preservedStrictLeader := true
		strictLeaderKey := corpusResultKey{
			corpusID: strictLeader.corpusID,
			fileName: strictLeader.file.FileName,
		}
		// The relaxed result keeps context for every candidate match. Use it when
		// it represents the same strict leader.
		for _, result := range relaxedRanked {
			key := corpusResultKey{corpusID: result.corpusID, fileName: result.file.FileName}
			if key == strictLeaderKey {
				strictLeader = result
				preservedStrictLeader = false
				break
			}
		}
		add(strictLeader, 1, preservedStrictLeader)
	}
	for i, result := range relaxedRanked {
		add(result, i+1, false)
	}
	return candidates
}

// newRerankDocument selects the highest-scoring match, with its line number as
// the tie-breaker. It includes at most three context lines on each side. The
// matchAt and matchEnd fields identify the matched bytes in the selected text.
func newRerankDocument(result corpusSearchResult) rerankDocument {
	document := rerankDocument{
		Path:     result.file.FileName,
		Language: result.file.Language,
	}
	if len(result.file.LineMatches) == 0 {
		return document
	}

	best := &result.file.LineMatches[0]
	for i := 1; i < len(result.file.LineMatches); i++ {
		candidate := &result.file.LineMatches[i]
		if candidate.Score > best.Score ||
			(candidate.Score == best.Score && candidate.LineNumber < best.LineNumber) {
			best = candidate
		}
	}

	before := strings.ToValidUTF8(
		string(trimRerankContext(best.Before, searchContextLines, true)),
		"\uFFFD",
	)
	lineMatchAt, lineMatchEnd := rerankLineMatchSpan(best.Line, best.LineFragments)
	lineBefore := strings.ToValidUTF8(string(best.Line[:lineMatchAt]), "\uFFFD")
	lineMatch := strings.ToValidUTF8(string(best.Line[lineMatchAt:lineMatchEnd]), "\uFFFD")
	lineAfter := strings.ToValidUTF8(string(best.Line[lineMatchEnd:]), "\uFFFD")
	after := strings.ToValidUTF8(
		string(trimRerankContext(best.After, searchContextLines, false)),
		"\uFFFD",
	)
	document.Text = before + lineBefore + lineMatch + lineAfter + after
	document.matchAt = len(before) + len(lineBefore)
	document.matchEnd = document.matchAt + len(lineMatch)

	for _, fragment := range best.LineFragments {
		if fragment.SymbolInfo == nil {
			continue
		}
		symbol := fragment.SymbolInfo
		document.Symbol = symbol.Sym
		if symbol.Kind != "" {
			document.Symbol = symbol.Kind + " " + document.Symbol
		}
		if symbol.Parent != "" {
			document.Symbol = symbol.Parent + " " + document.Symbol
		}
		break
	}
	return document
}

func rerankLineMatchSpan(
	line []byte,
	fragments []zoekt.LineFragmentMatch,
) (int, int) {
	start, end := len(line), -1
	for _, fragment := range fragments {
		fragmentStart := fragment.LineOffset
		if fragmentStart < 0 || fragmentStart >= len(line) || fragment.MatchLength <= 0 {
			continue
		}
		fragmentEnd := fragmentStart + fragment.MatchLength
		if fragmentEnd < fragmentStart || fragmentEnd > len(line) {
			fragmentEnd = len(line)
		}
		start = min(start, fragmentStart)
		end = max(end, fragmentEnd)
	}
	if end <= start {
		return 0, len(line)
	}
	return start, end
}

// serializeLateOnDocumentWithMatch returns valid UTF-8 text and three byte
// offsets in that text: the metadata end, match start, and match end.
func serializeLateOnDocumentWithMatch(
	document rerankDocument,
) (string, int, int, int) {
	var metadata strings.Builder
	if document.Path != "" {
		fmt.Fprintf(&metadata, "path: %s\n", rerankPathTail(document.Path))
	}
	if document.Language != "" {
		fmt.Fprintf(&metadata, "language: %s\n", document.Language)
	}
	if document.Symbol != "" {
		fmt.Fprintf(&metadata, "symbol: %s\n", document.Symbol)
	}
	header := strings.ToValidUTF8(metadata.String(), "\uFFFD")
	if len(header) > maxRerankDocumentBytes/4 {
		header = rerankUTF8Prefix(header, maxRerankDocumentBytes/4)
	}
	text, matchAt, matchEnd := rerankValidTextSpan(
		document.Text,
		document.matchAt,
		document.matchEnd,
	)
	bodyStart, bodyEnd := rerankTextWindowBounds(
		text,
		matchAt,
		matchEnd,
		maxRerankDocumentBytes-len(header),
	)
	body := text[bodyStart:bodyEnd]
	keptMatchAt := max(matchAt, bodyStart)
	keptMatchEnd := min(matchEnd, bodyEnd)
	if keptMatchEnd <= keptMatchAt {
		keptMatchAt, keptMatchEnd = bodyStart, bodyEnd
	}
	return header + body,
		len(header),
		len(header) + keptMatchAt - bodyStart,
		len(header) + keptMatchEnd - bodyStart
}

// rerankPathTail limits path metadata to the final path components.
func rerankPathTail(value string) string {
	parts := strings.Split(value, "/")
	if len(parts) <= rerankPathComponentLimit {
		return value
	}
	return strings.Join(parts[len(parts)-rerankPathComponentLimit:], "/")
}

func rerankValidTextSpan(text string, matchAt, matchEnd int) (string, int, int) {
	if utf8.ValidString(text) {
		return text, matchAt, matchEnd
	}
	text = strings.ToValidUTF8(text, "\uFFFD")
	return text, 0, len(text)
}

func rerankTextWindowBounds(text string, matchAt, matchEnd, limit int) (int, int) {
	if limit <= 0 || text == "" {
		return 0, 0
	}
	if len(text) <= limit {
		return 0, len(text)
	}
	if matchAt < 0 || matchEnd <= matchAt || matchEnd > len(text) {
		return 0, backupToRuneBoundary([]byte(text), limit)
	}
	matchAt = backupToRuneBoundary([]byte(text), matchAt)
	matchEnd = backupToRuneBoundary([]byte(text), matchEnd)
	if matchEnd <= matchAt {
		return 0, backupToRuneBoundary([]byte(text), limit)
	}
	if matchEnd-matchAt >= limit {
		return matchAt, matchAt + len(rerankUTF8Prefix(text[matchAt:matchEnd], limit))
	}

	remaining := limit - (matchEnd - matchAt)
	before := min(matchAt, remaining/2)
	after := min(len(text)-matchEnd, remaining-before)
	remaining -= before + after
	before += remaining
	start := matchAt - before
	for start < matchAt && !utf8.RuneStart(text[start]) {
		start++
	}
	end := backupToRuneBoundary([]byte(text), matchEnd+after)
	return start, end
}

func rerankUTF8Prefix(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := backupToRuneBoundary([]byte(text), limit)
	return text[:end]
}

// applyRerankDisplayConfig copies line matches and removes model-only context
// that the user did not request.
func applyRerankDisplayConfig(
	results []corpusSearchResult,
	config searchConfig,
) []corpusSearchResult {
	contextLines := 0
	if config.opts != nil {
		contextLines = config.opts.NumContextLines
	}
	out := make([]corpusSearchResult, len(results))
	for i, result := range results {
		out[i] = result
		if len(result.file.LineMatches) == 0 {
			continue
		}
		file := result.file
		file.LineMatches = make([]zoekt.LineMatch, len(result.file.LineMatches))
		contextMatches := displayedContextLines(
			result.file.LineMatches,
			config.contextMatchLimit,
		)
		for j, source := range result.file.LineMatches {
			match := source
			_, keepContext := contextMatches[source.LineNumber]
			if contextLines == 0 ||
				(contextMatches != nil && !keepContext) {
				match.Before = nil
				match.After = nil
			} else {
				if config.afterOnly {
					match.Before = nil
				} else {
					match.Before = trimRerankContext(
						source.Before,
						contextLines,
						true,
					)
				}
				match.After = trimRerankContext(
					source.After,
					contextLines,
					false,
				)
			}
			file.LineMatches[j] = match
		}
		out[i].file = file
	}
	return out
}

func trimRerankContext(data []byte, limit int, before bool) []byte {
	lines := splitContextBytes(data)
	if limit <= 0 || len(lines) == 0 {
		return nil
	}
	if len(lines) > limit {
		if before {
			lines = lines[len(lines)-limit:]
		} else {
			lines = lines[:limit]
		}
	}
	out := bytes.Join(lines, []byte("\n"))
	if bytes.HasSuffix(data, []byte("\n")) {
		out = append(out, '\n')
	}
	return out
}

func mergeDirtyFilesByCorpus(left, right dirtyFilesByCorpus) dirtyFilesByCorpus {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}
	merged := make(dirtyFilesByCorpus, len(left)+len(right))
	for corpus, files := range left {
		copied := make(dirtyFileSet, len(files))
		for name := range files {
			copied[name] = struct{}{}
		}
		merged[corpus] = copied
	}
	for corpus, files := range right {
		current := merged[corpus]
		if current == nil {
			current = make(dirtyFileSet, len(files))
			merged[corpus] = current
		}
		for name := range files {
			current[name] = struct{}{}
		}
	}
	return merged
}
