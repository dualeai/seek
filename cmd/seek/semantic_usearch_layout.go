package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"
)

// A USearch graph shard holds the coarse vectors of whole files. Its keys
// start at 0 in each shard (semanticUSearchVectorKey of the row in the shard),
// and the search adds the shard's first row. So the bytes of a shard depend
// only on its own rows. A new generation can reuse the graph file of each
// shard whose rows did not change. One shard costs 0.44 to 0.67 s of CPU to
// build. With one key space for the whole generation, a one-file commit moved
// every later key and rebuilt all 329 shards of a Kubernetes checkout. Now
// such a commit builds 1 shard and links 378. Its generation builds in 3.6 to
// 3.9 s, not in 19.0 to 21.3 s.
//
// The row files (vectors.f16 and coarse.snorm16) stay one file each, and each
// build writes them again. In the same build, these writes take 0.66 s, but
// the row writer waits 1.7 to 1.9 s for input. So the writes do not set the
// build time, and row files for each shard would not make the build faster.
const (
	// semanticUSearchMaxShardRows is the largest shard: 4,096 rows, 16,384
	// keys.
	semanticUSearchMaxShardRows = 4_096
	// semanticUSearchMinShardRows is the smallest host-sized shard. A graph of
	// 256 rows builds in 17 ms on one CPU.
	semanticUSearchMinShardRows = 256
	// A shard closes after an anchor file once it holds the shard size minus
	// 1/semanticUSearchCutFloorDivisor of it. The anchors come about every
	// 1/semanticUSearchCutAnchorDivisor of the shard size, in rows.
	semanticUSearchCutFloorDivisor  = 16
	semanticUSearchCutAnchorDivisor = 32
)

// semanticUSearchShardRowsFor gives the shard size of a generation of rows
// rows on cpus CPUs. It is the largest power of two from 256 to 4,096 that is
// at most rows / cpus, or 256 when rows / cpus is smaller. So a generation of
// 256 rows or more per CPU builds its graphs on all CPUs. One shard of 4,096
// rows takes 436 ms on one CPU, and 16 shards of 256 rows take 17 ms on 16
// CPUs. A power of two keeps the size, and so the cut, the same when a commit
// adds or removes a few rows.
func semanticUSearchShardRowsFor(rows uint64, cpus int) uint64 {
	perCPU := rows / uint64(max(1, cpus))
	size := uint64(semanticUSearchMaxShardRows)
	for size > semanticUSearchMinShardRows && size > perCPU {
		size /= 2
	}
	return size
}

// semanticUSearchShardPlan is one shard of a generation: its rows from start.
// digest hashes the model input key and the unit ID of each row.
type semanticUSearchShardPlan struct {
	start  uint64
	rows   uint64
	digest [sha256.Size]byte
}

// semanticUSearchPlanRow is the model input key and the unit ID of one row.
type semanticUSearchPlanRow [2 * sha256.Size]byte

// semanticUSearchPlanFile is the rows of one file, from row start, and the
// anchor hash of its path (semanticUSearchCutAnchor).
type semanticUSearchPlanFile struct {
	anchor uint64
	start  uint64
	rows   []semanticUSearchPlanRow
}

// semanticUSearchShardPlanner cuts the rows of a generation into shards. The
// build gives it each file's rows in row order.
//
// The shard size comes from the host (semanticUSearchShardRowsFor). It is
// known when the rows reach 4,096 for each CPU, or at the end. Until then, the
// planner holds the files. The cut rule then reads each file in order:
//
//   - A file of more rows than the shard size gets shards of its own, of equal
//     size.
//   - A file that does not fit in the open shard closes the shard first.
//   - After a file, the open shard closes when it holds at least 15/16 of the
//     shard size and the file is an anchor.
//
// A file is an anchor when a hash of its path, read as a fraction, is below a
// limit. The limit is its rows divided by 1/32 of the shard size. The path,
// not the content, decides the hash, so an edit does not move the anchors. An
// edit that adds or removes rows moves the cuts only up to the next anchor
// that both layouts reach. The shards after that anchor keep their rows, so
// their graphs stay.
//
// On a Kubernetes checkout (1,347,164 rows in 30,194 files), the rule gives
// 379 shards of 3,555 rows on average. A cut that fills each shard until the
// next file does not fit gives 373 shards, but its cuts drift after an edit.
// In a simulation of 400 changes, an edited file changed 1.06 shards on
// average (99th percentile 3). A new file changed 1.55, and a removed file
// 1.46. A lower bound of half the size gives 470 shards. Each shard adds a
// cost to every search: one view and one search for each query token. The 379
// shards cost no measurable search time against the 329 shards of a fixed
// cut. In ten paired rounds of six queries, the graph stage was faster in five
// rounds. The median change was +8 ms for the six queries.
type semanticUSearchShardPlanner struct {
	cpus     int
	size     uint64
	held     []semanticUSearchPlanFile
	heldRows uint64
	next     uint64
	open     uint64
	start    uint64
	hash     hash.Hash
	plans    []semanticUSearchShardPlan
}

func newSemanticUSearchShardPlanner(cpus int) *semanticUSearchShardPlanner {
	return &semanticUSearchShardPlanner{cpus: max(1, cpus), hash: sha256.New()}
}

// addFile reads the rows of one file, which must follow the rows of the
// files before it. It returns the shards that this file closed.
func (planner *semanticUSearchShardPlanner) addFile(units []semanticUnit) ([]semanticUSearchShardPlan, error) {
	if len(units) == 0 {
		return nil, nil
	}
	file := semanticUSearchPlanFile{
		anchor: semanticUSearchCutAnchor(units[0].path),
		start:  planner.next,
		rows:   make([]semanticUSearchPlanRow, len(units)),
	}
	for index, unit := range units {
		if unit.row != planner.next+uint64(index) || unit.path != units[0].path {
			return nil, fmt.Errorf("semantic row %d is out of order for the USearch shard plan", unit.row)
		}
		copy(file.rows[index][:sha256.Size], unit.key[:])
		copy(file.rows[index][sha256.Size:], unit.id[:])
	}
	planner.next += uint64(len(units))
	if planner.size == 0 {
		planner.held = append(planner.held, file)
		planner.heldRows += uint64(len(units))
		if planner.heldRows < semanticUSearchMaxShardRows*uint64(planner.cpus) {
			return nil, nil
		}
		planner.setSize(semanticUSearchShardRowsFor(planner.heldRows, planner.cpus))
		return planner.take(), nil
	}
	planner.cut(file)
	return planner.take(), nil
}

// finish closes the last shard and returns the shards that are not out yet.
func (planner *semanticUSearchShardPlanner) finish() []semanticUSearchShardPlan {
	if planner.size == 0 {
		planner.setSize(semanticUSearchShardRowsFor(planner.heldRows, planner.cpus))
	}
	planner.close()
	return planner.take()
}

// shardRows gives the shard size, or 0 before it is known.
func (planner *semanticUSearchShardPlanner) shardRows() uint64 {
	return planner.size
}

func (planner *semanticUSearchShardPlanner) setSize(size uint64) {
	planner.size = size
	for _, file := range planner.held {
		planner.cut(file)
	}
	planner.held = nil
}

func (planner *semanticUSearchShardPlanner) cut(file semanticUSearchPlanFile) {
	size := planner.size
	count := uint64(len(file.rows))
	if count > size {
		planner.close()
		pieces := (count + size - 1) / size
		for piece := range pieces {
			first, last := count*piece/pieces, count*(piece+1)/pieces
			for index := first; index < last; index++ {
				planner.add(file.start+index, &file.rows[index])
			}
			planner.close()
		}
		return
	}
	if planner.open+count > size {
		planner.close()
	}
	for index := range file.rows {
		planner.add(file.start+uint64(index), &file.rows[index])
	}
	if planner.open >= size-size/semanticUSearchCutFloorDivisor &&
		semanticUSearchCutAnchorHit(file.anchor, count, size/semanticUSearchCutAnchorDivisor) {
		planner.close()
	}
}

func (planner *semanticUSearchShardPlanner) add(row uint64, identity *semanticUSearchPlanRow) {
	if planner.open == 0 {
		planner.start = row
	}
	planner.open++
	_, _ = planner.hash.Write(identity[:])
}

func (planner *semanticUSearchShardPlanner) close() {
	if planner.open == 0 {
		return
	}
	plan := semanticUSearchShardPlan{start: planner.start, rows: planner.open}
	planner.hash.Sum(plan.digest[:0])
	planner.hash.Reset()
	planner.plans = append(planner.plans, plan)
	planner.open = 0
}

func (planner *semanticUSearchShardPlanner) take() []semanticUSearchShardPlan {
	plans := planner.plans
	planner.plans = nil
	return plans
}

// semanticUSearchCutAnchor hashes a file path for the cut rule.
func semanticUSearchCutAnchor(path string) uint64 {
	sum := sha256.Sum256(append([]byte("seek-usearch-cut-v1\x00"), path...))
	return binary.LittleEndian.Uint64(sum[:8])
}

// semanticUSearchCutAnchorHit reports whether a file of rows rows with the
// path hash anchor is an anchor. Its chance is rows / spacing, so the anchors
// come about every spacing rows, whatever the file sizes.
func semanticUSearchCutAnchorHit(anchor, rows, spacing uint64) bool {
	spacing = max(1, spacing)
	return rows >= spacing || anchor < rows*(math.MaxUint64/spacing)
}

// semanticUSearchShardID identifies the graph of one shard. It hashes the
// USearch runtime and layout, and the row count. It also hashes the key, the
// unit ID, and the coarse vectors of each row. Two generations can hold one
// key with other vectors (another model provider), so the vectors are in the
// ID. A graph file with this ID holds exactly these vectors under these keys.
func semanticUSearchShardID(
	compatibility string,
	plan semanticUSearchShardPlan,
	coarse semanticCoarseSet,
) string {
	hash := sha256.New()
	writeSemanticHashField(hash, []byte("seek-usearch-shard-v1"))
	writeSemanticHashField(hash, []byte(compatibility))
	writeSemanticHashField(hash, plan.digest[:])
	var count [8]byte
	binary.LittleEndian.PutUint64(count[:], plan.rows)
	_, _ = hash.Write(count[:])
	for row := range coarse.Len() {
		vectors := coarse.At(row)
		_, _ = hash.Write(unsafe.Slice((*byte)(unsafe.Pointer(vectors)), unsafe.Sizeof(*vectors)))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// semanticUSearchShardSource is a graph shard of a published generation that
// a build can reuse. link is true when the build's bind removes the source's
// generation. A link then shares the file with no copy, and the bind records
// the new checks (refreshSemanticUSearchChecks). A build copies a source in
// another corpus. A link would change the ctime of its file. That corpus would
// then hash the file again at each search until its next bind.
type semanticUSearchShardSource struct {
	path  string
	shard semanticUSearchShard
	link  bool
}

// semanticUSearchShardSources maps a shard ID to a shard that holds that graph.
type semanticUSearchShardSources map[string]semanticUSearchShardSource

// add adds the graph shards of the published generation in dir. A shard whose
// ID is already in sources keeps that source. A manifest that does not open,
// or that has no valid graph, adds nothing: reuse only saves graph builds.
func (sources semanticUSearchShardSources) add(dir string, link bool) {
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil || !semanticSourceHasGraph(manifest.Source) ||
		validateSemanticUSearchShardMetadata(
			manifest.Rows, manifest.USearchShardRows, manifest.USearchFiles, true,
		) != nil {
		return
	}
	for _, shard := range manifest.USearchFiles {
		if _, ok := sources[shard.ID]; !ok {
			sources[shard.ID] = semanticUSearchShardSource{path: filepath.Join(dir, shard.Name), shard: shard, link: link}
		}
	}
}

// semanticUSearchShardReuse is a source that a build put in its generation,
// or verified and will link once every shard is ready (placed is false).
type semanticUSearchShardReuse struct {
	source   semanticUSearchShardSource
	verified os.FileInfo
	placed   bool
}

// verifySemanticUSearchShardSource checks the graph file of a link source
// before a build decides to reuse it. The file must be a regular file of the
// recorded length. A file that its recorded check does not match must hash to
// its recorded SHA-256, or the build makes the shard again.
func verifySemanticUSearchShardSource(source semanticUSearchShardSource) (semanticUSearchShardReuse, error) {
	info, err := os.Lstat(source.path)
	if err != nil {
		return semanticUSearchShardReuse{}, err
	}
	if !info.Mode().IsRegular() || uint64(info.Size()) != source.shard.Bytes {
		return semanticUSearchShardReuse{}, fmt.Errorf("USearch shard %s is not its recorded file", source.path)
	}
	if semanticRowsCheckOf(info) != source.shard.Check {
		semanticShardsHashed.Add(1)
		if err := validateSemanticArtifact(source.path, source.shard.semanticArtifact); err != nil {
			return semanticUSearchShardReuse{}, err
		}
	}
	return semanticUSearchShardReuse{source: source, verified: info}, nil
}

// place links the verified graph file at path and returns the check of the
// new file. A link must give the verified file, with no change since the
// check. Otherwise, or when the file system refuses the link, a copy hashes
// the bytes that it copies.
func (reuse semanticUSearchShardReuse) place(path string) (semanticRowsCheck, error) {
	source := reuse.source
	if source.link {
		info, err := os.Lstat(source.path)
		if err == nil && os.SameFile(info, reuse.verified) &&
			semanticRowsCheckOf(info) == semanticRowsCheckOf(reuse.verified) && os.Link(source.path, path) == nil {
			linked, err := os.Lstat(path)
			if err == nil && os.SameFile(reuse.verified, linked) {
				return semanticRowsCheckOf(linked), nil
			}
			_ = os.Remove(path)
		}
	}
	return copySemanticUSearchShard(source, path)
}

// copySemanticUSearchShard copies the graph file of source to path. The bytes
// that it copies must hash to the recorded SHA-256. A copy does not change the
// source file, so a build can make it as soon as it decides.
func copySemanticUSearchShard(source semanticUSearchShardSource, path string) (semanticRowsCheck, error) {
	input, err := openRegularSemanticFile(source.path)
	if err != nil {
		return semanticRowsCheck{}, err
	}
	defer func() { _ = input.Close() }()
	copied, err := writeSemanticArtifact(path, func(writer io.Writer) error {
		_, err := io.Copy(writer, input)
		return err
	})
	if err == nil && copied != source.shard.semanticArtifact {
		err = fmt.Errorf("USearch shard %s does not match its recorded checksum", source.path)
	}
	var info os.FileInfo
	if err == nil {
		info, err = os.Lstat(path)
	}
	if err != nil {
		_ = os.Remove(path)
		return semanticRowsCheck{}, err
	}
	return semanticRowsCheckOf(info), nil
}

// refreshSemanticUSearchChecks records a new check for each graph shard of
// the generation in dir whose file changed after its check. It does so only
// when the full hash still matches. A link and an unlink change the ctime of a
// reused file. So after a bind removes the generation that a build linked
// from, every reused shard fails its check. Each search process would then
// hash it again (1.06 GB for a Kubernetes checkout). A failure keeps the old
// checks.
func refreshSemanticUSearchChecks(dir string) {
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil || len(manifest.USearchFiles) == 0 {
		return
	}
	checks := make([]semanticRowsCheck, len(manifest.USearchFiles))
	refreshed := make([]bool, len(manifest.USearchFiles))
	work := make(chan int)
	var wait sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(manifest.USearchFiles)) {
		wait.Go(func() {
			for index := range work {
				shard := manifest.USearchFiles[index]
				path := filepath.Join(dir, shard.Name)
				info, err := os.Lstat(path)
				if err != nil || !info.Mode().IsRegular() || uint64(info.Size()) != shard.Bytes ||
					semanticRowsCheckOf(info) == shard.Check {
					continue
				}
				semanticShardsHashed.Add(1)
				if validateSemanticArtifact(path, shard.semanticArtifact) != nil {
					continue
				}
				// The file must be the one that the hash read.
				if after, err := os.Lstat(path); err == nil && os.SameFile(info, after) &&
					semanticRowsCheckOf(after) == semanticRowsCheckOf(info) {
					checks[index], refreshed[index] = semanticRowsCheckOf(info), true
				}
			}
		})
	}
	for index := range manifest.USearchFiles {
		work <- index
	}
	close(work)
	wait.Wait()
	hashed := manifest.USearchFiles
	rewriteSemanticManifest(dir, func(manifest *semanticManifest) bool {
		changed := false
		for index := range manifest.USearchFiles {
			shard := &manifest.USearchFiles[index]
			if index < len(hashed) && refreshed[index] && shard.Name == hashed[index].Name &&
				shard.semanticArtifact == hashed[index].semanticArtifact {
				shard.Check = checks[index]
				changed = true
			}
		}
		return changed
	})
}
