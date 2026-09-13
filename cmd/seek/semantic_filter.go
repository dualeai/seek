package main

import (
	"bytes"
	"context"
	"fmt"
	mathbits "math/bits"
	"unicode"
	"unicode/utf8"

	grafanaregexp "github.com/grafana/regexp"
	"github.com/sourcegraph/zoekt/query"
)

// semanticFilterPlan holds the supported nodes from Zoekt's simplified query
// tree. All language and path entries are ANDed. Paths match stored corpus
// paths, which use slash separators.
type semanticFilterPlan struct {
	languages []string
	paths     []semanticPathFilter
}

type semanticPathFilter struct {
	substring     string
	loweredRunes  []rune
	regexp        *grafanaregexp.Regexp
	negated       bool
	caseSensitive bool
}

// compileSemanticFilterPlan accepts positive language nodes and positive or
// negated filename substring or regular-expression nodes. It rejects all other
// nodes.
func compileSemanticFilterPlan(nodes []query.Q) (*semanticFilterPlan, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("semantic filter is empty")
	}
	plan := &semanticFilterPlan{}
	for _, node := range nodes {
		negated := false
		if not, ok := node.(*query.Not); ok {
			negated = true
			node = not.Child
		}
		switch typed := node.(type) {
		case *query.Language:
			if negated || typed.Language == "" {
				return nil, fmt.Errorf("unsupported semantic language filter")
			}
			plan.languages = append(plan.languages, typed.Language)
		case *query.Substring:
			matcher, err := newSemanticSubstringFilter(typed, negated)
			if err != nil {
				return nil, err
			}
			plan.paths = append(plan.paths, matcher)
		case *query.Regexp:
			matcher, err := newSemanticRegexpFilter(typed, negated)
			if err != nil {
				return nil, err
			}
			plan.paths = append(plan.paths, matcher)
		default:
			return nil, fmt.Errorf("unsupported semantic filter node %T", node)
		}
	}
	return plan, nil
}

func newSemanticSubstringFilter(node *query.Substring, negated bool) (semanticPathFilter, error) {
	if node == nil || !node.FileName || node.Content || node.Pattern == "" {
		return semanticPathFilter{}, fmt.Errorf("invalid semantic filename substring")
	}
	filter := semanticPathFilter{
		substring:     node.Pattern,
		negated:       negated,
		caseSensitive: node.CaseSensitive,
	}
	if !node.CaseSensitive {
		filter.loweredRunes = make([]rune, 0, utf8.RuneCountInString(node.Pattern))
		for _, character := range node.Pattern {
			filter.loweredRunes = append(filter.loweredRunes, unicode.ToLower(character))
		}
		if len(filter.loweredRunes) < 3 {
			compiled, err := grafanaregexp.Compile("(?i)" + grafanaregexp.QuoteMeta(node.Pattern))
			if err != nil {
				return semanticPathFilter{}, fmt.Errorf("compile semantic filename substring: %w", err)
			}
			filter.regexp = compiled
		}
	}
	return filter, nil
}

func newSemanticRegexpFilter(node *query.Regexp, negated bool) (semanticPathFilter, error) {
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
	return semanticPathFilter{regexp: compiled, negated: negated}, nil
}

func (plan *semanticFilterPlan) matches(path, language string) bool {
	if plan == nil {
		return true
	}
	for _, want := range plan.languages {
		if language != want {
			return false
		}
	}
	for _, filter := range plan.paths {
		matched := filter.matches(path)
		if matched == filter.negated {
			return false
		}
	}
	return true
}

func (filter semanticPathFilter) matches(path string) bool {
	if filter.regexp != nil {
		return filter.regexp.MatchString(path)
	}
	if filter.caseSensitive {
		return bytes.Contains([]byte(path), []byte(filter.substring))
	}
	return semanticFoldedSubstring(path, filter.loweredRunes)
}

// semanticFoldedSubstring matches Zoekt's long case-insensitive substring
// check. It lowers each rune with unicode.ToLower instead of using a regular
// expression's SimpleFold cycle.
func semanticFoldedSubstring(path string, lowered []rune) bool {
	if len(lowered) == 0 {
		return false
	}
	for start := range path {
		remaining := path[start:]
		matched := true
		for _, want := range lowered {
			if len(remaining) == 0 {
				matched = false
				break
			}
			got, size := utf8.DecodeRuneInString(remaining)
			if unicode.ToLower(got) != want {
				matched = false
				break
			}
			remaining = remaining[size:]
		}
		if matched {
			return true
		}
	}
	return false
}

type semanticFilterMode uint8

const (
	semanticFilterAll semanticFilterMode = iota + 1
	semanticFilterNone
	semanticFilterPartial
)

// semanticFilterMask has one bit for each global semantic row. ALL and NONE
// omit bits and shardAllowed. PARTIAL owns exactly (rows+7)/8 bitmap bytes and
// one count for each USearch shard in manifest order. allowed counts rows.
// exactRows contains the complete sorted selection only when its length equals
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
const semanticFilteredExactRows = 256

// buildSemanticFilterMask evaluates each adjacent group of rows for one file
// once. It writes bitmap ranges and shard counts during that same pass.
func buildSemanticFilterMask(
	ctx context.Context,
	generation *semanticGeneration,
	filter *semanticFilterPlan,
) (semanticFilterMask, error) {
	if generation == nil || filter == nil {
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
			if generation.rows[end].fileLanguage != first.fileLanguage {
				return semanticFilterMask{}, fmt.Errorf("semantic file %q has inconsistent language", first.path)
			}
			end++
		}
		matchedFile := filter.matches(first.path, first.fileLanguage)
		if matchedFile {
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

func (mask semanticFilterMask) allows(row uint64) bool {
	if mask.mode == semanticFilterAll {
		return row < mask.rows
	}
	return mask.mode == semanticFilterPartial && row < mask.rows && row/8 < uint64(len(mask.bits)) &&
		mask.bits[row/8]&(1<<(row&7)) != 0
}

func (mask semanticFilterMask) selectedRows() []uint64 {
	if uint64(len(mask.exactRows)) == mask.allowed {
		return mask.exactRows
	}
	return mask.selectedRowsInRange(0, mask.rows)
}

func (mask semanticFilterMask) selectedRowsInRange(start, end uint64) []uint64 {
	end = min(end, mask.rows)
	if start >= end {
		return nil
	}
	rows := make([]uint64, 0, int(min(mask.allowed, end-start)))
	return mask.appendSelectedRowsInRange(rows, start, end)
}

func (mask semanticFilterMask) appendSelectedRowsInRange(rows []uint64, start, end uint64) []uint64 {
	end = min(end, mask.rows)
	if start >= end {
		return rows
	}
	if mask.mode == semanticFilterAll {
		for row := start; row < end; row++ {
			rows = append(rows, row)
		}
		return rows
	}
	if mask.mode != semanticFilterPartial {
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
