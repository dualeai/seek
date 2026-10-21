package main

import (
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// Vectors are incremental: every row stores the key of its model input, and a
// build copies the vectors of each row that a live generation or the unit
// store already holds. So a new commit, a folder change, a second worktree, or
// a new search root embeds only windows that neither holds. A build reads the
// previous generations of its own node; a Git node with none reads the live
// generations of the other corpora of its repository (its linked worktrees).

// semanticRowKey identifies one model input.
type semanticRowKey [sha256.Size]byte

// semanticRowKeyPrefix holds everything besides the token IDs that decides a
// row's vectors: the model and tokenizer, the representation, the codec, and
// the inference runtime.
var semanticRowKeyPrefix = sync.OnceValue(func() []byte {
	hash := sha256.New()
	for _, field := range []string{
		"seek-semantic-row-key-v1",
		semanticModelCompatibility(),
		semanticRepresentationCompatibility(),
		semanticVectorCodec,
		semanticInferenceRuntime(),
	} {
		writeSemanticHashField(hash, []byte(field))
	}
	return hash.Sum(nil)
})

// makeSemanticRowKey hashes the model compatibility key and the exact encoder
// input token IDs. The input holds the path tail: for a Git node the
// repository-relative path, last 4 parts, so the same file in any worktree or
// under any search root gives the same key; a folder node uses its
// node-relative path, so a new search root can give its files new keys. A key
// hit always serves the same model input.
func makeSemanticRowKey(input []int) semanticRowKey {
	buffer := make([]byte, 0, sha256.Size+4*len(input))
	buffer = append(buffer, semanticRowKeyPrefix()...)
	for _, id := range input {
		buffer = binary.LittleEndian.AppendUint32(buffer, uint32(id))
	}
	return semanticRowKey(sha256.Sum256(buffer))
}

// semanticReuseRef names the vectors that a row copies: a row of a live
// generation, or a record of the unit store.
type semanticReuseRef struct {
	generation *semanticGeneration
	row        uint64
	record     []byte
}

// vectors returns the stored codes of the reference.
func (ref semanticReuseRef) vectors() (*semanticFineSNORM16Vectors, *semanticCoarseSNORM16Vectors) {
	if ref.record != nil {
		return (*semanticFineSNORM16Vectors)(unsafe.Pointer(&ref.record[32])),
			(*semanticCoarseSNORM16Vectors)(unsafe.Pointer(&ref.record[32+semanticFineVectorBytesPerUnit]))
	}
	return &ref.generation.vectors[ref.row], &ref.generation.coarse[ref.row]
}

// semanticFileReuseKey identifies a file's model input without reading it:
// the same path tail and content give the same rows.
type semanticFileReuseKey struct {
	tail      string
	contentID semanticContentID
}

type semanticFileReuse struct {
	generation *semanticGeneration
	first      uint64
	count      uint64
}

// semanticReuseIndex maps the rows that a build can copy: the rows of live
// generations, and the unit store. A nil index copies nothing.
type semanticReuseIndex struct {
	generations []*semanticGeneration
	rows        map[semanticRowKey]semanticReuseRef
	files       map[semanticFileReuseKey]semanticFileReuse
	units       []*semanticUnitSegment
}

// semanticGitLineage names the content lineage of a Git node: its linked
// worktrees share the common directory, and so share content.
func semanticGitLineage(paths gitPaths) string {
	return "git:" + pathIdentityOrClean(paths.CommonDir)
}

const semanticLineageFile = ".lineage"

// loadSemanticReuse opens what a build in staging can copy from: the other
// generations of indexDir; for a node with none, the live generations of the
// corpora of the same lineage (a linked worktree's); and the unit store. A
// node with no lineage (a folder) reads no other corpus: an unrelated corpus
// shares nothing and costs load time and memory. extra adds generations that
// the build staged and has not yet published. A source that does not open is
// skipped: reuse only saves model work.
func loadSemanticReuse(indexDir, staging, lineage string, extra ...string) *semanticReuseIndex {
	started := time.Now()
	cacheRoot, rootErr := seekUserCacheRoot()
	dirs := semanticGenerationDirsIn(indexDir, staging)
	if len(dirs) == 0 && lineage != "" && rootErr == nil {
		corpora, _ := filepath.Glob(filepath.Join(cacheRoot, corporaDir, "*", "index"))
		for _, other := range corpora {
			if other != indexDir && readCacheFile(filepath.Dir(other), semanticLineageFile) == lineage {
				dirs = append(dirs, semanticGenerationDirsIn(other, staging)...)
			}
		}
	}
	// extra holds generations that are built but not yet published, such as
	// the staged base of the same build.
	dirs = append(dirs, extra...)
	generations := make([]*semanticGeneration, len(dirs))
	var wait sync.WaitGroup
	for index, dir := range dirs {
		wait.Go(func() {
			generation, err := openGenerationAt(dir)
			if err != nil {
				slog.Debug("semantic generation not reused", "dir", dir, "error", err)
				return
			}
			generations[index] = generation
		})
	}
	wait.Wait()
	index := &semanticReuseIndex{
		rows:  make(map[semanticRowKey]semanticReuseRef),
		files: make(map[semanticFileReuseKey]semanticFileReuse),
	}
	if rootErr == nil {
		index.units = openSemanticUnits(cacheRoot)
	}
	rows := 0
	for _, generation := range generations {
		if generation == nil {
			continue
		}
		index.generations = append(index.generations, generation)
		index.add(generation)
		rows += len(generation.rows)
	}
	slog.Debug("Loaded semantic reuse sources", "generations", len(index.generations), "rows", rows,
		"unit_segments", len(index.units), "elapsed", time.Since(started))
	return index
}

// add indexes the rows of generation by file (path tail and content) and by
// row key. A file or key that an earlier generation added keeps that source.
func (index *semanticReuseIndex) add(generation *semanticGeneration) {
	rows := generation.rows
	for first := 0; first < len(rows); {
		end := first + 1
		for end < len(rows) && rows[end].path == rows[first].path && rows[end].contentID == rows[first].contentID {
			end++
		}
		key := semanticFileReuseKey{tail: rerankPathTail(rows[first].path), contentID: rows[first].contentID}
		if _, ok := index.files[key]; !ok {
			index.files[key] = semanticFileReuse{generation: generation, first: uint64(first), count: uint64(end - first)}
		}
		for row := first; row < end; row++ {
			if _, ok := index.rows[rows[row].key]; !ok {
				index.rows[rows[row].key] = semanticReuseRef{generation: generation, row: uint64(row)}
			}
		}
		first = end
	}
}

// fileRows returns the rows of a file that a live generation holds under the
// same path tail and content, set for path. The caller assigns row numbers.
func (index *semanticReuseIndex) fileRows(path string, contentID semanticContentID) ([]semanticUnit, bool) {
	if index == nil {
		return nil, false
	}
	match, ok := index.files[semanticFileReuseKey{tail: rerankPathTail(path), contentID: contentID}]
	if !ok {
		return nil, false
	}
	for offset := range match.count {
		if !semanticRowVectorsValid(match.generation, match.first+offset) {
			return nil, false
		}
	}
	rows := make([]semanticUnit, match.count)
	var idBuffer []byte
	for offset := range rows {
		row := match.generation.rows[match.first+uint64(offset)]
		row.path = path
		row.id, idBuffer = makeSemanticUnitIDBuffered(row, idBuffer)
		row.reuse = &semanticReuseRef{generation: match.generation, row: match.first + uint64(offset)}
		rows[offset] = row
	}
	return rows, true
}

// markReused points each row whose key a live generation or the unit store
// holds, with valid codes, at those vectors.
func (index *semanticReuseIndex) markReused(rows []semanticUnit) {
	if index == nil {
		return
	}
	for position := range rows {
		key := rows[position].key
		if ref, ok := index.rows[key]; ok && semanticRowVectorsValid(ref.generation, ref.row) {
			rows[position].reuse = &ref
			continue
		}
		for _, segment := range index.units {
			if record, ok := segment.find(key); ok {
				ref := semanticReuseRef{record: record}
				if fine, coarse := ref.vectors(); semanticVectorsValid(fine, coarse) {
					rows[position].reuse = &ref
				}
				break
			}
		}
	}
}

func (index *semanticReuseIndex) Close() {
	if index == nil {
		return
	}
	for _, generation := range index.generations {
		_ = generation.Close()
	}
	closeSemanticUnits(index.units)
}

// semanticGenerationDirsIn lists the published generations of indexDir,
// except skip.
func semanticGenerationDirsIn(indexDir, skip string) []string {
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		dir := filepath.Join(indexDir, entry.Name())
		if entry.IsDir() && strings.HasPrefix(entry.Name(), semanticGenerationPrefix) && dir != skip {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
