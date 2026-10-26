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
	"github.com/sourcegraph/zoekt/index"
)

const (
	// semanticUnitMaxBytes bounds the source span of one unit; the window
	// split then cuts each unit into 128-token model inputs. It is a format
	// value, not a worker setting.
	semanticUnitMaxBytes = 16 << 10
	// semanticExtractorID names the extraction rule, eligibility included
	// (semanticContentIsEligible). The generation key and the manifest check
	// read it, so a generation of another rule is built again.
	semanticExtractorID = "ctags-gaps-v7-windows-zoekt-text-zoekt-language-half-format"
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
	// pathCut is transient. It marks a row whose window header ends inside
	// its path line, so the key holds only a prefix of the model path tail.
	pathCut bool
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
	offset int
	symbol string
}

// extractSemanticUnitsWithID splits content into units. contentID is the
// SHA-256 of content, from the caller. The caller has checked that content is
// eligible (semanticContentIsEligible). The units cover each source byte
// once, in parts of at most semanticUnitMaxBytes. Valid ctags starts create
// symbol units, and the bytes before the first start create a gap unit. A
// parser failure, invalid UTF-8, or a file without valid symbols creates
// fallback units. Every unit takes the file's language (semanticFileLanguage).
func extractSemanticUnitsWithID(
	path string,
	content []byte,
	contentID semanticContentID,
	entries []*ctags.Entry,
	parseErr error,
) []semanticUnit {
	var idBuffer []byte
	language := semanticFileLanguage(path, content)
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
		anchors = append(anchors, semanticAnchor{offset: lineStarts[entry.Line-1], symbol: symbol})
	}
	slices.SortFunc(anchors, func(a, b semanticAnchor) int {
		return cmp.Or(cmp.Compare(a.offset, b.offset), strings.Compare(a.symbol, b.symbol))
	})
	// The first anchor of each offset, by symbol, starts the unit.
	anchors = slices.CompactFunc(anchors, func(a, b semanticAnchor) bool { return a.offset == b.offset })
	if len(anchors) == 0 {
		if parserResult == semanticParserCTags {
			parserResult = semanticParserNoSymbols
		}
		return appendSemanticSpan(nil, path, content, contentID, 0, len(content),
			semanticUnitFallback, parserResult, language, "", &idBuffer)
	}

	units := make([]semanticUnit, 0, len(anchors)+1)
	if anchors[0].offset > 0 {
		units = appendSemanticSpan(units, path, content, contentID, 0,
			anchors[0].offset, semanticUnitGap, parserResult, language, "", &idBuffer)
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
			language, current.symbol, &idBuffer)
	}
	return units
}

// semanticFileLanguage names the language of a file as Zoekt names it in the
// shard (index.DetermineLanguageIfUnknown). So a vector hit, its model input
// and a strict hit of the same file all name one language. ctags names some
// languages in its own way ("Sh" for "Shell").
func semanticFileLanguage(path string, content []byte) string {
	document := index.Document{Name: path, Content: content}
	index.DetermineLanguageIfUnknown(&document)
	return document.Language
}

// semanticContentIsEligible reports whether content gets semantic rows: only
// when Zoekt keeps the content with the options of indexBuildOptions (SizeMax
// and TrigramMax at documentContentLimit). Zoekt's own check decides. It keeps
// a file of 1 or 2 bytes, or a file with a NUL byte, by name only
// (index.SkipReasonTooSmall, index.SkipReasonBinary). A semantic hit shows the
// text that Zoekt keeps (readSemanticEvidence), so such a file gets no rows. An
// empty file has no text to describe.
//
// A file of more than half the shard format gets no rows either. Zoekt stores
// its content and at least one posting byte for each position. So its shard
// passes the format (2.01 bytes of shard for each byte of the most repetitive
// content). The shard guard then keeps it by name only (guardWrappedShards).
// A smaller file can still pass the format (6.6 bytes for each byte of random
// text); its rows then give no hit.
func semanticContentIsEligible(content []byte) bool {
	if len(content) == 0 || !documentContentFits(int64(len(content))) || 2*int64(len(content)) > shardFormatLimit {
		return false
	}
	// With TrigramMax at the content limit, the check does not count
	// trigrams, so its zero value allocates nothing.
	var checker index.DocChecker
	return checker.Check(content, int(documentContentLimit), false) == index.SkipReasonNone
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
			// The caller owns content until it finishes encoding this unit.
			// Keeping a view avoids a second full-file allocation during index
			// builds.
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

// makeSemanticUnitIDBuffered hashes the stable v1 byte encoding of unit. The
// encoding starts with the version prefix, the length-prefixed path and
// content ID, and the little-endian start and end. Then come the kind and
// parser-result bytes and the length-prefixed unit language and symbol.
// buffer is caller-owned scratch storage. The returned slice can be reused and
// is not part of the ID.
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
