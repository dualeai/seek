package main

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/sourcegraph/zoekt"
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
// (plainPalette) emits no codes, so the plain path is byte-identical to the
// pre-color formatter. Color is presentation-only and chosen once per run from
// useColor; tests pass plainPalette so golden strings stay stable.
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

func formatCorpusResultsWithContext(
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
	limit int,
	maxMatches int,
	displayMode corpusDisplayMode,
	pal palette,
) string {
	if len(results) == 0 {
		return ""
	}

	deduped := rankCorpusResultsForDisplay(results, dirtyByCorpus)

	// Apply file-count limit (0 or negative = unlimited). Remember the full
	// count so we can tell the consumer how many files were hidden — silent
	// truncation reads as "this is everything" when it is not.
	totalFiles := len(deduped)
	if limit > 0 && len(deduped) > limit {
		deduped = deduped[:limit]
	}

	// Collapse duplicate lines, apply the per-file match limit, and put the
	// survivors in line order. Recording how many were dropped feeds the
	// "N more matches" notice.
	hiddenMatches := make([]int, len(deduped))
	for i := range deduped {
		deduped[i].file.LineMatches, hiddenMatches[i] =
			normalizeLineMatches(deduped[i].file.LineMatches, maxMatches)
	}

	// Pre-size the builder from the number of files and matches.
	matches := 0
	for _, result := range deduped {
		matches += len(result.file.LineMatches)
	}
	var sb strings.Builder
	sb.Grow(len(deduped)*200 + matches*80)
	for i, result := range deduped {
		if i > 0 {
			sb.WriteByte('\n')
		}
		formatCorpusFileMatch(&sb, result, displayMode, hiddenMatches[i], pal)
	}

	if hidden := totalFiles - len(deduped); hidden > 0 {
		sb.WriteByte('\n')
		fmt.Fprintf(&sb, "… %d more files (showing %d of %d)\n", hidden, len(deduped), totalFiles)
	}

	// No trailing newline after the last line
	s := sb.String()
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
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

func rankCorpusResultsBM25(
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
) []corpusSearchResult {
	ranked := deduplicateCorpusResults(results, dirtyByCorpus)
	sort.SliceStable(ranked, func(i, j int) bool {
		return lessCorpusResultBM25(ranked[i], ranked[j])
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
		isUncommitted := result.kind == corpusKindGit && result.file.Repository == repoUncommitted
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

// normalizeLineMatches keeps the best distinct lines, then orders them for
// display. Current zoekt emits one LineMatch per source line, but duplicate
// handling keeps this formatter safe for synthetic and future callers.
//
// Ranking must select the survivors before line sorting. The render loop also
// needs ascending lines to suppress overlapping context correctly. The helper
// never changes the caller's slice.
func normalizeLineMatches(lms []zoekt.LineMatch, maxMatches int) ([]zoekt.LineMatch, int) {
	if len(lms) < 2 {
		return lms, 0
	}

	strictlyOrdered := true
	for i := 1; i < len(lms); i++ {
		if lms[i].LineNumber <= lms[i-1].LineNumber {
			strictlyOrdered = false
			break
		}
	}
	if strictlyOrdered && (maxMatches <= 0 || len(lms) <= maxMatches) {
		return lms, 0
	}

	// Never write through to the caller's slice: it shares backing arrays with
	// the search result, which the caller may still hold.
	out := make([]zoekt.LineMatch, 0, len(lms))
	at := make(map[int]int, len(lms))
	for _, lm := range lms {
		i, seen := at[lm.LineNumber]
		if !seen {
			i = len(out)
			out = append(out, lm)
			at[lm.LineNumber] = i
			continue
		}
		// Union the fragments so a merged line stays fully colored. Copy
		// rather than append in place; out[i].LineFragments still aliases the
		// caller's array at this point.
		if len(out[i].LineFragments) == 0 && len(lm.LineFragments) == 0 {
			continue
		}
		merged := make([]zoekt.LineFragmentMatch, 0, len(out[i].LineFragments)+len(lm.LineFragments))
		merged = append(merged, out[i].LineFragments...)
		merged = append(merged, lm.LineFragments...)
		out[i].LineFragments = merged
	}

	unique := len(out)
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
	sort.Slice(out, func(i, j int) bool { return out[i].LineNumber < out[j].LineNumber })
	return out, unique - len(out)
}

// Zoekt can add equal term scores in different orders. Round scores to 12
// decimal places before the line-number tie-break so parallel searches agree.
func stableLineMatchScore(score float64) float64 {
	return math.Round(score * 1e12)
}

func formatCorpusFileMatch(sb *strings.Builder, result corpusSearchResult, displayMode corpusDisplayMode, hiddenMatches int, pal palette) {
	fm := result.file
	lang := fm.Language
	if lang == "" {
		lang = "unknown"
	}

	// File header. In multi-corpus mode emit the absolute, directly-openable
	// path (displayRoot joined to the corpus-relative FileName) so an agent
	// never has to reconstruct it; the corpus tag then only marks the kind.
	// In single-corpus mode emit the operand-relative displayName, which
	// collapses an explicitly-selected file to its basename so the output
	// reads as the file the user selected.
	showRoot := displayMode == showCorpusContext && result.displayRoot != ""
	sb.WriteString("## ")
	if showRoot {
		writeColoredSanitized(sb, pal.file, pal.reset, []byte(filepath.Join(result.displayRoot, fm.FileName)))
	} else {
		name := result.displayName
		if name == "" {
			name = fm.FileName
		}
		writeColoredSanitized(sb, pal.file, pal.reset, []byte(name))
	}
	sb.WriteString(" (")
	writeSanitized(sb, []byte(lang))
	sb.WriteByte(')')

	if fm.Repository == repoUncommitted {
		sb.WriteString(" [")
		sb.WriteString(repoUncommitted)
		sb.WriteByte(']')
	}
	if showRoot {
		switch result.kind {
		case corpusKindFolder:
			sb.WriteString(" [folder]")
		default:
			sb.WriteString(" [git]")
		}
	}
	sb.WriteByte('\n')

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
			// or file-only matches where matchLine=0 and beforeLines is empty.
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
			sb.WriteByte('\n')
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
		sb.WriteByte(' ')

		// Symbol kind from first line fragment
		if len(lm.LineFragments) > 0 && lm.LineFragments[0].SymbolInfo != nil && lm.LineFragments[0].SymbolInfo.Kind != "" {
			sb.WriteByte('[')
			writeSanitized(sb, []byte(lm.LineFragments[0].SymbolInfo.Kind))
			sb.WriteString("] ")
		}

		writeMatchLineContent(sb, bytes.TrimRight(lm.Line, "\n"), lm.LineFragments, pal)
		sb.WriteByte('\n')

		lastEmittedLine = matchLine

		// Emit "after" context lines directly from bytes, but stop before any
		// line that would overlap with the next match's before-context or the
		// next match itself.
		afterCount := countContextLines(lm.After)
		afterLimit := afterCount
		if i+1 < len(fm.LineMatches) {
			nextMatch := int(fm.LineMatches[i+1].LineNumber)
			nextBeforeLen := countContextLines(fm.LineMatches[i+1].Before)
			nextFirstBefore := nextMatch - nextBeforeLen
			for k := range afterCount {
				if matchLine+1+k >= nextFirstBefore {
					afterLimit = k
					break
				}
			}
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
		fmt.Fprintf(sb, "… %d more matches in this file\n", hiddenMatches)
	}
}

// writeLineNum writes the (optionally colored) line number. No padding or
// indent — the gutter is intentionally dense to save tokens for agents.
func writeLineNum(sb *strings.Builder, lineNum int, pal palette) {
	sb.WriteString(pal.lineNo)
	sb.WriteString(strconv.Itoa(lineNum))
	sb.WriteString(pal.reset)
}

// writeContextLine writes a context line: line number, a space, then the
// sanitized (and length-capped) content.
func writeContextLine(sb *strings.Builder, lineNum int, content []byte, pal palette) {
	writeLineNum(sb, lineNum, pal)
	sb.WriteByte(' ')
	writeCappedContent(sb, content)
	sb.WriteByte('\n')
}

// writeCappedContent writes a context line's content: if it exceeds
// maxLineBytes it is cut at a rune boundary and a "…+N bytes" marker appended.
// Context lines carry no match, so a simple tail trim is safe.
func writeCappedContent(sb *strings.Builder, content []byte) {
	n := len(content)
	if n <= maxLineBytes {
		writeSanitized(sb, content)
		return
	}
	end := backupToRuneBoundary(content, maxLineBytes)
	writeSanitized(sb, content[:end])
	fmt.Fprintf(sb, " …+%d bytes", n-end)
}

// writeMatchLineContent writes a match line's content, windowing very long
// lines AROUND their matches (so the match stays visible even if it sits deep
// in a minified line) and coloring each matched span. The line is segmented at
// raw fragment offsets first, then each segment is sanitized — so offsets are
// never re-indexed and a control byte inside a match cannot shift the spans.
func writeMatchLineContent(sb *strings.Builder, content []byte, frags []zoekt.LineFragmentMatch, pal palette) {
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
		s := f.LineOffset
		e := f.LineOffset + f.MatchLength
		if s < 0 {
			s = 0
		}
		if e > n {
			e = n
		}
		if s >= e { // zero-length, or match at/after the trimmed newline
			continue
		}
		spans = append(spans, span{s, e})
	}
	// Insertion sort by start. We avoid sort.Slice deliberately: its interface
	// dispatch forces `spans` to escape to the heap (one alloc per match line).
	// Fragments are almost always already sorted, so this is O(n) in practice.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j-1].start > spans[j].start; j-- {
			spans[j-1], spans[j] = spans[j], spans[j-1]
		}
	}
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

	// 2. Window long lines around the matches. Every span lies fully inside
	// [firstStart-CTX, lastEnd+CTX], so windowing never bisects a match.
	winStart, winEnd := 0, n
	headDropped, tailDropped := 0, 0
	if n > maxLineBytes && len(spans) > 0 {
		winStart = spans[0].start - matchCtxBytes
		if winStart < 0 {
			winStart = 0
		}
		winEnd = spans[len(spans)-1].end + matchCtxBytes
		if winEnd > n {
			winEnd = n
		}
		// 3. Keep cuts on rune boundaries: a mid-rune split would corrupt the
		// segment and break sanitization across separate segments.
		winStart = backupToRuneBoundary(content, winStart)
		winEnd = backupToRuneBoundary(content, winEnd)
		headDropped = winStart
		tailDropped = n - winEnd
	}

	if headDropped > 0 {
		fmt.Fprintf(sb, "…+%d bytes ", headDropped)
	}

	// 4. Walk spans inside the window, coloring each matched (visible) span.
	cur := winStart
	for _, sp := range spans {
		s, e := sp.start, sp.end
		if e <= winStart || s >= winEnd { // defensive; spans are inside the window
			continue
		}
		if s < winStart {
			s = winStart
		}
		if e > winEnd {
			e = winEnd
		}
		if cur < s {
			writeSanitized(sb, content[cur:s])
		}
		seg := content[s:e]
		if pal.match != "" && containsVisible(seg) {
			sb.WriteString(pal.match)
			writeSanitized(sb, seg)
			sb.WriteString(pal.reset)
		} else {
			writeSanitized(sb, seg)
		}
		cur = e
	}
	if cur < winEnd {
		writeSanitized(sb, content[cur:winEnd])
	}

	if tailDropped > 0 {
		fmt.Fprintf(sb, " …+%d bytes", tailDropped)
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
func writeSanitized(sb *strings.Builder, b []byte) {
	clean := true
	for _, r := range string(b) {
		if stripRune(r) {
			clean = false
			break
		}
	}
	if clean {
		sb.Write(b)
		return
	}
	for _, r := range string(b) {
		if stripRune(r) {
			continue
		}
		sb.WriteRune(r)
	}
}

// writeColoredSanitized wraps a sanitized byte slice in the given color/reset.
func writeColoredSanitized(sb *strings.Builder, color, reset string, b []byte) {
	sb.WriteString(color)
	writeSanitized(sb, b)
	sb.WriteString(reset)
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
