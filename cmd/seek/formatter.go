package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"

	"github.com/sourcegraph/zoekt"
)

// formatResults formats zoekt FileMatch results into the output format.
//
// Pipeline:
//  1. Deduplicate — uncommitted wins over committed; committed results for
//     dirty files are suppressed (the committed content is stale).
//  2. Sort — BM25 score descending, filename ascending as tiebreaker.
//  3. File limit — keep at most limit files (0 or negative = unlimited).
//  4. Match limit — keep at most maxMatches LineMatches per file
//     (0 or negative = unlimited). Keeps the earliest matches by line number.
//  5. Format — render file headers, line numbers, context, and symbol tags.
//
// Returns "" when all results are suppressed (caller should treat as no match).
func formatResults(files []zoekt.FileMatch, dirtyFiles map[string]bool, limit, maxMatches int) string {
	if len(files) == 0 {
		return ""
	}

	// Deduplicate: uncommitted version wins over committed
	deduped := deduplicateFiles(files, dirtyFiles)

	// Sort by score descending
	sort.SliceStable(deduped, func(i, j int) bool {
		if deduped[i].Score != deduped[j].Score {
			return deduped[i].Score > deduped[j].Score
		}
		return deduped[i].FileName < deduped[j].FileName
	})

	// Apply file-count limit (0 or negative = unlimited).
	if limit > 0 && len(deduped) > limit {
		deduped = deduped[:limit]
	}

	// Apply per-file match limit (0 or negative = unlimited).
	if maxMatches > 0 {
		for i := range deduped {
			if len(deduped[i].LineMatches) > maxMatches {
				deduped[i].LineMatches = deduped[i].LineMatches[:maxMatches]
			}
		}
	}

	// Compute the digit width of the largest line number across all files
	// so every line number in the output uses a consistent field width.
	width := maxLineNumWidth(deduped)

	// Pre-size the builder: ~200 bytes per file header + ~80 bytes per match.
	matches := 0
	for _, fm := range deduped {
		matches += len(fm.LineMatches)
	}
	var sb strings.Builder
	sb.Grow(len(deduped)*200 + matches*80)
	for i, fm := range deduped {
		if i > 0 {
			sb.WriteByte('\n')
		}
		formatFileMatch(&sb, fm, width)
	}

	// No trailing newline after the last line
	s := sb.String()
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}

// deduplicateFiles groups FileMatches by filename, preferring uncommitted
// versions. When dirtyFiles is non-nil, committed-shard results for dirty
// files are suppressed even when the uncommitted shard has no match — the
// committed content is stale (e.g. the matched symbol was renamed locally).
func deduplicateFiles(files []zoekt.FileMatch, dirtyFiles map[string]bool) []zoekt.FileMatch {
	type dedup struct {
		idx         int
		uncommitted bool
	}
	byPath := make(map[string]dedup, len(files))
	for i, fm := range files {
		isUncommitted := fm.Repository == repoUncommitted
		existing, ok := byPath[fm.FileName]
		if !ok {
			byPath[fm.FileName] = dedup{idx: i, uncommitted: isUncommitted}
		} else if isUncommitted && !existing.uncommitted {
			byPath[fm.FileName] = dedup{idx: i, uncommitted: isUncommitted}
		}
	}
	result := make([]zoekt.FileMatch, 0, len(byPath))
	for _, entry := range byPath {
		// Suppress stale committed results for dirty files: the committed
		// shard has HEAD content which is outdated for modified files.
		if !entry.uncommitted && dirtyFiles[files[entry.idx].FileName] {
			continue
		}
		result = append(result, files[entry.idx])
	}
	return result
}

// maxLineNumWidth returns the digit count of the largest line number that will
// be displayed. After-context lines can extend past the match line; before-context
// lines are always smaller, so only match + after-count is checked.
func maxLineNumWidth(files []zoekt.FileMatch) int {
	maxLine := 0
	for _, fm := range files {
		for _, lm := range fm.LineMatches {
			lineNum := int(lm.LineNumber)
			afterCount := countContextLines(lm.After)
			if end := lineNum + afterCount; end > maxLine {
				maxLine = end
			}
		}
	}
	if maxLine == 0 {
		return 1
	}
	return len(strconv.Itoa(maxLine))
}

// formatFileMatch formats a single FileMatch into the output format.
// Context lines (Before/After) are rendered with the same indentation as match
// lines but without symbol annotations, making matches visually prominent.
func formatFileMatch(sb *strings.Builder, fm zoekt.FileMatch, width int) {
	lang := fm.Language
	if lang == "" {
		lang = "unknown"
	}

	// File header
	sb.WriteString("## ")
	sb.WriteString(fm.FileName)
	sb.WriteString(" (")
	sb.WriteString(lang)
	sb.WriteByte(')')

	if fm.Repository == repoUncommitted {
		sb.WriteString(" [")
		sb.WriteString(repoUncommitted)
		sb.WriteByte(']')
	}
	sb.WriteByte('\n')

	// Track the last line number we emitted so we can insert a blank separator
	// between non-contiguous regions of context.
	lastEmittedLine := 0

	for i, lm := range fm.LineMatches {
		matchLine := int(lm.LineNumber)

		// Compute context "before" line count and boundaries.
		// Uses countContextLines (0-alloc) instead of splitContextLines.
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
					writeContextLine(sb, lineNum, line, width)
				}
			}
		}

		// Emit the match line itself
		sb.WriteString("  ")
		writeLineNum(sb, matchLine, width)
		sb.WriteByte(' ')

		// Symbol kind from first line fragment
		if len(lm.LineFragments) > 0 && lm.LineFragments[0].SymbolInfo != nil && lm.LineFragments[0].SymbolInfo.Kind != "" {
			sb.WriteByte('[')
			sb.WriteString(lm.LineFragments[0].SymbolInfo.Kind)
			sb.WriteString("] ")
		}

		sb.Write(bytes.TrimRight(lm.Line, "\n"))
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
				writeContextLine(sb, lineNum, parts[k], width)
				lastEmittedLine = lineNum
			}
		}
	}
}

// writeLineNum right-aligns lineNum within a field of width digits.
func writeLineNum(sb *strings.Builder, lineNum, width int) {
	s := strconv.Itoa(lineNum)
	// Go 1.22+: range over negative int iterates zero times.
	for range width - len(s) {
		sb.WriteByte(' ')
	}
	sb.WriteString(s)
}

// writeContextLine writes a context line from raw bytes: two-space indent,
// right-aligned line number, a space separator, the content, and a newline.
func writeContextLine(sb *strings.Builder, lineNum int, content []byte, width int) {
	sb.WriteString("  ")
	writeLineNum(sb, lineNum, width)
	sb.WriteByte(' ')
	sb.Write(content)
	sb.WriteByte('\n')
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

// splitContextLines is the string-returning variant of splitContextBytes.
// Used by tests; the hot path uses splitContextBytes to avoid per-line copies.
func splitContextLines(data []byte) []string {
	raw := splitContextBytes(data)
	if raw == nil {
		return nil
	}
	lines := make([]string, len(raw))
	for i, r := range raw {
		lines[i] = string(r)
	}
	return lines
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
