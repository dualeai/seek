package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	semanticFormatVersion = 6
	// Compatibility revisions are cache barriers for behavior that asset hashes
	// and typed format settings cannot describe. anchored-spherical-v2 adds
	// normalization for finite values whose scale cannot fit in float32.
	// semanticVectorCodec identifies fine-vector encoding separately.
	semanticRepresentationRevision = "anchored-spherical-v2"
	semanticModelAlgorithmRevision = "lateon-code-edge-proxy-no-symbol-centroids-v5"
	semanticGenerationPrefix       = "semantic-"
	semanticManifestFile           = "manifest.json"
	semanticRowsFile               = "rows.bin"
	semanticVectorsFile            = "vectors.snorm16"
	semanticCoarseFile             = "coarse.snorm16"
	semanticUSearchFilePrefix      = "index-"
	semanticUSearchFileSuffix      = ".usearch"
	semanticRowsMagic              = "SEEKSROW"
	semanticRowsHeaderBytes        = len(semanticRowsMagic) + 8
	semanticRowFixedBytes          = 3*8 + 3*sha256.Size + 4 + 4 + 4 + 2
	semanticMetadataFieldMax       = 1 << 20
)

// semanticRowsHashedBytes counts the rows.bin bytes that a full check read,
// and semanticShardsHashed the USearch shards that one hashed; tests read them
// to see that a search trusts a checked file.
var (
	semanticRowsHashedBytes atomic.Int64
	semanticShardsHashed    atomic.Int64
)

// semanticNormalizedSquaredNormTolerance limits accepted float32 centroid and
// query squared norms. The SNORM16 integer-norm tolerance is derived from it.
const semanticNormalizedSquaredNormTolerance = 1e-3

var semanticModelCompatibility = sync.OnceValue(func() string {
	return semanticModelCompatibilityFor(lateOnCompressedModel, lateOnCompressedTokenizer)
})

func semanticModelCompatibilityFor(model, tokenizer []byte) string {
	hash := sha256.New()
	for _, field := range [][]byte{
		[]byte(semanticModelAlgorithmRevision),
		model,
		tokenizer,
	} {
		writeSemanticHashField(hash, field)
	}
	return semanticModelAlgorithmRevision + "-" + hex.EncodeToString(hash.Sum(nil)[:16])
}

func semanticRepresentationCompatibility() string {
	return fmt.Sprintf(
		"%s-c%d-r%d-f%d-r%d-d%d",
		semanticRepresentationRevision,
		semanticCoarseCentroidsPerUnit,
		semanticCoarseRefinementPasses,
		semanticFineCentroidsPerUnit,
		semanticFineRefinementPasses,
		semanticEmbeddingDimensions,
	)
}

type semanticVector [semanticEmbeddingDimensions]float32

type semanticArtifact struct {
	Bytes  uint64 `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// semanticSizedArtifact records the exact length of rebuildable vector data.
// It has no whole-file checksum, so open cannot detect all same-length content
// changes. Hash validation would add an index-sized read.
type semanticSizedArtifact struct {
	Bytes uint64 `json:"bytes"`
}

type semanticUSearchShard struct {
	Name  string `json:"name"`
	Start uint64 `json:"start"`
	Rows  uint64 `json:"rows"`
	semanticArtifact
	// Check is the shard file as the full check at build saw it.
	Check semanticRowsCheck `json:"check"`
}

// semanticRowsCheck records a file as the full check at build saw it: a later
// open that finds the same length, device, inode, mtime, and ctime trusts the
// file; any other value runs the full check again.
type semanticRowsCheck struct {
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns"`
}

// semanticManifest binds the semantic artifacts to their exact formats.
// VectorCodec identifies the fine-vector byte encoding. VectorStride is the
// byte width of one semantic row, including all fine centroids, not one centroid
// vector. Runtime is the bundled ONNX Runtime version that produced the vectors;
// see semanticInferenceRuntime for why a run-time upgrade must not reuse them.
type semanticManifest struct {
	Format         uint32                 `json:"format"`
	Source         string                 `json:"source"`
	Model          string                 `json:"model"`
	Extractor      string                 `json:"extractor"`
	Representation string                 `json:"representation"`
	VectorCodec    string                 `json:"vector_codec"`
	VectorStride   uint32                 `json:"vector_stride"`
	USearch        string                 `json:"usearch"`
	Runtime        string                 `json:"runtime"`
	Dimensions     uint32                 `json:"dimensions"`
	Rows           uint64                 `json:"rows"`
	RowsFile       semanticArtifact       `json:"rows_file"`
	RowsCheck      semanticRowsCheck      `json:"rows_check"`
	VectorsFile    semanticSizedArtifact  `json:"vectors_file"`
	CoarseFile     semanticSizedArtifact  `json:"coarse_file"`
	USearchFiles   []semanticUSearchShard `json:"usearch_files"`
}

// semanticGeneration owns one open semantic generation. vectors and coarse
// alias the memory in vectorMap and coarseMap and must not be used after
// Close. A generation validates each graph before its first native use.
type semanticGeneration struct {
	manifest          semanticManifest
	rows              []semanticUnit
	vectors           []semanticFineSNORM16Vectors
	vectorMap         []byte
	coarse            []semanticCoarseSNORM16Vectors
	coarseMap         []byte
	usearch           []string
	usearchValidation []semanticUSearchValidation
	onUSearchDamage   func()
	usearchDamageOnce sync.Once
}

type semanticUSearchValidation struct {
	once sync.Once
	err  error
}

// semanticInferenceRuntime reports the bundled ONNX Runtime version. It reads the
// embedded runtime manifest, so it neither initializes the run time nor decodes
// the model, and it is safe on lexical-only paths that never load either.
//
// The version belongs in the generation key because a run-time upgrade can change
// the numbers the model produces: ONNX Runtime 1.29 changed reshape fusion and
// constant folding, which changes how a graph is partitioned and therefore the
// stored vectors. An index outlives the binary that wrote it, so without this a
// new run time queries vectors an older one produced.
//
// The execution provider is deliberately absent. Resolving it needs the compiled
// model cache directory, which needs the model bytes, and decoding those on a
// lexical-only search would undo the work that keeps that path free of model
// cost. Providers that pass the run-time comparison in
// verifyLateOnAcceleratedProvider agree to far tighter limits than this key could
// police.
var semanticInferenceRuntime = sync.OnceValue(func() string {
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil || bundle.version == "" {
		return "unknown"
	}
	return bundle.version
})

func semanticGenerationKey(source string) string {
	hash := sha256.New()
	hash.Write([]byte("seek-semantic-generation-v1\x00"))
	for _, field := range []string{
		source,
		semanticModelCompatibility(),
		semanticExtractorID,
		semanticRepresentationCompatibility(),
		semanticVectorCodec,
		semanticUSearchLayout(),
		semanticUSearchAssetKey(),
		semanticInferenceRuntime(),
		fmt.Sprint(semanticFormatVersion),
	} {
		writeSemanticHashField(hash, []byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

func semanticGenerationDir(indexDir, source string) string {
	return filepath.Join(indexDir, semanticGenerationPrefix+semanticGenerationKey(source))
}

func prepareSemanticGenerationDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create semantic staging directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("set semantic staging permissions: %w", err)
	}
	return nil
}

func writeSemanticGenerationFromParts(
	ctx context.Context,
	dir string,
	source string,
	units []semanticUnit,
	vectorsArtifact semanticSizedArtifact,
	coarseArtifact semanticSizedArtifact,
	usearchShards []semanticUSearchShard,
) (semanticManifest, error) {
	if source == "" {
		return semanticManifest{}, fmt.Errorf("semantic generation identity is missing")
	}
	if err := ctx.Err(); err != nil {
		return semanticManifest{}, err
	}
	wantVectorBytes, sizeOK := checkedSemanticVectorBytes(uint64(len(units)))
	if !sizeOK {
		return semanticManifest{}, fmt.Errorf("semantic vector size is invalid")
	}
	if vectorsArtifact.Bytes != wantVectorBytes {
		return semanticManifest{}, fmt.Errorf("semantic vector artifact has %d bytes, want %d", vectorsArtifact.Bytes, wantVectorBytes)
	}
	if wantCoarse, ok := checkedSemanticCoarseBytes(uint64(len(units))); !ok || coarseArtifact.Bytes != wantCoarse {
		return semanticManifest{}, fmt.Errorf("semantic coarse artifact has %d bytes, want %d", coarseArtifact.Bytes, wantCoarse)
	}
	usearchCompatibility, err := semanticUSearchCompatibility()
	if err != nil {
		return semanticManifest{}, err
	}
	if err := prepareSemanticGenerationDirectory(dir); err != nil {
		return semanticManifest{}, err
	}
	// The shard build hashed each shard when it saved it. The lazy check at
	// first use hashes a shard again only if its file changed.
	if err := validateSemanticUSearchShardMetadata(uint64(len(units)), usearchShards, semanticSourceHasGraph(source)); err != nil {
		return semanticManifest{}, err
	}
	rowsArtifact, err := writeSemanticRows(filepath.Join(dir, semanticRowsFile), units)
	if err != nil {
		return semanticManifest{}, fmt.Errorf("write semantic generation: %w", err)
	}
	rowsInfo, err := os.Lstat(filepath.Join(dir, semanticRowsFile))
	if err != nil {
		return semanticManifest{}, fmt.Errorf("write semantic generation: %w", err)
	}

	manifest := semanticManifest{
		Format:         semanticFormatVersion,
		Source:         source,
		Model:          semanticModelCompatibility(),
		Extractor:      semanticExtractorID,
		Representation: semanticRepresentationCompatibility(),
		VectorCodec:    semanticVectorCodec,
		VectorStride:   semanticFineVectorBytesPerUnit,
		USearch:        usearchCompatibility,
		Runtime:        semanticInferenceRuntime(),
		Dimensions:     semanticEmbeddingDimensions,
		Rows:           uint64(len(units)),
		RowsFile:       rowsArtifact,
		RowsCheck:      semanticRowsCheckOf(rowsInfo),
		VectorsFile:    vectorsArtifact,
		CoarseFile:     coarseArtifact,
		USearchFiles:   usearchShards,
	}
	if err := writeSemanticManifest(filepath.Join(dir, semanticManifestFile), manifest); err != nil {
		return semanticManifest{}, err
	}
	return manifest, nil
}

// The stored row layout starts with magic and a little-endian uint64 row count.
// Each row then stores row, start, and end as uint64 values; three SHA-256
// values (unit ID, content ID, window key); path, language and symbol lengths
// as uint32; and kind and parser result as bytes. The path, language, and
// symbol bytes follow in that order with no padding. Keep readSemanticRowsFrom
// and checkedSemanticRowsMinimumBytes in sync with this layout.
func writeSemanticRows(path string, units []semanticUnit) (semanticArtifact, error) {
	return writeSemanticArtifact(path, func(writer io.Writer) error {
		buffered := bufio.NewWriterSize(writer, 256<<10)
		var fileHeader [semanticRowsHeaderBytes]byte
		copy(fileHeader[:], semanticRowsMagic)
		binary.LittleEndian.PutUint64(fileHeader[len(semanticRowsMagic):], uint64(len(units)))
		if _, err := buffered.Write(fileHeader[:]); err != nil {
			return err
		}
		var fixed [semanticRowFixedBytes]byte
		for index, unit := range units {
			if unit.row != uint64(index) || unit.end <= unit.start ||
				len(unit.path) > semanticMetadataFieldMax ||
				len(unit.language) > math.MaxUint32 ||
				len(unit.symbol) > math.MaxUint32 {
				return fmt.Errorf("invalid semantic row %d", index)
			}
			binary.LittleEndian.PutUint64(fixed[0:8], unit.row)
			binary.LittleEndian.PutUint64(fixed[8:16], unit.start)
			binary.LittleEndian.PutUint64(fixed[16:24], unit.end)
			copy(fixed[24:56], unit.id[:])
			copy(fixed[56:88], unit.contentID[:])
			copy(fixed[88:120], unit.key[:])
			binary.LittleEndian.PutUint32(fixed[120:124], uint32(len(unit.path)))
			binary.LittleEndian.PutUint32(fixed[124:128], uint32(len(unit.language)))
			binary.LittleEndian.PutUint32(fixed[128:132], uint32(len(unit.symbol)))
			fixed[132] = byte(unit.kind)
			fixed[133] = byte(unit.parserResult)
			if _, err := buffered.Write(fixed[:]); err != nil {
				return err
			}
			for _, value := range []string{unit.path, unit.language, unit.symbol} {
				if _, err := buffered.WriteString(value); err != nil {
					return err
				}
			}
		}
		return buffered.Flush()
	})
}

func writeSemanticArtifact(path string, write func(io.Writer) error) (semanticArtifact, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return semanticArtifact{}, err
	}
	hash := sha256.New()
	writeErr := write(io.MultiWriter(file, hash))
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return semanticArtifact{}, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return semanticArtifact{}, fmt.Errorf("semantic artifact is not a regular file")
	}
	return semanticArtifact{Bytes: uint64(info.Size()), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func writeSemanticManifest(path string, manifest semanticManifest) error {
	content, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	content = append(content, '\n')
	if _, err := writeSemanticArtifact(path, func(writer io.Writer) error {
		_, err := writer.Write(content)
		return err
	}); err != nil {
		return fmt.Errorf("write semantic manifest: %w", err)
	}
	return nil
}

func inspectSemanticArtifact(path string) (semanticArtifact, error) {
	file, err := openRegularSemanticFile(path)
	if err != nil {
		return semanticArtifact{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return semanticArtifact{}, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return semanticArtifact{}, err
	}
	return semanticArtifact{Bytes: uint64(info.Size()), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// openSemanticGenerationForSearch trusts a rows.bin whose length, device,
// inode, mtime, and ctime match the recorded check; it hashes no graph shard
// until a native plan needs it.
func openSemanticGenerationForSearch(dir, source string) (*semanticGeneration, error) {
	return openSemanticGenerationWithOptions(dir, source, false)
}

// openSemanticGenerationRows opens a generation for its rows and vectors
// only (reuse, the unit store): it reads no USearch shard, so a generation
// that an older build made with another graph rule still gives its rows.
func openSemanticGenerationRows(dir, source string) (*semanticGeneration, error) {
	return openSemanticGenerationWithOptions(dir, source, true)
}

func openSemanticGenerationWithOptions(
	dir string,
	source string,
	rowsOnly bool,
) (*semanticGeneration, error) {
	manifest, err := readSemanticManifestChecked(dir, source, !rowsOnly)
	if err != nil {
		return nil, err
	}
	if rowsOnly {
		manifest.USearchFiles = nil
	}
	rows, err := readCheckedSemanticRows(
		filepath.Join(dir, semanticRowsFile),
		manifest.RowsFile,
		manifest.RowsCheck,
		manifest.Rows,
	)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", semanticRowsFile, err)
	}
	vectorsFile, err := openRegularSemanticFile(filepath.Join(dir, semanticVectorsFile))
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", semanticVectorsFile, err)
	}
	defer func() { _ = vectorsFile.Close() }()
	vectors, vectorMap, err := mapSemanticVectors(vectorsFile, manifest.Rows)
	if err != nil {
		return nil, err
	}
	coarse, coarseMap, err := mapSemanticCoarse(filepath.Join(dir, semanticCoarseFile), manifest.Rows)
	if err != nil {
		_ = unix.Munmap(vectorMap)
		return nil, fmt.Errorf("validate %s: %w", semanticCoarseFile, err)
	}
	usearchPaths := make([]string, len(manifest.USearchFiles))
	for index, shard := range manifest.USearchFiles {
		usearchPaths[index] = filepath.Join(dir, shard.Name)
	}
	return &semanticGeneration{
		manifest:          manifest,
		rows:              rows,
		vectors:           vectors,
		vectorMap:         vectorMap,
		coarse:            coarse,
		coarseMap:         coarseMap,
		usearch:           usearchPaths,
		usearchValidation: make([]semanticUSearchValidation, len(manifest.USearchFiles)),
	}, nil
}

func (generation *semanticGeneration) validateUSearchShard(index int) error {
	if generation == nil || index < 0 || index >= len(generation.usearch) ||
		index >= len(generation.manifest.USearchFiles) {
		return fmt.Errorf("USearch semantic shard %d is unavailable", index)
	}
	if len(generation.usearchValidation) != len(generation.usearch) {
		return fmt.Errorf("USearch semantic validation state is invalid")
	}
	validation := &generation.usearchValidation[index]
	validation.once.Do(func() {
		shard := generation.manifest.USearchFiles[index]
		// A shard that the check at build saw is trusted, as rows.bin is;
		// any other file is hashed in full.
		if info, err := os.Lstat(generation.usearch[index]); err == nil && info.Mode().IsRegular() &&
			uint64(info.Size()) == shard.Bytes && semanticRowsCheckOf(info) == shard.Check {
			return
		}
		semanticShardsHashed.Add(1)
		validation.err = validateSemanticArtifact(generation.usearch[index], shard.semanticArtifact)
		if validation.err != nil && generation.onUSearchDamage != nil {
			generation.usearchDamageOnce.Do(generation.onUSearchDamage)
		}
	})
	if validation.err != nil {
		return fmt.Errorf("validate %s: %w", generation.manifest.USearchFiles[index].Name, validation.err)
	}
	return nil
}

// readSemanticManifest reads and checks the manifest of dir, its USearch
// shard list included: the build gate and the open apply one rule.
func readSemanticManifest(dir, source string) (semanticManifest, error) {
	return readSemanticManifestChecked(dir, source, true)
}

// readSemanticManifestChecked reads and checks the manifest of dir; graph also
// checks its USearch shard list.
func readSemanticManifestChecked(dir, source string, graph bool) (semanticManifest, error) {
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil {
		return semanticManifest{}, err
	}
	if err := validateSemanticManifest(manifest, source); err != nil {
		return semanticManifest{}, err
	}
	if graph {
		if err := validateSemanticUSearchShardMetadata(manifest.Rows, manifest.USearchFiles, semanticSourceHasGraph(manifest.Source)); err != nil {
			return semanticManifest{}, err
		}
	}
	return manifest, nil
}

// readSemanticManifestUnchecked decodes the manifest of dir without checking
// its source, so a build can name the source of another generation; the open
// that follows checks the manifest in full.
func readSemanticManifestUnchecked(dir string) (semanticManifest, error) {
	file, err := openRegularSemanticFile(filepath.Join(dir, semanticManifestFile))
	if err != nil {
		return semanticManifest{}, err
	}
	content, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return semanticManifest{}, err
	}
	manifest, err := decodeSemanticManifest(content)
	if err != nil {
		return semanticManifest{}, fmt.Errorf("decode semantic manifest: %w", err)
	}
	return manifest, nil
}

// Close unmaps vector data and invalidates generation.vectors and
// generation.coarse.
func (generation *semanticGeneration) Close() error {
	if generation == nil || len(generation.vectorMap) == 0 {
		return nil
	}
	err := unix.Munmap(generation.vectorMap)
	generation.vectorMap = nil
	generation.vectors = nil
	if len(generation.coarseMap) > 0 {
		err = errors.Join(err, unix.Munmap(generation.coarseMap))
	}
	generation.coarseMap = nil
	generation.coarse = nil
	return err
}

func decodeSemanticManifest(content []byte) (semanticManifest, error) {
	if err := validateJSONFields(json.NewDecoder(bytes.NewReader(content))); err != nil {
		return semanticManifest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var manifest semanticManifest
	if err := decoder.Decode(&manifest); err != nil {
		return semanticManifest{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return semanticManifest{}, err
	}
	return manifest, nil
}

func validateJSONFields(decoder *json.Decoder) error {
	if err := validateJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate JSON field %q", name)
			}
			seen[name] = struct{}{}
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("JSON object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("JSON array is not closed")
		}
	default:
		return fmt.Errorf("invalid JSON delimiter %q", delimiter)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("JSON has trailing data")
		}
		return err
	}
	return nil
}

func validateSemanticManifest(manifest semanticManifest, source string) error {
	usearchCompatibility, err := semanticUSearchCompatibility()
	if err != nil {
		return err
	}
	if manifest.Format != semanticFormatVersion || manifest.Source != source ||
		manifest.Model != semanticModelCompatibility() ||
		manifest.Extractor != semanticExtractorID ||
		manifest.Representation != semanticRepresentationCompatibility() ||
		manifest.VectorCodec != semanticVectorCodec ||
		manifest.VectorStride != semanticFineVectorBytesPerUnit ||
		manifest.USearch != usearchCompatibility ||
		manifest.Runtime != semanticInferenceRuntime() ||
		manifest.Dimensions != semanticEmbeddingDimensions {
		return fmt.Errorf("semantic generation is incompatible")
	}
	wantVectorBytes, ok := checkedSemanticVectorBytes(manifest.Rows)
	wantCoarseBytes, coarseOK := checkedSemanticCoarseBytes(manifest.Rows)
	minimumRowsBytes, rowsOK := checkedSemanticRowsMinimumBytes(manifest.Rows)
	if !ok || manifest.VectorsFile.Bytes != wantVectorBytes ||
		!coarseOK || manifest.CoarseFile.Bytes != wantCoarseBytes ||
		!rowsOK || manifest.RowsFile.Bytes < minimumRowsBytes {
		return fmt.Errorf("semantic generation has invalid lengths")
	}
	if _, err := decodeSemanticDigest(manifest.RowsFile.SHA256); err != nil {
		return err
	}
	return nil
}

func checkedSemanticRowsMinimumBytes(rows uint64) (uint64, bool) {
	headerBytes := uint64(semanticRowsHeaderBytes)
	fixedRowBytes := uint64(semanticRowFixedBytes)
	if rows > (math.MaxUint64-headerBytes)/fixedRowBytes {
		return 0, false
	}
	return headerBytes + rows*fixedRowBytes, true
}

func semanticUSearchShardName(index int) string {
	return fmt.Sprintf("%s%05d%s", semanticUSearchFilePrefix, index, semanticUSearchFileSuffix)
}

// validateSemanticUSearchShardMetadata checks that the shards cover every
// row, or that a generation with no graph has none.
func validateSemanticUSearchShardMetadata(rows uint64, shards []semanticUSearchShard, graph bool) error {
	if rows == 0 || !graph {
		if len(shards) != 0 {
			return fmt.Errorf("semantic generation with no graph has USearch shards")
		}
		return nil
	}
	if len(shards) == 0 {
		return fmt.Errorf("semantic generation has an invalid USearch shard count")
	}
	next := uint64(0)
	for index, shard := range shards {
		if shard.Name != semanticUSearchShardName(index) || shard.Start != next ||
			shard.Rows == 0 || shard.Rows > rows-next || shard.Bytes == 0 {
			return fmt.Errorf("USearch shard %d metadata is invalid", index)
		}
		if _, err := decodeSemanticDigest(shard.SHA256); err != nil {
			return err
		}
		next += shard.Rows
	}
	if next != rows {
		return fmt.Errorf("USearch shards cover %d rows, want %d", next, rows)
	}
	return nil
}

func checkedSemanticCoarseBytes(rows uint64) (uint64, bool) {
	const rowBytes = semanticCoarseVectorBytesPerUnit
	if rows > math.MaxUint64/rowBytes {
		return 0, false
	}
	return rows * rowBytes, true
}

// mapSemanticCoarse maps the coarse vectors of every row, as
// mapSemanticVectors maps the fine ones.
func mapSemanticCoarse(path string, count uint64) ([]semanticCoarseSNORM16Vectors, []byte, error) {
	file, err := openRegularSemanticFile(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	wantBytes, ok := checkedSemanticCoarseBytes(count)
	info, err := file.Stat()
	if !ok || err != nil || uint64(info.Size()) != wantBytes {
		return nil, nil, fmt.Errorf("semantic coarse file length mismatch")
	}
	if count == 0 {
		return nil, nil, nil
	}
	mapped, err := unix.Mmap(int(file.Fd()), 0, int(wantBytes), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return nil, nil, fmt.Errorf("map semantic coarse vectors: %w", err)
	}
	coarse := unsafe.Slice((*semanticCoarseSNORM16Vectors)(unsafe.Pointer(&mapped[0])), int(count))
	return coarse, mapped, nil
}

func checkedSemanticVectorBytes(rows uint64) (uint64, bool) {
	const rowBytes = semanticFineVectorBytesPerUnit
	if rows > math.MaxUint64/rowBytes {
		return 0, false
	}
	return rows * rowBytes, true
}

func decodeSemanticDigest(value string) ([]byte, error) {
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != sha256.Size {
		return nil, fmt.Errorf("semantic artifact has an invalid SHA-256")
	}
	return digest, nil
}

func openRegularSemanticFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("semantic artifact is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("semantic artifact changed while opening")
	}
	return file, nil
}

func validateSemanticArtifact(path string, want semanticArtifact) error {
	got, err := inspectSemanticArtifact(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("semantic artifact checksum or length mismatch")
	}
	return nil
}

// readCheckedSemanticRows reads rows.bin. A file whose device, inode, mtime,
// or ctime differs from the recorded check is hashed against the manifest; a
// file that the check saw is trusted.
func readCheckedSemanticRows(
	path string,
	want semanticArtifact,
	check semanticRowsCheck,
	count uint64,
) ([]semanticUnit, error) {
	file, err := openRegularSemanticFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || uint64(info.Size()) != want.Bytes {
		return nil, fmt.Errorf("semantic artifact length mismatch")
	}
	if semanticRowsCheckOf(info) == check {
		return readSemanticRowsFrom(file, count, false)
	}
	rows, err := readVerifiedSemanticRows(file, want.SHA256, count)
	if err == nil {
		// The full check passed: record the file as it is now, so later
		// searches trust it again.
		refreshSemanticRowsCheck(filepath.Dir(path), semanticRowsCheckOf(info))
	}
	return rows, err
}

// refreshSemanticRowsCheck rewrites a generation's manifest with a new rows
// check: a temporary file renamed over the manifest, so a reader sees the old
// or the new one. A failure keeps the old check: the next open checks again.
func refreshSemanticRowsCheck(dir string, check semanticRowsCheck) {
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil {
		return
	}
	manifest.RowsCheck = check
	temp := filepath.Join(dir, fmt.Sprintf(".%s.%d", semanticManifestFile, os.Getpid()))
	_ = os.Remove(temp)
	if err := writeSemanticManifest(temp, manifest); err != nil {
		_ = os.Remove(temp)
		return
	}
	if err := os.Rename(temp, filepath.Join(dir, semanticManifestFile)); err != nil {
		_ = os.Remove(temp)
	}
}

func semanticRowsCheckOf(info os.FileInfo) semanticRowsCheck {
	device, inode, mtime := fileInfoIdentity(info)
	check := semanticRowsCheck{Device: device, Inode: inode, MtimeNs: mtime}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		check.CtimeNs = statCtimeNano(*st)
	}
	return check
}

func readVerifiedSemanticRows(
	reader io.Reader,
	wantSHA256 string,
	count uint64,
) ([]semanticUnit, error) {
	hash := sha256.New()
	counted := &semanticCountingReader{reader: io.TeeReader(reader, hash)}
	rows, err := readSemanticRowsFrom(counted, count, true)
	semanticRowsHashedBytes.Add(counted.bytes)
	if err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != wantSHA256 {
		return nil, fmt.Errorf("semantic artifact checksum mismatch")
	}
	return rows, nil
}

type semanticCountingReader struct {
	reader io.Reader
	bytes  int64
}

func (r *semanticCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

// readSemanticRowsFrom parses rows. verifyIDs recomputes each unit ID: the
// full check does it; a file that the recorded check saw is trusted, so its
// open hashes nothing.
func readSemanticRowsFrom(reader io.Reader, count uint64, verifyIDs bool) ([]semanticUnit, error) {
	buffered := bufio.NewReaderSize(reader, 256<<10)
	var header [semanticRowsHeaderBytes]byte
	if _, err := io.ReadFull(buffered, header[:]); err != nil ||
		string(header[:len(semanticRowsMagic)]) != semanticRowsMagic {
		return nil, fmt.Errorf("semantic row header is invalid")
	}
	storedCount := binary.LittleEndian.Uint64(header[len(semanticRowsMagic):])
	if storedCount != count {
		return nil, fmt.Errorf("semantic row count is invalid")
	}
	rows := make([]semanticUnit, int(count))
	var fixed [semanticRowFixedBytes]byte
	var fields []byte
	var idBuffer []byte
	internedLanguages := make(map[string]string)
	var previousPath string
	for index := range rows {
		unit := &rows[index]
		if _, err := io.ReadFull(buffered, fixed[:]); err != nil {
			return nil, fmt.Errorf("read semantic row %d: %w", index, err)
		}
		unit.row = binary.LittleEndian.Uint64(fixed[0:8])
		unit.start = binary.LittleEndian.Uint64(fixed[8:16])
		unit.end = binary.LittleEndian.Uint64(fixed[16:24])
		copy(unit.id[:], fixed[24:56])
		copy(unit.contentID[:], fixed[56:88])
		copy(unit.key[:], fixed[88:120])
		pathBytes := binary.LittleEndian.Uint32(fixed[120:124])
		languageBytes := binary.LittleEndian.Uint32(fixed[124:128])
		symbolBytes := binary.LittleEndian.Uint32(fixed[128:132])
		if pathBytes > semanticMetadataFieldMax || languageBytes > semanticMetadataFieldMax ||
			symbolBytes > semanticMetadataFieldMax {
			return nil, fmt.Errorf("semantic row field is too large")
		}
		fieldsBytes := uint64(pathBytes) + uint64(languageBytes) + uint64(symbolBytes)
		if cap(fields) < int(fieldsBytes) {
			fields = make([]byte, int(fieldsBytes))
		} else {
			fields = fields[:int(fieldsBytes)]
		}
		if _, err := io.ReadFull(buffered, fields); err != nil {
			return nil, err
		}
		languageAt := int(pathBytes)
		symbolAt := languageAt + int(languageBytes)
		pathField := fields[:languageAt]
		if previousPath != "" && previousPath == string(pathField) {
			unit.path = previousPath
		} else {
			unit.path = string(pathField)
			previousPath = unit.path
		}
		unit.language = internSemanticRowLanguage(internedLanguages, fields[languageAt:symbolAt])
		unit.symbol = string(fields[symbolAt:])
		unit.kind = semanticUnitKind(fixed[132])
		unit.parserResult = semanticParserResult(fixed[133])
		wantID := unit.id
		if verifyIDs {
			wantID, idBuffer = makeSemanticUnitIDBuffered(*unit, idBuffer)
		}
		if unit.row != uint64(index) || unit.path == "" || unit.end <= unit.start ||
			unit.end-unit.start > semanticUnitMaxBytes ||
			unit.id != wantID ||
			unit.kind < semanticUnitGap || unit.kind > semanticUnitFallback ||
			unit.parserResult < semanticParserCTags || unit.parserResult > semanticParserInvalidUTF8 {
			return nil, fmt.Errorf("semantic row %d is invalid", index)
		}
	}
	if extra, err := buffered.ReadByte(); err != io.EOF || extra != 0 {
		return nil, fmt.Errorf("semantic row file has trailing data")
	}
	return rows, nil
}

func internSemanticRowLanguage(interned map[string]string, raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if value, ok := interned[string(raw)]; ok {
		return value
	}
	value := string(raw)
	interned[value] = value
	return value
}

// mapSemanticVectors checks the exact payload length and maps its bytes directly
// as int16 rows. This view requires a little-endian target. It does not validate
// component codes; exactSemanticRange validates each vector before scoring it.
func mapSemanticVectors(file *os.File, count uint64) ([]semanticFineSNORM16Vectors, []byte, error) {
	wantBytes, ok := checkedSemanticVectorBytes(count)
	if !ok {
		return nil, nil, fmt.Errorf("semantic vector length overflows")
	}
	info, err := file.Stat()
	if err != nil || uint64(info.Size()) != wantBytes {
		return nil, nil, fmt.Errorf("semantic vector file length mismatch")
	}
	if count == 0 {
		return nil, nil, nil
	}
	mapped, err := unix.Mmap(
		int(file.Fd()),
		0,
		int(wantBytes),
		unix.PROT_READ,
		unix.MAP_PRIVATE,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("map semantic vectors: %w", err)
	}
	byteOrder := uint16(1)
	if *(*byte)(unsafe.Pointer(&byteOrder)) != 1 {
		_ = unix.Munmap(mapped)
		return nil, nil, fmt.Errorf("semantic vector codec needs a little-endian target")
	}
	vectors := unsafe.Slice((*semanticFineSNORM16Vectors)(unsafe.Pointer(&mapped[0])), int(count))
	return vectors, mapped, nil
}

type semanticHit struct {
	row   uint64
	score float32
}

// exactSemanticSearch scores every row of vectors. Like the other exact
// scorers, it keeps the rows whose summed fine MaxSim reaches floor, best
// first.
func exactSemanticSearch(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query *semanticQueryEmbedding,
	floor float32,
) ([]semanticHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queryVectors, err := semanticQueryTokenVectors(query)
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, nil
	}
	return exactSemanticRowSelection(ctx, vectors, queryVectors, nil, floor)
}

// exactSemanticSortedCandidates scores a sorted set of unique row IDs. Search
// planning already produces this order, which keeps vector reads sequential.
func exactSemanticSortedCandidates(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query *semanticQueryEmbedding,
	rows []uint64,
	floor float32,
) ([]semanticHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queryVectors, err := semanticQueryTokenVectors(query)
	if err != nil {
		return nil, err
	}
	return exactSemanticSortedRows(ctx, vectors, queryVectors, rows, floor)
}

// exactSemanticSortedRows scores an owned normalized query buffer against
// sorted unique row IDs. Exact scoring validates and scales the query in place.
func exactSemanticSortedRows(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query []semanticVector,
	rows []uint64,
	floor float32,
) ([]semanticHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	for index, row := range rows {
		if row >= uint64(len(vectors)) || (index > 0 && row <= rows[index-1]) {
			return nil, fmt.Errorf("semantic candidate row %d is invalid", row)
		}
	}
	return exactSemanticRowSelection(ctx, vectors, query, rows, floor)
}

// exactSemanticRowSelection validates and scales its owned normalized query
// buffer in place. It scores every row of vectors when rows is nil. This keeps
// a full recovery scan from allocating one uint64 for every row. The workers
// keep only the rows that reach floor, so the one sort sees only those rows.
func exactSemanticRowSelection(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query []semanticVector,
	rows []uint64,
	floor float32,
) ([]semanticHit, error) {
	scaledQuery, err := prepareSemanticSNORM16Query(query)
	if err != nil {
		return nil, err
	}
	rowCount := len(rows)
	if rows == nil {
		rowCount = len(vectors)
	}
	// One row-token pair compares all fine centroids. Include query length in the
	// worker count so long descriptive queries do not run large exact routes on
	// one or two CPUs.
	const rowTokenPairsPerWorker = 512
	work := rowCount * max(1, len(query))
	workers := min(runtime.GOMAXPROCS(0), max(1, work/rowTokenPairsPerWorker))
	if workers == 1 {
		hits, err := exactSemanticRange(ctx, vectors, scaledQuery, rows, 0, rowCount, floor)
		if err = errors.Join(err, ctx.Err()); err != nil {
			return nil, err
		}
		sortSemanticHits(hits)
		return hits, nil
	}
	hits := make([][]semanticHit, workers)
	errs := make([]error, workers+1)
	var wait sync.WaitGroup
	for worker := range workers {
		start := rowCount * worker / workers
		end := rowCount * (worker + 1) / workers
		wait.Go(func() {
			hits[worker], errs[worker] = exactSemanticRange(ctx, vectors, scaledQuery, rows, start, end, floor)
		})
	}
	wait.Wait()
	errs[workers] = ctx.Err()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	merged := slices.Concat(hits...)
	sortSemanticHits(merged)
	return merged, nil
}

func validateNormalizedSemanticVector(vector *semanticVector) error {
	var squaredNorm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("has a non-finite value")
		}
		squaredNorm += float64(value * value)
	}
	if math.Abs(squaredNorm-1) > semanticNormalizedSquaredNormTolerance {
		return fmt.Errorf("has squared norm %.6f, want 1", squaredNorm)
	}
	return nil
}

// exactSemanticRange scores the rows from start to end and gives the rows
// whose summed fine MaxSim reaches floor, in row order.
func exactSemanticRange(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	scaledQuery []semanticVector,
	rows []uint64,
	start int,
	end int,
	floor float32,
) ([]semanticHit, error) {
	if start >= end {
		return nil, nil
	}
	var hits []semanticHit
	contextCheckRows := max(1, 4096/max(1, len(scaledQuery)))
	nextContextCheck := 0
	for index := start; index < end; index++ {
		progress := index - start
		if progress == nextContextCheck {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nextContextCheck += contextCheckRows
		}
		row := uint64(index)
		if rows != nil {
			row = rows[index]
		}
		// Keep the mapped row in place. Ranging over its array value would copy
		// 1,920 bytes once for validation and once for every query token.
		rowVectors := &vectors[row]
		for centroid := range rowVectors {
			if err := validateSemanticSNORM16Vector(&rowVectors[centroid]); err != nil {
				return nil, fmt.Errorf("semantic row %d centroid %d: %w", row, centroid, err)
			}
		}
		var score float32
		for queryIndex := range scaledQuery {
			queryVector := &scaledQuery[queryIndex]
			maximum := float32(math.Inf(-1))
			for centroid := range rowVectors {
				maximum = max(maximum, semanticSNORM16VectorDot(queryVector, &rowVectors[centroid]))
			}
			if math.IsInf(float64(maximum), 0) || math.IsNaN(float64(maximum)) {
				return nil, fmt.Errorf("semantic row %d produced an invalid score", row)
			}
			score += maximum
		}
		if score >= floor {
			hits = append(hits, semanticHit{row: row, score: score})
		}
	}
	return hits, nil
}

// sortSemanticHits puts the best score first; a lower row breaks a tie.
func sortSemanticHits(hits []semanticHit) {
	slices.SortFunc(hits, compareSemanticHits)
}

func compareSemanticHits(left, right semanticHit) int {
	if left.score > right.score {
		return -1
	}
	if left.score < right.score {
		return 1
	}
	if left.row < right.row {
		return -1
	}
	if left.row > right.row {
		return 1
	}
	return 0
}
