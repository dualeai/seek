package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The content-addressed unit store holds the windows that a node's bound
// generation replaced, keyed by their model input. A build copies a window
// from a live generation or from the store. So the model does not embed again
// the content that the store holds: a branch switch back and forth embeds
// nothing new. The store holds only replaced windows (the live generations
// hold the rest), so it adds little disk. It sits in the corpora tree under
// the one cache budget. No live generation references its segments, so they
// go first, oldest first, when the cache is over budget.
const (
	semanticUnitsDir         = "units"
	semanticUnitsMagic       = "SEEKUNIT"
	semanticUnitsHeaderBytes = len(semanticUnitsMagic) + 8
	semanticUnitRecordBytes  = 32 + semanticFineVectorBytesPerUnit + semanticCoarseVectorBytesPerUnit
	semanticUnitsSegmentExt  = ".seg"
)

// semanticUnitsDirFor is the store of one row-key prefix
// (semanticRowKeyPrefix): a new model, representation, codec, or inference
// runtime starts a new store.
func semanticUnitsDirFor(cacheRoot string) string {
	return filepath.Join(cacheRoot, corporaDir, semanticUnitsDir, hex.EncodeToString(semanticRowKeyPrefix()[:16]))
}

// semanticUnitSegment is one mapped segment: records sorted by key.
type semanticUnitSegment struct {
	path    string
	mapped  []byte
	records int
}

func (segment *semanticUnitSegment) record(index int) []byte {
	at := semanticUnitsHeaderBytes + index*semanticUnitRecordBytes
	return segment.mapped[at : at+semanticUnitRecordBytes]
}

// find returns the record of key, by binary search on the mapped file.
func (segment *semanticUnitSegment) find(key semanticRowKey) ([]byte, bool) {
	low := sort.Search(segment.records, func(index int) bool {
		return bytes.Compare(segment.record(index)[:32], key[:]) >= 0
	})
	if low < segment.records && bytes.Equal(segment.record(low)[:32], key[:]) {
		return segment.record(low), true
	}
	return nil, false
}

// openSemanticUnits maps every segment of the store, newest first. A segment
// that does not map is skipped: the store only saves model work.
func openSemanticUnits(cacheRoot string) []*semanticUnitSegment {
	entries, err := os.ReadDir(semanticUnitsDirFor(cacheRoot))
	if err != nil {
		return nil
	}
	var segments []*semanticUnitSegment
	for _, entry := range slices.Backward(entries) {
		if !strings.HasSuffix(entry.Name(), semanticUnitsSegmentExt) {
			continue
		}
		if segment, err := mapSemanticUnitSegment(filepath.Join(semanticUnitsDirFor(cacheRoot), entry.Name())); err == nil {
			segments = append(segments, segment)
		}
	}
	return segments
}

// mapSemanticUnitSegment maps one segment file and checks its length and its
// header (magic and record count).
func mapSemanticUnitSegment(path string) (*semanticUnitSegment, error) {
	file, err := openRegularSemanticFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size < int64(semanticUnitsHeaderBytes) || (size-int64(semanticUnitsHeaderBytes))%semanticUnitRecordBytes != 0 {
		return nil, fmt.Errorf("semantic unit segment has an invalid length")
	}
	mapped, err := unix.Mmap(int(file.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	records := int((size - int64(semanticUnitsHeaderBytes)) / semanticUnitRecordBytes)
	if string(mapped[:len(semanticUnitsMagic)]) != semanticUnitsMagic ||
		binary.LittleEndian.Uint64(mapped[len(semanticUnitsMagic):]) != uint64(records) {
		_ = unix.Munmap(mapped)
		return nil, fmt.Errorf("semantic unit segment header is invalid")
	}
	return &semanticUnitSegment{path: path, mapped: mapped, records: records}, nil
}

func closeSemanticUnits(segments []*semanticUnitSegment) {
	for _, segment := range segments {
		_ = unix.Munmap(segment.mapped)
	}
}

// archiveSupersededUnits writes one new segment into the store before the
// bind removes the generations of indexDir other than keep. The segment holds
// the windows of those generations that keep and the store do not hold. The
// caller holds the node's publish lock. A failure loses only reuse.
//
// The new segment also takes in existing segments, smallest first, while the
// next one holds at most twice the records taken so far. Those segments are
// then removed. With one segment per bind, the segment count would grow with
// no limit. Every build maps and probes each segment: 1,000 extra segments
// cost 180 ms on a one-file change. With the merge, the store holds about
// log2 of its record count in segments. Each record is written again about as
// often.
func archiveSupersededUnits(indexDir string, keep ...string) {
	var others []string
	for _, dir := range semanticGenerationDirsIn(indexDir, "") {
		if !slices.Contains(keep, dir) {
			others = append(others, dir)
		}
	}
	if len(others) == 0 {
		return
	}
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return
	}
	held := map[semanticRowKey]struct{}{}
	for _, dir := range keep {
		live, err := openGenerationAt(dir)
		if err != nil {
			return
		}
		for _, row := range live.rows {
			held[row.key] = struct{}{}
		}
		_ = live.Close()
	}
	segments := openSemanticUnits(cacheRoot)
	defer closeSemanticUnits(segments)
	type pending struct {
		key    semanticRowKey
		fine   []byte
		coarse []byte
	}
	var records []pending
	var opened []*semanticGeneration
	defer func() {
		for _, generation := range opened {
			_ = generation.Close()
		}
	}()
	for _, dir := range others {
		generation, err := openGenerationAt(dir)
		if err != nil {
			continue
		}
		opened = append(opened, generation)
		for index, row := range generation.rows {
			if _, ok := held[row.key]; ok || !semanticRowVectorsValid(generation, uint64(index)) {
				continue
			}
			if semanticUnitsHold(segments, row.key) {
				continue
			}
			held[row.key] = struct{}{}
			records = append(records, pending{
				key:    row.key,
				fine:   unsafe.Slice((*byte)(unsafe.Pointer(&generation.vectors[index])), semanticFineVectorBytesPerUnit),
				coarse: unsafe.Slice((*byte)(unsafe.Pointer(&generation.coarse[index])), semanticCoarseVectorBytesPerUnit),
			})
		}
	}
	if len(records) == 0 {
		return
	}
	smallest := slices.Clone(segments)
	slices.SortFunc(smallest, func(a, b *semanticUnitSegment) int { return a.records - b.records })
	var merged []*semanticUnitSegment
	for _, segment := range smallest {
		if segment.records > 2*len(records) {
			break
		}
		for index := range segment.records {
			record := segment.record(index)
			key := semanticRowKey(record[:32])
			if _, ok := held[key]; ok {
				continue
			}
			held[key] = struct{}{}
			records = append(records, pending{
				key:    key,
				fine:   record[32 : 32+semanticFineVectorBytesPerUnit],
				coarse: record[32+semanticFineVectorBytesPerUnit:],
			})
		}
		merged = append(merged, segment)
	}
	slices.SortFunc(records, func(a, b pending) int { return bytes.Compare(a.key[:], b.key[:]) })
	dir := semanticUnitsDirFor(cacheRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	name := fmt.Sprintf("%020d-%d%s", time.Now().UnixNano(), os.Getpid(), semanticUnitsSegmentExt)
	temp := filepath.Join(dir, "."+name)
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	var header [semanticUnitsHeaderBytes]byte
	copy(header[:], semanticUnitsMagic)
	binary.LittleEndian.PutUint64(header[len(semanticUnitsMagic):], uint64(len(records)))
	_, writeErr := writer.Write(header[:])
	for _, record := range records {
		if writeErr != nil {
			break
		}
		_, writeErr = writer.Write(record.key[:])
		if writeErr == nil {
			_, writeErr = writer.Write(record.fine)
		}
		if writeErr == nil {
			_, writeErr = writer.Write(record.coarse)
		}
	}
	if writeErr == nil {
		writeErr = writer.Flush()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil || os.Rename(temp, filepath.Join(dir, name)) != nil {
		_ = os.Remove(temp)
		return
	}
	// The new segment holds every record of the merged ones. A process that
	// still maps one keeps its mapping after the unlink.
	for _, segment := range merged {
		_ = os.Remove(segment.path)
	}
}

// openGenerationAt opens the generation in dir for reading its rows.
func openGenerationAt(dir string) (*semanticGeneration, error) {
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil {
		return nil, err
	}
	return openSemanticGenerationRows(dir, manifest.Source)
}

func semanticUnitsHold(segments []*semanticUnitSegment, key semanticRowKey) bool {
	for _, segment := range segments {
		if _, ok := segment.find(key); ok {
			return true
		}
	}
	return false
}

// semanticRowVectorsValid checks the stored codes of one row before a build
// copies them: damage in one generation must not spread to the next.
func semanticRowVectorsValid(generation *semanticGeneration, row uint64) bool {
	if row >= uint64(len(generation.vectors)) || row >= uint64(len(generation.coarse)) {
		return false
	}
	return semanticVectorsValid(&generation.vectors[row], &generation.coarse[row])
}

func semanticVectorsValid(fine *semanticFineF16Vectors, coarse *semanticCoarseSNORM16Vectors) bool {
	for centroid := range fine {
		if validateSemanticF16Vector(&fine[centroid]) != nil {
			return false
		}
	}
	for centroid := range coarse {
		if validateSemanticSNORM16Vector(&coarse[centroid]) != nil {
			return false
		}
	}
	return true
}

// semanticUnitsSegments lists the store's segments as eviction candidates.
func semanticUnitsSegments(cacheRoot string) []corpusDirEntry {
	base := filepath.Join(cacheRoot, corporaDir, semanticUnitsDir)
	stores, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var entries []corpusDirEntry
	for _, store := range stores {
		segments, err := os.ReadDir(filepath.Join(base, store.Name()))
		if err != nil {
			continue
		}
		for _, segment := range segments {
			if !strings.HasSuffix(segment.Name(), semanticUnitsSegmentExt) {
				continue
			}
			info, err := segment.Info()
			if err != nil {
				continue
			}
			entries = append(entries, corpusDirEntry{
				name:   corpusID(semanticUnitsDir + "/" + store.Name() + "/" + segment.Name()),
				path:   filepath.Join(base, store.Name(), segment.Name()),
				usedAt: info.ModTime(),
			})
		}
	}
	return entries
}
