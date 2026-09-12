package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sort"
	"strings"
	"unicode/utf8"

	ctags "github.com/sourcegraph/go-ctags"
)

const (
	// semanticUnitMaxBytes bounds copied source while keeping enough code for
	// the 128-token model input. It is a format value, not a worker setting.
	semanticUnitMaxBytes = 16 << 10
	semanticExtractorID  = "ctags-gaps-v4-source-skips"
)

type semanticUnitKind uint8

const (
	semanticUnitGap semanticUnitKind = iota + 1
	semanticUnitSymbol
	semanticUnitFallback
	semanticUnitPacked
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
	language     string
	symbol       string
	text         []byte
	// modelInput is transient. The exact packer supplies the token IDs that the
	// encoder consumes, then the collector clears them before storage.
	modelInput []int
}

type semanticTagParser interface {
	Parse(path string, content []byte) ([]*ctags.Entry, error)
	Close()
}

type semanticAnchor struct {
	offset   int
	language string
	symbol   string
}

// extractSemanticUnits covers each eligible non-empty source byte once. It
// excludes binary and generated files. Valid ctags starts create symbol units.
// Prefixes and parser gaps create gap units. A parser failure or a file without
// valid symbols creates fallback units.
func extractSemanticUnits(
	path string,
	content []byte,
	entries []*ctags.Entry,
	parseErr error,
) []semanticUnit {
	if len(content) == 0 || semanticContentIsBinary(content) || semanticContentIsGenerated(content) {
		return nil
	}
	contentID := semanticContentID(sha256.Sum256(content))
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
	sort.Slice(anchors, func(i, j int) bool {
		if anchors[i].offset != anchors[j].offset {
			return anchors[i].offset < anchors[j].offset
		}
		if anchors[i].symbol != anchors[j].symbol {
			return anchors[i].symbol < anchors[j].symbol
		}
		return anchors[i].language < anchors[j].language
	})
	anchors = compactSemanticAnchors(anchors)
	if len(anchors) == 0 {
		if parserResult == semanticParserCTags {
			parserResult = semanticParserNoSymbols
		}
		return appendSemanticSpan(nil, path, content, contentID, 0, len(content),
			semanticUnitFallback, parserResult, "", "")
	}

	units := make([]semanticUnit, 0, len(anchors)+1)
	if anchors[0].offset > 0 {
		units = appendSemanticSpan(units, path, content, contentID, 0,
			anchors[0].offset, semanticUnitGap, parserResult, "", "")
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
			current.language, current.symbol)
	}
	return units
}

// semanticContentIsBinary matches the standard source-index boundary for the
// semantic branch. A NUL byte cannot occur in normal text source and avoids
// sending binary payloads through ctags, tokenization, and the model.
func semanticContentIsBinary(content []byte) bool {
	return bytes.IndexByte(content, 0) >= 0
}

// semanticContentIsGenerated recognizes the source marker defined by the Go
// generated-code convention in the first 4 KiB. Zoekt keeps these files on its
// complete lexical path. The semantic branch omits them because generated
// symbols add duplicate model work and often reduce descriptive-search
// precision.
func semanticContentIsGenerated(content []byte) bool {
	header := content[:min(len(content), 4096)]
	for len(header) > 0 {
		line := header
		if end := bytes.IndexByte(header, '\n'); end >= 0 {
			line = header[:end]
			header = header[end+1:]
		} else {
			header = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.HasPrefix(line, []byte("// Code generated ")) &&
			bytes.HasSuffix(line, []byte(" DO NOT EDIT.")) {
			return true
		}
	}
	return false
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

func compactSemanticAnchors(anchors []semanticAnchor) []semanticAnchor {
	if len(anchors) < 2 {
		return anchors
	}
	out := anchors[:1]
	lastOffset := anchors[0].offset
	for _, item := range anchors[1:] {
		if item.offset == lastOffset {
			continue
		}
		out = append(out, item)
		lastOffset = item.offset
	}
	return out
}

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
		unit.id = makeSemanticUnitID(unit)
		units = append(units, unit)
		start = partEnd
	}
	return units
}

func makeSemanticUnitID(unit semanticUnit) semanticUnitID {
	hash := sha256.New()
	hash.Write([]byte("seek-semantic-unit-v1\x00"))
	writeSemanticHashField(hash, []byte(unit.path))
	writeSemanticHashField(hash, unit.contentID[:])
	var number [8]byte
	binary.LittleEndian.PutUint64(number[:], unit.start)
	hash.Write(number[:])
	binary.LittleEndian.PutUint64(number[:], unit.end)
	hash.Write(number[:])
	hash.Write([]byte{byte(unit.kind), byte(unit.parserResult)})
	writeSemanticHashField(hash, []byte(unit.language))
	writeSemanticHashField(hash, []byte(unit.symbol))
	var id semanticUnitID
	copy(id[:], hash.Sum(nil))
	return id
}

func writeSemanticHashField(writer hash.Hash, value []byte) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}
