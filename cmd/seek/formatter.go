package main

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"unicode"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// Long-line caps. A match line longer than maxLineBytes is windowed around
// its matches (keeping matchCtxBytes of source on each side); a context line
// longer than maxLineBytes is tail-trimmed. Both bounds are SOURCE bytes —
// ANSI color escapes and the "…+N bytes" markers do not count toward them.
//
// TODO: expose as a config flag if someone asks. Kept as consts to honour the
// "zero new flags" contract.
const (
	maxLineBytes  = 1024
	matchCtxBytes = 512
)

// palette holds the ANSI codes used to colorize output. The zero value
// (plainPalette) emits no codes. Color is presentation-only and chosen once
// per run from useColor; tests pass plainPalette so golden strings stay
// stable.
type palette struct {
	file, lineNo, match, reset string
}

var (
	plainPalette = palette{}
	ansiPalette  = palette{
		file:   "\x1b[36m",   // cyan
		lineNo: "\x1b[2m",    // dim
		match:  "\x1b[1;31m", // bold red
		reset:  "\x1b[0m",
	}
)

type dirtyFileSet map[string]struct{}

func (s dirtyFileSet) contains(name string) bool {
	_, ok := s[name]
	return ok
}

type dirtyFilesByCorpus map[corpusID]dirtyFileSet

type corpusDisplayMode uint8

const (
	hideCorpusContext corpusDisplayMode = iota
	showCorpusContext
)

// corpusFormatWriter drops each write's error: bufio.Writer keeps the first
// one, and Flush returns it.
type corpusFormatWriter struct{ *bufio.Writer }

func (writer corpusFormatWriter) writeBytes(data []byte)  { _, _ = writer.Write(data) }
func (writer corpusFormatWriter) writeString(text string) { _, _ = writer.WriteString(text) }
func (writer corpusFormatWriter) writeByte(value byte)    { _ = writer.WriteByte(value) }
func (writer corpusFormatWriter) writeRune(value rune)    { _, _ = writer.WriteRune(value) }

// writeCorpusResultsWithContext streams each file block to w. It avoids one
// corpus-sized output string when an unlimited search returns many files.
func writeCorpusResultsWithContext(
	w io.Writer,
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
	limit int,
	maxMatches int,
	displayMode corpusDisplayMode,
	pal palette,
) (bool, error) {
	deduped, hiddenMatches, totalFiles := prepareCorpusResultsForDisplay(
		results,
		dirtyByCorpus,
		limit,
		maxMatches,
	)
	if len(deduped) == 0 {
		return false, nil
	}
	// NewWriter returns w itself when w is a large enough bufio.Writer.
	writer := corpusFormatWriter{bufio.NewWriter(w)}
	renderCorpusResults(writer, deduped, hiddenMatches, totalFiles, displayMode, pal)
	return true, writer.Flush()
}

func renderCorpusResults(
	writer corpusFormatWriter,
	results []corpusSearchResult,
	hiddenMatches []int,
	totalFiles int,
	displayMode corpusDisplayMode,
	pal palette,
) {
	for index, result := range results {
		if index > 0 {
			writer.writeByte('\n')
		}
		formatCorpusFileMatch(writer, result, displayMode, hiddenMatches[index], pal)
	}
	if hidden := totalFiles - len(results); hidden > 0 {
		writer.writeByte('\n')
		_, _ = fmt.Fprintf(
			writer,
			"… %d more %s (showing %d of %d)\n",
			hidden,
			plural(hidden, "file", "files"),
			len(results),
			totalFiles,
		)
	}
}

func prepareCorpusResultsForDisplay(
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
	limit int,
	maxMatches int,
) ([]corpusSearchResult, []int, int) {
	if len(results) == 0 {
		return nil, nil, 0
	}
	deduped := rankCorpusResultsForDisplay(results, dirtyByCorpus)
	totalFiles := len(deduped)
	if limit > 0 && len(deduped) > limit {
		deduped = deduped[:limit]
	}
	hiddenMatches := make([]int, len(deduped))
	for index := range deduped {
		deduped[index].file.LineMatches, hiddenMatches[index] =
			normalizeLineMatches(deduped[index].file.LineMatches, maxMatches)
	}
	return deduped, hiddenMatches, totalFiles
}

func rankCorpusResultsForDisplay(
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
) []corpusSearchResult {
	ranked := deduplicateCorpusResults(results, dirtyByCorpus)
	sort.SliceStable(ranked, func(i, j int) bool {
		left := ranked[i]
		right := ranked[j]
		if left.rankOverride > 0 && right.rankOverride > 0 && left.rankOverride != right.rankOverride {
			return left.rankOverride < right.rankOverride
		}
		return lessCorpusResultBM25(left, right)
	})
	return ranked
}

func lessCorpusResultBM25(left, right corpusSearchResult) bool {
	if left.file.Score != right.file.Score {
		return left.file.Score > right.file.Score
	}
	if left.file.FileName != right.file.FileName {
		return left.file.FileName < right.file.FileName
	}
	if left.displayRoot != right.displayRoot {
		return left.displayRoot < right.displayRoot
	}
	return left.corpusID < right.corpusID
}

func deduplicateCorpusResults(
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
) []corpusSearchResult {
	byPath := make(map[corpusResultKey]dedupEntry, len(results))
	for i, result := range results {
		// Only a Git node has the uncommitted repository; a folder
		// repository is "folder_" + its ID.
		isUncommitted := result.file.Repository == repoUncommitted
		key := corpusResultKey{corpusID: result.corpusID, fileName: result.file.FileName}
		chooseDedupEntry(byPath, key, i, isUncommitted)
	}
	out := make([]corpusSearchResult, 0, len(byPath))
	for _, entry := range byPath {
		result := results[entry.idx]
		dirtyFiles := dirtyByCorpus[result.corpusID]
		if !entry.uncommitted && dirtyFiles.contains(result.file.FileName) {
			continue
		}
		out = append(out, result)
	}
	return out
}

type corpusResultKey struct {
	corpusID corpusID
	fileName string
}

type dedupEntry struct {
	idx         int
	uncommitted bool
}

func chooseDedupEntry[K comparable](entries map[K]dedupEntry, key K, idx int, uncommitted bool) {
	existing, ok := entries[key]
	if !ok || (uncommitted && !existing.uncommitted) {
		entries[key] = dedupEntry{idx: idx, uncommitted: uncommitted}
	}
}

// normalizeLineMatches keeps the best maxMatches lines (every line when
// maxMatches <= 0), orders them for display, and returns how many it drops.
// Ranking must select the survivors before line sorting. The render loop also
// needs ascending lines to suppress overlapping context correctly. The helper
// never changes the caller's slice.
func normalizeLineMatches(lms []zoekt.LineMatch, maxMatches int) ([]zoekt.LineMatch, int) {
	if len(lms) < 2 {
		return lms, 0
	}
	byLine := func(left, right zoekt.LineMatch) int { return cmp.Compare(left.LineNumber, right.LineNumber) }
	if slices.IsSortedFunc(lms, byLine) && (maxMatches <= 0 || len(lms) <= maxMatches) {
		return lms, 0
	}

	// Never write through to the caller's slice: it shares backing arrays with
	// the search result, which the caller may still hold.
	out := slices.Clone(lms)
	if maxMatches > 0 && len(out) > maxMatches {
		sort.Slice(out, func(i, j int) bool {
			leftScore := stableLineMatchScore(out[i].Score)
			rightScore := stableLineMatchScore(out[j].Score)
			if leftScore != rightScore {
				return leftScore > rightScore
			}
			return out[i].LineNumber < out[j].LineNumber
		})
		out = out[:maxMatches]
	}
	slices.SortFunc(out, byLine)
	return out, len(lms) - len(out)
}

// Zoekt can add equal term scores in different orders. Round scores to 12
// decimal places before the line-number tie-break so parallel searches agree.
func stableLineMatchScore(score float64) float64 {
	return math.Round(score * 1e12)
}

// namesOnlyQuery reports a query whose root is type:file: an output mode that
// prints file names only on every route.
func namesOnlyQuery(raw query.Q) bool {
	typed, ok := query.Simplify(raw).(*query.Type)
	return ok && typed.Type == query.TypeFileName
}

// keepFileNamesOnly gives each result the one name line that Zoekt's own
// type:file search returns, so a model route that ranked content matches
// prints the same names-only form. A result that has Zoekt's name line keeps
// it with its highlight.
func keepFileNamesOnly(results []corpusSearchResult) {
	for index := range results {
		file := &results[index].file
		name := zoekt.LineMatch{Line: []byte(file.FileName), FileName: true}
		for _, line := range file.LineMatches {
			if line.FileName {
				name = line
				break
			}
		}
		file.LineMatches = []zoekt.LineMatch{name}
		file.ChunkMatches = nil
	}
}

func formatCorpusFileMatch(sb corpusFormatWriter, result corpusSearchResult, displayMode corpusDisplayMode, hiddenMatches int, pal palette) {
	fm := result.file
	lang := fm.Language
	if lang == "" {
		lang = "unknown"
	}

	// File header: the operand as typed plus the path below it
	// (applyHeaders); a caller that set no header gets the node-relative path,
	// or the absolute one in multi-node output. The [git]/[folder] tag marks
	// the node kind in multi-node output.
	showRoot := displayMode == showCorpusContext && result.displayRoot != ""
	sb.writeString("## ")
	sb.writeString(pal.file)
	writeSanitizedString(sb, displayHeader(result, showRoot))
	sb.writeString(pal.reset)
	sb.writeString(" (")
	writeSanitizedString(sb, lang)
	sb.writeByte(')')

	if fm.Repository == repoUncommitted {
		sb.writeString(" [")
		sb.writeString(repoUncommitted)
		sb.writeByte(']')
	}
	if showRoot {
		switch result.kind {
		case corpusKindFolder:
			sb.writeString(" [folder]")
		default:
			sb.writeString(" [git]")
		}
	}
	sb.writeByte('\n')
	// Copies with the same content and match print once: the other
	// paths follow the header, in tie order.
	if len(result.also) > 0 {
		sb.writeString("also: ")
		for index, duplicate := range result.also {
			if index > 0 {
				sb.writeString(", ")
			}
			path := duplicate.header
			switch {
			case path != "":
			case showRoot:
				path = duplicate.displayPath()
			default:
				path = duplicate.path
			}
			writeSanitizedString(sb, path)
		}
		sb.writeByte('\n')
	}

	// Track the last line number we emitted so we can insert a blank separator
	// between non-contiguous regions of context.
	lastEmittedLine := 0

	for i, lm := range fm.LineMatches {
		matchLine := int(lm.LineNumber)

		// Compute context "before" line count and boundaries.
		// Count without materializing context lines on the hot path.
		beforeCount := countContextLines(lm.Before)
		firstBeforeLine := matchLine - beforeCount
		skipLines := 0
		if firstBeforeLine < 1 {
			// Guard against before-context exceeding file start (matchLine near 0)
			// or file-only matches where matchLine=0 and lm.Before is empty.
			skipLines = 1 - firstBeforeLine
			if skipLines >= beforeCount {
				beforeCount = 0
			}
			firstBeforeLine = 1
		}

		// Insert a blank separator if there is a gap between the previous
		// region (match + its after-context) and this region (before-context +
		// match). Skip for the very first match.
		if i > 0 && firstBeforeLine > lastEmittedLine+1 {
			sb.writeByte('\n')
		}

		// Emit "before" context lines directly from bytes, skipping any that
		// overlap with the previous region's already-emitted lines.
		if beforeCount > 0 {
			parts := splitContextBytes(lm.Before)
			for idx, line := range parts {
				if idx < skipLines {
					continue
				}
				lineNum := firstBeforeLine + (idx - skipLines)
				if lineNum > lastEmittedLine {
					writeContextLine(sb, lineNum, line, pal)
				}
			}
		}

		// Emit the match line itself: "<lineNo> [kind] <content>".
		writeLineNum(sb, matchLine, pal)
		sb.writeByte(' ')

		// Symbol kind from first line fragment
		if len(lm.LineFragments) > 0 && lm.LineFragments[0].SymbolInfo != nil && lm.LineFragments[0].SymbolInfo.Kind != "" {
			sb.writeByte('[')
			writeSanitizedString(sb, lm.LineFragments[0].SymbolInfo.Kind)
			sb.writeString("] ")
		}

		writeMatchLineContent(sb, bytes.TrimRight(lm.Line, "\n"), lm.LineFragments, pal)
		sb.writeByte('\n')

		lastEmittedLine = matchLine

		// Emit "after" context lines directly from bytes, but stop before any
		// line that would overlap with the next match's before-context or the
		// next match itself.
		afterLimit := countContextLines(lm.After)
		if i+1 < len(fm.LineMatches) {
			next := fm.LineMatches[i+1]
			nextFirstBefore := next.LineNumber - countContextLines(next.Before)
			afterLimit = min(afterLimit, max(0, nextFirstBefore-matchLine-1))
		}

		if afterLimit > 0 {
			parts := splitContextBytes(lm.After)
			for k := 0; k < afterLimit && k < len(parts); k++ {
				lineNum := matchLine + 1 + k
				writeContextLine(sb, lineNum, parts[k], pal)
				lastEmittedLine = lineNum
			}
		}
	}

	if hiddenMatches > 0 {
		_, _ = fmt.Fprintf(sb, "… %d more %s in this file\n", hiddenMatches, plural(hiddenMatches, "match", "matches"))
	}
}

// writeLineNum writes the (optionally colored) line number. No padding or
// indent — the gutter is intentionally dense to save tokens for agents.
func writeLineNum(sb corpusFormatWriter, lineNum int, pal palette) {
	sb.writeString(pal.lineNo)
	sb.writeString(strconv.Itoa(lineNum))
	sb.writeString(pal.reset)
}

// writeContextLine writes a context line: line number, a space, then the
// sanitized (and length-capped) content.
func writeContextLine(sb corpusFormatWriter, lineNum int, content []byte, pal palette) {
	writeLineNum(sb, lineNum, pal)
	sb.writeByte(' ')
	writeCappedContent(sb, content)
	sb.writeByte('\n')
}

// writeCappedContent writes a context line's content: if it exceeds
// maxLineBytes it is cut at a rune boundary and a "…+N bytes" marker appended.
// Context lines carry no match, so a simple tail trim is safe.
func writeCappedContent(sb corpusFormatWriter, content []byte) {
	n := len(content)
	if n <= maxLineBytes {
		writeSanitized(sb, content)
		return
	}
	end := backupToRuneBoundary(content, maxLineBytes)
	writeSanitized(sb, content[:end])
	_, _ = fmt.Fprintf(sb, " …+%d bytes", n-end)
}

// writeMatchLineContent writes a match line's content, windowing very long
// lines AROUND their matches (so the match stays visible even if it sits deep
// in a minified line) and coloring each matched span. The line is segmented at
// raw fragment offsets first, then each segment is sanitized — so offsets are
// never re-indexed and a control byte inside a match cannot shift the spans.
func writeMatchLineContent(sb corpusFormatWriter, content []byte, frags []zoekt.LineFragmentMatch, pal palette) {
	n := len(content)

	// 1. Build clamped, non-empty, coalesced spans. zoekt documents fragments
	// as sorted ascending by offset and non-overlapping, but only enforces that
	// on its ChunkMatch path — not the LineMatch path seek uses. So sort the
	// local spans defensively before coalescing: a single out-of-order fragment
	// would otherwise be silently dropped by the "start <= prev.end" merge.
	//
	// A stack-backed array keeps the common case (≤16 fragments per line) off the
	// heap; only a pathologically fragmented line spills to a heap-grown slice.
	type span struct{ start, end int }
	var buf [16]span
	spans := buf[:0]
	for _, f := range frags {
		s, e := max(f.LineOffset, 0), min(f.LineOffset+f.MatchLength, n)
		if s >= e { // zero-length, or match at/after the trimmed newline
			continue
		}
		spans = append(spans, span{s, e})
	}
	slices.SortFunc(spans, func(left, right span) int { return cmp.Compare(left.start, right.start) })
	merged := spans[:0]
	for _, sp := range spans {
		if k := len(merged); k > 0 && sp.start <= merged[k-1].end {
			if sp.end > merged[k-1].end {
				merged[k-1].end = sp.end
			}
			continue
		}
		merged = append(merged, sp)
	}
	spans = merged

	// 2. Window long lines around the matches. The window runs from
	// matchCtxBytes before the first span to matchCtxBytes after the last
	// span, so every span lies fully inside it and no match is cut.
	winStart, winEnd := 0, n
	if n > maxLineBytes && len(spans) > 0 {
		// 3. Keep cuts on rune boundaries: a mid-rune split would corrupt the
		// segment and break sanitization across separate segments.
		winStart = backupToRuneBoundary(content, max(spans[0].start-matchCtxBytes, 0))
		winEnd = backupToRuneBoundary(content, min(spans[len(spans)-1].end+matchCtxBytes, n))
	}

	if winStart > 0 {
		_, _ = fmt.Fprintf(sb, "…+%d bytes ", winStart)
	}

	// 4. Walk spans inside the window, coloring each matched (visible) span.
	cur := winStart
	for _, sp := range spans {
		s, e := sp.start, sp.end
		// Invalid UTF-8 can move winEnd back over a span.
		s, e = max(s, winStart), min(e, winEnd)
		if s >= e {
			continue
		}
		if cur < s {
			writeSanitized(sb, content[cur:s])
		}
		seg := content[s:e]
		if pal.match != "" && containsVisible(seg) {
			sb.writeString(pal.match)
			writeSanitized(sb, seg)
			sb.writeString(pal.reset)
		} else {
			writeSanitized(sb, seg)
		}
		cur = e
	}
	if cur < winEnd {
		writeSanitized(sb, content[cur:winEnd])
	}

	if winEnd < n {
		_, _ = fmt.Fprintf(sb, " …+%d bytes", n-winEnd)
	}
}

// stripRune reports whether output must omit r. It drops C0, C1, and DEL control
// characters except tab. It also drops bidirectional formatting and line or
// paragraph separators because they can alter the displayed source layout. It
// keeps zero-width joiners for scripts and emoji sequences.
func stripRune(r rune) bool {
	if r < 0x80 {
		// Strip C0 controls and DEL, but keep tab.
		return (r < 0x20 || r == 0x7f) && r != '\t'
	}
	if unicode.IsControl(r) { // C1 controls (U+0080–U+009F)
		return true
	}
	switch r {
	case '\u061C', // ALM
		'\u200E', '\u200F', // LRM, RLM
		'\u202A', '\u202B', '\u202C', '\u202D', '\u202E', // LRE RLE PDF LRO RLO
		'\u2066', '\u2067', '\u2068', '\u2069', // LRI RLI FSI PDI
		'\u2028', '\u2029': // line / paragraph separator
		return true
	}
	return false
}

// writeSanitized writes b after it removes code points selected by stripRune.
// It iterates runes because C1 values overlap UTF-8 continuation bytes. When no
// rune is removed, it writes the original bytes unchanged.
//
// Invalid UTF-8: a clean line is written verbatim (bytes preserved); a line that
// also needs stripping goes through the rune path, where invalid bytes decode to
// U+FFFD.
func writeSanitized(sb corpusFormatWriter, b []byte) {
	clean := true
	for _, r := range string(b) {
		if stripRune(r) {
			clean = false
			break
		}
	}
	if clean {
		sb.writeBytes(b)
		return
	}
	for _, r := range string(b) {
		if stripRune(r) {
			continue
		}
		sb.writeRune(r)
	}
}

func writeSanitizedString(sb corpusFormatWriter, text string) {
	clean := true
	for _, char := range text {
		if stripRune(char) {
			clean = false
			break
		}
	}
	if clean {
		sb.writeString(text)
		return
	}
	for _, char := range text {
		if stripRune(char) {
			continue
		}
		sb.writeRune(char)
	}
}

// containsVisible reports whether b has any rune that survives sanitization
// (i.e. a non-control rune, or tab). Used to avoid emitting an empty color pair
// around an all-control match span.
func containsVisible(b []byte) bool {
	for _, r := range string(b) {
		if !stripRune(r) {
			return true
		}
	}
	return false
}

// backupToRuneBoundary returns the largest index <= pos at which a rune starts,
// i.e. it backs up off any UTF-8 continuation byte. Used to keep window/cap
// cuts on rune boundaries.
func backupToRuneBoundary(b []byte, pos int) int {
	for pos > 0 && pos < len(b) && b[pos]&0xC0 == 0x80 {
		pos--
	}
	return pos
}

// splitContextBytes splits raw context bytes (from LineMatch.Before or .After)
// into sub-slices sharing the original data. A trailing newline is treated as
// a terminator, not an empty line.
func splitContextBytes(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	return bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
}

// countContextLines counts how many context lines are in the raw bytes
// without allocating. It mirrors splitContextBytes' trimming logic:
// a trailing newline is ignored (it's a terminator, not an empty line).
func countContextLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	return bytes.Count(data, []byte("\n")) + 1
}

// plural gives one for a count of 1 and many otherwise.
func plural(count int, one, many string) string {
	if count == 1 {
		return one
	}
	return many
}
