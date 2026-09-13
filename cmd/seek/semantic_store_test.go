package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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

func testSemanticFineVectors(embeddings []semanticUnitEmbedding) []semanticFineVectors {
	vectors := make([]semanticFineVectors, len(embeddings))
	for row := range embeddings {
		vectors[row] = embeddings[row].fine
	}
	return vectors
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
	collectSemanticBatches(
		encoded,
		filepath.Join(dir, semanticVectorsFile),
		nil,
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
		ctx,
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
	dir := filepath.Join(t.TempDir(), ".build-semantic")
	manifest, err := writeTestSemanticGeneration(t.Context(), dir, "head-1", units, embeddings)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Rows != 1 || manifest.Dimensions != semanticEmbeddingDimensions {
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

func TestSemanticFormatThreeManifestIsIncompatible(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	manifest, err := writeTestSemanticGeneration(
		t.Context(), t.TempDir(), "head-format", units, embeddings,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Format = 3
	if err := validateSemanticManifest(manifest, "head-format"); err == nil {
		t.Fatal("format 3 semantic manifest stayed compatible")
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
	vectors := make([]semanticFineVectors, 4)
	for centroid := range semanticFineCentroidsPerUnit {
		vectors[0][centroid][0] = 1
		vectors[1][centroid][0] = 0.5
		vectors[1][centroid][1] = float32(math.Sqrt(0.75))
		vectors[2][centroid][0] = 1
		vectors[3][centroid][0] = -1
	}
	query := semanticVector{}
	query[0] = 1
	hits, err := exactSemanticSearch(t.Context(), vectors, testSemanticQuery(query), 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []semanticHit{{row: 0, score: 1}, {row: 2, score: 1}, {row: 1, score: 0.5}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits=%+v, want %+v", hits, want)
	}
}

func TestExactSemanticSearchParallelMatchesSerialExpected(t *testing.T) {
	vectors := make([]semanticFineVectors, 600)
	defaultVector := semanticVector{2: 1}
	for row := range vectors {
		vectors[row] = testSemanticUnitEmbedding(defaultVector).fine
	}
	best := semanticVector{0: 1}
	vectors[17] = testSemanticUnitEmbedding(best).fine
	vectors[300] = testSemanticUnitEmbedding(best).fine
	third := semanticVector{0: 0.5, 1: float32(math.Sqrt(0.75))}
	vectors[599] = testSemanticUnitEmbedding(third).fine
	query := testSemanticQuery(best)
	want := []semanticHit{
		{row: 17, score: 1},
		{row: 300, score: 1},
		{row: 599, score: 0.5},
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
	_, err := exactSemanticSearch(ctx, make([]semanticFineVectors, 10_000), nil, 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestExactSemanticSearchRejectsInvalidMappedVector(t *testing.T) {
	vectors := make([]semanticFineVectors, 1)
	for centroid := range vectors[0] {
		vectors[0][centroid][0] = 1
	}
	vectors[0][3][0] = float32(math.NaN())
	query := semanticVector{}
	query[0] = 1
	if _, err := exactSemanticSearch(t.Context(), vectors, testSemanticQuery(query), 1); err == nil {
		t.Fatal("invalid semantic vector must fail")
	}
}

func TestSemanticContentIDUsesRawBytes(t *testing.T) {
	content := []byte{'a', 0xff, 'b'}
	unit := extractSemanticUnits(string([]byte{'x', 0xff}), content, nil, nil)[0]
	if unit.contentID != semanticContentID(sha256.Sum256(content)) {
		t.Fatal("content ID changed raw bytes")
	}
}
