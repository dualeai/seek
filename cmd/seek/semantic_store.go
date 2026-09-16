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
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	semanticFormatVersion = 5
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
	semanticUSearchFilePrefix      = "index-"
	semanticUSearchFileSuffix      = ".usearch"
	semanticRowsMagic              = "SEEKSROW"
	semanticRowsHeaderBytes        = len(semanticRowsMagic) + 8
	semanticRowFixedBytes          = 3*8 + 2*sha256.Size + 4 + 2 + 2 + 2 + 2
	semanticManifestMaxBytes       = 64 << 10
	semanticMetadataFieldMax       = 1 << 20
	semanticMaxRows                = 10_000_000
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
	VectorsFile    semanticSizedArtifact  `json:"vectors_file"`
	USearchFiles   []semanticUSearchShard `json:"usearch_files"`
}

// semanticGeneration owns one open semantic generation. vectors aliases the
// memory in vectorMap and must not be used after Close. A search-opened
// generation validates each graph before its first native use. A fully opened
// generation records aggregate graph validation in usearchErr.
type semanticGeneration struct {
	manifest          semanticManifest
	rows              []semanticUnit
	vectors           []semanticFineSNORM16Vectors
	vectorMap         []byte
	usearch           []string
	usearchErr        error
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
	usearchShards []semanticUSearchShard,
) (semanticManifest, error) {
	if len(units) > semanticMaxRows {
		return semanticManifest{}, fmt.Errorf("semantic row count %d exceeds limit %d", len(units), semanticMaxRows)
	}
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
	usearchCompatibility, err := semanticUSearchCompatibility()
	if err != nil {
		return semanticManifest{}, err
	}
	if err := prepareSemanticGenerationDirectory(dir); err != nil {
		return semanticManifest{}, err
	}
	if err := validateSemanticUSearchShards(dir, uint64(len(units)), usearchShards); err != nil {
		return semanticManifest{}, err
	}
	rowsArtifact, err := writeSemanticRows(filepath.Join(dir, semanticRowsFile), units)
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
		VectorsFile:    vectorsArtifact,
		USearchFiles:   usearchShards,
	}
	if err := writeSemanticManifest(filepath.Join(dir, semanticManifestFile), manifest); err != nil {
		return semanticManifest{}, err
	}
	return manifest, nil
}

// The stored row layout starts with magic and a little-endian uint64 row count.
// Each row then stores row, start, and end as uint64 values; two SHA-256 values;
// path length as uint32; language, file-language, and symbol lengths as uint16;
// and kind and parser result as bytes. The path, language, file-language, and
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
				len(unit.language) > math.MaxUint16 ||
				len(unit.fileLanguage) > math.MaxUint16 ||
				len(unit.symbol) > math.MaxUint16 {
				return fmt.Errorf("invalid semantic row %d", index)
			}
			binary.LittleEndian.PutUint64(fixed[0:8], unit.row)
			binary.LittleEndian.PutUint64(fixed[8:16], unit.start)
			binary.LittleEndian.PutUint64(fixed[16:24], unit.end)
			copy(fixed[24:24+sha256.Size], unit.id[:])
			copy(fixed[24+sha256.Size:24+2*sha256.Size], unit.contentID[:])
			binary.LittleEndian.PutUint32(fixed[88:92], uint32(len(unit.path)))
			binary.LittleEndian.PutUint16(fixed[92:94], uint16(len(unit.language)))
			binary.LittleEndian.PutUint16(fixed[94:96], uint16(len(unit.fileLanguage)))
			binary.LittleEndian.PutUint16(fixed[96:98], uint16(len(unit.symbol)))
			fixed[98] = byte(unit.kind)
			fixed[99] = byte(unit.parserResult)
			if _, err := buffered.Write(fixed[:]); err != nil {
				return err
			}
			for _, value := range []string{unit.path, unit.language, unit.fileLanguage, unit.symbol} {
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
	if len(content) > semanticManifestMaxBytes {
		return fmt.Errorf("semantic manifest is too large")
	}
	artifact, err := writeSemanticArtifact(path, func(writer io.Writer) error {
		_, err := writer.Write(content)
		return err
	})
	if err != nil {
		return fmt.Errorf("write semantic manifest: %w", err)
	}
	if artifact.Bytes != uint64(len(content)) {
		return fmt.Errorf("semantic manifest write was short")
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

// openSemanticGeneration fully validates stored artifacts. Search opens use
// openSemanticGenerationForSearch so an exact-only plan does not hash unused
// graph shards.
func openSemanticGeneration(dir, source string) (*semanticGeneration, error) {
	return openSemanticGenerationWithOptions(dir, source, true)
}

func openSemanticGenerationForSearch(dir, source string) (*semanticGeneration, error) {
	return openSemanticGenerationWithOptions(dir, source, false)
}

func openSemanticGenerationWithOptions(
	dir string,
	source string,
	validateGraphs bool,
) (*semanticGeneration, error) {
	manifest, err := readSemanticManifest(dir, source)
	if err != nil {
		return nil, err
	}
	rows, err := readCheckedSemanticRows(
		filepath.Join(dir, semanticRowsFile),
		manifest.RowsFile,
		manifest.Rows,
	)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", semanticRowsFile, err)
	}
	if err := validateSemanticUSearchShardMetadata(manifest.Rows, manifest.USearchFiles); err != nil {
		return nil, err
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
	usearchPaths := make([]string, len(manifest.USearchFiles))
	for index, shard := range manifest.USearchFiles {
		usearchPaths[index] = filepath.Join(dir, shard.Name)
	}
	generation := &semanticGeneration{
		manifest:  manifest,
		rows:      rows,
		vectors:   vectors,
		vectorMap: vectorMap,
		usearch:   usearchPaths,
	}
	if validateGraphs {
		generation.usearchErr = validateSemanticUSearchShardArtifacts(dir, manifest.USearchFiles)
	} else {
		generation.usearchValidation = make([]semanticUSearchValidation, len(manifest.USearchFiles))
	}
	return generation, nil
}

func (generation *semanticGeneration) validateUSearchShard(index int) error {
	if generation == nil || index < 0 || index >= len(generation.usearch) ||
		index >= len(generation.manifest.USearchFiles) {
		return fmt.Errorf("USearch semantic shard %d is unavailable", index)
	}
	if generation.usearchErr != nil {
		return generation.usearchErr
	}
	if len(generation.usearchValidation) == 0 {
		return nil
	}
	if len(generation.usearchValidation) != len(generation.usearch) {
		return fmt.Errorf("USearch semantic validation state is invalid")
	}
	validation := &generation.usearchValidation[index]
	validation.once.Do(func() {
		shard := generation.manifest.USearchFiles[index]
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

func readSemanticManifest(dir, source string) (semanticManifest, error) {
	manifestPath := filepath.Join(dir, semanticManifestFile)
	file, err := openRegularSemanticFile(manifestPath)
	if err != nil {
		return semanticManifest{}, err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, semanticManifestMaxBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return semanticManifest{}, err
	}
	if len(content) > semanticManifestMaxBytes {
		return semanticManifest{}, fmt.Errorf("semantic manifest is too large")
	}
	manifest, err := decodeSemanticManifest(content)
	if err != nil {
		return semanticManifest{}, fmt.Errorf("decode semantic manifest: %w", err)
	}
	if err := validateSemanticManifest(manifest, source); err != nil {
		return semanticManifest{}, err
	}
	return manifest, nil
}

// Close unmaps vector data and invalidates generation.vectors.
func (generation *semanticGeneration) Close() error {
	if generation == nil || len(generation.vectorMap) == 0 {
		return nil
	}
	err := unix.Munmap(generation.vectorMap)
	generation.vectorMap = nil
	generation.vectors = nil
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
		manifest.Dimensions != semanticEmbeddingDimensions ||
		manifest.Rows > semanticMaxRows {
		return fmt.Errorf("semantic generation is incompatible")
	}
	wantVectorBytes, ok := checkedSemanticVectorBytes(manifest.Rows)
	minimumRowsBytes, rowsOK := checkedSemanticRowsMinimumBytes(manifest.Rows)
	if !ok || manifest.VectorsFile.Bytes != wantVectorBytes ||
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

func validateSemanticUSearchShards(dir string, rows uint64, shards []semanticUSearchShard) error {
	if err := validateSemanticUSearchShardMetadata(rows, shards); err != nil {
		return err
	}
	return validateSemanticUSearchShardArtifacts(dir, shards)
}

func validateSemanticUSearchShardMetadata(rows uint64, shards []semanticUSearchShard) error {
	if rows == 0 {
		if len(shards) != 0 {
			return fmt.Errorf("empty semantic generation has USearch shards")
		}
		return nil
	}
	if len(shards) == 0 || len(shards) > semanticMaxRows {
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

func validateSemanticUSearchShardArtifacts(dir string, shards []semanticUSearchShard) error {
	for _, shard := range shards {
		if err := validateSemanticArtifact(filepath.Join(dir, shard.Name), shard.semanticArtifact); err != nil {
			return fmt.Errorf("validate %s: %w", shard.Name, err)
		}
	}
	return nil
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

func readCheckedSemanticRows(
	path string,
	want semanticArtifact,
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
	rows, err := readVerifiedSemanticRows(file, want.SHA256, count)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func readVerifiedSemanticRows(
	reader io.Reader,
	wantSHA256 string,
	count uint64,
) ([]semanticUnit, error) {
	hash := sha256.New()
	rows, err := readSemanticRowsFrom(io.TeeReader(reader, hash), count)
	if err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != wantSHA256 {
		return nil, fmt.Errorf("semantic artifact checksum mismatch")
	}
	return rows, nil
}

func readSemanticRowsFrom(reader io.Reader, count uint64) ([]semanticUnit, error) {
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
		copy(unit.id[:], fixed[24:24+sha256.Size])
		copy(unit.contentID[:], fixed[24+sha256.Size:24+2*sha256.Size])
		pathBytes := binary.LittleEndian.Uint32(fixed[88:92])
		languageBytes := binary.LittleEndian.Uint16(fixed[92:94])
		fileLanguageBytes := binary.LittleEndian.Uint16(fixed[94:96])
		symbolBytes := binary.LittleEndian.Uint16(fixed[96:98])
		if pathBytes > semanticMetadataFieldMax {
			return nil, fmt.Errorf("semantic row path is too large")
		}
		fieldsBytes := uint64(pathBytes) + uint64(languageBytes) +
			uint64(fileLanguageBytes) + uint64(symbolBytes)
		if fieldsBytes > 2*semanticMetadataFieldMax {
			return nil, fmt.Errorf("semantic row fields are too large")
		}
		if cap(fields) < int(fieldsBytes) {
			fields = make([]byte, int(fieldsBytes))
		} else {
			fields = fields[:int(fieldsBytes)]
		}
		if _, err := io.ReadFull(buffered, fields); err != nil {
			return nil, err
		}
		languageAt := int(pathBytes)
		fileLanguageAt := languageAt + int(languageBytes)
		symbolAt := fileLanguageAt + int(fileLanguageBytes)
		pathField := fields[:languageAt]
		if previousPath != "" && previousPath == string(pathField) {
			unit.path = previousPath
		} else {
			unit.path = string(pathField)
			previousPath = unit.path
		}
		unit.language = internSemanticRowLanguage(internedLanguages, fields[languageAt:fileLanguageAt])
		unit.fileLanguage = internSemanticRowLanguage(internedLanguages, fields[fileLanguageAt:symbolAt])
		unit.symbol = string(fields[symbolAt:])
		unit.kind = semanticUnitKind(fixed[98])
		unit.parserResult = semanticParserResult(fixed[99])
		var wantID semanticUnitID
		wantID, idBuffer = makeSemanticUnitIDBuffered(*unit, idBuffer)
		if unit.row != uint64(index) || unit.path == "" || unit.end <= unit.start ||
			unit.end-unit.start > semanticUnitMaxBytes ||
			unit.id != wantID ||
			unit.kind < semanticUnitGap || unit.kind > semanticUnitPacked ||
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

func exactSemanticSearch(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query *semanticQueryEmbedding,
	limit int,
) ([]semanticHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queryVectors, err := semanticQueryTokenVectors(query)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || len(vectors) == 0 {
		return nil, nil
	}
	return exactSemanticRowSelection(ctx, vectors, queryVectors, nil, len(vectors), limit)
}

// exactSemanticSortedCandidates scores a sorted set of unique row IDs. Search
// planning already produces this order, which keeps vector reads sequential.
func exactSemanticSortedCandidates(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query *semanticQueryEmbedding,
	rows []uint64,
	limit int,
) ([]semanticHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queryVectors, err := semanticQueryTokenVectors(query)
	if err != nil {
		return nil, err
	}
	return exactSemanticSortedRows(ctx, vectors, queryVectors, rows, limit)
}

// exactSemanticSortedRows scores an owned normalized query buffer against
// sorted unique row IDs. Exact scoring validates and scales the query in place.
func exactSemanticSortedRows(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query []semanticVector,
	rows []uint64,
	limit int,
) ([]semanticHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || len(rows) == 0 {
		return nil, nil
	}
	for index, row := range rows {
		if row >= uint64(len(vectors)) || (index > 0 && row <= rows[index-1]) {
			return nil, fmt.Errorf("semantic candidate row %d is invalid", row)
		}
	}
	return exactSemanticRows(ctx, vectors, query, rows, limit)
}

// exactSemanticRows scores an owned normalized query buffer against the given
// row IDs. Exact scoring validates and scales the query in place.
func exactSemanticRows(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query []semanticVector,
	rows []uint64,
	limit int,
) ([]semanticHit, error) {
	return exactSemanticRowSelection(ctx, vectors, query, rows, len(rows), limit)
}

// exactSemanticRowSelection validates and scales its owned normalized query
// buffer in place. It uses implicit consecutive row IDs when rows is nil. This
// keeps a full recovery scan from allocating one uint64 for every row.
func exactSemanticRowSelection(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	query []semanticVector,
	rows []uint64,
	rowCount int,
	limit int,
) ([]semanticHit, error) {
	scaledQuery, err := prepareSemanticSNORM16Query(query)
	if err != nil {
		return nil, err
	}
	// One row-token pair compares all fine centroids. Include query length in the
	// worker count so long descriptive queries do not run large exact routes on
	// one or two CPUs.
	const rowTokenPairsPerWorker = 512
	work := rowCount * max(1, len(query))
	workers := min(runtime.GOMAXPROCS(0), max(1, work/rowTokenPairsPerWorker))
	if workers == 1 {
		hits, err := exactSemanticRange(ctx, vectors, scaledQuery, rows, 0, rowCount, limit)
		return hits, errors.Join(err, ctx.Err())
	}
	type result struct {
		hits []semanticHit
		err  error
	}
	results := make(chan result, workers)
	for worker := range workers {
		start := rowCount * worker / workers
		end := rowCount * (worker + 1) / workers
		go func() {
			hits, err := exactSemanticRange(ctx, vectors, scaledQuery, rows, start, end, limit)
			results <- result{hits: hits, err: err}
		}()
	}
	merged := make([]semanticHit, 0, min(rowCount, workers*limit))
	var searchErr error
	for range workers {
		result := <-results
		merged = append(merged, result.hits...)
		searchErr = errors.Join(searchErr, result.err)
	}
	if err := errors.Join(searchErr, ctx.Err()); err != nil {
		return nil, err
	}
	sortSemanticHits(merged)
	if len(merged) > limit {
		merged = merged[:limit]
	}
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

func exactSemanticRange(
	ctx context.Context,
	vectors []semanticFineSNORM16Vectors,
	scaledQuery []semanticVector,
	rows []uint64,
	start int,
	end int,
	limit int,
) ([]semanticHit, error) {
	if limit <= 0 || start >= end {
		return nil, nil
	}
	hits := make([]semanticHit, 0, min(limit, end-start))
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
		hit := semanticHit{row: row, score: score}
		hits = keepBestSemanticHit(hits, hit, limit)
	}
	sortSemanticHits(hits)
	return hits, nil
}

// keepBestSemanticHit maintains a bounded worst-first heap. The root is the
// least useful retained hit. Updating the retained set costs O(log limit)
// instead of moving an ordered slice on every insertion.
func keepBestSemanticHit(hits []semanticHit, hit semanticHit, limit int) []semanticHit {
	if limit <= 0 {
		return hits
	}
	if len(hits) < limit {
		hits = append(hits, hit)
		semanticHitHeapUp(hits, len(hits)-1)
		return hits
	}
	if !semanticHitLess(hit, hits[0]) {
		return hits
	}
	hits[0] = hit
	semanticHitHeapDown(hits, 0)
	return hits
}

func semanticHitHeapUp(hits []semanticHit, index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !semanticHitWorse(hits[index], hits[parent]) {
			return
		}
		hits[index], hits[parent] = hits[parent], hits[index]
		index = parent
	}
}

func semanticHitHeapDown(hits []semanticHit, index int) {
	for {
		left := index*2 + 1
		if left >= len(hits) {
			return
		}
		worst := left
		right := left + 1
		if right < len(hits) && semanticHitWorse(hits[right], hits[left]) {
			worst = right
		}
		if !semanticHitWorse(hits[worst], hits[index]) {
			return
		}
		hits[index], hits[worst] = hits[worst], hits[index]
		index = worst
	}
}

func sortSemanticHits(hits []semanticHit) {
	slices.SortFunc(hits, compareSemanticHits)
}

func semanticHitLess(left, right semanticHit) bool {
	return compareSemanticHits(left, right) < 0
}

func semanticHitWorse(left, right semanticHit) bool {
	return compareSemanticHits(left, right) > 0
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
