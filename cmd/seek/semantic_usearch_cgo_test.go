//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	rows := make([]semanticUnit, len(embeddings))
	allowed := 0
	for row := range embeddings {
		embeddings[row] = testSemanticUnitEmbedding(semanticUSearchTestVector(row))
		prefix := "blocked"
		if row%2 == 0 {
			prefix = "allowed"
			allowed++
		}
		rows[row] = semanticUnit{
			row:          uint64(row),
			path:         fmt.Sprintf("%s/%05d.go", prefix, row),
			fileLanguage: "Go",
		}
	}
	if allowed <= semanticFilteredExactRows {
		t.Fatalf("allowed rows=%d, must exceed filtered exact limit %d", allowed, semanticFilteredExactRows)
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
		rows:     rows,
		vectors:  testSemanticFineVectors(embeddings),
		usearch:  paths,
	}
	batchQueries := []semanticVector{embeddings[0].fine[0], embeddings[126].fine[0]}
	batchMask := semanticFilterMask{
		mode: semanticFilterPartial, rows: uint64(len(rows)), allowed: uint64(allowed),
		bits: make([]byte, (len(rows)+7)/8), shardAllowed: make([]uint64, len(shards)),
	}
	for row := 0; row < len(rows); row += 2 {
		batchMask.bits[row/8] |= 1 << (uint(row) & 7)
		batchMask.shardAllowed[row/semanticUSearchUnitsPerShard]++
	}
	for _, test := range []struct {
		name     string
		mask     *semanticFilterMask
		filtered bool
	}{
		{name: "unfiltered"},
		{name: "filtered", mask: &batchMask, filtered: true},
	} {
		t.Run("batch-matches-single-"+test.name, func(t *testing.T) {
			batched, err := searchSemanticUSearchShardTokens(
				t.Context(), paths[0], shards[0], batchQueries, 32, test.mask, test.filtered,
			)
			if err != nil {
				t.Fatal(err)
			}
			for token, query := range batchQueries {
				single, err := searchSemanticUSearchShardTokens(
					t.Context(), paths[0], shards[0], []semanticVector{query}, 32,
					test.mask, test.filtered,
				)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(batched[token], single[0]) {
					t.Fatalf("token %d batch result differs from its single search", token)
				}
			}
		})
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

	for _, row := range []int{0, len(embeddings) - 1} {
		mask := semanticFilterMask{
			mode:         semanticFilterPartial,
			bits:         make([]byte, (len(embeddings)+7)/8),
			rows:         uint64(len(embeddings)),
			allowed:      1,
			shardAllowed: make([]uint64, len(shards)),
		}
		mask.bits[row/8] |= 1 << (uint(row) & 7)
		mask.shardAllowed[row/semanticUSearchUnitsPerShard] = 1
		candidates, err := searchSemanticUSearchFilteredCandidates(
			t.Context(),
			generation,
			testSemanticQuery(embeddings[row].fine[0]),
			&mask,
		)
		if err != nil {
			t.Fatalf("filtered native search row %d: %v", row, err)
		}
		if len(candidates) != 1 || candidates[0] != uint64(row) {
			t.Fatalf("filtered candidates for row %d=%v", row, candidates)
		}
	}

	logs := captureTestLogs(t, slog.LevelDebug)
	target := semanticUSearchUnitsPerShard
	hits, err := semanticUSearchFiltered(
		t.Context(),
		generation,
		testSemanticQuery(embeddings[target].fine[0]),
		semanticFilterForTest(t, "file:^allowed/"),
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	foundTarget := false
	for _, hit := range hits {
		if hit.row >= uint64(len(rows)) || !strings.HasPrefix(rows[hit.row].path, "allowed/") {
			t.Fatalf("filtered native search returned row %d", hit.row)
		}
		foundTarget = foundTarget || hit.row == uint64(target)
	}
	if !foundTarget {
		t.Fatalf("filtered native search omitted target row %d: %+v", target, hits)
	}
	foundNativeLog := false
	for _, record := range logs.Records() {
		if record.Message == "Searched semantic index" &&
			testLogAttrs(record)["backend"] == "filtered-usearch" {
			foundNativeLog = true
			break
		}
	}
	if !foundNativeLog {
		t.Fatal("filtered search did not use USearch")
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
	hits, err = semanticUSearchExactFallback(
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

func TestSearchGenerationDefersUnusedGraphValidation(t *testing.T) {
	const rowCount = semanticFilteredExactRows + 44
	units := make([]semanticUnit, rowCount)
	embeddings := make([]semanticUnitEmbedding, rowCount)
	for row := range units {
		path := fmt.Sprintf("drop/%03d.go", row)
		if row == 0 {
			path = "keep/target.go"
		}
		units[row] = semanticUnit{
			row: uint64(row), path: path, start: 1, end: 2,
			kind: semanticUnitSymbol, parserResult: semanticParserCTags,
			language: "Go", fileLanguage: "Go",
		}
		units[row].id = makeSemanticUnitID(units[row])
		embeddings[row] = testSemanticUnitEmbedding(semanticUSearchTestVector(row))
	}
	dir := t.TempDir()
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head-lazy", units, embeddings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, semanticUSearchShardName(0)), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := openSemanticGenerationForSearch(dir, "head-lazy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = generation.Close() })
	damaged := false
	generation.onUSearchDamage = func() { damaged = true }

	hits, err := semanticUSearchFiltered(
		t.Context(),
		generation,
		testSemanticQuery(embeddings[0].fine[0]),
		semanticFilterForTest(t, "file:^keep/"),
		1,
	)
	if err != nil || len(hits) != 1 || hits[0].row != 0 {
		t.Fatalf("exact filtered hits=%+v error=%v", hits, err)
	}
	if damaged {
		t.Fatal("exact-only search validated an unused graph")
	}
	if _, err := searchSemanticUSearchCandidates(
		t.Context(), generation, testSemanticQuery(embeddings[0].fine[0]),
	); err == nil {
		t.Fatal("native search accepted a damaged graph")
	}
	if !damaged {
		t.Fatal("native graph damage did not run invalidation callback")
	}
}

func TestSemanticUSearchExactFallbackRejectsExcessWork(t *testing.T) {
	rowCount := semanticUSearchUnitsPerShard + 1
	generation := &semanticGeneration{
		manifest: semanticManifest{
			Rows: uint64(rowCount),
			USearchFiles: []semanticUSearchShard{{
				Name: "unavailable.usearch", Rows: uint64(rowCount),
			}},
		},
		vectors:    make([]semanticFineVectors, rowCount),
		usearch:    []string{"unavailable.usearch"},
		usearchErr: errors.New("damaged graph"),
	}
	query := testSemanticQuery(semanticVector{0: 1})
	for token := 1; token < lateOnSequenceLength; token++ {
		start := token * semanticEmbeddingDimensions
		query.tokens[start] = 1
		query.scoreMask[token] = true
	}
	_, err := semanticUSearchExactFallback(t.Context(), generation, query, 1)
	if err == nil || !strings.Contains(err.Error(), "exact recovery work") {
		t.Fatalf("fallback error=%v", err)
	}
}

func TestPlanSemanticUSearchShardsBoundsSparseExactWork(t *testing.T) {
	const exactRows = semanticUSearchUnitsPerShard
	const shardCount = exactRows + 1
	shards := make([]semanticUSearchShard, shardCount)
	mask := semanticFilterMask{
		mode:         semanticFilterPartial,
		rows:         shardCount * 2,
		allowed:      shardCount,
		bits:         make([]byte, (shardCount*2+7)/8),
		shardAllowed: make([]uint64, shardCount),
	}
	for index := range shards {
		row := uint64(index * 2)
		shards[index] = semanticUSearchShard{Start: row, Rows: 2}
		mask.bits[row/8] |= byte(1 << (row & 7))
		mask.shardAllowed[index] = 1
	}
	generation := &semanticGeneration{
		manifest: semanticManifest{Rows: mask.rows, USearchFiles: shards},
	}
	plan, err := planSemanticUSearchShards(t.Context(), generation, &mask, lateOnSequenceLength)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.jobs) != 1 || len(plan.exactRows) != exactRows {
		t.Fatalf("plan jobs=%d exact rows=%d", len(plan.jobs), len(plan.exactRows))
	}
	for index, row := range plan.exactRows {
		if want := uint64(index * 2); row != want {
			t.Fatalf("exact row %d=%d, want %d", index, row, want)
		}
	}
	if job := plan.jobs[0]; job.index != shardCount-1 || !job.filtered || job.limit != semanticCoarseCentroidsPerUnit {
		t.Fatalf("overflow job=%+v", job)
	}
}

func TestPlanSemanticUSearchShardsBoundsOptionalExactRows(t *testing.T) {
	shards := []semanticUSearchShard{
		{Start: 0, Rows: semanticUSearchUnitsPerShard},
		{Start: semanticUSearchUnitsPerShard, Rows: semanticUSearchUnitsPerShard},
	}
	rows := uint64(2 * semanticUSearchUnitsPerShard)
	mask := semanticFilterMask{
		mode:         semanticFilterPartial,
		rows:         rows,
		allowed:      semanticUSearchUnitsPerShard / 2,
		bits:         make([]byte, (rows+7)/8),
		shardAllowed: []uint64{semanticUSearchUnitsPerShard / 4, semanticUSearchUnitsPerShard / 4},
	}
	setSemanticFilterRange(mask.bits, 0, semanticUSearchUnitsPerShard/4)
	setSemanticFilterRange(
		mask.bits,
		semanticUSearchUnitsPerShard,
		semanticUSearchUnitsPerShard+semanticUSearchUnitsPerShard/4,
	)
	generation := &semanticGeneration{
		manifest: semanticManifest{Rows: rows, USearchFiles: shards},
	}

	exact, err := planSemanticUSearchShards(t.Context(), generation, &mask, lateOnSequenceLength)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.jobs) != 0 || uint64(len(exact.exactRows)) != mask.allowed {
		t.Fatalf("small plan jobs=%d exact rows=%d", len(exact.jobs), len(exact.exactRows))
	}

	for len(shards) < 5 {
		shardStart := rows
		shards = append(shards, semanticUSearchShard{Start: shardStart, Rows: semanticUSearchUnitsPerShard})
		rows += semanticUSearchUnitsPerShard
		mask.rows = rows
		mask.allowed += semanticUSearchUnitsPerShard / 4
		mask.bits = append(mask.bits, make([]byte, semanticUSearchUnitsPerShard/8)...)
		mask.shardAllowed = append(mask.shardAllowed, semanticUSearchUnitsPerShard/4)
		setSemanticFilterRange(mask.bits, shardStart, shardStart+semanticUSearchUnitsPerShard/4)
	}
	generation.manifest.Rows = rows
	generation.manifest.USearchFiles = shards
	filtered, err := planSemanticUSearchShards(t.Context(), generation, &mask, lateOnSequenceLength)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.jobs) != 1 || len(filtered.exactRows) != 4*semanticUSearchUnitsPerShard/4 {
		t.Fatalf("large plan=%+v", filtered)
	}
	if job := filtered.jobs[0]; job.index != 4 || !job.filtered {
		t.Fatalf("large plan overflow job=%+v", job)
	}
}

func TestSemanticUSearchOptionalExactShardUsesQuarterBoundary(t *testing.T) {
	for _, test := range []struct {
		allowed uint64
		want    bool
	}{
		{allowed: 1_024, want: true},
		{allowed: 1_025, want: true},
		{allowed: 1_026, want: false},
		{allowed: 1_843, want: false},
	} {
		if got := semanticUSearchOptionalExactShard(test.allowed, semanticUSearchUnitsPerShard); got != test.want {
			t.Errorf("allowed rows %d: exact=%t, want %t", test.allowed, got, test.want)
		}
	}
}

func TestSemanticUSearchCentroidShiftMatchesStoredKeyLayout(t *testing.T) {
	shift, err := semanticUSearchCentroidShift()
	if err != nil {
		t.Fatal(err)
	}
	if 1<<shift != semanticCoarseCentroidsPerUnit {
		t.Fatalf("centroid shift=%d, count=%d", shift, semanticCoarseCentroidsPerUnit)
	}
}

func TestPlanSemanticUSearchShardsHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := planSemanticUSearchShards(ctx, &semanticGeneration{}, nil, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestMergeSemanticHitsKeepsBoundedTotalOrder(t *testing.T) {
	left := []semanticHit{{row: 1, score: 9}, {row: 9, score: 7}}
	right := []semanticHit{{row: 0, score: 9}, {row: 3, score: 8}, {row: 5, score: 1}}
	buffer := make([]semanticHit, 0, 3)
	got := mergeSemanticHits(buffer, left, right, 3)
	want := []semanticHit{{row: 0, score: 9}, {row: 1, score: 9}, {row: 3, score: 8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged hits=%+v, want %+v", got, want)
	}
	if len(got) != 3 || cap(got) != cap(buffer) {
		t.Fatalf("merged length/capacity=%d/%d, buffer capacity=%d", len(got), cap(got), cap(buffer))
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

func TestSemanticFilteredRouteBoundaryAndNativeFailure(t *testing.T) {
	const rows = semanticFilteredExactRows + 2
	units := make([]semanticUnit, rows)
	vectors := make([]semanticFineVectors, rows)
	vector := semanticVector{0: 1}
	for row := range rows {
		path := fmt.Sprintf("small/%03d.go", row)
		switch row {
		case semanticFilteredExactRows:
			path = "large/extra.go"
		case semanticFilteredExactRows + 1:
			path = "drop/final.go"
		}
		units[row] = semanticUnit{row: uint64(row), path: path, fileLanguage: "Go"}
		vectors[row] = testSemanticUnitEmbedding(vector).fine
	}
	wantErr := errors.New("damaged filtered graph")
	generation := &semanticGeneration{
		manifest: semanticManifest{
			Rows: rows,
			USearchFiles: []semanticUSearchShard{{
				Start: 0,
				Rows:  rows,
			}},
		},
		rows:       units,
		vectors:    vectors,
		usearch:    []string{"unused"},
		usearchErr: wantErr,
	}

	hits, err := semanticUSearchFiltered(
		t.Context(),
		generation,
		testSemanticQuery(vector),
		semanticFilterForTest(t, "file:^small/"),
		1,
	)
	if err != nil {
		t.Fatalf("exact route at %d rows: %v", semanticFilteredExactRows, err)
	}
	if len(hits) != 1 || hits[0].row != 0 {
		t.Fatalf("exact boundary hits=%+v", hits)
	}

	_, err = semanticUSearchFiltered(
		t.Context(),
		generation,
		testSemanticQuery(vector),
		semanticFilterForTest(t, "-file:^drop/"),
		1,
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("native route error=%v, want %v", err, wantErr)
	}

	hits, err = semanticUSearchFiltered(
		t.Context(),
		generation,
		nil,
		semanticFilterForTest(t, "file:^missing/"),
		1,
	)
	if err != nil || len(hits) != 0 {
		t.Fatalf("NONE route hits=%v error=%v", hits, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = semanticUSearchFiltered(
		canceled,
		generation,
		testSemanticQuery(vector),
		semanticFilterForTest(t, "-file:^drop/"),
		1,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("filtered cancellation error=%v, want cancellation", err)
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

func BenchmarkSemanticFilteredRetrieval(b *testing.B) {
	b.Setenv("SEEK_CACHE_DIR", b.TempDir())
	embeddings := make([]semanticUnitEmbedding, semanticUSearchUnitsPerShard*2)
	for row := range embeddings {
		embeddings[row] = testSemanticUnitEmbedding(semanticUSearchTestVector(row))
	}
	dir := b.TempDir()
	input := make(chan semanticCoarseBatch, len(embeddings)/semanticModelBatchRows+1)
	done := make(chan semanticUSearchBuildResult, 1)
	ctx, cancel := context.WithCancel(b.Context())
	b.Cleanup(cancel)
	go buildSemanticUSearchStream(ctx, dir, input, cancel, searchResources{}, done)
	for sequence, start := 0, 0; start < len(embeddings); sequence, start = sequence+1, start+semanticModelBatchRows {
		end := min(start+semanticModelBatchRows, len(embeddings))
		coarse := make([]semanticCoarseVectors, end-start)
		for row := start; row < end; row++ {
			coarse[row-start] = embeddings[row].coarse
		}
		input <- semanticCoarseBatch{sequence: sequence, coarse: coarse}
	}
	close(input)
	built := <-done
	if built.err != nil {
		b.Fatal(built.err)
	}
	paths := make([]string, len(built.shards))
	for index, shard := range built.shards {
		paths[index] = filepath.Join(dir, shard.Name)
	}

	for _, test := range []struct {
		name    string
		percent int
		tokens  int
	}{
		{name: "small-1-percent-2-tokens", percent: 1, tokens: 2},
		{name: "low-4-percent-2-tokens", percent: 4, tokens: 2},
		{name: "medium-25-percent-2-tokens", percent: 25, tokens: 2},
		{name: "upper-45-percent-2-tokens", percent: 45, tokens: 2},
		{name: "high-90-percent-2-tokens", percent: 90, tokens: 2},
		{name: "low-4-percent-32-tokens", percent: 4, tokens: 32},
		{name: "medium-25-percent-32-tokens", percent: 25, tokens: 32},
		{name: "high-90-percent-32-tokens", percent: 90, tokens: 32},
		{name: "low-4-percent-128-tokens", percent: 4, tokens: 128},
		{name: "medium-25-percent-128-tokens", percent: 25, tokens: 128},
		{name: "upper-45-percent-128-tokens", percent: 45, tokens: 128},
		{name: "high-90-percent-128-tokens", percent: 90, tokens: 128},
	} {
		rows := make([]semanticUnit, len(embeddings))
		allowedRows := make([]uint64, 0, len(rows)*test.percent/100+1)
		for row := range rows {
			prefix := "blocked"
			if row%100 < test.percent {
				prefix = "allowed"
				allowedRows = append(allowedRows, uint64(row))
			}
			rows[row] = semanticUnit{
				row:          uint64(row),
				path:         fmt.Sprintf("%s/%04d.go", prefix, row),
				fileLanguage: "Go",
			}
		}
		generation := &semanticGeneration{
			manifest: semanticManifest{
				Rows:         uint64(len(rows)),
				USearchFiles: built.shards,
			},
			rows:    rows,
			vectors: testSemanticFineVectors(embeddings),
			usearch: paths,
		}
		filter := semanticFilterForTest(b, "file:^allowed/")
		target := int(allowedRows[len(allowedRows)-1])
		prepared := testSemanticQuery(embeddings[target].fine[0])
		for token := 1; token < test.tokens; token++ {
			start := token * semanticEmbeddingDimensions
			copy(prepared.tokens[start:start+semanticEmbeddingDimensions], embeddings[target].fine[0][:])
			prepared.scoreMask[token] = true
		}
		benchmarkMask, err := buildSemanticFilterMask(b.Context(), generation, filter)
		if err != nil {
			b.Fatal(err)
		}
		plannedRoute, err := planSemanticUSearchShards(
			b.Context(), generation, &benchmarkMask, test.tokens,
		)
		if err != nil {
			b.Fatal(err)
		}
		forcedNativeShards := 0
		for _, allowed := range benchmarkMask.shardAllowed {
			if allowed > 0 {
				forcedNativeShards++
			}
		}
		exact, err := exactSemanticSortedCandidates(
			b.Context(), generation.vectors, prepared, allowedRows, 10,
		)
		if err != nil {
			b.Fatal(err)
		}
		wantRows := make(map[uint64]struct{}, len(exact))
		for _, hit := range exact {
			wantRows[hit.row] = struct{}{}
		}
		routes := []struct {
			name         string
			exactRows    int
			nativeShards int
			run          func(context.Context) ([]semanticHit, error)
		}{
			{
				name:         "planned",
				exactRows:    len(plannedRoute.exactRows),
				nativeShards: len(plannedRoute.jobs),
				run: func(ctx context.Context) ([]semanticHit, error) {
					return semanticUSearchFiltered(ctx, generation, prepared, filter, 10)
				},
			},
			{
				name:         "forced-native",
				nativeShards: forcedNativeShards,
				run: func(ctx context.Context) ([]semanticHit, error) {
					return benchmarkForcedNativeSemanticFilter(
						ctx, generation, prepared, filter, 10,
					)
				},
			},
			{
				name:      "forced-exact",
				exactRows: len(allowedRows),
				run: func(ctx context.Context) ([]semanticHit, error) {
					mask, err := buildSemanticFilterMask(ctx, generation, filter)
					if err != nil {
						return nil, err
					}
					return exactSemanticSortedCandidates(ctx, generation.vectors, prepared, mask.selectedRows(), 10)
				},
			},
		}
		for _, route := range routes {
			b.Run(test.name+"/"+route.name, func(b *testing.B) {
				if _, err := route.run(b.Context()); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				var hits []semanticHit
				for b.Loop() {
					hits, err = route.run(b.Context())
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(len(allowedRows))/float64(len(rows)), "allowed/row")
				b.ReportMetric(float64(route.exactRows), "exact-rows")
				b.ReportMetric(float64(route.nativeShards), "native-shards")
				foundTarget, recalled := false, 0
				for _, hit := range hits {
					if hit.row >= uint64(len(rows)) || hit.row%100 >= uint64(test.percent) {
						b.Fatalf("filtered retrieval returned row %d", hit.row)
					}
					foundTarget = foundTarget || hit.row == uint64(target)
					if _, ok := wantRows[hit.row]; ok {
						recalled++
					}
				}
				if !foundTarget {
					b.Fatalf("filtered retrieval omitted target row %d", target)
				}
				b.ReportMetric(float64(recalled)/float64(len(exact)), "top10-recall")
				b.ReportMetric(float64(max(0, len(exact)-len(hits))), "underfill")
			})
		}
	}
}

func benchmarkForcedNativeSemanticFilter(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	filter *semanticFilterPlan,
	limit int,
) ([]semanticHit, error) {
	mask, err := buildSemanticFilterMask(ctx, generation, filter)
	if err != nil {
		return nil, err
	}
	queryVectors, err := semanticQueryTokenVectors(query)
	if err != nil {
		return nil, err
	}
	plan := semanticUSearchQueryPlan{
		jobs: make([]semanticUSearchQueryShard, 0, len(generation.manifest.USearchFiles)),
	}
	for shardIndex, shard := range generation.manifest.USearchFiles {
		allowed := mask.shardAllowed[shardIndex]
		if allowed == 0 {
			continue
		}
		job := semanticUSearchQueryShard{index: shardIndex, limit: semanticUSearchVectorsPerQuery}
		if allowed != shard.Rows {
			job.filtered = true
			job.limit = int(min(
				uint64(semanticUSearchVectorsPerQuery),
				allowed*semanticCoarseCentroidsPerUnit,
			))
		}
		plan.jobs = append(plan.jobs, job)
	}
	candidates, err := executeSemanticUSearchPlan(ctx, generation, queryVectors, &mask, plan)
	if err != nil {
		return nil, err
	}
	return exactSemanticSortedRows(ctx, generation.vectors, queryVectors, candidates.rows, limit)
}

func BenchmarkPlanSemanticUSearchShardsScale(b *testing.B) {
	for _, shardCount := range []int{2, 50, 2_442} {
		b.Run(fmt.Sprintf("shards-%d", shardCount), func(b *testing.B) {
			shards := make([]semanticUSearchShard, shardCount)
			rows := uint64(0)
			for shard := range shards {
				shardRows := uint64(semanticUSearchUnitsPerShard)
				if remaining := uint64(semanticMaxRows) - rows; shardRows > remaining {
					shardRows = remaining
				}
				shards[shard] = semanticUSearchShard{Start: rows, Rows: shardRows}
				rows += shardRows
			}
			mask := semanticFilterMask{
				mode: semanticFilterPartial, rows: rows,
				bits: make([]byte, (rows+7)/8), shardAllowed: make([]uint64, shardCount),
			}
			for shardIndex, shard := range shards {
				allowed := max(uint64(1), shard.Rows*4/100)
				mask.shardAllowed[shardIndex] = allowed
				mask.allowed += allowed
				setSemanticFilterRange(mask.bits, shard.Start, shard.Start+allowed)
			}
			generation := &semanticGeneration{
				manifest: semanticManifest{Rows: rows, USearchFiles: shards},
			}
			b.ReportAllocs()
			var plan semanticUSearchQueryPlan
			b.ResetTimer()
			for b.Loop() {
				var err error
				plan, err = planSemanticUSearchShards(
					b.Context(), generation, &mask, lateOnSequenceLength,
				)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(plan.jobs)), "native-jobs")
			b.ReportMetric(float64(len(plan.exactRows)), "exact-rows")
		})
	}
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
