package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// The evidence of a semantic hit: the pipeline reads the text of each file
// that semantic retrieval found from the node's shards, checks it against the
// row, and builds the display result and the model document from the bytes
// that the vectors describe.

// semanticFileCandidate is one file of a semantic hit: its best row, and once
// its text is read, its display result and its model document.
type semanticFileCandidate struct {
	unit       semanticUnit
	proxyScore float32
	result     corpusSearchResult
	document   rerankDocument
}

// readSemanticEvidence reads the text of each candidate from the shards that
// read loaded, under the node's read lock. One Zoekt search for the candidate
// paths gives the whole content of the current copy of each file, from the
// shard mapping. The read checked under the lock that the shards and the rows
// hold the same index state. For a Git node, both then describe the same
// bytes. A folder build reads each file once for Zoekt and once for the
// vectors, so a save between the two reads makes them differ. On Kubernetes,
// 1,500 candidates take 9 ms. A read of the same files from Git blobs takes
// 98 ms, and a read from the disk takes 171 ms. A warm search of the
// Kubernetes repository that uses the model takes about 150 ms less than with
// the Git read.
//
// A file whose shard copy does not match its row (content ID, unit identity)
// is index damage. openNode then removes the family manifest and opens the
// node once more from a new capture. evidenceMismatch judges each mismatch
// first: an edit of a folder file since the walk is a tree change, and a
// mismatch that the repair did not remove makes openNode rebuild the vectors
// (nodeRepair); that file gives no vector hit. A shard copy that is a
// NOT-INDEXED marker has no text to show, as for a strict search, so that file
// gives no hit. The candidates must have different paths.
func readSemanticEvidence(
	ctx context.Context,
	read *nodeRead,
	node openedNode,
	candidates []semanticFileCandidate,
	contextLines int,
) ([]semanticFileCandidate, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	paths := make([]string, len(candidates))
	for index, candidate := range candidates {
		paths[index] = candidate.unit.path
	}
	opts := searchOpts
	opts.NumContextLines = 0
	opts.Whole = true
	files, err := read.search(ctx, query.NewFileNameSet(paths...), searchConfig{opts: &opts})
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]int, len(files))
	for index := range files {
		byPath[files[index].FileName] = index
	}
	kept := make([]semanticFileCandidate, 0, len(candidates))
	matches := make([]zoekt.FileMatch, 0, len(candidates))
	for _, candidate := range candidates {
		at, ok := byPath[candidate.unit.path]
		if !ok {
			switch evidenceMismatch(read.plan, candidate.unit.path, node.attempt) {
			case mismatchChangedFile:
				return nil, treeChanged(read.plan)
			case mismatchChangedAgain:
				continue
			case mismatchDamagedRows:
				node.repair.vectors.Store(true)
				continue
			}
			return nil, read.damage(fmt.Errorf("%w: no document holds the semantic rows of %q",
				errShardUnloadable, candidate.unit.path))
		}
		file := &files[at]
		unit, document, err := checkedSemanticCandidate(candidate.unit, file.Content, file.Language)
		document.modelBase = read.plan.modelPathBase()
		if err != nil {
			if nameOnlyContent(file.Content) {
				continue
			}
			switch evidenceMismatch(read.plan, candidate.unit.path, node.attempt) {
			case mismatchChangedFile:
				return nil, treeChanged(read.plan)
			case mismatchChangedAgain:
				continue
			case mismatchDamagedRows:
				node.repair.vectors.Store(true)
				continue
			}
			return nil, read.damage(fmt.Errorf("%w: semantic rows of %q: %w",
				errShardUnloadable, candidate.unit.path, err))
		}
		candidate.document = document
		kept = append(kept, candidate)
		matches = append(matches, semanticEvidenceFileMatch(file, unit, candidate.proxyScore, contextLines))
	}
	for index, result := range wrapCorpusResults(read.plan, matches) {
		kept[index].result = result
	}
	return kept, nil
}

// evidenceMismatchKind is what a mismatch between a file's shard copy and its
// rows means.
type evidenceMismatchKind int

const (
	// mismatchDamagedShard is index damage that the shard repair fixes when
	// the shard is the damaged side.
	mismatchDamagedShard evidenceMismatchKind = iota
	// mismatchChangedFile is a folder file that changed on disk since the
	// walk: a tree change.
	mismatchChangedFile
	// mismatchChangedAgain is a folder file that changed again after the
	// retry's build, as a log or an autosave does: it gives no vector hit.
	mismatchChangedAgain
	// mismatchDamagedRows is a mismatch that the repair or the new capture
	// did not remove: the vector rows are the damaged side.
	mismatchDamagedRows
)

// evidenceMismatch judges a mismatch for the file at name in the read of
// attempt. The check cannot tell which side is wrong, so the repair goes in
// two steps. The first read rebuilds the shards (mismatchDamagedShard), unless
// a folder file changed since the walk: a folder build reads each file once
// for Zoekt and once for the vectors, so a save between the two reads gives a
// mismatch with no damage. A later read that still finds a mismatch on an
// unchanged file has fresh shards, so the rows are damaged; a Git node has no
// folder walk, and its files are blobs or the overlay of this capture. A folder
// file that differs from the published manifest changed again after the
// build. A folder file also counts as changed again when the walk capture is
// stale on the first read, or when a later read finds no readable manifest or
// no manifest entry for it.
func evidenceMismatch(plan node, name string, attempt int) evidenceMismatchKind {
	if plan.folder == nil {
		if attempt == 0 {
			return mismatchDamagedShard
		}
		return mismatchDamagedRows
	}
	if attempt == 0 {
		_, selected, ok := plan.folder.current()
		if !ok {
			return mismatchChangedAgain
		}
		for _, candidate := range selected {
			if candidate.name == name {
				if folderFileChangedSince(candidate) {
					return mismatchChangedFile
				}
				return mismatchDamagedShard
			}
		}
		return mismatchDamagedShard
	}
	manifest, ok := readFolderManifest(plan.cacheDir, readStateFile(plan.cacheDir))
	if !ok {
		return mismatchChangedAgain
	}
	for _, entry := range manifest.Files {
		if entry.Name != name {
			continue
		}
		path := filepath.Join(plan.root, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil || !folderManifestFileMatchesCandidate(entry, newFolderCandidate(name, path, info)) {
			return mismatchChangedAgain
		}
		return mismatchDamagedRows
	}
	return mismatchChangedAgain
}

// folderFileChangedSince reports whether the file of a walk record differs on
// disk from that record.
func folderFileChangedSince(candidate folderCandidate) bool {
	info, err := os.Lstat(candidate.path)
	return err != nil || newFolderCandidate(candidate.name, candidate.path, info) != candidate
}

// nameOnlyContent reports whether content is a complete NOT-INDEXED marker:
// the content that Zoekt stores for a file that it keeps by name only.
func nameOnlyContent(content []byte) bool {
	return bytes.HasPrefix(content, []byte(zoektNotIndexedMarker)) && bytes.IndexByte(content, '\n') < 0
}

// checkedSemanticCandidate checks the shard bytes of a file against its row:
// the content ID and the unit identity. It gives the unit with its text, and
// the model document. The document takes Zoekt's language of the file, as the
// document of a strict hit does, so both routes give the model the same name.
func checkedSemanticCandidate(
	unit semanticUnit,
	content []byte,
	language string,
) (semanticUnit, rerankDocument, error) {
	if unit.end > uint64(len(content)) || unit.start >= unit.end ||
		semanticContentID(sha256.Sum256(content)) != unit.contentID {
		return semanticUnit{}, rerankDocument{}, fmt.Errorf("content does not match semantic row")
	}
	unit.text = content[int(unit.start):int(unit.end)]
	if makeSemanticUnitID(unit) != unit.id {
		return semanticUnit{}, rerankDocument{}, fmt.Errorf("unit identity does not match semantic row")
	}
	text := string(bytes.ToValidUTF8(unit.text, []byte("�")))
	return unit, rerankDocument{
		Path:     unit.path,
		Language: language,
		Symbol:   unit.symbol,
		Text:     text,
		matchEnd: len(text),
	}, nil
}

// semanticEvidenceFileMatch gives the display result of a semantic hit from
// the shard copy of its file: the line where the unit starts, with up to
// contextLines lines on each side. The line has no fragment, because no query
// term matched it. The result prints Zoekt's repository and language.
func semanticEvidenceFileMatch(
	file *zoekt.FileMatch,
	unit semanticUnit,
	score float32,
	contextLines int,
) zoekt.FileMatch {
	content := file.Content
	offset := int(unit.start)
	lineStart := bytes.LastIndexByte(content[:offset], '\n') + 1
	lineEnd := len(content)
	if relative := bytes.IndexByte(content[offset:], '\n'); relative >= 0 {
		lineEnd = offset + relative + 1
	}
	lineNumber := bytes.Count(content[:lineStart], []byte{'\n'}) + 1
	return zoekt.FileMatch{
		FileName:   file.FileName,
		Repository: file.Repository,
		Language:   file.Language,
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
