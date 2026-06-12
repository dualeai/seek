package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"

	"github.com/sourcegraph/zoekt"
)

type dirtyFileSet map[string]struct{}

func (s dirtyFileSet) contains(name string) bool {
	_, ok := s[name]
	return ok
}

type dirtyFilesByCorpus map[corpusID]dirtyFileSet

type corpusDisplayMode uint8

const (
	hideCorpusContext corpusDisplayMode = iota
	showCorpusContext
)

func formatCorpusResultsWithContext(
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
	limit int,
	maxMatches int,
	displayMode corpusDisplayMode,
) string {
	if len(results) == 0 {
		return ""
	}

	deduped := deduplicateCorpusResults(results, dirtyByCorpus)

	// Sort by score descending
	sort.SliceStable(deduped, func(i, j int) bool {
		left := deduped[i].file
		right := deduped[j].file
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		if left.FileName != right.FileName {
			return left.FileName < right.FileName
		}
		if deduped[i].displayRoot != deduped[j].displayRoot {
			return deduped[i].displayRoot < deduped[j].displayRoot
		}
		return deduped[i].corpusID < deduped[j].corpusID
	})

	// Apply file-count limit (0 or negative = unlimited).
	if limit > 0 && len(deduped) > limit {
		deduped = deduped[:limit]
	}

	// Apply per-file match limit (0 or negative = unlimited).
	if maxMatches > 0 {
		for i := range deduped {
			if len(deduped[i].file.LineMatches) > maxMatches {
				deduped[i].file.LineMatches = deduped[i].file.LineMatches[:maxMatches]
			}
		}
	}

	// Compute the digit width of the largest line number across all files
	// so every line number in the output uses a consistent field width.
	width := maxCorpusLineNumWidth(deduped)

	// Pre-size the builder: ~200 bytes per file header + ~80 bytes per match.
	matches := 0
	for _, result := range deduped {
		matches += len(result.file.LineMatches)
	}
	var sb strings.Builder
	sb.Grow(len(deduped)*200 + matches*80)
	for i, result := range deduped {
		if i > 0 {
			sb.WriteByte('\n')
		}
		formatCorpusFileMatch(&sb, result, width, displayMode)
	}

	// No trailing newline after the last line
	s := sb.String()
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}

func deduplicateCorpusResults(
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
) []corpusSearchResult {
	byPath := make(map[corpusResultKey]dedupEntry, len(results))
	for i, result := range results {
		isUncommitted := result.kind == corpusKindGit && result.file.Repository == repoUncommitted
		key := corpusResultKey{corpusID: result.corpusID, fileName: result.file.FileName}
		chooseDedupEntry(byPath, key, i, isUncommitted)
	}
	out := make([]corpusSearchResult, 0, len(byPath))
	for _, entry := range byPath {
		result := results[entry.idx]
		dirtyFiles := dirtyByCorpus[result.corpusID]
		if !entry.uncommitted && dirtyFiles.contains(result.file.FileName) {
			continue
		}
		out = append(out, result)
	}
	return out
}

type corpusResultKey struct {
	corpusID corpusID
	fileName string
}

type dedupEntry struct {
	idx         int
	uncommitted bool
}

func chooseDedupEntry[K comparable](entries map[K]dedupEntry, key K, idx int, uncommitted bool) {
	existing, ok := entries[key]
	if !ok || (uncommitted && !existing.uncommitted) {
		entries[key] = dedupEntry{idx: idx, uncommitted: uncommitted}
	}
}

func maxCorpusLineNumWidth(results []corpusSearchResult) int {
	maxLine := 0
	for _, result := range results {
		maxLine = maxFileLineEnd(maxLine, result.file)
	}
	return lineNumberWidth(maxLine)
}

func maxFileLineEnd(maxLine int, fm zoekt.FileMatch) int {
	for _, lm := range fm.LineMatches {
		lineNum := int(lm.LineNumber)
		afterCount := countContextLines(lm.After)
		if end := lineNum + afterCount; end > maxLine {
			maxLine = end
		}
	}
	return maxLine
}

func lineNumberWidth(maxLine int) int {
	if maxLine == 0 {
		return 1
	}
	return len(strconv.Itoa(maxLine))
}

func formatCorpusFileMatch(sb *strings.Builder, result corpusSearchResult, width int, displayMode corpusDisplayMode) {
	fm := result.file
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
	if displayMode == showCorpusContext && result.displayRoot != "" {
		switch result.kind {
		case corpusKindFolder:
			sb.WriteString(" [folder: ")
		default:
			sb.WriteString(" [git: ")
		}
		sb.WriteString(result.displayRoot)
		sb.WriteByte(']')
	}
	sb.WriteByte('\n')

	// Track the last line number we emitted so we can insert a blank separator
	// between non-contiguous regions of context.
	lastEmittedLine := 0

	for i, lm := range fm.LineMatches {
		matchLine := int(lm.LineNumber)

		// Compute context "before" line count and boundaries.
		// Count without materializing context lines on the hot path.
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
