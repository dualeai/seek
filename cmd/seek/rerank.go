package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp/syntax"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

const (
	// The order of accepted files fuses the lexical and the model rank with
	// weighted reciprocal-rank fusion: the lexical rank weighs twice the model
	// rank.
	rerankRRFConstant        = 60.0
	rerankLexicalWeight      = 2.0
	rerankPathComponentLimit = 4
	// maxRerankDocumentBytes bounds work before the backend packs model tokens.
	maxRerankDocumentBytes = 16 << 10
)

var errRerankUnavailable = errors.New("re-ranker unavailable")

// rerankQueryPlan is the model part of a query plan: the text that the model
// reads, the relaxed query that finds candidate files, and the filters.
type rerankQueryPlan struct {
	modelQuery string
	relaxedQ   query.Q
	// filters bound the vectors: the filter part of each filter and of each
	// or that holds descriptions (rerankMask). Zoekt evaluates them on each
	// node for the semantic row mask (nodeRowSelection).
	filters []query.Q
}

type rerankDocument struct {
	Path     string
	Language string
	Symbol   string
	Text     string
	// matchAt and matchEnd are byte offsets in Text.
	matchAt  int
	matchEnd int
	// modelBase is the model path base of the file's node (modelPathBase):
	// the model reads the path that the window header of its rows names.
	modelBase string
}

// searchRunConfig is the search policy and the model factory of one command.
type searchRunConfig struct {
	policy   searchPolicy
	newModel semanticModelFactory
}

// planRerankQuery plans the model routes from Zoekt's parsed tree, after
// Zoekt's own simplification, so a route never depends on how the query was
// spelled. A query of files that gives the model text uses the model: one
// word, an identifier and a phrase too. Description atoms feed the model;
// filters stay with Zoekt; modifiers change no route (querySupport in
// query_support_table_test.go). An or that holds descriptions gives the model
// every description atom in it, in query order, and relaxed retrieval any one
// of them; the strict query keeps the or, and its filters bound the vectors.
// A query of filters only gives the model the text of its positive patterns
// (rerankFilterText): sym:Foo reads Foo. It returns false for a query with no
// text for the model: one that matches nothing, one of a result type other
// than files (type:repo), and one whose filters have no positive pattern
// (negations, punctuation, repo: or branch: only).
func planRerankQuery(raw query.Q) (rerankQueryPlan, bool) {
	// type:file is an output mode at every depth: Zoekt matches a type:file
	// node as its child, so the plan reads the child.
	tree := query.Simplify(query.Map(raw, func(node query.Q) query.Q {
		if typed, ok := node.(*query.Type); ok && typed.Type == query.TypeFileName {
			return typed.Child
		}
		return node
	}))
	children := []query.Q{tree}
	if and, ok := tree.(*query.And); ok {
		children = and.Children
	}
	terms := make([]string, 0, len(children))
	descriptions := make([]query.Q, 0, len(children))
	filters := make([]query.Q, 0, len(children))
	relaxedFilters := make([]query.Q, 0, len(children))
	var filterText []string
	var punctuation []query.Q
	for _, child := range children {
		if term, text, ok := rerankDescriptionAtom(child); ok {
			terms = append(terms, text)
			descriptions = append(descriptions, term)
			continue
		}
		if rerankPunctuationAtom(child) {
			// A token such as "." or "^" gives the model nothing to read and
			// changes no route: Zoekt keeps it in both lexical queries.
			punctuation = append(punctuation, child)
			continue
		}
		if rerankFilter(child) {
			// Relaxed retrieval keeps the filter as Zoekt reads it, as it
			// keeps a punctuation token.
			filterText = rerankFilterText(child, filterText)
			relaxedFilters = append(relaxedFilters, child)
			if mask := rerankMask(child); mask != nil {
				filters = append(filters, mask)
			}
			continue
		}
		// An or that holds descriptions is not a filter: its atoms feed the
		// model and relaxed retrieval, and the strict query keeps the or. Its
		// filters still bound the vectors.
		for _, atom := range rerankDescriptionAtoms(child, nil) {
			term, text, _ := rerankDescriptionAtom(atom)
			terms = append(terms, text)
			descriptions = append(descriptions, term)
		}
		if mask := rerankMask(child); mask != nil {
			filters = append(filters, mask)
			relaxedFilters = append(relaxedFilters, mask)
		}
	}
	if len(terms) == 0 {
		terms = filterText
	}
	if len(terms) == 0 {
		return rerankQueryPlan{}, false
	}
	relaxedChildren := make([]query.Q, 0, len(relaxedFilters)+len(punctuation)+1)
	if len(descriptions) > 0 {
		relaxedChildren = append(relaxedChildren, query.NewOr(descriptions...))
	}
	relaxedChildren = append(relaxedChildren, relaxedFilters...)
	relaxedChildren = append(relaxedChildren, punctuation...)
	plan := rerankQueryPlan{
		modelQuery: strings.Join(terms, " "),
		relaxedQ:   query.Simplify(query.NewAnd(relaxedChildren...)),
		filters:    filters,
	}
	return plan, true
}

// rerankDescriptionAtoms appends the description atoms of an and or an or, in
// query order. A negation holds none: its atoms filter.
func rerankDescriptionAtoms(node query.Q, atoms []query.Q) []query.Q {
	if _, _, ok := rerankDescriptionAtom(node); ok {
		return append(atoms, node)
	}
	switch typed := node.(type) {
	case *query.And:
		for _, child := range typed.Children {
			atoms = rerankDescriptionAtoms(child, atoms)
		}
	case *query.Or:
		for _, child := range typed.Children {
			atoms = rerankDescriptionAtoms(child, atoms)
		}
	}
	return atoms
}

// rerankMask returns the part of node that bounds the vectors, or nil when it
// bounds nothing: node with each description atom and each punctuation atom
// outside a negation read as true (rerankDescriptionsAsTrue).
func rerankMask(node query.Q) query.Q {
	mask := query.Simplify(rerankDescriptionsAsTrue(node))
	if always, ok := mask.(*query.Const); ok && always.Value {
		return nil
	}
	return mask
}

// rerankDescriptionsAsTrue returns node with each description atom outside a
// negation replaced by true. A punctuation atom filters nothing, as at the top
// level, so it reads as true too. An atom inside a negation is a filter and
// stays.
func rerankDescriptionsAsTrue(node query.Q) query.Q {
	if _, _, ok := rerankDescriptionAtom(node); ok || rerankPunctuationAtom(node) {
		return &query.Const{Value: true}
	}
	switch typed := node.(type) {
	case *query.And:
		children := make([]query.Q, len(typed.Children))
		for index, child := range typed.Children {
			children[index] = rerankDescriptionsAsTrue(child)
		}
		return query.NewAnd(children...)
	case *query.Or:
		children := make([]query.Q, len(typed.Children))
		for index, child := range typed.Children {
			children[index] = rerankDescriptionsAsTrue(child)
		}
		return query.NewOr(children...)
	}
	return node
}

// rerankFilterText appends the model text of the positive patterns of a
// filter: the pattern of a file, content or symbol atom, and the name of a
// language. A negation, a repository or branch atom, a result type and the
// API-only nodes add none.
func rerankFilterText(node query.Q, text []string) []string {
	switch typed := node.(type) {
	case *query.Substring:
		if hasWordCharacter(typed.Pattern) {
			text = append(text, typed.Pattern)
		}
	case *query.Regexp:
		if typed.Regexp != nil && regexpHasLiteralWord(typed.Regexp) {
			text = append(text, regexpModelText(typed.Regexp))
		}
	case *query.Symbol:
		text = rerankFilterText(typed.Expr, text)
	case *query.Language:
		text = append(text, typed.Language)
	case *query.And:
		for _, child := range typed.Children {
			text = rerankFilterText(child, text)
		}
	case *query.Or:
		for _, child := range typed.Children {
			text = rerankFilterText(child, text)
		}
	}
	return text
}

// rerankDescriptionAtom returns a description atom as the content-only node
// of relaxed retrieval, and the text that the model reads: any word, phrase,
// or regular expression with a literal letter or digit that is not a file or
// content filter.
func rerankDescriptionAtom(node query.Q) (query.Q, string, bool) {
	switch typed := node.(type) {
	case *query.Substring:
		if typed == nil || typed.FileName || typed.Content || !hasWordCharacter(typed.Pattern) {
			return nil, "", false
		}
		term := *typed
		term.Content = true
		return &term, typed.Pattern, true
	case *query.Regexp:
		if typed == nil || typed.FileName || typed.Content || typed.Regexp == nil {
			return nil, "", false
		}
		text := regexpModelText(typed.Regexp)
		if !regexpHasLiteralWord(typed.Regexp) {
			// A token such as "^", "\d+" or "[^a-z]" gives the model nothing to
			// read: its letters, if any, are class names, not text.
			return nil, "", false
		}
		term := *typed
		term.Content = true
		return &term, text, true
	default:
		return nil, "", false
	}
}

// rerankPunctuationAtom reports a word or regular expression with no letter
// and no digit outside a file or content filter.
func rerankPunctuationAtom(node query.Q) bool {
	switch typed := node.(type) {
	case *query.Substring:
		return typed != nil && !typed.FileName && !typed.Content && !hasWordCharacter(typed.Pattern)
	case *query.Regexp:
		return typed != nil && !typed.FileName && !typed.Content && typed.Regexp != nil &&
			!regexpHasLiteralWord(typed.Regexp)
	default:
		return false
	}
}

// regexpHasLiteralWord reports a regular expression with a literal letter or
// digit: escapes and character classes do not count.
func regexpHasLiteralWord(re *syntax.Regexp) bool {
	if re.Op == syntax.OpLiteral {
		return hasWordCharacter(string(re.Rune))
	}
	for _, sub := range re.Sub {
		if regexpHasLiteralWord(sub) {
			return true
		}
	}
	return false
}

// hasWordCharacter reports whether text holds a letter or a digit.
func hasWordCharacter(text string) bool {
	return strings.IndexFunc(text, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}) >= 0
}

// regexpModelText renders a parsed regular expression close to how it was
// typed, for the model to read: Go's printer adds flag groups such as
// (?-s:...) that no user wrote. Zoekt removes capture groups, so the
// renderer puts back the parentheses that the tree needs: an empty group in a
// sequence (Foo() or a::b()), an alternation in a sequence, and a repeat of
// more than one character.
func regexpModelText(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		return string(re.Rune)
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return "."
	case syntax.OpBeginLine, syntax.OpBeginText:
		return "^"
	case syntax.OpEndLine, syntax.OpEndText:
		return "$"
	case syntax.OpEmptyMatch:
		return ""
	case syntax.OpCapture:
		return "(" + regexpModelText(re.Sub[0]) + ")"
	case syntax.OpStar:
		return regexpModelRepeated(re.Sub[0]) + "*"
	case syntax.OpPlus:
		return regexpModelRepeated(re.Sub[0]) + "+"
	case syntax.OpQuest:
		return regexpModelRepeated(re.Sub[0]) + "?"
	case syntax.OpConcat:
		parts := make([]string, len(re.Sub))
		for i, sub := range re.Sub {
			switch sub.Op {
			case syntax.OpEmptyMatch:
				parts[i] = "()"
			case syntax.OpAlternate:
				parts[i] = "(" + regexpModelText(sub) + ")"
			default:
				parts[i] = regexpModelText(sub)
			}
		}
		return strings.Join(parts, "")
	case syntax.OpAlternate:
		parts := make([]string, len(re.Sub))
		for i, sub := range re.Sub {
			parts[i] = regexpModelText(sub)
		}
		return strings.Join(parts, "|")
	default:
		return re.String()
	}
}

// regexpModelRepeated renders the operand of a repeat, in parentheses when it
// is more than one character.
func regexpModelRepeated(sub *syntax.Regexp) string {
	text := regexpModelText(sub)
	switch {
	case sub.Op == syntax.OpConcat, sub.Op == syntax.OpAlternate,
		sub.Op == syntax.OpLiteral && len(sub.Rune) > 1:
		return "(" + text + ")"
	}
	return text
}

// rerankFilter reports a node that Zoekt applies as a filter of the
// description atoms: a file, content, language, or symbol atom, a negation, a
// repository or branch atom (which Seek passes to Zoekt unchanged), or an and
// or an or of filters. An and or an or that holds a description is not a
// filter.
func rerankFilter(node query.Q) bool {
	switch typed := node.(type) {
	case *query.Substring, *query.Regexp:
		_, _, description := rerankDescriptionAtom(node)
		return !description
	case *query.Or:
		for _, child := range typed.Children {
			if !rerankFilter(child) {
				return false
			}
		}
		return true
	case *query.And:
		for _, child := range typed.Children {
			if !rerankFilter(child) {
				return false
			}
		}
		return true
	default:
		// Not, Language, Symbol, Repo, Branch, RawConfig, Meta, Type, and the
		// API-only nodes keep their Zoekt meaning.
		return true
	}
}

// rerankModelSearchConfig collects at least the standard symmetric context for
// model input. The original config must be applied again before display.
func rerankModelSearchConfig(config searchConfig) searchConfig {
	out := config
	opts := *config.opts
	if opts.NumContextLines < searchContextLines {
		opts.NumContextLines = searchContextLines
	}
	out.opts = &opts
	out.afterOnly = false
	// The display limits apply after the rank: a node's Zoekt top -n is not the
	// final top -n, so the model reads every strict file with its context.
	out.contextFileLimit = 0
	out.contextMatchLimit = 0
	return out
}

// newRerankDocument selects the highest-scoring match, with its line number as
// the tie-breaker. It includes at most three context lines on each side. The
// matchAt and matchEnd fields identify the matched bytes in the selected text.
func newRerankDocument(result corpusSearchResult) rerankDocument {
	document := rerankDocument{
		Path:      result.file.FileName,
		Language:  result.file.Language,
		modelBase: result.modelBase,
	}
	if len(result.file.LineMatches) == 0 {
		return document
	}

	best := &result.file.LineMatches[bestLineMatch(result.file.LineMatches)]

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

// bestLineMatch is the index of the highest-scoring match, with the lowest
// line number on a tie, or -1 with no match.
func bestLineMatch(matches []zoekt.LineMatch) int {
	best := -1
	for index, match := range matches {
		if best < 0 || match.Score > matches[best].Score ||
			(match.Score == matches[best].Score && match.LineNumber < matches[best].LineNumber) {
			best = index
		}
	}
	return best
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
		fmt.Fprintf(&metadata, "path: %s\n", rerankPathTail(semanticModelPath(document.modelBase, document.Path)))
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
	at := len(value)
	for range rerankPathComponentLimit {
		if at = strings.LastIndexByte(value[:at], '/'); at < 0 {
			return value
		}
	}
	return value[at+1:]
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
	contextLines := config.opts.NumContextLines
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
