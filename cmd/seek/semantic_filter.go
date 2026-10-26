package main

import (
	"context"
	"fmt"
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

func newSemanticSubstringFilter(node *query.Substring) (semanticPathFilter, error) {
	if node == nil || !node.FileName || node.Content || node.Pattern == "" {
		return semanticPathFilter{}, fmt.Errorf("invalid semantic filename substring")
	}
	filter := semanticPathFilter{
		substring:     node.Pattern,
		caseSensitive: node.CaseSensitive,
	}
	if !node.CaseSensitive {
		if utf8.RuneCountInString(node.Pattern) < 3 {
			compiled, err := grafanaregexp.Compile("(?i)" + grafanaregexp.QuoteMeta(node.Pattern))
			if err != nil {
				return semanticPathFilter{}, fmt.Errorf("compile semantic filename substring: %w", err)
			}
			filter.regexp = compiled
		} else {
			filter.substring = strings.ToLower(node.Pattern)
		}
	}
	return filter, nil
}

func newSemanticRegexpFilter(node *query.Regexp) (semanticPathFilter, error) {
	if node == nil || !node.FileName || node.Content || node.Regexp == nil {
		return semanticPathFilter{}, fmt.Errorf("invalid semantic filename regular expression")
	}
	prefix := ""
	if !node.CaseSensitive {
		prefix = "(?i)"
	}
	compiled, err := grafanaregexp.Compile(prefix + node.RegexpString())
	if err != nil {
		return semanticPathFilter{}, fmt.Errorf("compile semantic filename regular expression: %w", err)
	}
	return semanticPathFilter{regexp: compiled}, nil
}

// newSemanticPathFilter builds the filter of one file: atom, a substring or a
// regular expression (fileNameAtom).
func newSemanticPathFilter(atom query.Q) (semanticPathFilter, error) {
	switch typed := atom.(type) {
	case *query.Substring:
		return newSemanticSubstringFilter(typed)
	case *query.Regexp:
		return newSemanticRegexpFilter(typed)
	}
	return semanticPathFilter{}, fmt.Errorf("file atom %s is %T, not a substring or a regular expression", atom, atom)
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

type semanticFilterMode uint8

const (
	semanticFilterAll semanticFilterMode = iota + 1
	semanticFilterNone
	semanticFilterPartial
)

// semanticFilterMask has one bit for each global semantic row. The modes
// semanticFilterAll and semanticFilterNone have no bits and no shardAllowed. A
// semanticFilterPartial mask has (rows+7)/8 bitmap bytes and one count for each
// USearch shard, in manifest order. allowed counts the selected rows.
// exactRows holds the complete sorted selection only when its length equals
// allowed.
type semanticFilterMask struct {
	mode         semanticFilterMode
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
// for the bitmap filter of USearch's filtered search.
func buildSemanticFilterMask(
	ctx context.Context,
	generation *semanticGeneration,
	keep func(path string) bool,
) (semanticFilterMask, error) {
	if generation == nil || keep == nil {
		return semanticFilterMask{}, fmt.Errorf("semantic filter input is missing")
	}
	rowCount := uint64(len(generation.rows))
	if generation.manifest.Rows != rowCount {
		return semanticFilterMask{}, fmt.Errorf("semantic row count does not match its manifest")
	}
	shards := generation.manifest.USearchFiles
	if len(shards) > 0 {
		var next uint64
		for index, shard := range shards {
			if shard.Start != next || shard.Rows > rowCount-shard.Start {
				return semanticFilterMask{}, fmt.Errorf("semantic shard %d is outside its row set", index)
			}
			next = shard.Start + shard.Rows
		}
		if next != rowCount {
			return semanticFilterMask{}, fmt.Errorf("semantic shards do not cover their row set")
		}
	}
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
		if first.row != uint64(at) || first.path == "" {
			return semanticFilterMask{}, fmt.Errorf("semantic row %d has invalid identity", at)
		}
		end := at + 1
		for end < len(generation.rows) && generation.rows[end].path == first.path {
			if end&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return semanticFilterMask{}, err
				}
			}
			if generation.rows[end].row != uint64(end) {
				return semanticFilterMask{}, fmt.Errorf("semantic row %d has invalid identity", end)
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
	switch allowed {
	case 0:
		return semanticFilterMask{mode: semanticFilterNone, rows: rowCount}, nil
	case rowCount:
		return semanticFilterMask{
			mode: semanticFilterAll, rows: rowCount, allowed: allowed,
		}, nil
	default:
		return semanticFilterMask{
			mode: semanticFilterPartial, bits: bits, rows: rowCount,
			allowed: allowed, shardAllowed: shardAllowed, exactRows: exactRows,
		}, nil
	}
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
