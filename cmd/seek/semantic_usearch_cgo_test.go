//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSemanticUSearchCompatibilityUsesLoadedRuntime(t *testing.T) {
	compatibility, err := semanticUSearchCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == "" {
		t.Fatal("USearch compatibility identity is empty")
	}
	if semanticUSearchRuntimeVer == "" {
		t.Fatal("loaded USearch runtime did not report a version")
	}
	wantCompatibility := "usearch-" + semanticUSearchRuntimeVer + "-" + semanticUSearchLayout()
	if compatibility != wantCompatibility {
		t.Fatalf("USearch compatibility=%q, want %q", compatibility, wantCompatibility)
	}
	digest := sha256.Sum256(semanticUSearchCompressedRuntime)
	wantAssetKey := hex.EncodeToString(digest[:16])
	if got := semanticUSearchAssetKey(); got != wantAssetKey {
		t.Fatalf("USearch asset key=%q, want %q", got, wantAssetKey)
	}
}

func TestSemanticUSearchRoundTripAndExactFallback(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	embeddings := make([]semanticUnitEmbedding, semanticUSearchUnitsPerShard*2+17)
	for row := range embeddings {
		embeddings[row] = testSemanticUnitEmbedding(semanticUSearchTestVector(row))
	}

	dir := t.TempDir()
	streamInput := make(chan semanticCoarseBatch, len(embeddings)/semanticModelBatchRows+1)
	streamDone := make(chan semanticUSearchBuildResult, 1)
	streamCtx, streamCancel := context.WithCancel(t.Context())
	defer streamCancel()
	go buildSemanticUSearchStream(streamCtx, dir, streamInput, streamCancel, searchResources{}, streamDone)
	batchCount := (len(embeddings) + semanticModelBatchRows - 1) / semanticModelBatchRows
	for sequence := batchCount - 1; sequence >= 0; sequence-- {
		start := sequence * semanticModelBatchRows
		end := min(start+semanticModelBatchRows, len(embeddings))
		coarse := make([]semanticCoarseVectors, end-start)
		for row := start; row < end; row++ {
			coarse[row-start] = embeddings[row].coarse
		}
		streamInput <- semanticCoarseBatch{sequence: sequence, coarse: coarse}
	}
	close(streamInput)
	streamResult := <-streamDone
	if streamResult.err != nil {
		t.Fatal(streamResult.err)
	}
	shards := streamResult.shards
	if len(shards) != 3 {
		t.Fatalf("shards=%d, want 3", len(shards))
	}
	paths := make([]string, len(shards))
	for index, shard := range shards {
		paths[index] = filepath.Join(dir, shard.Name)
	}
	generation := &semanticGeneration{
		manifest: semanticManifest{Rows: uint64(len(embeddings)), USearchFiles: shards},
		vectors:  testSemanticFineVectors(embeddings),
		usearch:  paths,
	}

	for _, row := range []int{0, 1, 127, semanticUSearchUnitsPerShard - 1,
		semanticUSearchUnitsPerShard, len(embeddings) - 1} {
		query := testSemanticQuery(embeddings[row].fine[0])
		candidates, err := searchSemanticUSearchCandidates(
			context.Background(),
			generation,
			query,
		)
		if err != nil {
			t.Fatalf("native search row %d: %v", row, err)
		}
		candidateFound := false
		for _, candidate := range candidates {
			if candidate == uint64(row) {
				candidateFound = true
				break
			}
		}
		if !candidateFound {
			t.Fatalf("row %d missing from native candidates: %v", row, candidates)
		}
		hits, err := semanticUSearchExactFallback(context.Background(), generation, query, 10)
		if err != nil {
			t.Fatalf("search row %d: %v", row, err)
		}
		found := false
		for _, hit := range hits {
			if hit.row == uint64(row) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("row %d missing from its top 10: %+v", row, hits)
		}
	}

	if err := os.WriteFile(paths[0], []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := searchSemanticUSearchCandidates(
		context.Background(),
		generation,
		testSemanticQuery(embeddings[0].fine[0]),
	); err == nil {
		t.Fatal("native search accepted a damaged USearch shard")
	}
	hits, err := semanticUSearchExactFallback(
		context.Background(),
		generation,
		testSemanticQuery(embeddings[0].fine[0]),
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].row != 0 {
		t.Fatalf("exact fallback hits=%+v, want row 0", hits)
	}
}

func TestSemanticUSearchVectorKeyFormatExamples(t *testing.T) {
	tests := []struct {
		row      uint64
		centroid int
		key      uint64
	}{
		{row: 0, centroid: 0, key: 0},
		{row: 0, centroid: 3, key: 3},
		{row: 1, centroid: 0, key: 4},
		{row: 1, centroid: 3, key: 7},
		{row: 127, centroid: 2, key: 510},
		{row: 9_999_999, centroid: 3, key: 39_999_999},
	}
	for _, test := range tests {
		if got := semanticUSearchVectorKey(test.row, test.centroid); got != test.key {
			t.Errorf(
				"encode row %d centroid %d: key=%d, want %d",
				test.row,
				test.centroid,
				got,
				test.key,
			)
		}
		gotRow, gotCentroid := semanticUSearchUnitAndCentroid(test.key)
		if gotRow != test.row || gotCentroid != test.centroid {
			t.Errorf(
				"decode key %d: row=%d centroid=%d, want %d and %d",
				test.key,
				gotRow,
				gotCentroid,
				test.row,
				test.centroid,
			)
		}
	}
}

func TestRunDefaultJoinedSearchUsesUSearchAboveExactThreshold(t *testing.T) {
	requireTools(t)
	logs := captureTestLogs(t, slog.LevelDebug)
	folder := t.TempDir()
	targetVector := semanticUSearchTestVector(10_000)
	vectorsByPath := map[string]semanticVector{
		"00-target.go": targetVector,
		"strict.go":    semanticUSearchTestVector(9_999),
	}
	writeFileAt(
		t,
		folder,
		"00-target.go",
		"package sample\n// SEMANTIC_TARGET dispatches the selected request\n",
	)
	for index := range semanticUSearchVectorsPerQuery + 4 {
		path := fmt.Sprintf("a-%03d.go", index)
		writeFileAt(
			t,
			folder,
			path,
			fmt.Sprintf("package sample\n// unrelated text %03d\n", index),
		)
		vectorsByPath[path] = semanticUSearchTestVector(index)
	}
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	plan := planFolderTestCorpus(t, folder)
	scorer := &hybridTestScorer{
		vectorForUnit: func(unit semanticUnit) semanticVector {
			return vectorsByPath[unit.path]
		},
		queryVector: &targetVector,
	}

	output, err := captureStdout(t, func() error {
		return runSearchCommand(
			t.Context(),
			"alpha beta",
			[]string{folder},
			0,
			0,
			defaultSearchConfig(),
			searchRunConfig{
				policy: defaultSearchPolicy(),
				newModel: func(context.Context) (semanticModel, error) {
					return scorer, nil
				},
			},
		)
	})
	if err != nil {
		t.Fatal(err)
	}

	state, _, err := folderCorpusFingerprint(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := openSemanticGeneration(
		semanticGenerationDir(plan.indexDir, state),
		state,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = generation.Close() })
	if len(generation.vectors) <= semanticUSearchVectorsPerQuery {
		t.Fatalf(
			"semantic rows=%d, want more than exact threshold %d",
			len(generation.vectors),
			semanticUSearchVectorsPerQuery,
		)
	}
	targetRow := -1
	for row, unit := range generation.rows {
		if unit.path == "00-target.go" {
			targetRow = row
			break
		}
	}
	if targetRow < 0 {
		t.Fatal("semantic generation omitted the target row")
	}
	if !reflect.DeepEqual(generation.vectors[targetRow][0], targetVector) {
		t.Fatalf("target vector=%v", generation.vectors[targetRow][0][:2])
	}
	rows, err := searchSemanticUSearchCandidates(
		t.Context(),
		generation,
		testSemanticQuery(targetVector),
	)
	if err != nil {
		t.Fatalf("native USearch: %v", err)
	}
	found := false
	for _, row := range rows {
		found = found || row == uint64(targetRow)
	}
	if !found {
		t.Fatalf("native USearch omitted target row %d from %d candidates", targetRow, len(rows))
	}
	if !strings.Contains(output, "## 00-target.go") ||
		!strings.Contains(output, "SEMANTIC_TARGET") {
		t.Fatalf("joined search omitted the native candidate:\n%s", output)
	}
	for _, record := range logs.Records() {
		if record.Message == "Searched semantic index" &&
			testLogAttrs(record)["backend"] == "usearch" {
			return
		}
	}
	t.Fatal("joined command did not report the USearch retrieval backend")
}

func semanticUSearchTestVector(row int) semanticVector {
	var vector semanticVector
	var norm float64
	for column := range vector {
		value := float32(math.Sin(float64((row+1)*(column+3))) +
			0.5*math.Cos(float64((row+7)*(column+1))))
		vector[column] = value
		norm += float64(value * value)
	}
	scale := float32(1 / math.Sqrt(norm))
	for column := range vector {
		vector[column] *= scale
	}
	return vector
}
