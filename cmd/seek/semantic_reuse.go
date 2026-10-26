package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// Vectors are incremental. Every row stores the key of its model input. A
// build copies the vectors of each row that a live generation or the unit
// store already holds. So a new commit, a folder change, a second worktree, or
// a new search root embeds only windows that neither holds. A build reads the
// previous generations of its own node. A node with none reads the live
// generations of the corpora that share its content. For a Git node, these are
// its linked worktrees. For a folder node, these are the folders that hold it
// or that it holds.

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

// makeSemanticRowKey hashes the row key prefix (semanticRowKeyPrefix) and the
// exact encoder input token IDs. The input holds the last 4 parts of the
// file's model path (semanticModelPath, rerankPathComponentLimit). So the same
// file in any worktree or under any search root gives the same key. A key hit
// always serves the same model input.
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
func (ref semanticReuseRef) vectors() (*semanticFineF16Vectors, *semanticCoarseSNORM16Vectors) {
	if ref.record != nil {
		return (*semanticFineF16Vectors)(unsafe.Pointer(&ref.record[32])),
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

// semanticFileReuse is a run of adjacent rows of one generation. In files, it
// holds the rows of one file. In skipped, it can hold the rows of several
// files.
type semanticFileReuse struct {
	generation *semanticGeneration
	first      uint64
	count      uint64
}

// semanticReuseIndex maps the rows that a build can copy: the rows of live
// generations, and the unit store. shards maps the graph shards of the live
// generations by ID. base is the model path base of the build's node
// (modelPathBase). A nil index copies nothing.
type semanticReuseIndex struct {
	base        string
	generations []*semanticGeneration
	rows        map[semanticRowKey]semanticReuseRef
	files       map[semanticFileReuseKey]semanticFileReuse
	units       []*semanticUnitSegment
	shards      semanticUSearchShardSources
	// skipped holds the row ranges that add left out of rows and files: the
	// plain tails of other corpora that no node file has. markReused indexes
	// their keys in pendingRows once, for the first file that needs them.
	skipped     []semanticFileReuse
	pendingOnce sync.Once
	pendingRows map[semanticRowKey]semanticReuseRef
}

// semanticGitLineage names the content lineage of a Git node: its linked
// worktrees share the common directory, and so share content.
func semanticGitLineage(paths gitPaths) string {
	return "git:" + pathIdentityOrClean(paths.CommonDir)
}

const semanticLineageFile = ".lineage"

// semanticFolderLineagePrefix starts the lineage of a folder node; its root
// follows.
const semanticFolderLineagePrefix = "folder:"

// semanticFolderLineage names the content lineage of a folder node by its
// root (a file root by the file). Folders whose roots nest share the files of
// the inner one.
func (plan node) semanticFolderLineage() string {
	return semanticFolderLineagePrefix + filepath.ToSlash(plan.root)
}

// folderSemanticSource is the semantic source of a folder node in state. It
// is the state and a hash of the model path base, because the model reads the
// files by absolute path. A renamed or moved folder keeps its corpus (the
// corpus ID is the root's file identity), and its new path then gives a new
// generation.
func (plan node) folderSemanticSource(state string) string {
	if state == "" {
		return ""
	}
	base := sha256.Sum256([]byte(plan.modelPathBase()))
	return state + "-" + hex.EncodeToString(base[:8])
}

// modelPathBase is the slash path that the model reads a node's file paths
// from (semanticModelPath). A Git node needs none: its repository-relative
// paths do not depend on the search root. A folder node is rooted at its
// search root, so its node-relative paths change with the root. The model
// reads its files by absolute path. So a folder build can copy the vectors of
// a folder that holds it or that it holds. A file root's base is the file's
// directory.
func (plan node) modelPathBase() string {
	switch {
	case plan.kind != corpusKindFolder:
		return ""
	case plan.rootType == rootTypeFile:
		return filepath.ToSlash(filepath.Dir(plan.root))
	}
	return filepath.ToSlash(plan.root)
}

// semanticModelPath is the path that the model reads for name, a
// node-relative slash path of a node whose model path base is base.
func semanticModelPath(base, name string) string {
	if base == "" {
		return name
	}
	return path.Join(base, name)
}

// semanticModelPathTail is rerankPathTail(semanticModelPath(base, name)) for
// a clean node-relative name. A name of rerankPathComponentLimit parts or more
// holds its whole tail, so it needs no joined path.
func semanticModelPathTail(base, name string) string {
	if strings.Count(name, "/") >= rerankPathComponentLimit-1 {
		return rerankPathTail(name)
	}
	return rerankPathTail(semanticModelPath(base, name))
}

// sharedLineage reports whether the corpus of lineage other shares content
// with a node of lineage mine. Git lineages share when equal. Folder lineages
// share when equal or when one root holds the other. An example is a search of
// a subfolder of a folder that an earlier search named. A file root holds no
// other root.
func sharedLineage(mine, other string) bool {
	mineRoot, mineFolder := strings.CutPrefix(mine, semanticFolderLineagePrefix)
	otherRoot, otherFolder := strings.CutPrefix(other, semanticFolderLineagePrefix)
	switch {
	case !mineFolder && !otherFolder:
		return mine == other
	case mineFolder && otherFolder:
		return mineRoot == otherRoot || semanticLineageHolds(mine, other) || semanticLineageHolds(other, mine)
	}
	return false
}

// semanticLineageHolds reports whether the root of folder lineage outer holds
// the root of folder lineage inner and is not the same root.
func semanticLineageHolds(outer, inner string) bool {
	outerRoot, outerFolder := strings.CutPrefix(outer, semanticFolderLineagePrefix)
	innerRoot, innerFolder := strings.CutPrefix(inner, semanticFolderLineagePrefix)
	return outerFolder && innerFolder && outerRoot != innerRoot &&
		(outerRoot == "/" || strings.HasPrefix(innerRoot, outerRoot+"/"))
}

// loadSemanticReuse opens what a build in staging can copy from. These are
// the other generations of indexDir and the unit store. A node with no other
// generation also reads the live generations of the corpora that share its
// lineage (sharedLineage). A node with no lineage reads no other corpus: an
// unrelated corpus shares nothing and costs load time and memory. base is the
// node's model path base (modelPathBase). Each generation keys its rows with
// the base that its manifest records. extra adds generations that the build
// staged and did not publish yet. A source that does not open is skipped:
// reuse only saves model work.
//
// Every shared generation loads, whatever its size. A size limit that skips a
// corpus would make the build embed again what that corpus holds. names are
// the node-relative names of the files that the build reads, or nil for all.
// Take a generation of a corpus whose root holds the node's root. The tail
// rule applies to it when the names and its files whose tail is not plain
// (semanticPlainTail) cover at most half of its files. Then loadSemanticReuse
// indexes now only the rows of that generation whose model path tail is the
// tail of a name or is not plain. A file key holds the whole tail, so no
// other row matches a file. A row key holds the tail only as far as the
// window header keeps it. So the row keys of the other plain tails wait for
// the first file that can match them (matchesOtherTails).
//
// On 100,000 files of 10 rows, the full index takes 115-133 ms (94-111 ms with
// sized maps). The tails take 9 ms for a node with 5 % of the files and
// 63-64 ms for 50 %. The later row keys take 85-87 ms and 41-45 ms, so a build
// that needs them pays about the full index. With 1 row per file, the full
// index takes 14-16 ms and the tails 4-11 ms. Every load also reads rows.bin
// (26-35 ms at 500,000 rows).
func loadSemanticReuse(indexDir, staging, lineage, base string, names []string, extra ...string) *semanticReuseIndex {
	started := time.Now()
	cacheRoot, rootErr := seekUserCacheRoot()
	dirs := semanticGenerationDirsIn(indexDir, staging)
	// The bind of the build removes the node's other generations, so their
	// graph files can move to the new generation as links.
	own := len(dirs)
	// holder marks the generations of a corpus whose root holds the node's.
	holder := make([]bool, len(dirs))
	if len(dirs) == 0 && lineage != "" && rootErr == nil {
		corpora, _ := filepath.Glob(filepath.Join(cacheRoot, corporaDir, "*", "index"))
		for _, other := range corpora {
			otherLineage := readCacheFile(filepath.Dir(other), semanticLineageFile)
			if other == indexDir || !sharedLineage(lineage, otherLineage) {
				continue
			}
			found := semanticGenerationDirsIn(other, staging)
			dirs = append(dirs, found...)
			for range found {
				holder = append(holder, semanticLineageHolds(otherLineage, lineage))
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
	holder = append(holder, make([]bool, len(extra))...)
	// A holder generation takes the tail rule (add) when two sets cover at
	// most half of its files (semanticGenerationShape). The sets are the
	// names, and the files whose tail is not plain, which add keeps too. The
	// maps take a size hint from the rows that add keeps.
	byTails := make([]bool, len(generations))
	rowsHint, filesHint := 0, 0
	for position, generation := range generations {
		if generation == nil {
			continue
		}
		shape := semanticGenerationShape(generation)
		if names != nil && holder[position] && 2*(len(names)+shape.notPlainFiles) <= shape.files {
			byTails[position] = true
			rowsHint += len(generation.rows)*len(names)/shape.files + shape.notPlainRows
			filesHint += len(names) + shape.notPlainFiles
			continue
		}
		rowsHint += len(generation.rows)
		filesHint += shape.files
	}
	index := &semanticReuseIndex{
		base:   base,
		rows:   make(map[semanticRowKey]semanticReuseRef, rowsHint),
		files:  make(map[semanticFileReuseKey]semanticFileReuse, filesHint),
		shards: make(semanticUSearchShardSources),
	}
	if rootErr == nil {
		index.units = openSemanticUnits(cacheRoot)
	}
	var tails map[string]struct{}
	if slices.Contains(byTails, true) {
		tails = make(map[string]struct{}, len(names))
		for _, name := range names {
			tails[semanticModelPathTail(base, name)] = struct{}{}
		}
	}
	rows := 0
	for position, generation := range generations {
		if generation == nil {
			continue
		}
		index.generations = append(index.generations, generation)
		if byTails[position] {
			index.add(generation, tails)
		} else {
			index.add(generation, nil)
		}
		index.shards.add(dirs[position], position < own)
		rows += len(generation.rows)
	}
	slog.Debug("Loaded semantic reuse sources", "generations", len(index.generations), "rows", rows,
		"unit_segments", len(index.units), "graph_shards", len(index.shards), "elapsed", time.Since(started))
	return index
}

// add indexes the rows of generation by file (model path tail, from the base
// its manifest records, and content) and by row key. A file or key that an
// earlier add indexed keeps that source. A non-nil tails keeps only the rows
// whose tail it holds or is not plain (semanticPlainTail), and add records the
// other rows in skipped.
func (index *semanticReuseIndex) add(generation *semanticGeneration, tails map[string]struct{}) {
	base := generation.manifest.ModelPathBase
	rows := generation.rows
	for first := 0; first < len(rows); {
		end := first + 1
		for end < len(rows) && rows[end].path == rows[first].path && rows[end].contentID == rows[first].contentID {
			end++
		}
		// A tail that is not plain can share a row key with a node file's
		// tail, so its rows stay indexed now.
		tail := semanticModelPathTail(base, rows[first].path)
		if _, ok := tails[tail]; tails != nil && !ok && semanticPlainTail(tail) {
			if last := len(index.skipped) - 1; last >= 0 && index.skipped[last].generation == generation &&
				index.skipped[last].first+index.skipped[last].count == uint64(first) {
				index.skipped[last].count += uint64(end - first)
			} else {
				index.skipped = append(index.skipped, semanticFileReuse{generation: generation, first: uint64(first), count: uint64(end - first)})
			}
			first = end
			continue
		}
		key := semanticFileReuseKey{tail: tail, contentID: rows[first].contentID}
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
// same model path tail and content, set for name. It reports false when a row
// of the match has no valid vectors. The caller assigns row numbers.
func (index *semanticReuseIndex) fileRows(name string, contentID semanticContentID) ([]semanticUnit, bool) {
	if index == nil {
		return nil, false
	}
	match, ok := index.files[semanticFileReuseKey{tail: semanticModelPathTail(index.base, name), contentID: contentID}]
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
		row.path = name
		row.id, idBuffer = makeSemanticUnitIDBuffered(row, idBuffer)
		row.reuse = &semanticReuseRef{generation: match.generation, row: match.first + uint64(offset)}
		rows[offset] = row
	}
	return rows, true
}

// matchesOtherTails reports whether the rows of one file can have the key of
// a pending row with another model path tail. That is so when the window
// header does not hold the whole path line (pathCut), or the file's tail is
// not plain (semanticPlainTail). A pending row's tail is always plain (add).
func (index *semanticReuseIndex) matchesOtherTails(rows []semanticUnit) bool {
	for position := range rows {
		if rows[position].pathCut {
			return true
		}
	}
	return len(rows) > 0 && !semanticPlainTail(semanticModelPathTail(index.base, rows[0].path))
}

// semanticPlainTail reports a tail that the model input holds byte for byte:
// printable ASCII with no [MASK] and no space at its end. The tokenizer
// changes other tails:
//   - NFC maps some characters to ASCII (U+212A to "K").
//   - The input sanitizer maps invalid UTF-8 and NUL to U+FFFD.
//   - A [MASK] strips the white space before it. This is in the tail, or after
//     the path line when a unit's text starts with [MASK].
//   - A newline ends the path line early.
func semanticPlainTail(tail string) bool {
	for at := range len(tail) {
		if tail[at] < ' ' || tail[at] > '~' {
			return false
		}
	}
	return !strings.Contains(tail, "[MASK]") && !strings.HasSuffix(tail, " ")
}

// semanticShape is the file count of a generation and its estimate of the
// files and rows whose tail is not plain.
type semanticShape struct {
	files, notPlainFiles, notPlainRows int
}

// semanticGenerationShape counts the files of a generation, whose rows of one
// file are next to each other. From every 16th file, it estimates the files
// and rows whose tail is not plain (semanticPlainTail), which add keeps. A
// check of every file costs about as much as the larger maps save.
func semanticGenerationShape(generation *semanticGeneration) (shape semanticShape) {
	const sample = 16
	rows := generation.rows
	for first := 0; first < len(rows); {
		end := first + 1
		for end < len(rows) && rows[end].path == rows[first].path {
			end++
		}
		if shape.files%sample == 0 && !semanticPlainTail(semanticModelPathTail(generation.manifest.ModelPathBase, rows[first].path)) {
			shape.notPlainFiles += sample
			shape.notPlainRows += sample * (end - first)
		}
		shape.files++
		first = end
	}
	shape.notPlainFiles = min(shape.notPlainFiles, shape.files)
	shape.notPlainRows = min(shape.notPlainRows, len(rows))
	return shape
}

// markReused points each row whose key a live generation or the unit store
// holds, with valid codes, at those vectors. The rows of a file that can match
// a row with another tail (matchesOtherTails) also look up the row keys that
// add skipped. The first such file indexes them.
func (index *semanticReuseIndex) markReused(rows []semanticUnit) {
	if index == nil {
		return
	}
	var pendingRows map[semanticRowKey]semanticReuseRef
	if len(index.skipped) > 0 && index.matchesOtherTails(rows) {
		index.pendingOnce.Do(func() {
			total := 0
			for _, skip := range index.skipped {
				total += int(skip.count)
			}
			index.pendingRows = make(map[semanticRowKey]semanticReuseRef, total)
			for _, skip := range index.skipped {
				for row := skip.first; row < skip.first+skip.count; row++ {
					if _, ok := index.pendingRows[skip.generation.rows[row].key]; !ok {
						index.pendingRows[skip.generation.rows[row].key] = semanticReuseRef{generation: skip.generation, row: row}
					}
				}
			}
		})
		pendingRows = index.pendingRows
	}
	for position := range rows {
		key := rows[position].key
		ref, ok := index.rows[key]
		if !ok {
			ref, ok = pendingRows[key]
		}
		if ok && semanticRowVectorsValid(ref.generation, ref.row) {
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

// modelBase is the model path base of the build's node; "" for a nil index.
func (index *semanticReuseIndex) modelBase() string {
	if index == nil {
		return ""
	}
	return index.base
}

// usearchShards gives the graph shards that a build can reuse, by ID.
func (index *semanticReuseIndex) usearchShards() semanticUSearchShardSources {
	if index == nil {
		return nil
	}
	return index.shards
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
