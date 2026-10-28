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
	// A compatibility revision changes the generation key for behavior that
	// the asset hashes and the typed format settings cannot describe.
	// anchored-spherical-v2 includes the normalization of finite values whose
	// scale float32 cannot hold. semanticVectorCodec names the fine-vector
	// encoding on its own.
	semanticRepresentationRevision = "anchored-spherical-v2"
	semanticModelAlgorithmRevision = "lateon-code-edge-proxy-no-symbol-centroids-v5"
	semanticGenerationPrefix       = "semantic-"
	semanticManifestFile           = "manifest.json"
	semanticRowsFile               = "rows.bin"
	semanticVectorsFile            = "vectors.f16"
	semanticCoarseFile             = "coarse.snorm16"
	semanticUSearchFilePrefix      = "index-"
	semanticUSearchFileSuffix      = ".usearch"
	semanticRowsMagic              = "SEEKSROW"
	semanticRowsHeaderBytes        = len(semanticRowsMagic) + 8
	semanticRowFixedBytes          = 3*8 + 3*sha256.Size + 4 + 4 + 4 + 2
	semanticMetadataFieldMax       = 1 << 20
)

// semanticRowsHashed counts the full checks of rows.bin, and
// semanticShardsHashed the USearch shards that a check hashed. A full check
// reads the whole file, which a warm search must not do again. Each count is
// one atomic add at that read, and the read costs much more than the add.
// Tests read the counts to prove that a search trusts a checked file.
var (
	semanticRowsHashed   atomic.Int64
	semanticShardsHashed atomic.Int64
)

// semanticNormalizedSquaredNormTolerance limits the squared norms of accepted
// float32 centroids and query tokens. The stored-vector tolerances of the
// SNORM16 and binary16 codecs come from it.
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

// semanticRepresentationCompatibility names the stored representation in the
// generation key. The "r0" after the fine count is the number of refinement
// passes of the fine centroids: Seek stores them as selectCentroids selects
// them.
func semanticRepresentationCompatibility() string {
	return fmt.Sprintf(
		"%s-c%d-r%d-f%d-r0-d%d",
		semanticRepresentationRevision,
		semanticCoarseCentroidsPerUnit,
		semanticCoarseRefinementPasses,
		semanticFineCentroidsPerUnit,
		semanticEmbeddingDimensions,
	)
}

type semanticVector [semanticEmbeddingDimensions]float32

type semanticArtifact struct {
	Bytes  uint64 `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// semanticSizedArtifact records the exact length of vector data that a build
// can make again. It has no checksum of the whole file, so an open cannot find
// every change that keeps the length. A hash check would read the whole file
// at each open, a read of about the size of the index.
type semanticSizedArtifact struct {
	Bytes uint64 `json:"bytes"`
}

// semanticUSearchShard records one graph shard. ID identifies its graph
// (semanticUSearchShardID). A later build that gets the same ID reuses the
// file and does not build it again.
type semanticUSearchShard struct {
	Name  string `json:"name"`
	Start uint64 `json:"start"`
	Rows  uint64 `json:"rows"`
	ID    string `json:"id"`
	semanticArtifact
	// Check is the shard file as the full check at build saw it.
	Check semanticRowsCheck `json:"check"`
}

// semanticRowsCheck records a file as the full check at build saw it. A later
// open that finds the same length, device, inode, mtime, and ctime trusts the
// file. Any other value runs the full check again.
type semanticRowsCheck struct {
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns"`
}

// semanticManifest binds the semantic artifacts to their exact formats.
// VectorCodec identifies the fine-vector byte encoding. VectorStride is the
// byte width of one semantic row with all its fine centroids, not of one
// centroid vector. Runtime is the bundled ONNX Runtime version that made the
// vectors. semanticInferenceRuntime tells why a new run time must not reuse
// them. USearchShardRows is the shard size of the cut
// (semanticUSearchShardRowsFor). It comes from the host, so it is not in the
// generation key.
type semanticManifest struct {
	Format           uint32                 `json:"format"`
	Source           string                 `json:"source"`
	Model            string                 `json:"model"`
	Extractor        string                 `json:"extractor"`
	Representation   string                 `json:"representation"`
	VectorCodec      string                 `json:"vector_codec"`
	VectorStride     uint32                 `json:"vector_stride"`
	USearch          string                 `json:"usearch"`
	Runtime          string                 `json:"runtime"`
	Dimensions       uint32                 `json:"dimensions"`
	Rows             uint64                 `json:"rows"`
	RowsFile         semanticArtifact       `json:"rows_file"`
	RowsCheck        semanticRowsCheck      `json:"rows_check"`
	VectorsFile      semanticSizedArtifact  `json:"vectors_file"`
	CoarseFile       semanticSizedArtifact  `json:"coarse_file"`
	USearchShardRows uint64                 `json:"usearch_shard_rows"`
	USearchFiles     []semanticUSearchShard `json:"usearch_files"`
	// ModelPathBase is the model path base of the node that built the rows
	// (modelPathBase). The rows' windows name each file's path from it.
	ModelPathBase string `json:"model_path_base,omitempty"`
}

// semanticGeneration owns one open semantic generation. vectors and coarse
// alias the memory in vectorMap and coarseMap and must not be used after
// Close. A generation validates each graph before its first native use.
type semanticGeneration struct {
	manifest          semanticManifest
	rows              []semanticUnit
	vectors           []semanticFineF16Vectors
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

// semanticInferenceRuntime reports the bundled ONNX Runtime version. It reads
// the embedded runtime manifest, so it starts no run time and decodes no
// model. So it is safe on lexical-only paths that never load either.
//
// The version is in the generation key because a new run time can change the
// numbers that the model gives. A change to its graph optimizations, such as
// reshape fusion or constant folding, changes how it splits the graph. That
// changes the stored vectors. An index lives longer than the binary that
// wrote it. Without the version, a new run time would query vectors that an
// older one made.
//
// The key holds no execution provider. The provider needs the compiled model
// cache directory, which needs the model bytes. A decode of those bytes on a
// lexical-only search would add the model cost that this path avoids. A
// provider that passes the run-time comparison in
// verifyLateOnAcceleratedProvider meets much tighter limits than this key
// could check.
var semanticInferenceRuntime = sync.OnceValue(func() string {
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		return "unknown"
	}
	return bundle.Version
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

// writeSemanticGenerationFromParts writes rows.bin and the manifest of a
// generation whose vector files and graph shards are in dir. The build made
// dir (prepareSemanticGenerationDirectory) before it wrote those files.
func writeSemanticGenerationFromParts(
	ctx context.Context,
	dir string,
	source string,
	modelPathBase string,
	units []semanticUnit,
	vectorsArtifact semanticSizedArtifact,
	coarseArtifact semanticSizedArtifact,
	usearchShardRows uint64,
	usearchShards []semanticUSearchShard,
) error {
	if source == "" {
		return fmt.Errorf("semantic generation identity is missing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	wantVectorBytes, sizeOK := checkedSemanticVectorBytes(uint64(len(units)))
	if !sizeOK {
		return fmt.Errorf("semantic vector size is invalid")
	}
	if vectorsArtifact.Bytes != wantVectorBytes {
		return fmt.Errorf("semantic vector artifact has %d bytes, want %d", vectorsArtifact.Bytes, wantVectorBytes)
	}
	if wantCoarse, ok := checkedSemanticCoarseBytes(uint64(len(units))); !ok || coarseArtifact.Bytes != wantCoarse {
		return fmt.Errorf("semantic coarse artifact has %d bytes, want %d", coarseArtifact.Bytes, wantCoarse)
	}
	usearchCompatibility, err := semanticUSearchCompatibility()
	if err != nil {
		return err
	}
	// The shard build hashed each shard when it saved it. The lazy check at
	// first use hashes a shard again only if its file changed.
	if err := validateSemanticUSearchShardMetadata(
		uint64(len(units)), usearchShardRows, usearchShards, semanticSourceHasGraph(source),
	); err != nil {
		return err
	}
	rowsArtifact, err := writeSemanticRows(filepath.Join(dir, semanticRowsFile), units)
	if err != nil {
		return fmt.Errorf("write semantic generation: %w", err)
	}
	rowsInfo, err := os.Lstat(filepath.Join(dir, semanticRowsFile))
	if err != nil {
		return fmt.Errorf("write semantic generation: %w", err)
	}

	return writeSemanticManifest(filepath.Join(dir, semanticManifestFile), semanticManifest{
		Format:           semanticFormatVersion,
		Source:           source,
		Model:            semanticModelCompatibility(),
		Extractor:        semanticExtractorID,
		Representation:   semanticRepresentationCompatibility(),
		VectorCodec:      semanticVectorCodec,
		VectorStride:     semanticFineVectorBytesPerUnit,
		USearch:          usearchCompatibility,
		Runtime:          semanticInferenceRuntime(),
		Dimensions:       semanticEmbeddingDimensions,
		Rows:             uint64(len(units)),
		RowsFile:         rowsArtifact,
		RowsCheck:        semanticRowsCheckOf(rowsInfo),
		VectorsFile:      vectorsArtifact,
		CoarseFile:       coarseArtifact,
		USearchShardRows: usearchShardRows,
		USearchFiles:     usearchShards,
		ModelPathBase:    modelPathBase,
	})
}

// writeSemanticRows writes the rows file. It starts with the magic and a
// little-endian uint64 row count. Then each row stores these fields:
//   - row, start, and end as uint64 values;
//   - three SHA-256 values (unit ID, content ID, window key);
//   - the path, language, and symbol lengths as uint32;
//   - kind and parser result as bytes.
//
// The path, language, and symbol bytes follow in that order with no padding.
// readSemanticRowsFrom and checkedSemanticRowsMinimumBytes must match this
// layout.
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

// inspectSemanticArtifact returns the length and the SHA-256 of the regular
// file at path. The length is the count of bytes that the hash read, so both
// describe the same bytes, and the read needs no stat.
func inspectSemanticArtifact(path string) (semanticArtifact, error) {
	file, err := openRegularSemanticFile(path)
	if err != nil {
		return semanticArtifact{}, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return semanticArtifact{}, err
	}
	return semanticArtifact{Bytes: uint64(size), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// openSemanticGenerationForSearch trusts a rows.bin whose length, device,
// inode, mtime, and ctime match the recorded check. It hashes no graph shard
// until a native plan needs it.
func openSemanticGenerationForSearch(dir, source string) (*semanticGeneration, error) {
	return openSemanticGenerationWithOptions(dir, source, false)
}

// openSemanticGenerationRows opens a generation for its rows and vectors only
// (reuse, the unit store). It neither checks nor reads a USearch shard, so a
// damaged graph shard does not stop its rows.
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
	rows, err := readCheckedSemanticRows(dir, manifest)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", semanticRowsFile, err)
	}
	vectors, vectorMap, err := mapSemanticRows[semanticFineF16Vectors](filepath.Join(dir, semanticVectorsFile), manifest.Rows)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", semanticVectorsFile, err)
	}
	coarse, coarseMap, err := mapSemanticRows[semanticCoarseSNORM16Vectors](filepath.Join(dir, semanticCoarseFile), manifest.Rows)
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

// validateUSearchShard checks the graph shard at index once. The open makes a
// path and a check state for each shard of the manifest, and the search plan
// gives only the index of a manifest shard.
func (generation *semanticGeneration) validateUSearchShard(index int) error {
	validation := &generation.usearchValidation[index]
	validation.once.Do(func() {
		shard := generation.manifest.USearchFiles[index]
		// A shard that the check at build saw is trusted, as rows.bin is.
		// Any other file gets a full hash.
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

// readSemanticManifest reads and checks the manifest of dir, with its USearch
// shard list. So the build gate and the open apply one rule.
func readSemanticManifest(dir, source string) (semanticManifest, error) {
	return readSemanticManifestChecked(dir, source, true)
}

// readSemanticManifestChecked reads and checks the manifest of dir. With graph,
// it also checks its USearch shard list.
func readSemanticManifestChecked(dir, source string, graph bool) (semanticManifest, error) {
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil {
		return semanticManifest{}, err
	}
	if err := validateSemanticManifest(manifest, source); err != nil {
		return semanticManifest{}, err
	}
	if graph {
		if err := validateSemanticUSearchShardMetadata(
			manifest.Rows, manifest.USearchShardRows, manifest.USearchFiles, semanticSourceHasGraph(manifest.Source),
		); err != nil {
			return semanticManifest{}, err
		}
	}
	return manifest, nil
}

// readSemanticManifestUnchecked decodes the manifest of dir and does not check
// its source. So a build can name the source of another generation. The open
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

// validateJSONValue reads one JSON value and rejects an object with a
// duplicate key. Decoder.Token returns an error for a token out of place. So a
// value never starts with a closing delimiter, an object key is always a
// string, and the token after the last member or element closes its object or
// array.
func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, _ := nameToken.(string)
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate JSON field %q", name)
			}
			seen[name] = struct{}{}
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("JSON object is not closed")
		}
	case json.Delim('['):
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("JSON array is not closed")
		}
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
	return validateSemanticDigest(manifest.RowsFile.SHA256)
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
// row, each with at most shardRows rows (a power of two from 256 to 4,096). A
// generation with no graph must have no shard.
func validateSemanticUSearchShardMetadata(rows, shardRows uint64, shards []semanticUSearchShard, graph bool) error {
	if rows == 0 || !graph {
		if len(shards) != 0 || shardRows != 0 {
			return fmt.Errorf("semantic generation with no graph has USearch shards")
		}
		return nil
	}
	if len(shards) == 0 {
		return fmt.Errorf("semantic generation has an invalid USearch shard count")
	}
	if shardRows < semanticUSearchMinShardRows || shardRows > semanticUSearchMaxShardRows ||
		shardRows&(shardRows-1) != 0 {
		return fmt.Errorf("semantic generation has an invalid USearch shard size %d", shardRows)
	}
	next := uint64(0)
	for index, shard := range shards {
		if shard.Name != semanticUSearchShardName(index) || shard.Start != next ||
			shard.Rows == 0 || shard.Rows > rows-next || shard.Rows > shardRows || shard.Bytes == 0 {
			return fmt.Errorf("USearch shard %d metadata is invalid", index)
		}
		if err := validateSemanticDigest(shard.SHA256); err != nil {
			return err
		}
		if err := validateSemanticDigest(shard.ID); err != nil {
			return fmt.Errorf("USearch shard %d has an invalid ID", index)
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

func checkedSemanticVectorBytes(rows uint64) (uint64, bool) {
	const rowBytes = semanticFineVectorBytesPerUnit
	if rows > math.MaxUint64/rowBytes {
		return 0, false
	}
	return rows * rowBytes, true
}

// validateSemanticDigest checks that value is a hex SHA-256 digest.
func validateSemanticDigest(value string) error {
	if !validSHA256Hex(value) {
		return fmt.Errorf("semantic artifact has an invalid SHA-256")
	}
	return nil
}

// validSHA256Hex reports whether value is a SHA-256 digest in hexadecimal.
func validSHA256Hex(value string) bool {
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size
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

// readCheckedSemanticRows reads the rows.bin of the generation in dir. A file
// that the recorded check of manifest saw is trusted. When the device, inode,
// mtime, or ctime differs, it hashes the file and compares the hash with the
// manifest.
func readCheckedSemanticRows(dir string, manifest semanticManifest) ([]semanticUnit, error) {
	file, err := openRegularSemanticFile(filepath.Join(dir, semanticRowsFile))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || uint64(info.Size()) != manifest.RowsFile.Bytes {
		return nil, fmt.Errorf("semantic artifact length mismatch")
	}
	if semanticRowsCheckOf(info) == manifest.RowsCheck {
		return readSemanticRowsFrom(file, manifest.Rows, false)
	}
	rows, err := readVerifiedSemanticRows(file, manifest.RowsFile.SHA256, manifest.Rows)
	if err == nil {
		// The full check passed: record the file as it is now, so later
		// searches trust it again.
		refreshSemanticRowsCheck(dir, semanticRowsCheckOf(info))
	}
	return rows, err
}

// refreshSemanticRowsCheck rewrites a generation's manifest with a new rows
// check. A failure keeps the old check: the next open checks again.
func refreshSemanticRowsCheck(dir string, check semanticRowsCheck) {
	rewriteSemanticManifest(dir, func(manifest *semanticManifest) bool {
		manifest.RowsCheck = check
		return true
	})
}

// rewriteSemanticManifest reads the manifest of dir and lets update change it.
// When update returns true, it writes a temporary file and renames it over the
// manifest. So a reader sees the old manifest or the new one.
func rewriteSemanticManifest(dir string, update func(*semanticManifest) bool) {
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil || !update(&manifest) {
		return
	}
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
	return semanticRowsCheck{
		Device:  device,
		Inode:   inode,
		MtimeNs: mtime,
		CtimeNs: statCtimeNano(*info.Sys().(*syscall.Stat_t)),
	}
}

func readVerifiedSemanticRows(
	reader io.Reader,
	wantSHA256 string,
	count uint64,
) ([]semanticUnit, error) {
	semanticRowsHashed.Add(1)
	hash := sha256.New()
	rows, err := readSemanticRowsFrom(io.TeeReader(reader, hash), count, true)
	if err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != wantSHA256 {
		return nil, fmt.Errorf("semantic artifact checksum mismatch")
	}
	return rows, nil
}

// readSemanticRowsFrom parses rows. verifyIDs computes each unit ID again,
// which the full check does. A file that the recorded check saw is trusted, so
// its open hashes nothing.
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

// mapSemanticRows checks that the file at path holds exactly count rows of T
// and maps its bytes as those rows. The view needs a little-endian target, as
// every supported build target is. It does not check the codes. The exact
// scorer (exactSemanticRange) and a copy (semanticRowVectorsValid) check each
// vector before they use it.
func mapSemanticRows[T any](path string, count uint64) ([]T, []byte, error) {
	file, err := openRegularSemanticFile(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	rowBytes := uint64(unsafe.Sizeof(*new(T)))
	info, err := file.Stat()
	if err != nil || count > math.MaxUint64/rowBytes || uint64(info.Size()) != count*rowBytes {
		return nil, nil, fmt.Errorf("semantic artifact length mismatch")
	}
	if count == 0 {
		return nil, nil, nil
	}
	mapped, err := unix.Mmap(int(file.Fd()), 0, int(count*rowBytes), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return nil, nil, fmt.Errorf("map semantic artifact: %w", err)
	}
	return unsafe.Slice((*T)(unsafe.Pointer(&mapped[0])), int(count)), mapped, nil
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
	vectors []semanticFineF16Vectors,
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

// exactSemanticSortedCandidates scores a sorted set of unique row IDs. The
// search plan gives this order, which keeps the vector reads sequential.
func exactSemanticSortedCandidates(
	ctx context.Context,
	vectors []semanticFineF16Vectors,
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

// exactSemanticSortedRows scores a normalized query against sorted unique row
// IDs. The query buffer does not change.
func exactSemanticSortedRows(
	ctx context.Context,
	vectors []semanticFineF16Vectors,
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

// exactSemanticRowSelection scores every row of vectors when rows is nil. This
// keeps a full recovery scan from allocating one uint64 for every row. The
// workers keep only the rows that reach floor, so the one sort sees only those
// rows.
func exactSemanticRowSelection(
	ctx context.Context,
	vectors []semanticFineF16Vectors,
	query []semanticVector,
	rows []uint64,
	floor float32,
) ([]semanticHit, error) {
	packed, err := prepareSemanticFineQuery(query)
	if err != nil {
		return nil, err
	}
	rowCount := len(rows)
	if rows == nil {
		rowCount = len(vectors)
	}
	// One row-token pair compares all fine centroids. The worker count includes
	// the query length, so a long query does not run a large exact route on one
	// or two CPUs.
	const rowTokenPairsPerWorker = 512
	work := rowCount * max(1, len(query))
	workers := min(runtime.GOMAXPROCS(0), max(1, work/rowTokenPairsPerWorker))
	if workers == 1 {
		hits, err := exactSemanticRange(ctx, vectors, &packed, rows, 0, rowCount, floor)
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
			hits[worker], errs[worker] = exactSemanticRange(ctx, vectors, &packed, rows, start, end, floor)
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

// prepareSemanticFineQuery checks each normalized query token and rounds it to
// binary16, as the stored rows are. It packs the tokens for the fastest kernel
// family of the host CPU. The query buffer does not change.
func prepareSemanticFineQuery(query []semanticVector) (semanticNumKongPack, error) {
	backend, err := semanticNumKongBackend()
	if err != nil {
		return semanticNumKongPack{}, err
	}
	return prepareSemanticFineQueryFor(backend, query)
}

func prepareSemanticFineQueryFor(backend int, query []semanticVector) (semanticNumKongPack, error) {
	if len(query) == 0 {
		return semanticNumKongPack{}, fmt.Errorf("semantic query has no scoring token")
	}
	encoded := make([]semanticF16Vector, len(query))
	for token := range query {
		stored, err := encodeSemanticF16Vector(&query[token])
		if err != nil {
			return semanticNumKongPack{}, fmt.Errorf("semantic query token %d: %w", token, err)
		}
		encoded[token] = stored
	}
	return packSemanticF16Query(backend, encoded), nil
}

// semanticFineBlockRows gives the rows of one kernel call: about 4,096
// row-token pairs, within 16 to 512 rows. So the dot products of a block stay
// in the CPU cache, and a canceled search stops after one block.
func semanticFineBlockRows(tokens int) int {
	return max(16, min(512, 4096/max(1, tokens)))
}

// exactSemanticRange scores the rows from start to end and gives the rows
// whose summed fine MaxSim reaches floor, in row order. Row i is rows[i], or i
// when rows is nil; the rows must increase. NumKong's exact batched binary16
// dot products score one block of rows for each call: semantic_numkong.go
// gives the reasons, and semantic_vector_f16.go gives the rounding error. The
// same call checks that each stored centroid is finite with a squared norm
// near 1, because the vectors file has no checksum.
func exactSemanticRange(
	ctx context.Context,
	vectors []semanticFineF16Vectors,
	query *semanticNumKongPack,
	rows []uint64,
	start int,
	end int,
	floor float32,
) ([]semanticHit, error) {
	if start >= end {
		return nil, nil
	}
	blockRows := semanticFineBlockRows(query.tokens)
	scratch := getSemanticFineScratch(min(blockRows, end-start), query.tokens, rows != nil)
	defer putSemanticFineScratch(scratch)
	var hits []semanticHit
	for index := start; index < end; index += blockRows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count := min(blockRows, end-index)
		var blockIDs []uint64
		rowAt := func(offset int) uint64 { return uint64(index + offset) }
		if rows != nil {
			blockIDs = rows[index : index+count]
			rowAt = func(offset int) uint64 { return blockIDs[offset] }
		}
		// The kernel reads the rows of the block in C, so each row must be
		// in vectors. The rows increase, so the last row bounds the block.
		if last := rowAt(count - 1); last >= uint64(len(vectors)) {
			return nil, fmt.Errorf("semantic row %d is out of range", last)
		}
		bad, centroid := scoreSemanticFineBlock(vectors, blockIDs, uint64(index), count, query, scratch)
		if bad >= 0 {
			return nil, fmt.Errorf("semantic row %d centroid %d: stored vector is not finite or not normalized", rowAt(bad), centroid)
		}
		for offset, score := range scratch.scores[:count] {
			if math.IsInf(float64(score), 0) || math.IsNaN(float64(score)) {
				return nil, fmt.Errorf("semantic row %d produced an invalid score", rowAt(offset))
			}
			if score >= floor {
				hits = append(hits, semanticHit{row: rowAt(offset), score: score})
			}
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
