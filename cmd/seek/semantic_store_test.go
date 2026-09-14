package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
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

func testSemanticRowsAndVectors(t *testing.T) ([]semanticUnit, []semanticUnitEmbedding) {
	t.Helper()
	content := []byte("one\ntwo\nthree\n")
	units := extractSemanticUnits("raw/\xff.go", content, nil, nil)
	if len(units) != 1 {
		t.Fatalf("units=%d", len(units))
	}
	units[0].row = 0
	units[0].fileLanguage = "Go"
	units[0].id = makeSemanticUnitID(units[0])
	vector := semanticVector{}
	vector[0] = 0.6
	vector[1] = 0.8
	return units, []semanticUnitEmbedding{testSemanticUnitEmbedding(vector)}
}

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

func testSemanticFineVectors(embeddings []semanticUnitEmbedding) []semanticFineSNORM16Vectors {
	vectors := make([]semanticFineSNORM16Vectors, len(embeddings))
	for row := range embeddings {
		for centroid := range embeddings[row].fine {
			stored, err := encodeSemanticSNORM16Vector(&embeddings[row].fine[centroid])
			if err != nil {
				panic(err)
			}
			vectors[row][centroid] = stored
		}
	}
	return vectors
}

func testSemanticStoredFineVectors(vector semanticVector) semanticFineSNORM16Vectors {
	return testSemanticFineVectors([]semanticUnitEmbedding{testSemanticUnitEmbedding(vector)})[0]
}

func testSemanticQuery(vector semanticVector) *semanticQueryEmbedding {
	query := &semanticQueryEmbedding{
		tokens:    make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions),
		scoreMask: make([]bool, lateOnSequenceLength),
	}
	copy(query.tokens[:semanticEmbeddingDimensions], vector[:])
	query.scoreMask[0] = true
	return query
}

func testSemanticUSearchCompatibility(t *testing.T) string {
	t.Helper()
	compatibility, err := semanticUSearchCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	return compatibility
}

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
			sequence:   sequence,
			units:      append([]semanticUnit(nil), units[start:end]...),
			embeddings: embeddings[start:end],
		}
	}
	close(encoded)
	done := make(chan semanticBuildOutput, 1)
	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	collectSemanticBatches(
		encoded,
		filepath.Join(dir, semanticVectorsFile),
		nil,
		cancel,
		done,
	)
	output := <-done
	if output.err != nil {
		return semanticManifest{}, output.err
	}

	shards, err := writeTestUSearchShard(dir, len(units))
	if err != nil {
		return semanticManifest{}, err
	}
	return writeSemanticGenerationFromParts(
		buildCtx,
		dir,
		source,
		output.units,
		output.vectorsArtifact,
		shards,
	)
}

func writeTestUSearchShard(dir string, rows int) ([]semanticUSearchShard, error) {
	if rows == 0 {
		return nil, nil
	}
	name := semanticUSearchShardName(0)
	path := filepath.Join(dir, name)
	content := []byte("test-usearch-v1")
	content = append(content, byte(rows))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return nil, err
	}
	artifact, err := inspectSemanticArtifact(path)
	if err != nil {
		return nil, err
	}
	return []semanticUSearchShard{{
		Name:             name,
		Start:            0,
		Rows:             uint64(rows),
		semanticArtifact: artifact,
	}}, nil
}

func TestSemanticGenerationRoundTrip(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	embeddings[0] = testSemanticSNORM16GoldenEmbedding()
	dir := filepath.Join(t.TempDir(), ".build-semantic")
	manifest, err := writeTestSemanticGeneration(t.Context(), dir, "head-1", units, embeddings)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != 5 || manifest.Rows != 1 || manifest.Dimensions != 48 ||
		manifest.VectorCodec != "snorm16-le-rne-s32767-v1" ||
		manifest.VectorStride != 1_920 {
		t.Fatalf("manifest=%+v", manifest)
	}
	got, err := openSemanticGeneration(dir, "head-1")
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
	wantVectors := testSemanticFineVectors(embeddings)
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
	generation, err := openSemanticGeneration(dir, "head-empty")
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
	b.ResetTimer()
	for b.Loop() {
		decoded, err := readSemanticRowsFrom(bytes.NewReader(encoded), 4_096)
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
	b.ResetTimer()
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
			b.Run("full-validation", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					generation, err := openSemanticGeneration(dir, source)
					if err != nil {
						b.Fatal(err)
					}
					if err := generation.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
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
			language: "Go", fileLanguage: "Go", symbol: fmt.Sprintf("Symbol%d", row),
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
			Name: name, Start: uint64(start), Rows: uint64(rows), semanticArtifact: artifact,
		}
	}
	if _, err := writeSemanticGenerationFromParts(
		b.Context(), dir, source, units, semanticSizedArtifact{Bytes: vectorBytes}, shards,
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
			fileLanguage: "Go",
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

type semanticCountingReader struct {
	reader io.Reader
	bytes  int
}

func (reader *semanticCountingReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	reader.bytes += read
	return read, err
}

func TestReadVerifiedSemanticRowsReadsInputOnce(t *testing.T) {
	rows := []semanticUnit{{
		row: 0, path: "main.go", start: 1, end: 2, kind: semanticUnitSymbol,
		parserResult: semanticParserCTags, language: "Go", fileLanguage: "Go",
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
	reader := &semanticCountingReader{reader: bytes.NewReader(encoded)}
	decoded, err := readVerifiedSemanticRows(reader, artifact.SHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || reader.bytes != len(encoded) {
		t.Fatalf("decoded rows=%d bytes=%d, want 1 and %d", len(decoded), reader.bytes, len(encoded))
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
		{name: "stride", change: func(value *semanticManifest) { value.VectorStride++ }},
		{name: "bytes", change: func(value *semanticManifest) { value.VectorsFile.Bytes++ }},
		{name: "representation", change: func(value *semanticManifest) { value.Representation = "anchored-spherical-f32-v1" }},
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
				t.Skipf("symlink unavailable: %v", err)
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
			if generation, err := openSemanticGeneration(dir, test.source); err == nil {
				_ = generation.Close()
				t.Fatal("damaged generation must fail")
			}
		})
	}
}

func TestOpenSemanticGenerationKeepsExactDataAfterUSearchDamage(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	dir := filepath.Join(t.TempDir(), "semantic")
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head-1", units, embeddings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, semanticUSearchShardName(0)), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := openSemanticGeneration(dir, "head-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = generation.Close() })
	if generation.usearchErr == nil {
		t.Fatal("USearch damage was not recorded")
	}
	hits, err := exactSemanticSearch(t.Context(), generation.vectors, testSemanticQuery(embeddings[0].fine[0]), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].row != 0 {
		t.Fatalf("exact hits=%+v, want row 0", hits)
	}
}

func TestMappedSemanticSNORM16RejectsAccessedDamage(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	tests := []struct {
		name   string
		damage []byte
	}{
		{name: "reserved code", damage: []byte{0x00, 0x80}},
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
			generation, err := openSemanticGeneration(dir, "head-damage")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = generation.Close() })
			if _, err := exactSemanticSearch(
				t.Context(), generation.vectors, testSemanticQuery(embeddings[0].fine[0]), 1,
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

func TestValidateSemanticManifestChecksMinimumRowsLength(t *testing.T) {
	manifest := semanticManifest{
		Format:         semanticFormatVersion,
		Source:         "head",
		Model:          semanticModelCompatibility(),
		Extractor:      semanticExtractorID,
		Representation: semanticRepresentationCompatibility(),
		VectorCodec:    semanticVectorCodec,
		VectorStride:   semanticFineVectorBytesPerUnit,
		USearch:        testSemanticUSearchCompatibility(t),
		Dimensions:     semanticEmbeddingDimensions,
		Rows:           semanticMaxRows,
		RowsFile: semanticArtifact{
			Bytes:  16,
			SHA256: string(make([]byte, sha256.Size*2)),
		},
	}
	manifest.VectorsFile.Bytes, _ = checkedSemanticVectorBytes(manifest.Rows)
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

func TestWriteSemanticRowsRejectsLongFileLanguage(t *testing.T) {
	units := []semanticUnit{{
		row:          0,
		path:         "main.go",
		start:        0,
		end:          1,
		fileLanguage: strings.Repeat("x", math.MaxUint16+1),
	}}
	_, err := writeSemanticRows(filepath.Join(t.TempDir(), semanticRowsFile), units)
	if err == nil {
		t.Fatal("oversize file language must fail")
	}
}

func TestWriteSemanticGenerationHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := writeTestSemanticGeneration(ctx, t.TempDir(), "head", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestExactSemanticSearchStableOrder(t *testing.T) {
	vectors := make([]semanticFineSNORM16Vectors, 4)
	vectors[0] = testSemanticStoredFineVectors(semanticVector{0: 1})
	vectors[1] = testSemanticStoredFineVectors(semanticVector{0: 0.5, 1: float32(math.Sqrt(0.75))})
	vectors[2] = testSemanticStoredFineVectors(semanticVector{0: 1})
	vectors[3] = testSemanticStoredFineVectors(semanticVector{0: -1})
	query := semanticVector{}
	query[0] = 1
	hits, err := exactSemanticSearch(t.Context(), vectors, testSemanticQuery(query), 3)
	if err != nil {
		t.Fatal(err)
	}
	half := float32(16384) * semanticSNORM16InverseScale
	want := []semanticHit{{row: 0, score: 1}, {row: 2, score: 1}, {row: 1, score: half}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits=%+v, want %+v", hits, want)
	}
}

func TestKeepBestSemanticHitKeepsStableTopK(t *testing.T) {
	input := []semanticHit{
		{row: 7, score: 0.5},
		{row: 4, score: 1},
		{row: 9, score: -1},
		{row: 2, score: 1},
		{row: 3, score: 0.75},
		{row: 1, score: 0.75},
	}
	var hits []semanticHit
	for _, hit := range input {
		hits = keepBestSemanticHit(hits, hit, 4)
	}
	sortSemanticHits(hits)
	want := []semanticHit{
		{row: 2, score: 1},
		{row: 4, score: 1},
		{row: 1, score: 0.75},
		{row: 3, score: 0.75},
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits=%+v, want %+v", hits, want)
	}
}

func TestExactSemanticSearchParallelMatchesSerialExpected(t *testing.T) {
	vectors := make([]semanticFineSNORM16Vectors, 600)
	defaultVector := semanticVector{2: 1}
	for row := range vectors {
		vectors[row] = testSemanticStoredFineVectors(defaultVector)
	}
	best := semanticVector{0: 1}
	vectors[17] = testSemanticStoredFineVectors(best)
	vectors[300] = testSemanticStoredFineVectors(best)
	third := semanticVector{0: 0.5, 1: float32(math.Sqrt(0.75))}
	vectors[599] = testSemanticStoredFineVectors(third)
	query := testSemanticQuery(best)
	half := float32(16384) * semanticSNORM16InverseScale
	want := []semanticHit{
		{row: 17, score: 1},
		{row: 300, score: 1},
		{row: 599, score: half},
	}

	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	serial, err := exactSemanticSearch(t.Context(), vectors, query, len(want))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GOMAXPROCS(4)
	parallel, err := exactSemanticSearch(t.Context(), vectors, query, len(want))
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
	_, err := exactSemanticSearch(ctx, make([]semanticFineSNORM16Vectors, 10_000), nil, 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestExactSemanticSearchRejectsInvalidMappedVector(t *testing.T) {
	vectors := []semanticFineSNORM16Vectors{testSemanticStoredFineVectors(semanticVector{0: 1})}
	vectors[0][3][0] = math.MinInt16
	query := semanticVector{}
	query[0] = 1
	if _, err := exactSemanticSearch(t.Context(), vectors, testSemanticQuery(query), 1); err == nil {
		t.Fatal("invalid semantic vector must fail")
	}
}

var semanticExactBenchmarkHits []semanticHit

// BenchmarkExactSemanticRange isolates stored-vector validation and MaxSim
// scoring. It keeps vectors resident in heap memory, so it does not measure
// route planning, worker merge, mmap page faults, or query preparation.
func BenchmarkExactSemanticRange(b *testing.B) {
	const rowCount = 328
	vectors := make([]semanticFineSNORM16Vectors, rowCount)
	for row := range vectors {
		for centroid := range vectors[row] {
			vector := semanticUSearchTestVector(row*semanticFineCentroidsPerUnit + centroid + 1)
			stored, err := encodeSemanticSNORM16Vector(&vector)
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
			scaled, err := prepareSemanticSNORM16Query(query)
			if err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				hits, err := exactSemanticRange(ctx, vectors, scaled, nil, 0, len(vectors), 10)
				if err != nil {
					b.Fatal(err)
				}
				semanticExactBenchmarkHits = hits
			}
		})
	}
}

// BenchmarkExactSemanticRangeCandidateLimits compares a moderate result limit
// with the semantic unit limit used before file collapse.
func BenchmarkExactSemanticRangeCandidateLimits(b *testing.B) {
	const rowCount = 2_048
	vectors := make([]semanticFineSNORM16Vectors, rowCount)
	for row := range vectors {
		for centroid := range vectors[row] {
			vector := semanticUSearchTestVector(row*semanticFineCentroidsPerUnit + centroid + 1)
			stored, err := encodeSemanticSNORM16Vector(&vector)
			if err != nil {
				b.Fatal(err)
			}
			vectors[row][centroid] = stored
		}
	}
	query := []semanticVector{
		semanticUSearchTestVector(11),
		semanticUSearchTestVector(42),
	}
	scaled, err := prepareSemanticSNORM16Query(query)
	if err != nil {
		b.Fatal(err)
	}
	for _, limit := range []int{160, hybridSemanticUnitLimit} {
		b.Run(fmt.Sprintf("limit-%d", limit), func(b *testing.B) {
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				hits, err := exactSemanticRange(
					ctx, vectors, scaled, nil, 0, len(vectors), limit,
				)
				if err != nil {
					b.Fatal(err)
				}
				semanticExactBenchmarkHits = hits
			}
		})
	}
}

func TestSemanticContentIDUsesRawBytes(t *testing.T) {
	content := []byte{'a', 0xff, 'b'}
	unit := extractSemanticUnits(string([]byte{'x', 0xff}), content, nil, nil)[0]
	if unit.contentID != semanticContentID(sha256.Sum256(content)) {
		t.Fatal("content ID changed raw bytes")
	}
}
