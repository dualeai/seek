package main

import (
	"sort"
	"strconv"
	"strings"

	"github.com/sourcegraph/zoekt"
)

// repoUncommitted is the repository name used for uncommitted file shards.
// This constant ties indexer naming (indexUncommitted) to formatter detection.
const repoUncommitted = "uncommitted"

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

	// §8 rule 4: no trailing newline after the last line
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
		sb.WriteString(" [uncommitted]")
	}
	sb.WriteByte('\n')

	// Line entries
	for _, lm := range fm.LineMatches {
		sb.WriteString("  ")
		sb.WriteString(strconv.Itoa(int(lm.LineNumber)))
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
	}
}
