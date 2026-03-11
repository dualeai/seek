package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"

	"github.com/sourcegraph/zoekt"
)

// formatResults formats zoekt FileMatch results into the output format.
// Files are deduplicated (uncommitted wins), sorted by score descending.
func formatResults(files []zoekt.FileMatch) string {
	if len(files) == 0 {
		return ""
	}

	// Deduplicate: uncommitted version wins over committed
	deduped := deduplicateFiles(files)

	// Sort by score descending
	sort.SliceStable(deduped, func(i, j int) bool {
		if deduped[i].Score != deduped[j].Score {
			return deduped[i].Score > deduped[j].Score
		}
		return deduped[i].FileName < deduped[j].FileName
	})

	var sb strings.Builder
	for i, fm := range deduped {
		if i > 0 {
			sb.WriteByte('\n')
		}
		formatFileMatch(&sb, fm)
	}

	// No trailing newline after the last line
	return strings.TrimRight(sb.String(), "\n")
}

// deduplicateFiles groups FileMatches by filename, preferring uncommitted versions.
func deduplicateFiles(files []zoekt.FileMatch) []zoekt.FileMatch {
	type fileEntry struct {
		match       zoekt.FileMatch
		uncommitted bool
	}
	byPath := make(map[string]*fileEntry)
	for _, fm := range files {
		isUncommitted := fm.Repository == repoUncommitted
		existing, ok := byPath[fm.FileName]
		if !ok {
			byPath[fm.FileName] = &fileEntry{match: fm, uncommitted: isUncommitted}
		} else if isUncommitted && !existing.uncommitted {
			byPath[fm.FileName] = &fileEntry{match: fm, uncommitted: isUncommitted}
		}
	}
	result := make([]zoekt.FileMatch, 0, len(byPath))
	for _, entry := range byPath {
		result = append(result, entry.match)
	}
	return result
}

// formatFileMatch formats a single FileMatch into the output format.
// Context lines (Before/After) are rendered with the same indentation as match
// lines but without symbol annotations, making matches visually prominent.
func formatFileMatch(sb *strings.Builder, fm zoekt.FileMatch) {
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

		// Compute context "before" lines
		beforeLines := splitContextLines(lm.Before)
		firstBeforeLine := matchLine - len(beforeLines)
		if firstBeforeLine < 1 {
			// Guard against before-context exceeding file start (matchLine near 0)
			// or file-only matches where matchLine=0 and beforeLines is empty.
			skip := 1 - firstBeforeLine
			if skip >= len(beforeLines) {
				beforeLines = nil
			} else {
				beforeLines = beforeLines[skip:]
			}
			firstBeforeLine = 1
		}

		// Insert a blank separator if there is a gap between the previous
		// region (match + its after-context) and this region (before-context +
		// match). Skip for the very first match.
		if i > 0 && firstBeforeLine > lastEmittedLine+1 {
			sb.WriteByte('\n')
		}

		// Emit "before" context lines, skipping any that overlap with the
		// previous region's already-emitted lines.
		for j, cl := range beforeLines {
			lineNum := firstBeforeLine + j
			if lineNum <= lastEmittedLine {
				continue
			}
			writeContextLine(sb, lineNum, cl)
		}

		// Emit the match line itself
		sb.WriteString("  ")
		sb.WriteString(strconv.Itoa(matchLine))
		sb.WriteByte(' ')

		// Symbol kind from first line fragment
		if len(lm.LineFragments) > 0 && lm.LineFragments[0].SymbolInfo != nil && lm.LineFragments[0].SymbolInfo.Kind != "" {
			sb.WriteByte('[')
			sb.WriteString(lm.LineFragments[0].SymbolInfo.Kind)
			sb.WriteString("] ")
		}

		line := strings.TrimRight(string(lm.Line), "\n")
		sb.WriteString(line)
		sb.WriteByte('\n')

		lastEmittedLine = matchLine

		// Emit "after" context lines, but stop before any line that would
		// overlap with the next match's before-context or the next match itself.
		afterLines := splitContextLines(lm.After)
		afterLimit := len(afterLines)
		if i+1 < len(fm.LineMatches) {
			nextMatch := int(fm.LineMatches[i+1].LineNumber)
			nextBeforeLen := countContextLines(fm.LineMatches[i+1].Before)
			nextFirstBefore := nextMatch - nextBeforeLen
			for k := range afterLines {
				afterLineNum := matchLine + 1 + k
				if afterLineNum >= nextFirstBefore {
					afterLimit = k
					break
				}
			}
		}

		for k := range afterLimit {
			lineNum := matchLine + 1 + k
			writeContextLine(sb, lineNum, afterLines[k])
			lastEmittedLine = lineNum
		}
	}
}

// writeContextLine writes a single context line in the format "  {linenum} {content}\n".
func writeContextLine(sb *strings.Builder, lineNum int, content string) {
	sb.WriteString("  ")
	sb.WriteString(strconv.Itoa(lineNum))
	sb.WriteByte(' ')
	sb.WriteString(content)
	sb.WriteByte('\n')
}

// splitContextLines splits raw context bytes (from LineMatch.Before or .After)
// into individual trimmed lines. Empty trailing entries are removed.
func splitContextLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	raw := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	lines := make([]string, len(raw))
	for i, r := range raw {
		lines[i] = string(r)
	}
	return lines
}

// countContextLines counts how many context lines are in the raw bytes
// without allocating. It mirrors splitContextLines' trimming logic:
// a trailing newline is ignored (it's a terminator, not an empty line).
func countContextLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	return bytes.Count(data, []byte("\n")) + 1
}
