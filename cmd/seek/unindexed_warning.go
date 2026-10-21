package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/sync/errgroup"
)

// zoektNotIndexedMarker is the prefix that Zoekt stores in place of the content
// of a document it did not index, followed by its reason (notIndexedMarker in
// zoekt/index/shard_builder.go). Zoekt does not export it, so
// TestZoektNotIndexedMarker pins the value.
const zoektNotIndexedMarker = "NOT-INDEXED: "

// warnUnindexedFileOperands warns for each file named as an exact path operand
// that has no content match and no searchable content. Zoekt decides which
// documents it skips and stores that decision in the index; seek only reads it
// back, so a new Zoekt skip reason needs no change here. The documents that
// seek keeps by name carry seekNoBytesReason, which the warning words in
// seek's own terms.
//
// The check searches the index that this command opened for each plan (the
// opened-node memo). A successful search opens every plan.
//
// NOTE: folder and Git scopes get no such warning. Repositories with many
// binary files (images, archives) would print it on every search. Review this
// if users miss skipped content in those scopes.
func warnUnindexedFileOperands(
	ctx context.Context,
	plans []corpusPlan,
	opened *openedNodes,
	operands []string,
	results []corpusSearchResult,
	dirtyByCorpus dirtyFilesByCorpus,
) {
	files := regularFileOperands(operands)
	if len(files) == 0 {
		return
	}
	listings := make(dirListings)
	var checks []markerCheck
	for _, plan := range plans {
		dirty := dirtyByCorpus[plan.id]
		var names, pending []string
		for _, file := range files {
			name, ok := corpusFileName(plan, file)
			if !ok || hasContentResult(results, plan.id, name, dirty.contains(name)) {
				continue
			}
			// A Git or folder corpus indexes the on-disk name, which can differ
			// in case from the typed one. A single-file corpus matches by file
			// identity, so it needs no lookup. Look it up only now: on a large
			// directory the lookup costs a full directory read.
			if plan.rootType != rootTypeFile {
				if onDisk := realCaseWithin(filepath.Dir(file), file, listings); onDisk != file {
					if diskName, diskOK := corpusFileName(plan, onDisk); diskOK {
						if hasContentResult(results, plan.id, diskName, dirty.contains(diskName)) {
							continue
						}
						file, name = onDisk, diskName
					}
				}
			}
			names = append(names, name)
			pending = append(pending, file)
		}
		if len(names) == 0 {
			continue
		}
		if searched, ok := opened.plan(plan.id); ok {
			checks = append(checks, markerCheck{plan: searched, dirty: dirty, names: names, paths: pending})
		}
	}

	// Each check opens its corpus's shards again. Run them in parallel, then
	// warn in plan order.
	markerQuery := &query.Substring{Pattern: zoektNotIndexedMarker, Content: true, CaseSensitive: true}
	reasons := make([][]string, len(checks))
	var group errgroup.Group
	group.SetLimit(runtime.GOMAXPROCS(0))
	for i, check := range checks {
		group.Go(func() error {
			reasons[i] = check.reasons(ctx, markerQuery)
			return nil
		})
	}
	_ = group.Wait()
	for i, check := range checks {
		for j, reason := range reasons[i] {
			if reason != "" {
				slog.Warn("File content is not searchable", "file", check.paths[j], "reason", reason)
			}
		}
	}
}

// markerCheck holds the exact-file operands of one prepared corpus that have no
// content match: their corpus names and their paths.
type markerCheck struct {
	plan  corpusPlan
	dirty dirtyFileSet
	names []string
	paths []string
}

// reasons searches the corpus for Zoekt's marker and returns, for each name,
// Zoekt's stored reason, or "" when the file's content is searchable.
func (check markerCheck) reasons(ctx context.Context, markerQuery query.Q) []string {
	matches, err := searchPlannedCorpusParsed(ctx, check.plan, markerQuery, defaultSearchConfig())
	if err != nil {
		slog.Debug("Skipped the not-indexed check", "root", check.plan.root, "error", err)
		return nil
	}
	found := wrapCorpusResults(check.plan, matches)
	reasons := make([]string, len(check.names))
	for i, name := range check.names {
		reasons[i], _ = unindexedReason(found, name, check.dirty.contains(name), check.paths[i])
		if reasons[i] == zoektNotIndexedMarker+"contains too many trigrams" {
			// Seek's no-bytes reason (seekNoBytesReason), in its own words.
			reasons[i] = zoektNotIndexedMarker + "its content is not on disk or cannot be read"
		}
	}
	return reasons
}

// regularFileOperands returns the canonical paths of the operands that are
// regular files, with the case the user typed.
func regularFileOperands(operands []string) []string {
	var files []string
	for _, operand := range operands {
		abs, err := filepath.Abs(operand)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, canonicalCorpusPath(abs))
	}
	return files
}

// hasContentResult reports whether results hold a content match for name in
// one corpus. A match on the file name alone does not count: a name-only
// document matches by name while its content is not searchable.
func hasContentResult(results []corpusSearchResult, id corpusID, name string, dirty bool) bool {
	for _, result := range results {
		if result.corpusID == id && result.file.FileName == name &&
			currentCopy(result.file, dirty) && hasContentMatch(result.file) {
			return true
		}
	}
	return false
}

// currentCopy reports whether a document is the current copy of its file:
// for a dirty Git file only the uncommitted copy is, because the committed
// copy is stale.
func currentCopy(file zoekt.FileMatch, dirty bool) bool {
	return !dirty || file.Repository == repoUncommitted
}

// hasContentMatch reports whether a result matches file content, not only the
// file name. Zoekt's stored marker is searchable text, so a query such as "bin"
// matches it; a match on a marker line does not count, and unindexedReason
// decides that case.
func hasContentMatch(file zoekt.FileMatch) bool {
	for _, match := range file.LineMatches {
		if !match.FileName && !markerLine(match) {
			return true
		}
	}
	return false
}

// corpusFileName returns the corpus-relative name of path when plan indexes it.
func corpusFileName(plan corpusPlan, path string) (string, bool) {
	if plan.rootType == rootTypeFile {
		// The root has the on-disk letter case; the typed path can differ.
		return filepath.Base(plan.root), plan.root == path || sameFile(plan.root, path)
	}
	rel, ok := relWithin(plan.root, path)
	if !ok {
		return "", false
	}
	for skipped := range folderSkipDirs(plan) {
		if pathWithin(skipped, path) {
			return "", false
		}
	}
	return filepath.ToSlash(rel), true
}

// unindexedReason returns Zoekt's stored reason when the current copy of the
// document for name holds only the not-indexed marker. A real file can hold
// the marker text: then its stored line has a newline or the same bytes as the
// file on disk, and it does not count.
func unindexedReason(results []corpusSearchResult, name string, dirty bool, path string) (string, bool) {
	for _, result := range results {
		file := result.file
		if file.FileName != name || !currentCopy(file, dirty) {
			continue
		}
		line, ok := onlyMarkerLine(file)
		if !ok || fileHoldsExactly(path, line) {
			continue
		}
		return string(line), true
	}
	return "", false
}

// fileHoldsExactly reports whether the file on disk has exactly these bytes.
// It reads the file only when the sizes are equal.
func fileHoldsExactly(path string, content []byte) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(len(content)) {
		return false
	}
	onDisk, err := os.ReadFile(path)
	return err == nil && bytes.Equal(onDisk, content)
}

// onlyMarkerLine returns the first line when it is a complete marker document.
func onlyMarkerLine(file zoekt.FileMatch) ([]byte, bool) {
	if len(file.LineMatches) != 1 || !markerLine(file.LineMatches[0]) {
		return nil, false
	}
	return file.LineMatches[0].Line, true
}

// markerLine reports whether a line match can be a complete marker document:
// line 1, the marker prefix, and no newline.
func markerLine(match zoekt.LineMatch) bool {
	return match.LineNumber == 1 &&
		bytes.HasPrefix(match.Line, []byte(zoektNotIndexedMarker)) &&
		bytes.IndexByte(match.Line, '\n') < 0
}
