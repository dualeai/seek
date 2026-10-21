package main

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// windowSemanticUnits splits each unit into rows of at most one model input:
// every 128-token window of every unit is a row, so every byte reaches a
// vector. A window holds the tokens of one unit only. Each window repeats the
// unit's header (semanticWindowHeader), and its row keeps the byte span of its
// own body tokens, so the final model scores the window that the vector found:
// a unit's score is its best window. A window ends at a line or word start
// when one is in its second half.
func windowSemanticUnits(
	ctx context.Context,
	tokenizer *lateOnTokenizer,
	units []semanticUnit,
) ([]semanticUnit, error) {
	rows := make([]semanticUnit, 0, len(units))
	var idBuffer []byte
	for _, unit := range units {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		windows, err := semanticUnitWindows(tokenizer, unit)
		if err != nil {
			return nil, err
		}
		for _, window := range windows {
			row := unit
			row.start = unit.start + uint64(window.start)
			row.end = unit.start + uint64(window.end)
			row.text = unit.text[window.start:window.end]
			row.modelInput = window.input
			row.key = makeSemanticRowKey(window.input)
			if len(windows) > 1 {
				row.id, idBuffer = makeSemanticUnitIDBuffered(row, idBuffer)
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

type semanticWindow struct {
	start int
	end   int
	input []int
}

// semanticUnitWindows tokenizes one unit after its header and cuts the body
// tokens into windows that each fit one model input with the header.
func semanticUnitWindows(tokenizer *lateOnTokenizer, unit semanticUnit) ([]semanticWindow, error) {
	header := semanticWindowHeader(unit)
	text, rawOffset := sanitizedSemanticText(unit.text)
	bodyAt := len(lateOnDocumentPrefix) + len(header)
	ids, spans, err := tokenizer.encode(lateOnDocumentPrefix+header+text, true, true)
	if err != nil {
		return nil, err
	}
	if len(ids) != len(spans) || len(ids) < 2 || ids[0] != lateOnCLSTokenID ||
		ids[len(ids)-1] != lateOnSEPTokenID {
		return nil, fmt.Errorf("semantic window encoding of %q has an invalid layout", unit.path)
	}
	prefixEnd := 1
	for prefixEnd < len(ids) && ids[prefixEnd-1] != lateOnDocumentPrefixID {
		prefixEnd++
	}
	contentEnd := len(ids) - 1
	bodyStart := prefixEnd
	for bodyStart < contentEnd && spans[bodyStart].Start < bodyAt {
		bodyStart++
	}
	// Metadata uses at most one quarter of the content slots, as in the final
	// scorer's truncation.
	contentBudget := lateOnSequenceLength - prefixEnd - 1
	metadataTokens := min(bodyStart-prefixEnd, contentBudget/4)
	// The final scorer tokenizes a window's text again: a word cut at the
	// window edge can take one more token, so the window keeps spare slots
	// and the final model sees the whole window.
	room := contentBudget - metadataTokens - semanticWindowSpareTokens
	if room <= 0 {
		return nil, fmt.Errorf("semantic window of %q has no room for content", unit.path)
	}
	prefix := make([]int, 0, prefixEnd+metadataTokens)
	prefix = append(prefix, ids[:prefixEnd]...)
	prefix = append(prefix, ids[prefixEnd:prefixEnd+metadataTokens]...)
	body := ids[bodyStart:contentEnd]
	bodySpans := spans[bodyStart:contentEnd]
	byteAt := func(token int) int {
		if token >= len(bodySpans) {
			return len(unit.text)
		}
		return rawOffset(bodySpans[token].Start - bodyAt)
	}

	// A cut is valid between two tokens that start at different bytes, on a
	// rune start: a multi-byte character never splits between windows. The
	// tokenizer's offsets are contiguous (the space belongs to the next
	// token), so a word start is a token at or after white space.
	textAt := func(token int) int { return bodySpans[token].Start - bodyAt }
	validCut := func(cut int) bool {
		at := textAt(cut)
		return bodySpans[cut].Start > bodySpans[cut-1].Start && at > 0 && at < len(text) && utf8.RuneStart(text[at])
	}
	lineStart := func(cut int) bool {
		at := textAt(cut)
		return text[at-1] == '\n' || text[at] == '\n'
	}
	wordStart := func(cut int) bool {
		at := textAt(cut)
		return isSemanticSpace(text[at-1]) || isSemanticSpace(text[at])
	}
	windows := make([]semanticWindow, 0, max(1, (len(body)+room-1)/room))
	for first := 0; first < len(body) || len(windows) == 0; {
		last := min(first+room, len(body))
		if last < len(body) {
			last = semanticWindowCut(first, last, room, validCut, lineStart, wordStart)
		}
		start, end := byteAt(first), byteAt(last)
		if first == 0 {
			start = 0
		}
		input := make([]int, 0, len(prefix)+last-first+1)
		input = append(input, prefix...)
		input = append(input, body[first:last]...)
		input = append(input, lateOnSEPTokenID)
		if end > start || len(windows) == 0 {
			windows = append(windows, semanticWindow{start: start, end: max(end, start), input: input})
		}
		if last == first {
			break
		}
		first = last
	}
	// The last window ends at the unit end, so trailing bytes stay covered.
	windows[len(windows)-1].end = len(unit.text)
	return windows, nil
}

// semanticWindowSpareTokens is the room that a window leaves for the final
// scorer's tokenization of its text.
const semanticWindowSpareTokens = 2

// semanticWindowCut picks the end of a window that would end at last: the
// latest line start in the window's second half, else the latest word start,
// else the latest valid cut; a window with no valid cut ends at last.
func semanticWindowCut(first, last, room int, valid, lineStart, wordStart func(int) bool) int {
	for _, kind := range [...]func(int) bool{lineStart, wordStart, func(int) bool { return true }} {
		for cut := last; cut > first+room/2; cut-- {
			if valid(cut) && kind(cut) {
				return cut
			}
		}
	}
	return last
}

func isSemanticSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// semanticWindowHeader is the metadata that the model reads before each
// window: the node-relative path tail, the unit language, and its symbol. It
// is the final scorer's document header, so the vector and the final score
// read the same window text.
func semanticWindowHeader(unit semanticUnit) string {
	var header strings.Builder
	if unit.path != "" {
		fmt.Fprintf(&header, "path: %s\n", rerankPathTail(unit.path))
	}
	if unit.language != "" {
		fmt.Fprintf(&header, "language: %s\n", unit.language)
	}
	if unit.symbol != "" {
		fmt.Fprintf(&header, "symbol: %s\n", unit.symbol)
	}
	return sanitizeLateOnTokenizerInput(header.String())
}

// sanitizedSemanticText returns the tokenizer text of raw and a map from a
// byte offset in that text back to the raw offset. Invalid UTF-8 runs and NUL
// bytes become U+FFFD as in sanitizeLateOnTokenizerInput.
func sanitizedSemanticText(raw []byte) (string, func(int) int) {
	if utf8.Valid(raw) && !strings.Contains(string(raw), "\x00") {
		return string(raw), func(offset int) int { return min(max(offset, 0), len(raw)) }
	}
	var text strings.Builder
	text.Grow(len(raw) + 16)
	offsets := make([]int, 0, len(raw)+16)
	for at := 0; at < len(raw); {
		r, size := utf8.DecodeRune(raw[at:])
		if r == utf8.RuneError && size <= 1 {
			start := at
			for at < len(raw) {
				r, size = utf8.DecodeRune(raw[at:])
				if r != utf8.RuneError || size > 1 {
					break
				}
				at++
			}
			text.WriteString("�")
			for range len("�") {
				offsets = append(offsets, start)
			}
			continue
		}
		if r == 0 {
			text.WriteString("�")
			for range len("�") {
				offsets = append(offsets, at)
			}
		} else {
			text.Write(raw[at : at+size])
			for index := range size {
				offsets = append(offsets, at+index)
			}
		}
		at += size
	}
	return text.String(), func(offset int) int {
		if offset < 0 {
			return 0
		}
		if offset >= len(offsets) {
			return len(raw)
		}
		return offsets[offset]
	}
}
