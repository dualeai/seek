package main

import (
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
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSemanticModelCompatibilityCoversModelAndTokenizer(t *testing.T) {
	first := semanticModelCompatibilityFor([]byte("model-1"), []byte("tokenizer-1"))
	if first == "" || first != semanticModelCompatibilityFor([]byte("model-1"), []byte("tokenizer-1")) {
		t.Fatal("the same model assets produced an unstable compatibility identity")
	}
	if first == semanticModelCompatibilityFor([]byte("model-2"), []byte("tokenizer-1")) {
		t.Fatal("different model bytes produced one compatibility identity")
	}
	if first == semanticModelCompatibilityFor([]byte("model-1"), []byte("tokenizer-2")) {
		t.Fatal("different tokenizer bytes produced one compatibility identity")
	}
}

// testSemanticRowsAndVectors gives the one unit of a 3-line Go file whose path
// is not valid UTF-8, and its embedding: every centroid is (0.6, 0.8).
func testSemanticRowsAndVectors(t *testing.T) ([]semanticUnit, []semanticUnitEmbedding) {
	t.Helper()
	content := []byte("one\ntwo\nthree\n")
	units := extractSemanticUnits("raw/\xff.go", content, nil, nil)
	if len(units) != 1 {
		t.Fatalf("units=%d", len(units))
	}
	units[0].row = 0
	units[0].id = makeSemanticUnitID(units[0])
	vector := semanticVector{}
	vector[0] = 0.6
	vector[1] = 0.8
	return units, []semanticUnitEmbedding{testSemanticUnitEmbedding(vector)}
}

// testSemanticUnitEmbedding gives an embedding whose every coarse and fine
// centroid is vector.
func testSemanticUnitEmbedding(vector semanticVector) semanticUnitEmbedding {
	var embedding semanticUnitEmbedding
	for centroid := range embedding.coarse {
		embedding.coarse[centroid] = vector
	}
	for centroid := range embedding.fine {
		embedding.fine[centroid] = vector
	}
	return embedding
}

// testSemanticFineVectors encodes the fine centroids of each embedding as the
// vectors file stores them.
func testSemanticFineVectors(embeddings []semanticUnitEmbedding) []semanticFineF16Vectors {
	vectors := make([]semanticFineF16Vectors, len(embeddings))
	for row := range embeddings {
		for centroid := range embeddings[row].fine {
			stored, err := encodeSemanticF16Vector(&embeddings[row].fine[centroid])
			if err != nil {
				panic(err)
			}
			vectors[row][centroid] = stored
		}
	}
	return vectors
}

func testSemanticStoredFineVectors(vector semanticVector) semanticFineF16Vectors {
	return testSemanticFineVectors([]semanticUnitEmbedding{testSemanticUnitEmbedding(vector)})[0]
}

// testSemanticQuery gives a query with one scoring token, vector.
func testSemanticQuery(vector semanticVector) *semanticQueryEmbedding {
	query := &semanticQueryEmbedding{
		tokens:    make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions),
		scoreMask: make([]bool, lateOnSequenceLength),
	}
	copy(query.tokens[:semanticEmbeddingDimensions], vector[:])
	query.scoreMask[0] = true
	return query
}

// noSemanticFloor keeps every scored row.
var noSemanticFloor = float32(math.Inf(-1))

// firstSemanticHits gives at most count of the best hits.
func firstSemanticHits(hits []semanticHit, count int) []semanticHit {
	return hits[:min(count, len(hits))]
}

func testSemanticUSearchCompatibility(t *testing.T) string {
	t.Helper()
	compatibility, err := semanticUSearchCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	return compatibility
}

// writeTestSemanticGeneration writes a generation of units to dir with no
// model. It sends embeddings to the row collector of the build
// (collectSemanticBatches), writes placeholder USearch shards, and writes the
// manifest. It returns the manifest that it reads back from dir.
func writeTestSemanticGeneration(
	ctx context.Context,
	dir string,
	source string,
	units []semanticUnit,
	embeddings []semanticUnitEmbedding,
) (semanticManifest, error) {
	if err := ctx.Err(); err != nil {
		return semanticManifest{}, err
	}
	if len(units) != len(embeddings) {
		return semanticManifest{}, fmt.Errorf(
			"semantic row count %d does not match embedding count %d",
			len(units),
			len(embeddings),
		)
	}
	if err := prepareSemanticGenerationDirectory(dir); err != nil {
		return semanticManifest{}, err
	}
	batchCount := (len(units) + semanticModelBatchRows - 1) / semanticModelBatchRows
	encoded := make(chan semanticEncodedBatch, batchCount)
	for sequence := range batchCount {
		start := sequence * semanticModelBatchRows
		end := min(start+semanticModelBatchRows, len(units))
		encoded <- semanticEncodedBatch{
			units:      append([]semanticUnit(nil), units[start:end]...),
			embeddings: embeddings[start:end],
		}
	}
	close(encoded)
	copied := make(chan []semanticCopiedRow)
	close(copied)
	done := make(chan semanticBuildOutput, 1)
	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	collectSemanticBatches(encoded, copied, dir, nil, cancel, done)
	output := <-done
	if output.err != nil {
		return semanticManifest{}, output.err
	}

	// The build makes no USearch graph for an overlay (semanticSourceHasGraph).
	var shards []semanticUSearchShard
	var shardRows uint64
	if semanticSourceHasGraph(source) && len(units) > 0 {
		var err error
		if shards, err = writeTestUSearchShard(dir, len(units)); err != nil {
			return semanticManifest{}, err
		}
		shardRows = semanticUSearchMaxShardRows
	}
	if err := writeSemanticGenerationFromParts(
		buildCtx,
		dir,
		source,
		"",
		units,
		output.vectorsArtifact,
		output.coarseArtifact,
		shardRows,
		shards,
	); err != nil {
		return semanticManifest{}, err
	}
	return readSemanticManifestUnchecked(dir)
}

// writeTestUSearchShard writes placeholder graph files of at most
// semanticUSearchMaxShardRows rows each. Each ID is the SHA-256 of the file
// name, start row, and row count.
func writeTestUSearchShard(dir string, rows int) ([]semanticUSearchShard, error) {
	var shards []semanticUSearchShard
	for start := 0; start < rows; start += semanticUSearchMaxShardRows {
		count := min(semanticUSearchMaxShardRows, rows-start)
		name := semanticUSearchShardName(len(shards))
		path := filepath.Join(dir, name)
		content := []byte("test-usearch-v1")
		content = append(content, byte(count))
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return nil, err
		}
		artifact, err := inspectSemanticArtifact(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		id := sha256.Sum256(fmt.Appendf(nil, "%s %d %d", name, start, count))
		shards = append(shards, semanticUSearchShard{
			Check:            semanticRowsCheckOf(info),
			Name:             name,
			Start:            uint64(start),
			Rows:             uint64(count),
			ID:               hex.EncodeToString(id[:]),
			semanticArtifact: artifact,
		})
	}
	return shards, nil
}

func TestSemanticGenerationRoundTrip(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	embeddings[0] = testSemanticGoldenEmbedding()
	dir := filepath.Join(t.TempDir(), ".build-semantic")
	manifest, err := writeTestSemanticGeneration(t.Context(), dir, "head-1", units, embeddings)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != 6 || manifest.Rows != 1 || manifest.Dimensions != 48 ||
		manifest.VectorCodec != "f16-le-rne-v1" ||
		manifest.VectorStride != 1_920 {
		t.Fatalf("manifest=%+v", manifest)
	}
	got, err := openSemanticGenerationForSearch(dir, "head-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = got.Close() })
	wantRows := append([]semanticUnit(nil), units...)
	for index := range wantRows {
		wantRows[index].text = nil
	}
	if !reflect.DeepEqual(got.rows, wantRows) {
		t.Fatalf("rows changed:\nwant=%+v\ngot=%+v", wantRows, got.rows)
	}
	// The golden binary16 codes of -0.5, 0.5, sqrt(0.5), 1, and -1
	// (TestSemanticF16LittleEndianGolden).
	wantVectors := make([]semanticFineF16Vectors, 1)
	wantVectors[0][0][0], wantVectors[0][0][1], wantVectors[0][0][2] = 0xb800, 0x3800, 0x39a8
	for centroid := 1; centroid < 19; centroid++ {
		wantVectors[0][centroid][centroid] = semanticF16One
	}
	wantVectors[0][19][47] = 0xbc00
	if !reflect.DeepEqual(got.vectors, wantVectors) {
		t.Fatalf("vectors changed: want=%v got=%v", wantVectors, got.vectors)
	}
	wantUSearch := []string{filepath.Join(dir, semanticUSearchShardName(0))}
	if !reflect.DeepEqual(got.usearch, wantUSearch) {
		t.Fatalf("USearch paths=%q", got.usearch)
	}
	for _, name := range []string{semanticManifestFile, semanticRowsFile, semanticVectorsFile, semanticUSearchShardName(0)} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode=%o, want private", name, info.Mode().Perm())
		}
	}
}

func TestSemanticGenerationZeroRowsRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "semantic")
	manifest, err := writeTestSemanticGeneration(t.Context(), dir, "head-empty", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Rows != 0 || manifest.VectorsFile.Bytes != 0 {
		t.Fatalf("manifest=%+v", manifest)
	}
	generation, err := openSemanticGenerationForSearch(dir, "head-empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.rows) != 0 || len(generation.vectors) != 0 || len(generation.vectorMap) != 0 {
		t.Fatalf("empty generation has rows=%d vectors=%d mapped=%d", len(generation.rows), len(generation.vectors), len(generation.vectorMap))
	}
	if err := generation.Close(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkReadSemanticRows(b *testing.B) {
	encoded, _ := semanticRowsBenchmarkFixture(b)
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	for b.Loop() {
		decoded, err := readSemanticRowsFrom(bytes.NewReader(encoded), 4_096, true)
		if err != nil {
			b.Fatal(err)
		}
		runtime.KeepAlive(decoded)
	}
}

func BenchmarkReadVerifiedSemanticRows(b *testing.B) {
	encoded, artifact := semanticRowsBenchmarkFixture(b)
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	for b.Loop() {
		decoded, err := readVerifiedSemanticRows(bytes.NewReader(encoded), artifact.SHA256, 4_096)
		if err != nil {
			b.Fatal(err)
		}
		runtime.KeepAlive(decoded)
	}
}

func BenchmarkOpenSemanticGeneration(b *testing.B) {
	for _, rowCount := range []int{4_096, 204_694} {
		b.Run(fmt.Sprintf("rows-%d", rowCount), func(b *testing.B) {
			dir, source, shardCount := writeSemanticOpenBenchmarkGeneration(b, rowCount)
			b.ReportMetric(float64(shardCount), "shards")
			b.Run("search-lazy-graphs", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					generation, err := openSemanticGenerationForSearch(dir, source)
					if err != nil {
						b.Fatal(err)
					}
					if err := generation.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func writeSemanticOpenBenchmarkGeneration(
	b *testing.B,
	rowCount int,
) (string, string, int) {
	b.Helper()
	dir := b.TempDir()
	source := fmt.Sprintf("open-benchmark-%d", rowCount)
	units := make([]semanticUnit, rowCount)
	for row := range units {
		units[row] = semanticUnit{
			row: uint64(row), path: fmt.Sprintf("pkg/%06d/source.go", row/4),
			start: uint64(row % 4), end: uint64(row%4 + 1),
			kind: semanticUnitSymbol, parserResult: semanticParserCTags,
			language: "Go", symbol: fmt.Sprintf("Symbol%d", row),
		}
		units[row].id = makeSemanticUnitID(units[row])
	}
	vectorBytes, ok := checkedSemanticVectorBytes(uint64(rowCount))
	if !ok {
		b.Fatal("vector byte count overflow")
	}
	vectorsPath := filepath.Join(dir, semanticVectorsFile)
	vectorsFile, err := os.OpenFile(vectorsPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	if err := vectorsFile.Truncate(int64(vectorBytes)); err != nil {
		_ = vectorsFile.Close()
		b.Fatal(err)
	}
	if err := vectorsFile.Close(); err != nil {
		b.Fatal(err)
	}
	coarseBytes, _ := checkedSemanticCoarseBytes(uint64(rowCount))
	if err := os.WriteFile(filepath.Join(dir, semanticCoarseFile), make([]byte, coarseBytes), 0o600); err != nil {
		b.Fatal(err)
	}
	const shardRows = 4_096
	shardCount := (rowCount + shardRows - 1) / shardRows
	shards := make([]semanticUSearchShard, shardCount)
	graphBytes := bytes.Repeat([]byte{0x5a}, 256<<10)
	for shardIndex := range shards {
		start := shardIndex * shardRows
		rows := min(shardRows, rowCount-start)
		name := semanticUSearchShardName(shardIndex)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, graphBytes, 0o600); err != nil {
			b.Fatal(err)
		}
		artifact, err := inspectSemanticArtifact(path)
		if err != nil {
			b.Fatal(err)
		}
		shards[shardIndex] = semanticUSearchShard{
			Name: name, Start: uint64(start), Rows: uint64(rows), ID: artifact.SHA256, semanticArtifact: artifact,
		}
	}
	if err := writeSemanticGenerationFromParts(
		b.Context(), dir, source, "", units, semanticSizedArtifact{Bytes: vectorBytes},
		semanticSizedArtifact{Bytes: coarseBytes}, shardRows, shards,
	); err != nil {
		b.Fatal(err)
	}
	return dir, source, shardCount
}

func semanticRowsBenchmarkFixture(b *testing.B) ([]byte, semanticArtifact) {
	b.Helper()
	const rowCount = 4_096
	rows := make([]semanticUnit, rowCount)
	for row := range rows {
		rows[row] = semanticUnit{
			row:          uint64(row),
			path:         fmt.Sprintf("pkg/%04d/source.go", row/4),
			start:        uint64(row % 4),
			end:          uint64(row%4 + 1),
			kind:         semanticUnitSymbol,
			parserResult: semanticParserCTags,
			language:     "Go",
			symbol:       fmt.Sprintf("function Symbol%d", row),
		}
		rows[row].id = makeSemanticUnitID(rows[row])
	}
	path := filepath.Join(b.TempDir(), semanticRowsFile)
	artifact, err := writeSemanticRows(path, rows)
	if err != nil {
		b.Fatal(err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	return encoded, artifact
}

// countingReader counts the bytes that its reader gave.
type countingReader struct {
	reader io.Reader
	bytes  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

// TestReadVerifiedSemanticRowsReadsInputOnce checks that the full check of
// rows.bin reads its input once, to the end.
func TestReadVerifiedSemanticRowsReadsInputOnce(t *testing.T) {
	rows := []semanticUnit{{
		row: 0, path: "main.go", start: 1, end: 2, kind: semanticUnitSymbol,
		parserResult: semanticParserCTags, language: "Go",
	}}
	rows[0].id = makeSemanticUnitID(rows[0])
	path := filepath.Join(t.TempDir(), semanticRowsFile)
	artifact, err := writeSemanticRows(path, rows)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader := &countingReader{reader: bytes.NewReader(encoded)}
	decoded, err := readVerifiedSemanticRows(reader, artifact.SHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || reader.bytes != int64(len(encoded)) {
		t.Fatalf("decoded rows=%d bytes=%d, want 1 and %d", len(decoded), reader.bytes, len(encoded))
	}
}

// TestReadSemanticRowsRejectsWrongRowIndex gives the second row of a valid
// rows.bin the index 7. The read that trusts the file (no full check) must
// still fail: the filter mask uses the position of a row as its index
// (buildSemanticFilterMask).
func TestReadSemanticRowsRejectsWrongRowIndex(t *testing.T) {
	rows := []semanticUnit{
		{row: 0, path: "main.go", start: 0, end: 1, kind: semanticUnitSymbol, parserResult: semanticParserCTags},
		{row: 1, path: "main.go", start: 1, end: 2, kind: semanticUnitSymbol, parserResult: semanticParserCTags},
	}
	path := filepath.Join(t.TempDir(), semanticRowsFile)
	if _, err := writeSemanticRows(path, rows); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readSemanticRowsFrom(bytes.NewReader(encoded), 2, false); err != nil {
		t.Fatalf("valid rows: %v", err)
	}
	second := semanticRowsHeaderBytes + semanticRowFixedBytes + len(rows[0].path)
	binary.LittleEndian.PutUint64(encoded[second:], 7)
	if _, err := readSemanticRowsFrom(bytes.NewReader(encoded), 2, false); err == nil {
		t.Fatal("rows.bin with index 7 at position 1 was accepted")
	}
}

func TestSemanticGenerationKeyIncludesSource(t *testing.T) {
	first := semanticGenerationKey("head-1")
	if first != semanticGenerationKey("head-1") {
		t.Fatal("same identity changed the generation key")
	}
	if first == semanticGenerationKey("head-2") {
		t.Fatal("different source must change the generation key")
	}
	if len(first) != 32 {
		t.Fatalf("key length=%d, want 32", len(first))
	}
}

func TestSemanticFormatFourManifestIsIncompatible(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	manifest, err := writeTestSemanticGeneration(
		t.Context(), t.TempDir(), "head-format", units, embeddings,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Format = 4
	if err := validateSemanticManifest(manifest, "head-format"); err == nil {
		t.Fatal("format 4 semantic manifest stayed compatible")
	}
}

func TestSemanticManifestRejectsWrongVectorContract(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	manifest, err := writeTestSemanticGeneration(
		t.Context(), t.TempDir(), "head-contract", units, embeddings,
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*semanticManifest)
	}{
		{name: "codec", change: func(value *semanticManifest) { value.VectorCodec = "f32-le-v1" }},
		// A generation with 16-bit fixed-point fine vectors has the same stride
		// and length; only its codec tag tells it apart.
		{name: "SNORM16 codec", change: func(value *semanticManifest) { value.VectorCodec = "snorm16-le-rne-s32767-v1" }},
		{name: "stride", change: func(value *semanticManifest) { value.VectorStride++ }},
		{name: "bytes", change: func(value *semanticManifest) { value.VectorsFile.Bytes++ }},
		{name: "representation", change: func(value *semanticManifest) { value.Representation = "anchored-spherical-f32-v1" }},
		// A run-time upgrade can change the numbers the model produces, so a
		// new run time must not reuse the vectors of an older one.
		{name: "runtime", change: func(value *semanticManifest) { value.Runtime = "0.0.0-other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := manifest
			test.change(&changed)
			if err := validateSemanticManifest(changed, "head-contract"); err == nil {
				t.Fatal("wrong vector contract stayed compatible")
			}
		})
	}
}

func TestOpenSemanticGenerationRejectsDamage(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	tests := []struct {
		name   string
		damage func(*testing.T, string)
		source string
	}{
		{name: "wrong source", source: "head-2"},
		{name: "truncated vectors", source: "head-1", damage: func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, semanticVectorsFile)
			if err := os.Truncate(path, 4); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra vector byte", source: "head-1", damage: func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, semanticVectorsFile)
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.Write([]byte{0})
			if err := errors.Join(writeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "changed rows", source: "head-1", damage: func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, semanticRowsFile)
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt([]byte{'X'}, 0); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		// The last byte of rows.bin is the last language byte of the last row
		// ("Go"), which has no symbol. The changed file keeps a valid row
		// layout, so only the full check (hash and unit IDs) finds the change.
		{name: "changed language byte", source: "head-1", damage: func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, semanticRowsFile)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteAt([]byte{'X'}, info.Size()-1)
			if err := errors.Join(writeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			// A coarse file clock can keep the recorded times after the write;
			// a new time makes the open check the hash.
			later := time.Now().Add(time.Minute)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid row count", source: "head-1", damage: func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, semanticManifestFile)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var manifest semanticManifest
			if err := json.Unmarshal(content, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.Rows = math.MaxUint64
			content, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink artifact", source: "head-1", damage: func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, semanticVectorsFile)
			backup := filepath.Join(dir, "vectors-copy")
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(backup, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "semantic")
			if _, err := writeTestSemanticGeneration(t.Context(), dir, "head-1", units, embeddings); err != nil {
				t.Fatal(err)
			}
			if test.damage != nil {
				test.damage(t, dir)
			}
			if generation, err := openSemanticGenerationForSearch(dir, test.source); err == nil {
				_ = generation.Close()
				t.Fatal("damaged generation must fail")
			}
		})
	}
}

// TestOpenSemanticGenerationKeepsExactDataAfterUSearchDamage damages the
// USearch shard: the search open succeeds, the shard check that a native
// search runs fails, and the exact vectors still answer.
func TestOpenSemanticGenerationKeepsExactDataAfterUSearchDamage(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	dir := filepath.Join(t.TempDir(), "semantic")
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head-1", units, embeddings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, semanticUSearchShardName(0)), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := openSemanticGenerationForSearch(dir, "head-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = generation.Close() })
	if err := generation.validateUSearchShard(0); err == nil {
		t.Fatal("the USearch shard check accepted damage")
	}
	hits, err := exactSemanticSearch(t.Context(), generation.vectors, testSemanticQuery(embeddings[0].fine[0]), noSemanticFloor)
	if err != nil {
		t.Fatal(err)
	}
	if hits = firstSemanticHits(hits, 1); len(hits) != 1 || hits[0].row != 0 {
		t.Fatalf("exact hits=%+v, want row 0", hits)
	}
}

func TestMappedSemanticF16RejectsAccessedDamage(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	tests := []struct {
		name   string
		damage []byte
	}{
		{name: "NaN code", damage: []byte{0x00, 0x7e}},
		{name: "infinite code", damage: []byte{0x00, 0x7c}},
		{name: "norm above tolerance", damage: []byte{0x00, 0x3c}},
		{name: "zero norm", damage: make([]byte, semanticEmbeddingDimensions*2)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "semantic")
			if _, err := writeTestSemanticGeneration(t.Context(), dir, "head-damage", units, embeddings); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(filepath.Join(dir, semanticVectorsFile), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteAt(test.damage, 0)
			if err := errors.Join(writeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			generation, err := openSemanticGenerationForSearch(dir, "head-damage")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = generation.Close() })
			if _, err := exactSemanticSearch(
				t.Context(), generation.vectors, testSemanticQuery(embeddings[0].fine[0]), noSemanticFloor,
			); err == nil {
				t.Fatal("damaged accessed vector was accepted")
			}
		})
	}
}

func TestDecodeSemanticManifestRejectsUnknownAndDuplicateFields(t *testing.T) {
	for _, content := range []string{
		`{"format":1,"format":1}`,
		`{"unknown":1}`,
		`{} {}`,
	} {
		if _, err := decodeSemanticManifest([]byte(content)); err == nil {
			t.Fatalf("manifest %q must fail", content)
		}
	}
}

// testLargeSemanticManifest returns a valid manifest of rows rows, with one
// USearch shard entry per 4,096 rows.
func testLargeSemanticManifest(t *testing.T, rows uint64) semanticManifest {
	t.Helper()
	manifest := semanticManifest{
		Format:         semanticFormatVersion,
		Source:         "head",
		Model:          semanticModelCompatibility(),
		Extractor:      semanticExtractorID,
		Representation: semanticRepresentationCompatibility(),
		VectorCodec:    semanticVectorCodec,
		VectorStride:   semanticFineVectorBytesPerUnit,
		USearch:        testSemanticUSearchCompatibility(t),
		Runtime:        semanticInferenceRuntime(),
		Dimensions:     semanticEmbeddingDimensions,
		Rows:           rows,
	}
	manifest.RowsFile.Bytes, _ = checkedSemanticRowsMinimumBytes(rows)
	manifest.RowsFile.SHA256 = strings.Repeat("0", sha256.Size*2)
	manifest.VectorsFile.Bytes, _ = checkedSemanticVectorBytes(rows)
	manifest.CoarseFile.Bytes, _ = checkedSemanticCoarseBytes(rows)
	manifest.USearchShardRows = semanticUSearchMaxShardRows
	for start := uint64(0); start < rows; start += semanticUSearchMaxShardRows {
		manifest.USearchFiles = append(manifest.USearchFiles, semanticUSearchShard{
			Name: semanticUSearchShardName(len(manifest.USearchFiles)), Start: start,
			Rows:   min(semanticUSearchMaxShardRows, rows-start),
			ID:     strings.Repeat("0", sha256.Size*2),
			Bytes:  1,
			SHA256: strings.Repeat("0", sha256.Size*2),
		})
	}
	return manifest
}

// TestSemanticGenerationOfTenMillionRowsBinds writes the manifest of a
// generation of 10,000,001 rows: 2,442 shard entries and more than 64 KiB of
// JSON. No row count or manifest size limits a generation, so the manifest
// reads back with every shard and validates.
func TestSemanticGenerationOfTenMillionRowsBinds(t *testing.T) {
	manifest := testLargeSemanticManifest(t, 10_000_001)
	dir := t.TempDir()
	if err := writeSemanticManifest(filepath.Join(dir, semanticManifestFile), manifest); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, semanticManifestFile)); err != nil || info.Size() <= 64<<10 {
		t.Fatalf("manifest size: %v", err)
	}
	got, err := readSemanticManifest(dir, "head")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.USearchFiles) != 2_442 {
		t.Fatalf("shards=%d, want 2442", len(got.USearchFiles))
	}
	if err := validateSemanticUSearchShardMetadata(got.Rows, got.USearchShardRows, got.USearchFiles, true); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSemanticManifestChecksMinimumRowsLength(t *testing.T) {
	manifest := testLargeSemanticManifest(t, 1_765_377)
	manifest.RowsFile.Bytes = 16
	if err := validateSemanticManifest(manifest, "head"); err == nil {
		t.Fatal("short row file must fail before row allocation")
	}
}

func TestWriteSemanticGenerationRejectsInvalidRows(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	units[0].row = 1
	_, err := writeTestSemanticGeneration(t.Context(), t.TempDir(), "head", units, embeddings)
	if err == nil {
		t.Fatal("out-of-order row must fail")
	}
}

// TestWriteSemanticGenerationHonorsCancellation gives the generation writer a
// canceled context and valid empty parts: it returns the cancellation and
// writes no manifest.
func TestWriteSemanticGenerationHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	err := writeSemanticGenerationFromParts(
		ctx, dir, "head", "", nil, semanticSizedArtifact{}, semanticSizedArtifact{}, 0, nil,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, semanticManifestFile)); !os.IsNotExist(statErr) {
		t.Fatalf("a canceled write left a manifest: %v", statErr)
	}
}

// TestExactSemanticSearchStableOrder keeps the rows that reach the floor, a
// row exactly at the floor included, best first; a lower row breaks a tie.
func TestExactSemanticSearchStableOrder(t *testing.T) {
	vectors := make([]semanticFineF16Vectors, 4)
	vectors[0] = testSemanticStoredFineVectors(semanticVector{0: 1})
	vectors[1] = testSemanticStoredFineVectors(semanticVector{0: 0.5, 1: float32(math.Sqrt(0.75))})
	vectors[2] = testSemanticStoredFineVectors(semanticVector{0: 1})
	vectors[3] = testSemanticStoredFineVectors(semanticVector{0: -1})
	query := semanticVector{}
	query[0] = 1
	// binary16 stores 0.5 exactly, so row 1 scores 0.5.
	half := float32(0.5)
	hits, err := exactSemanticSearch(t.Context(), vectors, testSemanticQuery(query), half)
	if err != nil {
		t.Fatal(err)
	}
	want := []semanticHit{{row: 0, score: 1}, {row: 2, score: 1}, {row: 1, score: half}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits=%+v, want %+v", hits, want)
	}
}

// TestExactSemanticSearchParallelMatchesSerialExpected scores 2,048 rows with
// one query token. A worker takes 512 row-token pairs, so GOMAXPROCS 4 gives
// four workers of 512 rows. Row 600 of the second worker scores below row
// 1,024 of the third: the merge must sort the hits of all workers.
func TestExactSemanticSearchParallelMatchesSerialExpected(t *testing.T) {
	vectors := make([]semanticFineF16Vectors, 2_048)
	defaultVector := semanticVector{2: 1}
	for row := range vectors {
		vectors[row] = testSemanticStoredFineVectors(defaultVector)
	}
	best := semanticVector{0: 1}
	vectors[17] = testSemanticStoredFineVectors(best)
	vectors[1_024] = testSemanticStoredFineVectors(best)
	third := semanticVector{0: 0.5, 1: float32(math.Sqrt(0.75))}
	vectors[600] = testSemanticStoredFineVectors(third)
	vectors[2_047] = testSemanticStoredFineVectors(third)
	query := testSemanticQuery(best)
	// binary16 stores 0.5 exactly, so rows 600 and 2,047 score 0.5.
	half := float32(0.5)
	want := []semanticHit{
		{row: 17, score: 1},
		{row: 1_024, score: 1},
		{row: 600, score: half},
		{row: 2_047, score: half},
	}

	// The floor drops the 2,044 rows that score 0.
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	serial, err := exactSemanticSearch(t.Context(), vectors, query, half)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GOMAXPROCS(4)
	parallel, err := exactSemanticSearch(t.Context(), vectors, query, half)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serial, want) {
		t.Fatalf("serial hits=%+v, want %+v", serial, want)
	}
	if !reflect.DeepEqual(parallel, want) {
		t.Fatalf("parallel hits=%+v, want %+v", parallel, want)
	}
}

func TestExactSemanticSearchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := exactSemanticSearch(ctx, make([]semanticFineF16Vectors, 10_000), nil, noSemanticFloor)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestExactSemanticSearchRejectsInvalidMappedVector(t *testing.T) {
	vectors := []semanticFineF16Vectors{testSemanticStoredFineVectors(semanticVector{0: 1})}
	vectors[0][3][0] = 0x7e00
	query := semanticVector{}
	query[0] = 1
	if _, err := exactSemanticSearch(t.Context(), vectors, testSemanticQuery(query), noSemanticFloor); err == nil {
		t.Fatal("invalid semantic vector must fail")
	}
}

var semanticExactBenchmarkHits []semanticHit

// BenchmarkExactSemanticRange isolates stored-vector validation and MaxSim
// scoring at the retrieval radius. It keeps vectors resident in heap memory,
// so it does not measure route planning, worker merge, mmap page faults, or
// query preparation. Each query size takes the floor per scoring token of a
// 3-word description, such as "request authentication flow", times its token
// count. With the real model, that description has 6 scoring tokens and an
// empty-document score of 0.35 per token (semanticRetrievalFloor).
func BenchmarkExactSemanticRange(b *testing.B) {
	const rowCount = 328
	description := &semanticQueryEmbedding{
		tokens:     make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions),
		scoreMask:  make([]bool, lateOnSequenceLength),
		emptyScore: 0.35 * 6,
	}
	for token := range 6 {
		description.scoreMask[token] = true
	}
	floorPerToken := semanticRetrievalFloor(description.emptyScore, 6) / 6
	vectors := make([]semanticFineF16Vectors, rowCount)
	for row := range vectors {
		for centroid := range vectors[row] {
			vector := semanticUSearchTestVector(row*semanticFineCentroidsPerUnit + centroid + 1)
			stored, err := encodeSemanticF16Vector(&vector)
			if err != nil {
				b.Fatal(err)
			}
			vectors[row][centroid] = stored
		}
	}
	for _, tokenCount := range []int{2, 32, 128} {
		b.Run(fmt.Sprintf("tokens-%d", tokenCount), func(b *testing.B) {
			query := make([]semanticVector, tokenCount)
			for token := range query {
				query[token] = semanticUSearchTestVector(token*31 + 11)
			}
			packed, err := prepareSemanticFineQuery(query)
			if err != nil {
				b.Fatal(err)
			}
			floor := floorPerToken * float32(tokenCount)
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				hits, err := exactSemanticRange(ctx, vectors, &packed, nil, 0, len(vectors), floor)
				if err != nil {
					b.Fatal(err)
				}
				semanticExactBenchmarkHits = hits
			}
		})
	}
}

// TestSemanticContentIDUsesRawBytes checks the content ID rule of the search
// (checkedSemanticCandidate). A row matches the shard bytes of its file when
// its content ID is the SHA-256 of the raw bytes. The SHA-256 of the bytes
// with U+FFFD in place of 0xff does not match. Both digests come from
// "shasum -a 256".
func TestSemanticContentIDUsesRawBytes(t *testing.T) {
	content := []byte{'a', 0xff, 'b'}
	for _, test := range []struct {
		name    string
		digest  string
		wantErr bool
	}{
		{name: "raw bytes", digest: "01ce0241d2a0e71a4fecd5a8d71157fe2787197732fc15d889cbcf36c38e3c68"},
		{name: "U+FFFD", digest: "05087813392efc16fe8ff448920c6328e53af865df39419436659d9ffda90f7b", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest, err := hex.DecodeString(test.digest)
			if err != nil {
				t.Fatal(err)
			}
			var contentID semanticContentID
			copy(contentID[:], digest)
			unit := extractSemanticUnitsWithID(string([]byte{'x', 0xff}), content, contentID, nil, nil)[0]
			if _, _, err := checkedSemanticCandidate(unit, content, "Go"); (err != nil) != test.wantErr {
				t.Fatalf("error=%v, want error=%t", err, test.wantErr)
			}
		})
	}
}

// TestUSearchShardTrustedOnlyAsChecked opens a generation three times. The
// open trusts an untouched shard with no hash. It hashes a touched shard,
// which passes. A touched shard with other bytes of the same length fails.
func TestUSearchShardTrustedOnlyAsChecked(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	dir := filepath.Join(t.TempDir(), "semantic")
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head", units, embeddings); err != nil {
		t.Fatal(err)
	}
	shard := filepath.Join(dir, semanticUSearchShardName(0))
	validate := func() (bool, error) {
		t.Helper()
		generation, err := openSemanticGenerationForSearch(dir, "head")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = generation.Close() }()
		before := semanticShardsHashed.Load()
		err = generation.validateUSearchShard(0)
		return semanticShardsHashed.Load() > before, err
	}
	if hashed, err := validate(); hashed || err != nil {
		t.Fatalf("untouched shard: hashed=%t err=%v, want trusted", hashed, err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(shard, later, later); err != nil {
		t.Fatal(err)
	}
	if hashed, err := validate(); !hashed || err != nil {
		t.Fatalf("touched shard: hashed=%t err=%v, want a passing full check", hashed, err)
	}
	content, err := os.ReadFile(shard)
	if err != nil {
		t.Fatal(err)
	}
	content[0] ^= 0xff
	if err := os.WriteFile(shard, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validate(); err == nil {
		t.Fatal("a changed shard of the same length passed")
	}
}

// TestFullCheckRejectsRowWithWrongID writes a rows.bin whose hash matches the
// manifest but whose row holds a wrong unit ID. The manifest records no rows
// check, so the search open runs the full check, and that check fails.
func TestFullCheckRejectsRowWithWrongID(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	dir := filepath.Join(t.TempDir(), "semantic")
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head", units, embeddings); err != nil {
		t.Fatal(err)
	}
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil {
		t.Fatal(err)
	}
	broken := slices.Clone(units)
	broken[0].id[0] ^= 0xff
	path := filepath.Join(dir, semanticRowsFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	artifact, err := writeSemanticRows(path, broken)
	if err != nil {
		t.Fatal(err)
	}
	manifest.RowsFile = artifact
	manifest.RowsCheck = semanticRowsCheck{}
	if err := os.Remove(filepath.Join(dir, semanticManifestFile)); err != nil {
		t.Fatal(err)
	}
	if err := writeSemanticManifest(filepath.Join(dir, semanticManifestFile), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := openSemanticGenerationForSearch(dir, "head"); err == nil {
		t.Fatal("a row with a wrong unit ID passed the full check")
	}
}
