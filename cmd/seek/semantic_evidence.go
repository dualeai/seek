package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/sourcegraph/zoekt"
)

// The evidence of a semantic hit: the pipeline reads the source of each file
// that semantic retrieval found, checks it against the row, and builds the
// display result and the model document from the bytes that the vectors
// describe.

// semanticFileCandidate is one file of a semantic hit: its best row, and once
// the source is read, its display result and its model document.
type semanticFileCandidate struct {
	unit       semanticUnit
	proxyScore float32
	result     corpusSearchResult
	document   rerankDocument
}

// evidencePathspecPaths and evidencePathspecBytes choose how the evidence
// read lists its Git sources: as ls-tree pathspecs up to these sizes, else by
// reading the whole tree. Both give the same entries; the choice is speed.
const (
	evidencePathspecPaths = 256
	evidencePathspecBytes = 64 << 10
)

func configuredContextLines(config searchConfig) int {
	return max(0, config.opts.NumContextLines)
}

// materializeSemanticCandidates reads the evidence of each candidate from its
// blob at head: it checks the bytes against the row and builds the display
// result and the model document.
func materializeSemanticCandidates(
	ctx context.Context,
	plan node,
	paths gitPaths,
	head string,
	candidates []semanticFileCandidate,
	contextLines int,
) ([]semanticFileCandidate, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	wanted := make(map[string]struct{}, len(candidates))
	pathBytes := 0
	for _, candidate := range candidates {
		wanted[candidate.unit.path] = struct{}{}
		pathBytes += len(candidate.unit.path) + 1
	}
	// A few paths go to ls-tree as pathspecs, which skip the rest of a large
	// tree. More paths read the whole tree once: from about 500 paths it is as
	// fast on a Kubernetes checkout (31,250 entries, 20 ms), and it has no
	// argument list to pass ARG_MAX.
	args := []string{"ls-tree", "-r", "-z", "--full-tree", "--no-abbrev", head}
	if len(candidates) <= evidencePathspecPaths && pathBytes <= evidencePathspecBytes {
		args = append(args, "--")
		for _, candidate := range candidates {
			args = append(args, candidate.unit.path)
		}
	}
	entriesByPath := make(map[string]gitTreeEntry, len(candidates))
	err := readNativeGitTreeRecords(ctx, paths.RepoDir, args, func(entries []gitTreeEntry) error {
		for _, entry := range entries {
			if _, ok := wanted[entry.path]; !ok {
				continue
			}
			if _, duplicate := entriesByPath[entry.path]; duplicate {
				return fmt.Errorf("git returned duplicate path %q", entry.path)
			}
			entriesByPath[entry.path] = entry
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	entries := make([]gitTreeEntry, len(candidates))
	for index, candidate := range candidates {
		entry, ok := entriesByPath[candidate.unit.path]
		if !ok || entry.mode == "160000" {
			return nil, fmt.Errorf("git source %q is missing or is not a blob", candidate.unit.path)
		}
		entries[index] = entry
	}

	var infos []gitBlobInfo
	if err := checkNativeGitBlobs(ctx, paths.RepoDir, entries, func(chunk []gitBlobInfo) error {
		infos = append(infos, chunk...)
		return nil
	}); err != nil {
		return nil, err
	}
	byPath := make(map[string]int, len(candidates))
	for index, candidate := range candidates {
		byPath[candidate.unit.path] = index
	}
	checked := make([]bool, len(candidates))
	if err := readNativeGitBlobs(ctx, paths.RepoDir, infos, func(content fileContent) error {
		index, ok := byPath[content.name]
		if !ok || checked[index] || len(content.content) == 0 {
			return fmt.Errorf("git returned invalid semantic source %q", content.name)
		}
		unit, document, materializeErr := checkedSemanticCandidate(
			candidates[index].unit,
			content.content,
		)
		if materializeErr != nil {
			return fmt.Errorf("semantic source %q: %w", content.name, materializeErr)
		}
		candidates[index].document = document
		candidates[index].result = wrapCorpusResults(plan, []zoekt.FileMatch{
			semanticEvidenceFileMatch(fallbackGitRepositoryName(paths.RepoDir), unit, content.content,
				candidates[index].proxyScore, contextLines),
		})[0]
		checked[index] = true
		readSemaphore.Release(content.weight)
		return nil
	}); err != nil {
		return nil, err
	}
	for index, ok := range checked {
		if !ok {
			return nil, fmt.Errorf("semantic source %q was not read", candidates[index].unit.path)
		}
	}
	return candidates, nil
}

// materializeWorkTreeSemanticCandidates reads the evidence of each candidate
// from the file on disk below plan.root: a folder node's files, or the dirty
// files of a Git node's overlay, which print as repository.
func materializeWorkTreeSemanticCandidates(
	ctx context.Context,
	plan node,
	repository string,
	candidates []semanticFileCandidate,
	contextLines int,
) ([]semanticFileCandidate, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	for index := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unit := candidates[index].unit
		localName := filepath.FromSlash(unit.path)
		if !filepath.IsLocal(localName) {
			return nil, fmt.Errorf("semantic folder path %q is not local", unit.path)
		}
		path := filepath.Join(plan.root, localName)
		if plan.rootType == rootTypeFile {
			if unit.path != filepath.Base(plan.root) {
				return nil, fmt.Errorf("semantic file path %q does not match its corpus", unit.path)
			}
			path = plan.root
		}
		// A file that is gone, of another type, too large, unreadable, or
		// binary changed after the capture: the route reads the tree again.
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !documentContentFits(info.Size()) {
			slog.Debug("semantic folder source unavailable", "path", unit.path, "error", err)
			return nil, treeChanged(plan)
		}
		content, binary, err := readFolderFile(folderCandidate{path: path, size: info.Size()})
		if err != nil || binary {
			slog.Debug("semantic folder source unreadable", "path", unit.path, "binary", binary, "error", err)
			return nil, treeChanged(plan)
		}
		checked, document, materializeErr := checkedSemanticCandidate(unit, content)
		if materializeErr != nil {
			// The file changed after the capture: the bytes read are not the
			// indexed ones, so the route reads the tree again.
			slog.Debug("semantic folder source changed", "path", unit.path, "error", materializeErr)
			return nil, treeChanged(plan)
		}
		unit = checked
		candidates[index].document = document
		candidates[index].result = wrapCorpusResults(plan, []zoekt.FileMatch{
			semanticEvidenceFileMatch(
				repository,
				unit,
				content,
				candidates[index].proxyScore,
				contextLines,
			),
		})[0]
	}
	return candidates, nil
}

// checkedSemanticCandidate verifies that current source bytes still match the
// stored row and content identities before those bytes reach display or final
// model scoring.
func checkedSemanticCandidate(
	unit semanticUnit,
	content []byte,
) (semanticUnit, rerankDocument, error) {
	if unit.end > uint64(len(content)) || unit.start >= unit.end ||
		semanticContentID(sha256.Sum256(content)) != unit.contentID {
		return semanticUnit{}, rerankDocument{}, fmt.Errorf("content does not match semantic row")
	}
	unit.text = content[int(unit.start):int(unit.end)]
	if makeSemanticUnitID(unit) != unit.id {
		return semanticUnit{}, rerankDocument{}, fmt.Errorf("unit identity does not match semantic row")
	}
	text := string(bytes.ToValidUTF8(unit.text, []byte("\uFFFD")))
	return unit, rerankDocument{
		Path:     unit.path,
		Language: unit.language,
		Symbol:   unit.symbol,
		Text:     text,
		matchEnd: len(text),
	}, nil
}

func semanticEvidenceFileMatch(
	repository string,
	unit semanticUnit,
	content []byte,
	score float32,
	contextLines int,
) zoekt.FileMatch {
	offset := int(unit.start)
	lineStart := bytes.LastIndexByte(content[:offset], '\n') + 1
	lineEnd := len(content)
	if relative := bytes.IndexByte(content[offset:], '\n'); relative >= 0 {
		lineEnd = offset + relative + 1
	}
	lineNumber := bytes.Count(content[:lineStart], []byte{'\n'}) + 1
	return zoekt.FileMatch{
		FileName:   unit.path,
		Repository: repository,
		Language:   unit.language,
		Score:      float64(score),
		LineMatches: []zoekt.LineMatch{{
			Line:       append([]byte(nil), content[lineStart:lineEnd]...),
			LineNumber: lineNumber,
			Before:     semanticBeforeContext(content, lineStart, contextLines),
			After:      semanticAfterContext(content, lineEnd, contextLines),
		}},
	}
}

func semanticBeforeContext(content []byte, end, lines int) []byte {
	if lines <= 0 || end <= 0 {
		return nil
	}
	start := end
	for range lines {
		if start <= 0 {
			break
		}
		searchEnd := start
		if content[searchEnd-1] == '\n' {
			searchEnd--
		}
		previous := bytes.LastIndexByte(content[:searchEnd], '\n')
		start = previous + 1
	}
	return append([]byte(nil), content[start:end]...)
}

func semanticAfterContext(content []byte, start, lines int) []byte {
	if lines <= 0 || start >= len(content) {
		return nil
	}
	end := start
	for range lines {
		if end >= len(content) {
			break
		}
		relative := bytes.IndexByte(content[end:], '\n')
		if relative < 0 {
			end = len(content)
			break
		}
		end += relative + 1
	}
	return append([]byte(nil), content[start:end]...)
}
