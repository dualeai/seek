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
	"sort"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	semanticFormatVersion = 3
	// Compatibility revisions are cache barriers for behavior that asset hashes
	// and typed format settings cannot describe.
	semanticRepresentationRevision = "anchored-spherical-f32-v1"
	semanticModelAlgorithmRevision = "lateon-code-edge-proxy-no-symbol-centroids-v5"
	semanticGenerationPrefix       = "semantic-"
	semanticManifestFile           = "manifest.json"
	semanticRowsFile               = "rows.bin"
	semanticVectorsFile            = "vectors.f32"
	semanticUSearchFilePrefix      = "index-"
	semanticUSearchFileSuffix      = ".usearch"
	semanticRowsMagic              = "SEEKSROW"
	semanticManifestMaxBytes       = 64 << 10
	semanticMetadataFieldMax       = 1 << 20
	semanticMaxRows                = 10_000_000
	semanticFineVectorBytesPerUnit = semanticFineCentroidsPerUnit * semanticEmbeddingDimensions * 4
)

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

// semanticSizedArtifact records the length needed to map rebuildable vector
// data safely. It intentionally does not provide whole-file integrity. Reading
// the full vector file to hash it would add an index-sized pass to every build.
type semanticSizedArtifact struct {
	Bytes uint64 `json:"bytes"`
}

type semanticUSearchShard struct {
	Name  string `json:"name"`
	Start uint64 `json:"start"`
	Rows  uint64 `json:"rows"`
	semanticArtifact
}

type semanticManifest struct {
	Format         uint32                 `json:"format"`
	Source         string                 `json:"source"`
	Model          string                 `json:"model"`
	Extractor      string                 `json:"extractor"`
	Representation string                 `json:"representation"`
	USearch        string                 `json:"usearch"`
	Dimensions     uint32                 `json:"dimensions"`
	Rows           uint64                 `json:"rows"`
	RowsFile       semanticArtifact       `json:"rows_file"`
	VectorsFile    semanticSizedArtifact  `json:"vectors_file"`
	USearchFiles   []semanticUSearchShard `json:"usearch_files"`
}

// semanticGeneration owns one open semantic generation. vectors aliases the
// memory in vectorMap and must not be used after Close. usearchErr keeps the
// exact mapped vectors available when only the approximate index is damaged.
type semanticGeneration struct {
	manifest   semanticManifest
	rows       []semanticUnit
	vectors    []semanticFineVectors
	vectorMap  []byte
	usearch    []string
	usearchErr error
}

func semanticGenerationKey(source string) string {
	hash := sha256.New()
	hash.Write([]byte("seek-semantic-generation-v1\x00"))
	for _, field := range []string{
		source,
		semanticModelCompatibility(),
		semanticExtractorID,
		semanticRepresentationCompatibility(),
		semanticUSearchLayout(),
		semanticUSearchAssetKey(),
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
		USearch:        usearchCompatibility,
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

func writeSemanticRows(path string, units []semanticUnit) (semanticArtifact, error) {
	return writeSemanticArtifact(path, func(writer io.Writer) error {
		buffered := bufio.NewWriterSize(writer, 256<<10)
		var fileHeader [len(semanticRowsMagic) + 8]byte
		copy(fileHeader[:], semanticRowsMagic)
		binary.LittleEndian.PutUint64(fileHeader[len(semanticRowsMagic):], uint64(len(units)))
		if _, err := buffered.Write(fileHeader[:]); err != nil {
			return err
		}
		const fixedRowBytes = 3*8 + 2*sha256.Size + 4 + 2 + 2 + 2
		var fixed [fixedRowBytes]byte
		for index, unit := range units {
			if unit.row != uint64(index) || unit.end <= unit.start ||
				len(unit.path) > semanticMetadataFieldMax ||
				len(unit.language) > math.MaxUint16 || len(unit.symbol) > math.MaxUint16 {
				return fmt.Errorf("invalid semantic row %d", index)
			}
			binary.LittleEndian.PutUint64(fixed[0:8], unit.row)
			binary.LittleEndian.PutUint64(fixed[8:16], unit.start)
			binary.LittleEndian.PutUint64(fixed[16:24], unit.end)
			copy(fixed[24:24+sha256.Size], unit.id[:])
			copy(fixed[24+sha256.Size:24+2*sha256.Size], unit.contentID[:])
			binary.LittleEndian.PutUint32(fixed[88:92], uint32(len(unit.path)))
			binary.LittleEndian.PutUint16(fixed[92:94], uint16(len(unit.language)))
			binary.LittleEndian.PutUint16(fixed[94:96], uint16(len(unit.symbol)))
			fixed[96] = byte(unit.kind)
			fixed[97] = byte(unit.parserResult)
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

func encodeSemanticFineVectors(
	buffer []byte,
	embeddings []semanticUnitEmbedding,
	startRow int,
) ([]byte, error) {
	wantBytes := len(embeddings) * semanticFineVectorBytesPerUnit
	if startRow < 0 || len(buffer) < wantBytes {
		return nil, fmt.Errorf("semantic vector buffer is too small")
	}
	encoded := buffer[:wantBytes]
	at := 0
	for row, embedding := range embeddings {
		for centroid, vector := range embedding.fine {
			if err := validateNormalizedSemanticVector(vector); err != nil {
				return nil, fmt.Errorf("semantic row %d centroid %d: %w", startRow+row, centroid, err)
			}
			for _, value := range vector {
				binary.LittleEndian.PutUint32(encoded[at:at+4], math.Float32bits(value))
				at += 4
			}
		}
	}
	return encoded, nil
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

// openSemanticGeneration validates the manifest and row data, checks the vector
// file length before mapping it, and records validation failure for any USearch
// shard. It does not hash the rebuildable vector file.
func openSemanticGeneration(dir, source string) (*semanticGeneration, error) {
	manifest, err := readSemanticManifest(dir, source)
	if err != nil {
		return nil, err
	}
	rowsFile, err := openCheckedSemanticArtifact(
		filepath.Join(dir, semanticRowsFile),
		manifest.RowsFile,
	)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", semanticRowsFile, err)
	}
	defer func() { _ = rowsFile.Close() }()
	vectorsFile, err := openRegularSemanticFile(filepath.Join(dir, semanticVectorsFile))
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", semanticVectorsFile, err)
	}
	defer func() { _ = vectorsFile.Close() }()
	rows, err := readSemanticRowsFrom(rowsFile, manifest.Rows)
	if err != nil {
		return nil, err
	}
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
	if err := validateSemanticUSearchShards(dir, manifest.Rows, manifest.USearchFiles); err != nil {
		generation.usearchErr = err
	}
	return generation, nil
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
		manifest.USearch != usearchCompatibility ||
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
	const headerBytes = uint64(len(semanticRowsMagic) + 8)
	const fixedRowBytes = uint64(3*8 + 2*sha256.Size + 4 + 2 + 2 + 2)
	if rows > (math.MaxUint64-headerBytes)/fixedRowBytes {
		return 0, false
	}
	return headerBytes + rows*fixedRowBytes, true
}

func semanticUSearchShardName(index int) string {
	return fmt.Sprintf("%s%05d%s", semanticUSearchFilePrefix, index, semanticUSearchFileSuffix)
}

func validateSemanticUSearchShards(dir string, rows uint64, shards []semanticUSearchShard) error {
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
		if err := validateSemanticArtifact(filepath.Join(dir, shard.Name), shard.semanticArtifact); err != nil {
			return fmt.Errorf("validate %s: %w", shard.Name, err)
		}
		next += shard.Rows
	}
	if next != rows {
		return fmt.Errorf("USearch shards cover %d rows, want %d", next, rows)
	}
	return nil
}

func checkedSemanticVectorBytes(rows uint64) (uint64, bool) {
	const rowBytes = semanticFineCentroidsPerUnit * semanticEmbeddingDimensions * 4
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

func openCheckedSemanticArtifact(path string, want semanticArtifact) (*os.File, error) {
	file, err := openRegularSemanticFile(path)
	if err != nil {
		return nil, err
	}
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || uint64(info.Size()) != want.Bytes {
		return nil, fmt.Errorf("semantic artifact length mismatch")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != want.SHA256 {
		return nil, fmt.Errorf("semantic artifact checksum mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	valid = true
	return file, nil
}

func readSemanticRowsFrom(reader io.Reader, count uint64) ([]semanticUnit, error) {
	buffered := bufio.NewReaderSize(reader, 256<<10)
	magic := make([]byte, len(semanticRowsMagic))
	if _, err := io.ReadFull(buffered, magic); err != nil || string(magic) != semanticRowsMagic {
		return nil, fmt.Errorf("semantic row header is invalid")
	}
	var storedCount uint64
	if err := binary.Read(buffered, binary.LittleEndian, &storedCount); err != nil || storedCount != count {
		return nil, fmt.Errorf("semantic row count is invalid")
	}
	rows := make([]semanticUnit, int(count))
	for index := range rows {
		unit := &rows[index]
		for _, target := range []*uint64{&unit.row, &unit.start, &unit.end} {
			if err := binary.Read(buffered, binary.LittleEndian, target); err != nil {
				return nil, fmt.Errorf("read semantic row %d: %w", index, err)
			}
		}
		if _, err := io.ReadFull(buffered, unit.id[:]); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(buffered, unit.contentID[:]); err != nil {
			return nil, err
		}
		var pathBytes uint32
		var languageBytes, symbolBytes uint16
		if err := binary.Read(buffered, binary.LittleEndian, &pathBytes); err != nil {
			return nil, err
		}
		if err := binary.Read(buffered, binary.LittleEndian, &languageBytes); err != nil {
			return nil, err
		}
		if err := binary.Read(buffered, binary.LittleEndian, &symbolBytes); err != nil {
			return nil, err
		}
		kinds := make([]byte, 2)
		if _, err := io.ReadFull(buffered, kinds); err != nil {
			return nil, err
		}
		if pathBytes > semanticMetadataFieldMax {
			return nil, fmt.Errorf("semantic row path is too large")
		}
		fieldsBytes := uint64(pathBytes) + uint64(languageBytes) + uint64(symbolBytes)
		if fieldsBytes > 2*semanticMetadataFieldMax {
			return nil, fmt.Errorf("semantic row fields are too large")
		}
		fields := make([]byte, int(fieldsBytes))
		if _, err := io.ReadFull(buffered, fields); err != nil {
			return nil, err
		}
		languageAt := int(pathBytes)
		symbolAt := languageAt + int(languageBytes)
		unit.path = string(fields[:languageAt])
		unit.language = string(fields[languageAt:symbolAt])
		unit.symbol = string(fields[symbolAt:])
		unit.kind = semanticUnitKind(kinds[0])
		unit.parserResult = semanticParserResult(kinds[1])
		if unit.row != uint64(index) || unit.path == "" || unit.end <= unit.start ||
			unit.end-unit.start > semanticUnitMaxBytes ||
			unit.id != makeSemanticUnitID(*unit) ||
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

func mapSemanticVectors(file *os.File, count uint64) ([]semanticFineVectors, []byte, error) {
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
	vectors := unsafe.Slice((*semanticFineVectors)(unsafe.Pointer(&mapped[0])), int(count))
	return vectors, mapped, nil
}

type semanticHit struct {
	row   uint64
	score float32
}

func exactSemanticSearch(
	ctx context.Context,
	vectors []semanticFineVectors,
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
	rows := make([]uint64, len(vectors))
	for row := range vectors {
		rows[row] = uint64(row)
	}
	return exactSemanticRows(ctx, vectors, queryVectors, rows, limit)
}

func exactSemanticCandidates(
	ctx context.Context,
	vectors []semanticFineVectors,
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
	if limit <= 0 || len(rows) == 0 {
		return nil, nil
	}
	rows = append([]uint64(nil), rows...)
	sort.Slice(rows, func(left, right int) bool { return rows[left] < rows[right] })
	for index, row := range rows {
		if row >= uint64(len(vectors)) || (index > 0 && row == rows[index-1]) {
			return nil, fmt.Errorf("semantic candidate row %d is invalid", row)
		}
	}
	return exactSemanticRows(ctx, vectors, queryVectors, rows, limit)
}

func exactSemanticRows(
	ctx context.Context,
	vectors []semanticFineVectors,
	query []semanticVector,
	rows []uint64,
	limit int,
) ([]semanticHit, error) {
	workers := min(runtime.GOMAXPROCS(0), max(1, len(rows)/256))
	if workers == 1 {
		hits, err := exactSemanticRange(ctx, vectors, query, rows, limit)
		return hits, errors.Join(err, ctx.Err())
	}
	type result struct {
		hits []semanticHit
		err  error
	}
	results := make(chan result, workers)
	for worker := range workers {
		start := len(rows) * worker / workers
		end := len(rows) * (worker + 1) / workers
		go func() {
			hits, err := exactSemanticRange(ctx, vectors, query, rows[start:end], limit)
			results <- result{hits: hits, err: err}
		}()
	}
	merged := make([]semanticHit, 0, workers*limit)
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

func validateNormalizedSemanticVector(vector semanticVector) error {
	var squaredNorm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("has a non-finite value")
		}
		squaredNorm += float64(value * value)
	}
	if math.Abs(squaredNorm-1) > 1e-3 {
		return fmt.Errorf("has squared norm %.6f, want 1", squaredNorm)
	}
	return nil
}

func exactSemanticRange(
	ctx context.Context,
	vectors []semanticFineVectors,
	query []semanticVector,
	rows []uint64,
	limit int,
) ([]semanticHit, error) {
	hits := make([]semanticHit, 0, min(limit, len(rows)))
	for index, row := range rows {
		if index&4095 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		for centroid, vector := range vectors[row] {
			if err := validateNormalizedSemanticVector(vector); err != nil {
				return nil, fmt.Errorf("semantic row %d centroid %d: %w", row, centroid, err)
			}
		}
		var score float32
		for _, queryVector := range query {
			maximum := float32(math.Inf(-1))
			for _, centroid := range vectors[row] {
				maximum = max(maximum, semanticVectorDot(queryVector, centroid))
			}
			if math.IsInf(float64(maximum), 0) || math.IsNaN(float64(maximum)) {
				return nil, fmt.Errorf("semantic row %d produced an invalid score", row)
			}
			score += maximum
		}
		hit := semanticHit{row: row, score: score}
		position := sort.Search(len(hits), func(i int) bool {
			return semanticHitLess(hit, hits[i])
		})
		if position >= limit {
			continue
		}
		hits = append(hits, semanticHit{})
		copy(hits[position+1:], hits[position:])
		hits[position] = hit
		if len(hits) > limit {
			hits = hits[:limit]
		}
	}
	return hits, nil
}

func sortSemanticHits(hits []semanticHit) {
	sort.SliceStable(hits, func(i, j int) bool { return semanticHitLess(hits[i], hits[j]) })
}

func semanticHitLess(left, right semanticHit) bool {
	if left.score != right.score {
		return left.score > right.score
	}
	return left.row < right.row
}
