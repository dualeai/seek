package main

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"io"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// maxLineBytes and matchCtxBytes cap long lines. A match line longer than
// maxLineBytes shows a window around its matches, with matchCtxBytes of source
// on each side. A context line longer than maxLineBytes loses its tail. Both
// bounds count source bytes only: ANSI color codes and the "…+N bytes"
// markers do not count. The CLI has no flag for them.
const (
	maxLineBytes  = 1024
	matchCtxBytes = 512
)

// palette holds the ANSI codes that color the output. The zero value
// (plainPalette) writes no codes. Each run chooses the palette once
// (useColor). Tests pass plainPalette, so the expected strings stay fixed.
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

// dirtyFileSet holds the names of the dirty files of a Git node's capture.
type dirtyFileSet map[string]struct{}

func (s dirtyFileSet) contains(name string) bool {
	_, ok := s[name]
	return ok
}

// corpusDisplayMode selects whether a file header shows the node's root and
// kind (multi-node output).
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

// writeCorpusResultsWithContext streams the file blocks to w in rank order,
// each block as soon as it is ready. It ranks the results, keeps the first
// limit files (all when limit <= 0), and keeps at most maxMatches lines of
// each. No result writes nothing.
//
// A block depends only on its own result, so the blocks render on all CPUs,
// each into its own buffer, and the writer copies each one out as soon as it
// and the blocks before it are ready. On Kubernetes, the 117 MB of `-n 0 the`
// took about 420 ms to render on one goroutine and about 90 ms this way.
func writeCorpusResultsWithContext(
	w io.Writer,
	results []corpusSearchResult,
	limit int,
	maxMatches int,
	displayMode corpusDisplayMode,
	pal palette,
) error {
	if len(results) == 0 {
		return nil
	}
	ranked := rankCorpusResultsForDisplay(results)
	totalFiles := len(ranked)
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	// NewWriter returns w itself when w is a large enough bufio.Writer.
	writer := corpusFormatWriter{bufio.NewWriter(w)}
	blocks := make([][]byte, len(ranked))
	ready := make([]chan struct{}, len(ranked))
	for index := range ready {
		ready[index] = make(chan struct{})
	}
	var next atomic.Int64
	for range min(runtime.GOMAXPROCS(0), len(ranked)) {
		go func() {
			blockWriter := corpusFormatWriter{bufio.NewWriter(nil)}
			for {
				index := int(next.Add(1)) - 1
				if index >= len(ranked) {
					return
				}
				var block bytes.Buffer
				blockWriter.Reset(&block)
				result := ranked[index]
				lines, hidden := normalizeLineMatches(result.file.LineMatches, maxMatches)
				result.file.LineMatches = lines
				formatCorpusFileMatch(blockWriter, result, displayMode, hidden, pal)
				_ = blockWriter.Flush()
				blocks[index] = block.Bytes()
				close(ready[index])
			}
		}()
	}
	for index := range ranked {
		<-ready[index]
		if index > 0 {
			writer.writeByte('\n')
		}
		writer.writeBytes(blocks[index])
		blocks[index] = nil
	}
	if hidden := totalFiles - len(ranked); hidden > 0 {
		writer.writeByte('\n')
		_, _ = fmt.Fprintf(
			writer,
			"… %d more %s (showing %d of %d)\n",
			hidden,
			plural(hidden, "file", "files"),
			len(ranked),
			totalFiles,
		)
	}
	return writer.Flush()
}

// rankCorpusResultsForDisplay sorts a copy of results for display. Each node
// read returns one current copy per file, so the results need no
// deduplication. Equal results keep their input order, as in a stable sort. A
// result is 384 bytes, so the sort moves positions and copies each result
// once. On 30,000 results, a stable sort of the results took 40 to 55 ms.
func rankCorpusResultsForDisplay(results []corpusSearchResult) []corpusSearchResult {
	order := make([]int, len(results))
	for index := range order {
		order[index] = index
	}
	slices.SortFunc(order, func(left, right int) int {
		if c := compareCorpusResults(&results[left], &results[right]); c != 0 {
			return c
		}
		return cmp.Compare(left, right)
	})
	ranked := make([]corpusSearchResult, len(results))
	for position, index := range order {
		ranked[position] = results[index]
	}
	return ranked
}

// compareCorpusResults orders by the model rank when both results have one.
// Else it orders by score (highest first), then by file name, display root,
// and corpus ID, so the order is fixed.
func compareCorpusResults(left, right *corpusSearchResult) int {
	if left.rankOverride > 0 && right.rankOverride > 0 && left.rankOverride != right.rankOverride {
		return cmp.Compare(left.rankOverride, right.rankOverride)
	}
	switch {
	case left.file.Score > right.file.Score:
		return -1
	case left.file.Score < right.file.Score:
		return 1
	}
	if c := strings.Compare(left.file.FileName, right.file.FileName); c != 0 {
		return c
	}
	if c := strings.Compare(left.displayRoot, right.displayRoot); c != 0 {
		return c
	}
	return strings.Compare(string(left.corpusID), string(right.corpusID))
}

// normalizeLineMatches keeps the best maxMatches lines (every line when
// maxMatches <= 0), orders them for display, and returns how many it drops.
// The rank selects the kept lines before the sort by line number. The render
// loop needs ascending lines to skip context that overlaps. It never changes
// the caller's slice.
//
// The sorts move positions, and each kept line is copied once. A line match
// is more than 100 bytes, and Zoekt returns the lines by score, so a sort of
// the matches themselves moves each one many times. The line numbers of one
// file differ, so the order is the same for any sort.
func normalizeLineMatches(lms []zoekt.LineMatch, maxMatches int) ([]zoekt.LineMatch, int) {
	if len(lms) < 2 {
		return lms, 0
	}
	byLine := func(left, right zoekt.LineMatch) int { return cmp.Compare(left.LineNumber, right.LineNumber) }
	if slices.IsSortedFunc(lms, byLine) && (maxMatches <= 0 || len(lms) <= maxMatches) {
		return lms, 0
	}

	order := make([]int, len(lms))
	for index := range order {
		order[index] = index
	}
	if maxMatches > 0 && len(order) > maxMatches {
		slices.SortFunc(order, func(left, right int) int {
			leftScore := stableLineMatchScore(lms[left].Score)
			rightScore := stableLineMatchScore(lms[right].Score)
			if leftScore != rightScore {
				return cmp.Compare(rightScore, leftScore)
			}
			return cmp.Compare(lms[left].LineNumber, lms[right].LineNumber)
		})
		order = order[:maxMatches]
	}
	slices.SortFunc(order, func(left, right int) int { return cmp.Compare(lms[left].LineNumber, lms[right].LineNumber) })
	// Never write to the caller's slice: it shares its array with the search
	// result, which the caller can still hold.
	out := make([]zoekt.LineMatch, len(order))
	for position, index := range order {
		out[position] = lms[index]
	}
	return out, len(lms) - len(out)
}

// stableLineMatchScore rounds score to 12 decimal places for the line rank.
// Zoekt can add equal term scores in different orders. With the rounding, the
// line-number tie-break decides, and parallel searches agree.
// TODO(https://github.com/sourcegraph/zoekt/issues/1187): drop the rounding
// once Zoekt sums in a fixed order.
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
// type:file search returns. So a model route that ranked content matches
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

	// The file header is the operand as typed, with the path below it
	// (applyHeaders). The [git] or [folder] tag marks the node kind in
	// multi-node output.
	showRoot := displayMode == showCorpusContext && result.displayRoot != ""
	sb.writeString("## ")
	sb.writeString(pal.file)
	writeSanitizedString(sb, result.header)
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
			writeSanitizedString(sb, duplicate.header)
		}
		sb.writeByte('\n')
	}

	// lastEmittedLine is the last line number that the loop wrote. A blank
	// line separates two regions of context that do not touch.
	lastEmittedLine := 0

	for i, lm := range fm.LineMatches {
		matchLine := lm.LineNumber

		// Count the "before" context lines and find their bounds. The count
		// makes no slice of lines, because this code runs for each match line.
		beforeCount := countContextLines(lm.Before)
		firstBeforeLine := matchLine - beforeCount
		skipLines := 0
		if firstBeforeLine < 1 {
			// The before-context can start before the file (matchLine near 0).
			// A file-name match has matchLine=0 and an empty lm.Before.
			skipLines = 1 - firstBeforeLine
			if skipLines >= beforeCount {
				beforeCount = 0
			}
			firstBeforeLine = 1
		}

		// Write a blank separator when a gap is between the previous region
		// (match and its after-context) and this region (before-context and
		// match). The first match gets none.
		if i > 0 && firstBeforeLine > lastEmittedLine+1 {
			sb.writeByte('\n')
		}

		// Write the "before" context lines from the bytes. Skip each line that
		// the previous region already wrote. The loops cut one line at a time,
		// as splitContextBytes splits them, with no slice of lines for each
		// match.
		if beforeCount > 0 {
			rest := bytes.TrimSuffix(lm.Before, []byte("\n"))
			for idx := 0; ; idx++ {
				line, next, more := bytes.Cut(rest, []byte("\n"))
				if idx >= skipLines {
					if lineNum := firstBeforeLine + (idx - skipLines); lineNum > lastEmittedLine {
						writeContextLine(sb, lineNum, line, pal)
					}
				}
				if !more {
					break
				}
				rest = next
			}
		}

		// Write the match line: "<lineNo> [kind] <content>".
		writeLineNum(sb, matchLine, pal)
		sb.writeByte(' ')

		// The symbol kind comes from the first line fragment.
		if len(lm.LineFragments) > 0 && lm.LineFragments[0].SymbolInfo != nil && lm.LineFragments[0].SymbolInfo.Kind != "" {
			sb.writeByte('[')
			writeSanitizedString(sb, lm.LineFragments[0].SymbolInfo.Kind)
			sb.writeString("] ")
		}

		writeMatchLineContent(sb, bytes.TrimRight(lm.Line, "\n"), lm.LineFragments, pal)
		sb.writeByte('\n')

		lastEmittedLine = matchLine

		// Write the "after" context lines from the bytes. Stop before a line
		// that the next match's before-context or the next match holds.
		afterLimit := countContextLines(lm.After)
		if i+1 < len(fm.LineMatches) {
			next := fm.LineMatches[i+1]
			nextFirstBefore := next.LineNumber - countContextLines(next.Before)
			afterLimit = min(afterLimit, max(0, nextFirstBefore-matchLine-1))
		}

		if afterLimit > 0 {
			rest := bytes.TrimSuffix(lm.After, []byte("\n"))
			for k := 0; k < afterLimit; k++ {
				line, next, more := bytes.Cut(rest, []byte("\n"))
				lineNum := matchLine + 1 + k
				writeContextLine(sb, lineNum, line, pal)
				lastEmittedLine = lineNum
				if !more {
					break
				}
				rest = next
			}
		}
	}

	if hiddenMatches > 0 {
		_, _ = fmt.Fprintf(sb, "… %d more %s in this file\n", hiddenMatches, plural(hiddenMatches, "match", "matches"))
	}
}

// writeLineNum writes the line number, in color when pal has one. It writes
// no padding or indent: a dense gutter saves tokens for agents. The digits go
// straight into the free space of the buffer: strconv.Itoa allocates a string
// for each number of 100 or more, and this runs for each output line.
func writeLineNum(sb corpusFormatWriter, lineNum int, pal palette) {
	sb.writeString(pal.lineNo)
	sb.writeBytes(strconv.AppendInt(sb.AvailableBuffer(), int64(lineNum), 10))
	sb.writeString(pal.reset)
}

// writeContextLine writes a context line: the line number, a space, then the
// sanitized content with its length cap.
func writeContextLine(sb corpusFormatWriter, lineNum int, content []byte, pal palette) {
	writeLineNum(sb, lineNum, pal)
	sb.writeByte(' ')
	writeCappedContent(sb, content)
	sb.writeByte('\n')
}

// writeCappedContent writes a context line's content. Past maxLineBytes, it
// cuts the content at a rune boundary and adds a "…+N bytes" marker. A
// context line holds no match, so a cut of its tail is safe.
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

// writeMatchLineContent writes a match line's content and colors each matched
// span. A very long line shows a window around its matches, so a match deep in
// a minified line stays visible. It cuts the line at the raw fragment offsets
// first, then sanitizes each segment. So the offsets never change, and a
// control byte in a match cannot move the spans.
func writeMatchLineContent(sb corpusFormatWriter, content []byte, frags []zoekt.LineFragmentMatch, pal palette) {
	n := len(content)

	// 1. Build clamped, non-empty, merged spans. Zoekt documents fragments as
	// sorted by offset and not overlapping. It makes sure of that only on its
	// ChunkMatch path, not on the LineMatch path that Seek uses. So sort the
	// spans before the merge. Else the "start <= prev.end" merge would drop a
	// fragment out of order with no sign.
	//
	// An array on the stack keeps the usual case (16 fragments or fewer per
	// line) off the heap. Only a line with very many fragments grows a slice on
	// the heap.
	type span struct{ start, end int }
	var buf [16]span
	spans := buf[:0]
	for _, f := range frags {
		s, e := max(f.LineOffset, 0), min(f.LineOffset+f.MatchLength, n)
		if s >= e { // zero length, or a match at or after the trimmed newline
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
	// span. So every span is fully in it, and no match is cut.
	winStart, winEnd := 0, n
	if n > maxLineBytes && len(spans) > 0 {
		// 3. Keep cuts on rune boundaries. A cut in a rune would damage the
		// segment and break the sanitization of the segments on each side.
		winStart = backupToRuneBoundary(content, max(spans[0].start-matchCtxBytes, 0))
		winEnd = backupToRuneBoundary(content, min(spans[len(spans)-1].end+matchCtxBytes, n))
	}

	if winStart > 0 {
		_, _ = fmt.Fprintf(sb, "…+%d bytes ", winStart)
	}

	// 4. Walk the spans in the window, and color each visible matched span.
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

// stripRune reports whether output must omit r. It drops C0, C1, and DEL
// control characters except tab. It also drops bidirectional formatting and
// line or paragraph separators because they can alter the displayed source
// layout. It keeps zero-width joiners for scripts and emoji sequences.
func stripRune(r rune) bool {
	if r < 0x80 {
		// Strip C0 controls and DEL, but keep tab.
		return (r < 0x20 || r == 0x7f) && r != '\t'
	}
	if unicode.IsControl(r) { // C1 controls (U+0080 to U+009F)
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

// writeSanitized writes b after it removes the code points that stripRune
// selects. It reads runes, because C1 values overlap UTF-8 continuation bytes.
// When it removes no rune, it writes the original bytes unchanged.
//
// Invalid UTF-8: a clean line goes out as it is, with its bytes. A line that
// also needs a removal goes through the rune path, where invalid bytes decode
// to U+FFFD.
//
// The check decodes the bytes in place. A range over string(b) copies the
// bytes first, on the heap past 32 bytes, and all the output goes through
// here.
func writeSanitized(sb corpusFormatWriter, b []byte) {
	clean := true
	for index := 0; index < len(b); {
		r, size := rune(b[index]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRune(b[index:])
		}
		if stripRune(r) {
			clean = false
			break
		}
		index += size
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

// writeSanitizedString is writeSanitized for a string.
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

// containsVisible reports whether b has a rune that stripRune keeps. It
// prevents an empty color pair around a match span that holds only stripped
// runes.
func containsVisible(b []byte) bool {
	for _, r := range string(b) {
		if !stripRune(r) {
			return true
		}
	}
	return false
}

// backupToRuneBoundary returns the largest index <= pos at which a rune
// starts: it moves back over UTF-8 continuation bytes. The window and cap cuts
// use it to stay on rune boundaries.
func backupToRuneBoundary(b []byte, pos int) int {
	for pos > 0 && pos < len(b) && b[pos]&0xC0 == 0x80 {
		pos--
	}
	return pos
}

// utf8Prefix returns the longest start of text of at most limit bytes that
// does not cut a rune (backupToRuneBoundary).
func utf8Prefix(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := backupToRuneBoundary([]byte(text), limit)
	return text[:end]
}

// splitContextBytes splits raw context bytes (from LineMatch.Before or .After)
// into slices of the original data. A trailing newline ends the last line; it
// is not an empty line.
func splitContextBytes(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	return bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
}

// countContextLines counts the context lines in the raw bytes, with no
// allocation. It trims as splitContextBytes does: a trailing newline ends the
// last line and is not an empty line.
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
