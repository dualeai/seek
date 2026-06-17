package main

import (
	"bytes"
	"fmt"
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

	deduped := deduplicateCorpusResults(results, dirtyByCorpus)

	// Sort by score descending
	sort.SliceStable(deduped, func(i, j int) bool {
		left := deduped[i].file
		right := deduped[j].file
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		if left.FileName != right.FileName {
			return left.FileName < right.FileName
		}
		if deduped[i].displayRoot != deduped[j].displayRoot {
			return deduped[i].displayRoot < deduped[j].displayRoot
		}
		return deduped[i].corpusID < deduped[j].corpusID
	})

	// Apply file-count limit (0 or negative = unlimited). Remember the full
	// count so we can tell the consumer how many files were hidden — silent
	// truncation reads as "this is everything" when it is not.
	totalFiles := len(deduped)
	if limit > 0 && len(deduped) > limit {
		deduped = deduped[:limit]
	}

	// Apply per-file match limit (0 or negative = unlimited), recording how
	// many matches were dropped per file for the "N more matches" notice.
	hiddenMatches := make([]int, len(deduped))
	if maxMatches > 0 {
		for i := range deduped {
			if extra := len(deduped[i].file.LineMatches) - maxMatches; extra > 0 {
				hiddenMatches[i] = extra
				deduped[i].file.LineMatches = deduped[i].file.LineMatches[:maxMatches]
			}
		}
	}

	// Pre-size the builder: ~200 bytes per file header + ~80 bytes per match.
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

func formatCorpusFileMatch(sb *strings.Builder, result corpusSearchResult, displayMode corpusDisplayMode, hiddenMatches int, pal palette) {
	fm := result.file
	lang := fm.Language
	if lang == "" {
		lang = "unknown"
	}

	// File header. In multi-corpus mode emit the absolute, directly-openable
	// path (displayRoot joined to the corpus-relative FileName) so an agent
	// never has to reconstruct it; the corpus tag then only marks the kind.
	showRoot := displayMode == showCorpusContext && result.displayRoot != ""
	sb.WriteString("## ")
	if showRoot {
		writeColoredSanitized(sb, pal.file, pal.reset, []byte(filepath.Join(result.displayRoot, fm.FileName)))
	} else {
		writeColoredSanitized(sb, pal.file, pal.reset, []byte(fm.FileName))
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
		// segment AND break sanitize's per-segment distributivity.
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

// stripRune reports whether r must be removed from output. It drops C0/C1/DEL
// control characters (tab excepted) AND the bidirectional-formatting and
// line/paragraph-separator code points: those enable Trojan-Source-style visual
// spoofing (CVE-2021-42574) or would break the one-line-per-match layout. Zero-
// width joiners (U+200C/U+200D) are intentionally kept so legitimate scripts and
// emoji ZWJ sequences survive.
func stripRune(r rune) bool {
	if r < 0x80 {
		// ASCII fast path (the overwhelming common case): strip C0 controls and
		// DEL, keep tab. Avoids unicode.IsControl's range-table lookup per rune.
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

// writeSanitized writes b with stripRune'd code points removed. Iterating runes
// is mandatory: C1 controls (U+0080–U+009F) overlap UTF-8 continuation bytes, so
// a raw byte scan would corrupt multibyte runes. The fast path (nothing to
// strip — the common case) writes the bytes verbatim with no allocation, keeping
// the plain path byte-identical to clean input.
//
// Invalid UTF-8: a clean line is written verbatim (bytes preserved); a line that
// also needs stripping goes through the rune path, where invalid bytes decode to
// U+FFFD. Source files are valid UTF-8, so this divergence is immaterial here.
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
