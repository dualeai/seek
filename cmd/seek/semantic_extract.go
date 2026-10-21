package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"slices"
	"strings"
	"unicode/utf8"

	ctags "github.com/sourcegraph/go-ctags"
)

const (
	// semanticUnitMaxBytes bounds the source span of one unit; the window
	// split then cuts each unit into 128-token model inputs. It is a format
	// value, not a worker setting.
	semanticUnitMaxBytes = 16 << 10
	semanticExtractorID  = "ctags-gaps-v6-windows"
)

type semanticUnitKind uint8

const (
	semanticUnitGap semanticUnitKind = iota + 1
	semanticUnitSymbol
	semanticUnitFallback
)

type semanticParserResult uint8

const (
	semanticParserCTags semanticParserResult = iota + 1
	semanticParserNoSymbols
	semanticParserFailed
	semanticParserInvalidUTF8
)

type semanticContentID [sha256.Size]byte
type semanticUnitID [sha256.Size]byte

type semanticUnit struct {
	row          uint64
	id           semanticUnitID
	path         string
	contentID    semanticContentID
	start        uint64
	end          uint64
	kind         semanticUnitKind
	parserResult semanticParserResult
	// language describes this extracted unit in model and result documents.
	language string
	symbol   string
	text     []byte
	// key identifies the row's model input (makeSemanticRowKey): a build
	// copies the vectors of a key that a live generation or the unit store
	// holds.
	key semanticRowKey
	// modelInput is transient. The window split supplies the token IDs that
	// the encoder consumes; the build clears them, text, and reuse in the
	// stored copy of each row.
	modelInput []int
	// reuse is transient: the live generation row or unit store record whose
	// vectors this row copies, or nil when the model embeds it.
	reuse *semanticReuseRef
}

type semanticAnchor struct {
	offset   int
	language string
	symbol   string
}

// extractSemanticUnitsWithID extracts content whose SHA-256 the caller has
// (contentID). The caller has checked that content is eligible
// (semanticContentIsEligible). The units cover each source byte once, in
// parts of at most semanticUnitMaxBytes. Valid ctags starts create symbol
// units, and the bytes before the first start create a gap unit. A parser
// failure, invalid UTF-8, or a file without valid symbols creates fallback
// units.
func extractSemanticUnitsWithID(
	path string,
	content []byte,
	contentID semanticContentID,
	entries []*ctags.Entry,
	parseErr error,
) []semanticUnit {
	var idBuffer []byte
	parserResult := semanticParserCTags
	switch {
	case !utf8.Valid(content):
		parserResult = semanticParserInvalidUTF8
	case parseErr != nil:
		parserResult = semanticParserFailed
	}

	anchors := make([]semanticAnchor, 0, len(entries))
	lineStarts := semanticLineStarts(content)
	if parserResult != semanticParserCTags {
		entries = nil
	}
	for _, entry := range entries {
		if entry == nil || entry.Line < 1 || entry.Line > len(lineStarts) {
			continue
		}
		symbol := semanticSymbolMetadata(entry)
		if symbol == "" {
			continue
		}
		anchors = append(anchors, semanticAnchor{
			offset:   lineStarts[entry.Line-1],
			language: entry.Language,
			symbol:   symbol,
		})
	}
	slices.SortFunc(anchors, func(a, b semanticAnchor) int {
		return cmp.Or(cmp.Compare(a.offset, b.offset), strings.Compare(a.symbol, b.symbol),
			strings.Compare(a.language, b.language))
	})
	// The first anchor of each offset, by symbol then language, starts the unit.
	anchors = slices.CompactFunc(anchors, func(a, b semanticAnchor) bool { return a.offset == b.offset })
	if len(anchors) == 0 {
		if parserResult == semanticParserCTags {
			parserResult = semanticParserNoSymbols
		}
		return appendSemanticSpan(nil, path, content, contentID, 0, len(content),
			semanticUnitFallback, parserResult, "", "", &idBuffer)
	}

	units := make([]semanticUnit, 0, len(anchors)+1)
	if anchors[0].offset > 0 {
		units = appendSemanticSpan(units, path, content, contentID, 0,
			anchors[0].offset, semanticUnitGap, parserResult, "", "", &idBuffer)
	}
	for i, current := range anchors {
		end := len(content)
		if i+1 < len(anchors) {
			end = anchors[i+1].offset
		}
		if end <= current.offset {
			continue
		}
		units = appendSemanticSpan(units, path, content, contentID,
			current.offset, end, semanticUnitSymbol, parserResult,
			current.language, current.symbol, &idBuffer)
	}
	return units
}

func semanticContentIsEligible(content []byte) bool {
	return len(content) > 0 && !semanticContentIsBinary(content)
}

// semanticContentIsBinary matches the standard source-index boundary for the
// semantic branch. A NUL byte cannot occur in normal text source and avoids
// sending binary payloads through ctags, tokenization, and the model.
func semanticContentIsBinary(content []byte) bool {
	return bytes.IndexByte(content, 0) >= 0
}

func semanticSymbolMetadata(entry *ctags.Entry) string {
	if entry == nil {
		return ""
	}
	parts := make([]string, 0, 5)
	for _, value := range []string{
		entry.ParentKind,
		entry.Parent,
		entry.Kind,
		entry.Name,
		entry.Signature,
	} {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " ")
}

func semanticLineStarts(content []byte) []int {
	starts := []int{0}
	for i, value := range content {
		if value == '\n' && i+1 < len(content) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// appendSemanticSpan splits one source span and assigns a stable ID to each
// part. idBuffer must point to caller-owned scratch storage. The caller reuses
// that storage for all spans from one source file.
func appendSemanticSpan(
	units []semanticUnit,
	path string,
	content []byte,
	contentID semanticContentID,
	start int,
	end int,
	kind semanticUnitKind,
	parserResult semanticParserResult,
	language string,
	symbol string,
	idBuffer *[]byte,
) []semanticUnit {
	for start < end {
		partEnd := min(start+semanticUnitMaxBytes, end)
		if partEnd < end {
			if newline := bytes.LastIndexByte(content[start:partEnd], '\n'); newline >= semanticUnitMaxBytes/4 {
				partEnd = start + newline + 1
			}
		}
		unit := semanticUnit{
			path:         path,
			contentID:    contentID,
			start:        uint64(start),
			end:          uint64(partEnd),
			kind:         kind,
			parserResult: parserResult,
			language:     language,
			symbol:       symbol,
			// The caller owns content until it finishes encoding this unit. Keeping
			// a view avoids a second full-file allocation during index builds.
			text: content[start:partEnd],
		}
		unit.id, *idBuffer = makeSemanticUnitIDBuffered(unit, *idBuffer)
		units = append(units, unit)
		start = partEnd
	}
	return units
}

func makeSemanticUnitID(unit semanticUnit) semanticUnitID {
	id, _ := makeSemanticUnitIDBuffered(unit, nil)
	return id
}

// makeSemanticUnitIDBuffered hashes the stable v1 byte encoding. The encoding
// contains the version prefix; length-prefixed path and content ID; little-
// endian start and end; kind and parser-result bytes; and length-prefixed unit
// language and symbol, in that order. buffer is caller-owned scratch storage.
// The returned slice can be reused and is not part of the ID.
func makeSemanticUnitIDBuffered(unit semanticUnit, buffer []byte) (semanticUnitID, []byte) {
	const prefix = "seek-semantic-unit-v1\x00"
	wantBytes := len(prefix) + 4*8 + len(unit.path) + len(unit.contentID) + 2*8 + 2 +
		len(unit.language) + len(unit.symbol)
	buffer = slices.Grow(buffer[:0], wantBytes)
	buffer = append(buffer, prefix...)
	buffer = appendSemanticUnitIDField(buffer, unit.path)
	buffer = appendSemanticUnitIDField(buffer, unit.contentID[:])
	buffer = binary.LittleEndian.AppendUint64(buffer, unit.start)
	buffer = binary.LittleEndian.AppendUint64(buffer, unit.end)
	buffer = append(buffer, byte(unit.kind), byte(unit.parserResult))
	buffer = appendSemanticUnitIDField(buffer, unit.language)
	buffer = appendSemanticUnitIDField(buffer, unit.symbol)
	return semanticUnitID(sha256.Sum256(buffer)), buffer
}

// appendSemanticUnitIDField appends the little-endian length of value, then
// value.
func appendSemanticUnitIDField[T ~string | ~[]byte](buffer []byte, value T) []byte {
	buffer = binary.LittleEndian.AppendUint64(buffer, uint64(len(value)))
	return append(buffer, value...)
}

func writeSemanticHashField(writer hash.Hash, value []byte) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}
