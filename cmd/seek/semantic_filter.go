package main

import (
	"context"
	mathbits "math/bits"
	"strings"
	"unicode/utf8"

	grafanaregexp "github.com/grafana/regexp"
	"github.com/sourcegraph/zoekt/query"
)

// semanticPathFilter matches one file: atom as Zoekt does. A case-insensitive
// substring of 3 runes or more holds the pattern in lowercase.
type semanticPathFilter struct {
	substring     string
	regexp        *grafanaregexp.Regexp
	caseSensitive bool
}

// newSemanticSubstringFilter builds the filter of a file: substring. Zoekt
// makes the pattern from the literal of a parsed regular expression, so it is
// valid UTF-8, and its quoted form always compiles.
func newSemanticSubstringFilter(node *query.Substring) semanticPathFilter {
	filter := semanticPathFilter{
		substring:     node.Pattern,
		caseSensitive: node.CaseSensitive,
	}
	if !node.CaseSensitive {
		if utf8.RuneCountInString(node.Pattern) < 3 {
			filter.regexp = grafanaregexp.MustCompile("(?i)" + grafanaregexp.QuoteMeta(node.Pattern))
		} else {
			filter.substring = strings.ToLower(node.Pattern)
		}
	}
	return filter
}

// newSemanticRegexpFilter builds the filter of a file: regular expression. It
// compiles the pattern that compileRegexpAtoms compiled when it parsed the
// query, so the compile cannot fail.
func newSemanticRegexpFilter(node *query.Regexp) semanticPathFilter {
	prefix := ""
	if !node.CaseSensitive {
		prefix = "(?i)"
	}
	return semanticPathFilter{regexp: grafanaregexp.MustCompile(prefix + node.RegexpString())}
}

// newSemanticPathFilter builds the filter of one file: atom. The caller gives
// only an atom that fileNameAtom accepts: a substring or a regular expression
// with FileName set and Content not set. The atom is an explicit file: atom of
// the parsed query. The strict query holds each of them, because
// query.ExpandFileContent leaves a file: atom as it is. The relaxed and filter
// queries hold no other file: atom (planRerankQuery).
func newSemanticPathFilter(atom query.Q) semanticPathFilter {
	if substring, ok := atom.(*query.Substring); ok {
		return newSemanticSubstringFilter(substring)
	}
	return newSemanticRegexpFilter(atom.(*query.Regexp))
}

// matches reports whether path matches the filter. A long case-insensitive
// substring uses Zoekt's fold: strings.ToLower maps each rune with
// unicode.ToLower and each invalid byte to U+FFFD, as Zoekt's toLower does.
// Both lowered strings are valid UTF-8, so a byte match starts on a rune.
func (filter semanticPathFilter) matches(path string) bool {
	switch {
	case filter.regexp != nil:
		return filter.regexp.MatchString(path)
	case filter.caseSensitive:
		return strings.Contains(path, filter.substring)
	default:
		return strings.Contains(strings.ToLower(path), filter.substring)
	}
}

// semanticFilterMask has one bit for each global semantic row. allowed counts
// the selected rows. A mask that selects no row (allowed is 0) or every row
// (allowed equals rows) has no bits and no shardAllowed. A partial mask has
// (rows+7)/8 bitmap bytes and one count for each USearch shard, in manifest
// order. exactRows holds the complete sorted selection only when its length
// equals allowed.
type semanticFilterMask struct {
	bits         []byte
	rows         uint64
	allowed      uint64
	shardAllowed []uint64
	exactRows    []uint64
}

// semanticFilteredExactRows is the largest complete semantic-row selection
// that Seek scores directly without planning individual shard routes.
// TODO(https://github.com/unum-cloud/USearch/issues/817): let USearch score
// these rows once its C API has an exact filtered search.
const semanticFilteredExactRows = 256

// buildSemanticFilterMask evaluates keep once for each adjacent group of rows
// of one file. It writes bitmap ranges and shard counts during that same pass,
// for the bitmap filter of USearch's filtered search. The open of a
// generation checks that each row holds its index and a path
// (readSemanticRowsFrom), and that the shards cover the rows in order
// (validateSemanticUSearchShardMetadata). So the mask does not check them
// again.
func buildSemanticFilterMask(
	ctx context.Context,
	generation *semanticGeneration,
	keep func(path string) bool,
) (semanticFilterMask, error) {
	rowCount := uint64(len(generation.rows))
	shards := generation.manifest.USearchFiles
	var bits []byte
	var shardAllowed []uint64
	var exactRows []uint64
	exactRowsComplete := true
	var allowed uint64
	shardAt := 0
	seenMatch := false
	seenMiss := false
	for at := 0; at < len(generation.rows); {
		if err := ctx.Err(); err != nil {
			return semanticFilterMask{}, err
		}
		first := generation.rows[at]
		end := at + 1
		for end < len(generation.rows) && generation.rows[end].path == first.path {
			if end&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return semanticFilterMask{}, err
				}
			}
			end++
		}
		if keep(first.path) {
			seenMatch = true
			startRow, endRow := uint64(at), uint64(end)
			if seenMiss {
				if bits == nil {
					bits = make([]byte, (rowCount+7)/8)
					shardAllowed = make([]uint64, len(shards))
				}
				setSemanticFilterRange(bits, startRow, endRow)
				addSemanticFilterShardRange(shards, shardAllowed, &shardAt, startRow, endRow)
			}
			matched := endRow - startRow
			if exactRowsComplete {
				if allowed+matched <= semanticFilteredExactRows {
					if exactRows == nil {
						exactRows = make(
							[]uint64, 0, min(uint64(semanticFilteredExactRows), rowCount),
						)
					}
					for row := startRow; row < endRow; row++ {
						exactRows = append(exactRows, row)
					}
				} else {
					exactRows = nil
					exactRowsComplete = false
				}
			}
			allowed += matched
		} else if !seenMiss {
			seenMiss = true
			if seenMatch {
				bits = make([]byte, (rowCount+7)/8)
				shardAllowed = make([]uint64, len(shards))
				setSemanticFilterRange(bits, 0, uint64(at))
				addSemanticFilterShardRange(shards, shardAllowed, &shardAt, 0, uint64(at))
			}
		}
		at = end
	}
	if allowed == 0 || allowed == rowCount {
		return semanticFilterMask{rows: rowCount, allowed: allowed}, nil
	}
	return semanticFilterMask{
		bits: bits, rows: rowCount,
		allowed: allowed, shardAllowed: shardAllowed, exactRows: exactRows,
	}, nil
}

// partial reports whether the mask selects some rows, but not all of them.
func (mask semanticFilterMask) partial() bool {
	return mask.allowed != 0 && mask.allowed != mask.rows
}

func setSemanticFilterRange(bits []byte, start, end uint64) {
	for start < end && start&7 != 0 {
		bits[start/8] |= byte(1 << (start & 7))
		start++
	}
	for end-start >= 8 {
		bits[start/8] = 0xff
		start += 8
	}
	for start < end {
		bits[start/8] |= byte(1 << (start & 7))
		start++
	}
}

func addSemanticFilterShardRange(
	shards []semanticUSearchShard,
	counts []uint64,
	shardAt *int,
	start, end uint64,
) {
	for *shardAt < len(shards) && start >= shards[*shardAt].Start+shards[*shardAt].Rows {
		(*shardAt)++
	}
	row, index := start, *shardAt
	for row < end && index < len(shards) {
		shardEnd := shards[index].Start + shards[index].Rows
		partEnd := min(end, shardEnd)
		counts[index] += partEnd - row
		row = partEnd
		if row == shardEnd {
			index++
		}
	}
	*shardAt = index
}

// allows reports whether a partial mask selects row. Every caller holds a
// partial mask: a mask without bits allows no row.
func (mask semanticFilterMask) allows(row uint64) bool {
	return row < mask.rows && row/8 < uint64(len(mask.bits)) &&
		mask.bits[row/8]&(1<<(row&7)) != 0
}

// selectedRows returns the sorted rows that a partial mask selects.
func (mask semanticFilterMask) selectedRows() []uint64 {
	if uint64(len(mask.exactRows)) == mask.allowed {
		return mask.exactRows
	}
	return mask.appendSelectedRowsInRange(make([]uint64, 0, min(mask.allowed, mask.rows)), 0, mask.rows)
}

// appendSelectedRowsInRange appends the rows in [start, end) that a partial
// mask selects. A mask without bits appends no row.
func (mask semanticFilterMask) appendSelectedRowsInRange(rows []uint64, start, end uint64) []uint64 {
	end = min(end, mask.rows)
	if start >= end {
		return rows
	}
	firstByte := start / 8
	if firstByte >= uint64(len(mask.bits)) {
		return rows
	}
	lastByte := (end - 1) / 8
	lastByte = min(lastByte, uint64(len(mask.bits)-1))
	for byteIndex := firstByte; byteIndex <= lastByte; byteIndex++ {
		value := mask.bits[byteIndex]
		if byteIndex == firstByte {
			value &= byte(0xff << (start & 7))
		}
		if byteIndex == lastByte && end&7 != 0 {
			value &= byte(1<<(end&7)) - 1
		}
		for value != 0 {
			bit := uint64(mathbits.TrailingZeros8(value))
			rows = append(rows, byteIndex*8+bit)
			value &= value - 1
		}
	}
	return rows
}
