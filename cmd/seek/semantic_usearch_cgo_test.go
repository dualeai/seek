//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestSemanticUSearchCompatibilityUsesLoadedRuntime checks the identity that a
// manifest stores: it names the version that the loaded runtime reports,
// which must be the version that cicd/rerank-assets-upgrade.sh pins, and the
// storage layout. The asset key, the directory of the extracted runtime,
// follows the runtime bytes.
func TestSemanticUSearchCompatibilityUsesLoadedRuntime(t *testing.T) {
	compatibility, err := semanticUSearchCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "cicd", "rerank-assets-upgrade.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`(?m)^usearch_version=([0-9][0-9.]*)$`).FindSubmatch(script)
	if pinned == nil {
		t.Fatal("cicd/rerank-assets-upgrade.sh pins no USearch version")
	}
	// The layout settings of semantic_usearch_cgo.go: cosine (1), int8 (4),
	// connectivity 16, expansions 128 and 64, one thread, four centroids, at
	// most 4,096 rows per shard, shards cut at file boundaries with keys from
	// 0, revision 3. A change to one of them, or to the library bytes, must
	// change the identity, so that no generation reuses an incompatible graph.
	want := "usearch-" + string(pinned[1]) + "-" + semanticUSearchAssetKey() + "-metric1-quant4-m16-add128-search64-threads1-c4-maxrows4096-filecut-v3"
	if compatibility != want {
		t.Fatalf("USearch compatibility=%q, want %q", compatibility, want)
	}

	key := semanticUSearchAssetKey()
	if len(key) != 32 || strings.Trim(key, "0123456789abcdef") != "" {
		t.Fatalf("USearch asset key=%q, want 32 lowercase hex digits", key)
	}
	original := semanticUSearchCompressedRuntime
	t.Cleanup(func() { semanticUSearchCompressedRuntime = original })
	semanticUSearchCompressedRuntime = append([]byte(nil), original...)
	if got := semanticUSearchAssetKey(); got != key {
		t.Fatalf("the same runtime bytes gave asset key %q, then %q", key, got)
	}
	semanticUSearchCompressedRuntime[len(semanticUSearchCompressedRuntime)-1] ^= 0xff
	if got := semanticUSearchAssetKey(); got == key {
		t.Fatalf("other runtime bytes kept asset key %q", key)
	}
}

func TestSemanticUSearchRoundTripAndExactFallback(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	embeddings := make([]semanticUnitEmbedding, semanticUSearchMaxShardRows*2+17)
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
			row:  uint64(row),
			path: fmt.Sprintf("%s/%05d.go", prefix, row),
		}
	}
	if allowed <= semanticFilteredExactRows {
		t.Fatalf("allowed rows=%d, must exceed filtered exact limit %d", allowed, semanticFilteredExactRows)
	}

	dir := t.TempDir()
	streamResult := streamTestUSearchShards(t, dir, embeddings,
		fixedTestUSearchPlans(len(embeddings), semanticUSearchMaxShardRows), nil)
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
		manifest:          semanticManifest{Rows: uint64(len(embeddings)), USearchFiles: shards},
		rows:              rows,
		vectors:           testSemanticFineVectors(embeddings),
		usearch:           paths,
		usearchValidation: make([]semanticUSearchValidation, len(paths)),
	}
	batchQueries := []semanticVector{embeddings[0].fine[0], embeddings[126].fine[0]}
	batchMask := semanticFilterMask{
		mode: semanticFilterPartial, rows: uint64(len(rows)), allowed: uint64(allowed),
		bits: make([]byte, (len(rows)+7)/8), shardAllowed: make([]uint64, len(shards)),
	}
	for row := 0; row < len(rows); row += 2 {
		batchMask.bits[row/8] |= 1 << (uint(row) & 7)
		batchMask.shardAllowed[row/semanticUSearchMaxShardRows]++
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
			batched, saturated, err := searchSemanticUSearchShardTokens(
				t.Context(), paths[0], shards[0], batchQueries, 32, test.mask, test.filtered,
			)
			if err != nil || saturated {
				t.Fatalf("batch search saturated=%t error=%v", saturated, err)
			}
			for token, query := range batchQueries {
				single, saturated, err := searchSemanticUSearchShardTokens(
					t.Context(), paths[0], shards[0], []semanticVector{query}, 32,
					test.mask, test.filtered,
				)
				if err != nil || saturated {
					t.Fatalf("single search saturated=%t error=%v", saturated, err)
				}
				if !reflect.DeepEqual(batched[token], single[0]) {
					t.Fatalf("token %d batch result differs from its single search", token)
				}
			}
		})
	}

	for _, row := range []int{0, 1, 127, semanticUSearchMaxShardRows - 1,
		semanticUSearchMaxShardRows, len(embeddings) - 1} {
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
		hits, err := semanticUSearchExactFallback(context.Background(), generation, query, noSemanticFloor)
		if err != nil {
			t.Fatalf("search row %d: %v", row, err)
		}
		hits = firstSemanticHits(hits, 10)
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
		mask.shardAllowed[row/semanticUSearchMaxShardRows] = 1
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
	target := semanticUSearchMaxShardRows
	hits, err := semanticUSearchFiltered(
		t.Context(),
		generation,
		testSemanticQuery(embeddings[target].fine[0]),
		keepPathPrefix("allowed/"),
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
	// The query is the vector of a blocked row, which a search without the
	// mask finds first. With the mask, each hit must still be an allowed row.
	blocked := target + 1
	hits, err = semanticUSearchFiltered(
		t.Context(),
		generation,
		testSemanticQuery(embeddings[blocked].fine[0]),
		keepPathPrefix("allowed/"),
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.row >= uint64(len(rows)) || !strings.HasPrefix(rows[hit.row].path, "allowed/") {
			t.Fatalf("filtered native search near blocked row %d returned row %d", blocked, hit.row)
		}
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
		noSemanticFloor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if hits = firstSemanticHits(hits, 1); len(hits) != 1 || hits[0].row != 0 {
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
			language: "Go",
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
		keepPathPrefix("keep/"),
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

// TestSemanticUSearchExactFallbackRecoversAnySize fails the graph of a
// generation larger than one shard, with a 128-token query: recovery is
// still exact, with no work limit.
func TestSemanticUSearchExactFallbackRecoversAnySize(t *testing.T) {
	rowCount := semanticUSearchMaxShardRows + 1
	vectors := make([]semanticFineF16Vectors, rowCount)
	for row := range vectors {
		vectors[row] = testSemanticStoredFineVectors(semanticVector{0: 1})
	}
	generation := &semanticGeneration{
		manifest: semanticManifest{
			Rows: uint64(rowCount),
			USearchFiles: []semanticUSearchShard{{
				Name: "unavailable.usearch", Rows: uint64(rowCount),
			}},
		},
		vectors: vectors,
		// A missing graph file fails the lazy shard check, as a damaged
		// graph does.
		usearch:           []string{filepath.Join(t.TempDir(), "unavailable.usearch")},
		usearchValidation: make([]semanticUSearchValidation, 1),
	}
	query := testSemanticQuery(semanticVector{0: 1})
	for token := 1; token < lateOnSequenceLength; token++ {
		start := token * semanticEmbeddingDimensions
		query.tokens[start] = 1
		query.scoreMask[token] = true
	}
	hits, err := semanticUSearchExactFallback(t.Context(), generation, query, noSemanticFloor)
	if hits = firstSemanticHits(hits, 1); err != nil || len(hits) != 1 {
		t.Fatalf("fallback hits=%v error=%v, want one exact hit", hits, err)
	}
}

// TestPlanSemanticUSearchShardsSendsEverySparseShardToExact plans more sparse
// shards than one full shard of rows: no work budget sends any of them to
// native search.
func TestPlanSemanticUSearchShardsSendsEverySparseShardToExact(t *testing.T) {
	const shardCount = semanticUSearchMaxShardRows + 1
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
	plan, err := planSemanticUSearchShards(t.Context(), generation, &mask)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.jobs) != 0 || len(plan.exactRows) != shardCount {
		t.Fatalf("plan jobs=%d exact rows=%d, want 0 and %d", len(plan.jobs), len(plan.exactRows), shardCount)
	}
	for index, row := range plan.exactRows {
		if want := uint64(index * 2); row != want {
			t.Fatalf("exact row %d=%d, want %d", index, row, want)
		}
	}
}

// TestPlanSemanticUSearchShardsSendsQuarterShardsToExact plans shards at the
// 25 % pass boundary: each goes to exact scoring, however many there are.
func TestPlanSemanticUSearchShardsSendsQuarterShardsToExact(t *testing.T) {
	shards := []semanticUSearchShard{
		{Start: 0, Rows: semanticUSearchMaxShardRows},
		{Start: semanticUSearchMaxShardRows, Rows: semanticUSearchMaxShardRows},
	}
	rows := uint64(2 * semanticUSearchMaxShardRows)
	mask := semanticFilterMask{
		mode:         semanticFilterPartial,
		rows:         rows,
		allowed:      semanticUSearchMaxShardRows / 2,
		bits:         make([]byte, (rows+7)/8),
		shardAllowed: []uint64{semanticUSearchMaxShardRows / 4, semanticUSearchMaxShardRows / 4},
	}
	setSemanticFilterRange(mask.bits, 0, semanticUSearchMaxShardRows/4)
	setSemanticFilterRange(
		mask.bits,
		semanticUSearchMaxShardRows,
		semanticUSearchMaxShardRows+semanticUSearchMaxShardRows/4,
	)
	generation := &semanticGeneration{
		manifest: semanticManifest{Rows: rows, USearchFiles: shards},
	}

	exact, err := planSemanticUSearchShards(t.Context(), generation, &mask)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.jobs) != 0 || uint64(len(exact.exactRows)) != mask.allowed {
		t.Fatalf("small plan jobs=%d exact rows=%d", len(exact.jobs), len(exact.exactRows))
	}

	for len(shards) < 5 {
		shardStart := rows
		shards = append(shards, semanticUSearchShard{Start: shardStart, Rows: semanticUSearchMaxShardRows})
		rows += semanticUSearchMaxShardRows
		mask.rows = rows
		mask.allowed += semanticUSearchMaxShardRows / 4
		mask.bits = append(mask.bits, make([]byte, semanticUSearchMaxShardRows/8)...)
		mask.shardAllowed = append(mask.shardAllowed, semanticUSearchMaxShardRows/4)
		setSemanticFilterRange(mask.bits, shardStart, shardStart+semanticUSearchMaxShardRows/4)
	}
	// Shard 5 allows half of its rows and shard 6 all of them: both go to
	// native search, the first with the bitmap predicate.
	for _, allowed := range []uint64{semanticUSearchMaxShardRows / 2, semanticUSearchMaxShardRows} {
		shardStart := rows
		shards = append(shards, semanticUSearchShard{Start: shardStart, Rows: semanticUSearchMaxShardRows})
		rows += semanticUSearchMaxShardRows
		mask.rows = rows
		mask.allowed += allowed
		mask.bits = append(mask.bits, make([]byte, semanticUSearchMaxShardRows/8)...)
		mask.shardAllowed = append(mask.shardAllowed, allowed)
		setSemanticFilterRange(mask.bits, shardStart, shardStart+allowed)
	}
	generation.manifest.Rows = rows
	generation.manifest.USearchFiles = shards
	large, err := planSemanticUSearchShards(t.Context(), generation, &mask)
	if err != nil {
		t.Fatal(err)
	}
	if len(large.jobs) != 2 || len(large.exactRows) != 5*semanticUSearchMaxShardRows/4 {
		t.Fatalf("large plan jobs=%+v exact rows=%d, want 2 jobs and %d rows",
			large.jobs, len(large.exactRows), 5*semanticUSearchMaxShardRows/4)
	}
	if job := large.jobs[0]; job.index != 5 || !job.filtered || job.limit != semanticUSearchVectorsPerQuery {
		t.Fatalf("large plan half-allowed job=%+v", job)
	}
	if job := large.jobs[1]; job.index != 6 || job.filtered {
		t.Fatalf("large plan all-allowed job=%+v", job)
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
		if got := semanticUSearchOptionalExactShard(test.allowed, semanticUSearchMaxShardRows); got != test.want {
			t.Errorf("allowed rows %d: exact=%t, want %t", test.allowed, got, test.want)
		}
	}
}

// TestMarkSemanticRowRangeFollowsMask marks aligned and unaligned row ranges
// with and without a mask and compares each bitmap with a row-by-row mark.
func TestMarkSemanticRowRangeFollowsMask(t *testing.T) {
	const rows = 1_000
	random := rand.New(rand.NewPCG(3, 4))
	mask := semanticFilterMask{mode: semanticFilterPartial, rows: rows, bits: make([]byte, (rows+7)/8)}
	for row := range uint64(rows) {
		if random.IntN(3) == 0 {
			mask.bits[row/8] |= 1 << (row & 7)
		}
	}
	for _, span := range [][2]uint64{{0, rows}, {0, 64}, {64, 192}, {3, 130}, {500, 501}, {640, rows}, {7, 7}} {
		for _, rowMask := range []*semanticFilterMask{nil, &mask} {
			got := make([]uint64, (rows+63)/64)
			if err := markSemanticRowRange(got, rows, span[0], span[1], rowMask); err != nil {
				t.Fatal(err)
			}
			want := make([]uint64, (rows+63)/64)
			for row := span[0]; row < span[1]; row++ {
				if rowMask == nil || rowMask.allows(row) {
					want[row/64] |= 1 << (row % 64)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("rows %d-%d mask %t: bitmap differs from a row-by-row mark", span[0], span[1], rowMask != nil)
			}
		}
	}
	if err := markSemanticRowRange(make([]uint64, (rows+63)/64), rows, 10, rows+1, nil); err == nil {
		t.Fatal("a range past the generation was accepted")
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
	_, err := planSemanticUSearchShards(ctx, &semanticGeneration{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
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
	vectors := make([]semanticFineF16Vectors, rows)
	vector := semanticVector{0: 1}
	for row := range rows {
		path := fmt.Sprintf("small/%03d.go", row)
		switch row {
		case semanticFilteredExactRows:
			path = "large/extra.go"
		case semanticFilteredExactRows + 1:
			path = "drop/final.go"
		}
		units[row] = semanticUnit{row: uint64(row), path: path}
		vectors[row] = testSemanticStoredFineVectors(vector)
	}
	generation := &semanticGeneration{
		manifest: semanticManifest{
			Rows: rows,
			USearchFiles: []semanticUSearchShard{{
				Start: 0,
				Rows:  rows,
			}},
		},
		rows:    units,
		vectors: vectors,
		// A missing graph file fails the lazy shard check, as a damaged
		// graph does.
		usearch:           []string{filepath.Join(t.TempDir(), "damaged.usearch")},
		usearchValidation: make([]semanticUSearchValidation, 1),
	}

	hits, err := semanticUSearchFiltered(
		t.Context(),
		generation,
		testSemanticQuery(vector),
		keepPathPrefix("small/"),
		1,
	)
	if err != nil {
		t.Fatalf("exact route at %d rows: %v", semanticFilteredExactRows, err)
	}
	if len(hits) != 1 || hits[0].row != 0 {
		t.Fatalf("exact boundary hits=%+v", hits)
	}

	// The native route fails: recovery scores the allowed rows.
	hits, err = semanticUSearchFiltered(
		t.Context(),
		generation,
		testSemanticQuery(vector),
		dropPathPrefix("drop/"),
		1,
	)
	if err != nil || len(hits) != 1 || hits[0].row == semanticFilteredExactRows+1 {
		t.Fatalf("native route recovery hits=%+v error=%v, want one allowed exact hit", hits, err)
	}

	hits, err = semanticUSearchFiltered(
		t.Context(),
		generation,
		nil,
		keepPathPrefix("missing/"),
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
		dropPathPrefix("drop/"),
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
	// A search opens the generation that the descriptor binds this way.
	generation, err := plan.openJoinedSemanticGeneration(plan.folderSemanticSource(state))
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
	if !strings.Contains(output, "## "+filepath.Join(folder, "00-target.go")) ||
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
	embeddings := make([]semanticUnitEmbedding, semanticUSearchMaxShardRows*2)
	for row := range embeddings {
		embeddings[row] = testSemanticUnitEmbedding(semanticUSearchTestVector(row))
	}
	dir := b.TempDir()
	built := streamTestUSearchShards(b, dir, embeddings,
		fixedTestUSearchPlans(len(embeddings), semanticUSearchMaxShardRows), nil)
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
		limit   int
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
		{
			name:    "production-limit-medium-25-percent-32-tokens",
			percent: 25,
			tokens:  32,
			limit:   1024,
		},
	} {
		limit := test.limit
		if limit == 0 {
			limit = 10
		}
		rows := make([]semanticUnit, len(embeddings))
		allowedRows := make([]uint64, 0, len(rows)*test.percent/100+1)
		for row := range rows {
			prefix := "blocked"
			if row%100 < test.percent {
				prefix = "allowed"
				allowedRows = append(allowedRows, uint64(row))
			}
			rows[row] = semanticUnit{
				row:  uint64(row),
				path: fmt.Sprintf("%s/%04d.go", prefix, row),
			}
		}
		generation := &semanticGeneration{
			manifest: semanticManifest{
				Rows:         uint64(len(rows)),
				USearchFiles: built.shards,
			},
			rows:              rows,
			vectors:           testSemanticFineVectors(embeddings),
			usearch:           paths,
			usearchValidation: make([]semanticUSearchValidation, len(paths)),
		}
		keep := keepPathPrefix("allowed/")
		target := int(allowedRows[len(allowedRows)-1])
		prepared := testSemanticQuery(embeddings[target].fine[0])
		for token := 1; token < test.tokens; token++ {
			start := token * semanticEmbeddingDimensions
			copy(prepared.tokens[start:start+semanticEmbeddingDimensions], embeddings[target].fine[0][:])
			prepared.scoreMask[token] = true
		}
		benchmarkMask, err := buildSemanticFilterMask(b.Context(), generation, keep)
		if err != nil {
			b.Fatal(err)
		}
		plannedRoute, err := planSemanticUSearchShards(b.Context(), generation, &benchmarkMask)
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
			b.Context(), generation.vectors, prepared, allowedRows, noSemanticFloor,
		)
		if err != nil {
			b.Fatal(err)
		}
		exact = firstSemanticHits(exact, limit)
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
					return semanticUSearchFiltered(ctx, generation, prepared, keep, limit)
				},
			},
			{
				name:         "forced-native",
				nativeShards: forcedNativeShards,
				run: func(ctx context.Context) ([]semanticHit, error) {
					return benchmarkForcedNativeSemanticFilter(
						ctx, generation, prepared, keep, limit,
					)
				},
			},
			{
				name:      "forced-exact",
				exactRows: len(allowedRows),
				run: func(ctx context.Context) ([]semanticHit, error) {
					mask, err := buildSemanticFilterMask(ctx, generation, keep)
					if err != nil {
						return nil, err
					}
					hits, err := exactSemanticSortedCandidates(ctx, generation.vectors, prepared, mask.selectedRows(), noSemanticFloor)
					return firstSemanticHits(hits, limit), err
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
				b.ReportMetric(
					float64(recalled)/float64(len(exact)),
					fmt.Sprintf("top%d-recall", limit),
				)
				b.ReportMetric(float64(max(0, len(exact)-len(hits))), "underfill")
			})
		}
	}
}

func benchmarkForcedNativeSemanticFilter(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	keep func(path string) bool,
	limit int,
) ([]semanticHit, error) {
	mask, err := buildSemanticFilterMask(ctx, generation, keep)
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
	hits, err := exactSemanticSortedRows(ctx, generation.vectors, queryVectors, candidates.rows, noSemanticFloor)
	return firstSemanticHits(hits, limit), err
}

func BenchmarkPlanSemanticUSearchShardsScale(b *testing.B) {
	for _, shardCount := range []int{2, 50, 2_442} {
		b.Run(fmt.Sprintf("shards-%d", shardCount), func(b *testing.B) {
			shards := make([]semanticUSearchShard, shardCount)
			rows := uint64(0)
			for shard := range shards {
				shardRows := uint64(semanticUSearchMaxShardRows)
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
				plan, err = planSemanticUSearchShards(b.Context(), generation, &mask)
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

// TestSemanticUSearchSimilarityIsCosine builds one shard of keys near a
// random query and reads every key: the hit score, USearch's cosine of the
// int8 codes, estimates the f32 cosine with no scale bias, so a bound in
// cosine units means what it says.
func TestSemanticUSearchSimilarityIsCosine(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	if err := ensureSemanticUSearchRuntime(); err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewPCG(7, 8))
	near := func(query *semanticVector, alpha float64) semanticVector {
		var vector semanticVector
		for d := range vector {
			value := random.NormFloat64()
			if query != nil {
				value += alpha * float64(query[d]) * math.Sqrt(semanticEmbeddingDimensions)
			}
			vector[d] = float32(value)
		}
		if err := normalizeSemanticVector(&vector); err != nil {
			t.Fatal(err)
		}
		return vector
	}
	query := near(nil, 0)
	const rows = 1024
	coarse := make([][semanticCoarseCentroidsPerUnit]semanticVector, rows)
	var batches [][]semanticCoarseVectors
	for start := 0; start < rows; start += semanticModelBatchRows {
		batch := make([]semanticCoarseVectors, min(semanticModelBatchRows, rows-start))
		for offset := range batch {
			for centroid := range semanticCoarseCentroidsPerUnit {
				coarse[start+offset][centroid] = near(&query, random.Float64()*3)
				batch[offset][centroid] = coarse[start+offset][centroid]
			}
		}
		batches = append(batches, batch)
	}
	path := filepath.Join(t.TempDir(), semanticUSearchShardName(0))
	if err := buildSemanticUSearchShard(t.Context(), path, semanticCoarseSet{batches: batches, rows: rows}); err != nil {
		t.Fatal(err)
	}
	shard := semanticUSearchShard{Name: semanticUSearchShardName(0), Start: 0, Rows: rows}
	saved := semanticUSearchDeepenSimilarity
	semanticUSearchDeepenSimilarity = -10
	all, saturated, err := searchSemanticUSearchShardTokens(t.Context(), path, shard, []semanticVector{query}, rows*semanticCoarseCentroidsPerUnit, nil, false)
	semanticUSearchDeepenSimilarity = saved
	if err != nil || saturated {
		t.Fatalf("search of every key saturated=%t error=%v", saturated, err)
	}
	var count int
	var bias, worst float64
	for _, hit := range all[0] {
		row, centroid := semanticUSearchUnitAndCentroid(hit.row)
		var cosine float64
		for d := range query {
			cosine += float64(query[d]) * float64(coarse[row][centroid][d])
		}
		if cosine < 0.5 {
			continue
		}
		diff := float64(hit.score) - cosine
		count++
		bias += diff
		worst = max(worst, math.Abs(diff))
	}
	if count < 500 {
		t.Fatalf("only %d keys with a cosine of at least 0.5", count)
	}
	bias /= float64(count)
	t.Logf("estimate - cosine over %d keys: mean %+.4f, max %.4f", count, bias, worst)
	if math.Abs(bias) > 0.005 || worst > 0.03 {
		t.Fatalf("estimate - cosine over %d keys: mean %+.4f, max %.4f; want |mean| <= 0.005, max <= 0.03", count, bias, worst)
	}
}

// TestSemanticUSearchZeroDotIsOrthogonal builds one shard in which the keys of
// three rows in four have a zero int8 dot product with the query. USearch
// 2.26.4 and later give such a key the cosine 0. Earlier versions give it the
// distance 0, so each such key passes the bound as a cosine of 1.
func TestSemanticUSearchZeroDotIsOrthogonal(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	if err := ensureSemanticUSearchRuntime(); err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewPCG(9, 10))
	query := semanticVector{0: 1}
	const rows = 4 * semanticModelBatchRows
	var batches [][]semanticCoarseVectors
	for start := 0; start < rows; start += semanticModelBatchRows {
		batch := make([]semanticCoarseVectors, semanticModelBatchRows)
		for offset := range batch {
			for centroid := range semanticCoarseCentroidsPerUnit {
				vector := &batch[offset][centroid]
				for d := range vector {
					vector[d] = float32(random.NormFloat64())
				}
				// Row 1 of every four points mostly along the query axis; the
				// other rows have no component on it.
				vector[0] = 0
				if (start+offset)%4 == 1 {
					vector[0] = 20
				}
				if err := normalizeSemanticVector(vector); err != nil {
					t.Fatal(err)
				}
			}
		}
		batches = append(batches, batch)
	}
	path := filepath.Join(t.TempDir(), semanticUSearchShardName(0))
	if err := buildSemanticUSearchShard(t.Context(), path, semanticCoarseSet{batches: batches, rows: rows}); err != nil {
		t.Fatal(err)
	}
	shard := semanticUSearchShard{Name: semanticUSearchShardName(0), Start: 0, Rows: rows}
	saved := semanticUSearchDeepenSimilarity
	semanticUSearchDeepenSimilarity = -10
	all, saturated, err := searchSemanticUSearchShardTokens(t.Context(), path, shard, []semanticVector{query}, rows*semanticCoarseCentroidsPerUnit, nil, false)
	semanticUSearchDeepenSimilarity = saved
	if err != nil || saturated {
		t.Fatalf("search of every key saturated=%t error=%v", saturated, err)
	}
	if len(all[0]) != rows*semanticCoarseCentroidsPerUnit {
		t.Fatalf("read %d keys, want %d", len(all[0]), rows*semanticCoarseCentroidsPerUnit)
	}
	for _, hit := range all[0] {
		row, _ := semanticUSearchUnitAndCentroid(hit.row)
		if row%4 != 1 && math.Abs(float64(hit.score)) > 1e-6 {
			t.Fatalf("key %d has a zero int8 dot product and the cosine %.4f, want 0", hit.row, hit.score)
		}
		if row%4 == 1 && hit.score < 0.9 {
			t.Fatalf("key %d near the query has the cosine %.4f, want at least 0.9", hit.row, hit.score)
		}
	}
	kept, saturated, err := searchSemanticUSearchShardTokens(t.Context(), path, shard, []semanticVector{query}, semanticUSearchVectorsPerQuery, nil, false)
	if err != nil || saturated {
		t.Fatalf("search at the bound saturated=%t error=%v", saturated, err)
	}
	for _, hit := range kept[0] {
		if row, _ := semanticUSearchUnitAndCentroid(hit.row); row%4 != 1 {
			t.Fatalf("the bound kept key %d of orthogonal row %d with the cosine %.4f", hit.row, row, hit.score)
		}
	}
	if len(kept[0]) != rows/4*semanticCoarseCentroidsPerUnit {
		t.Fatalf("the bound kept %d keys, want the %d keys near the query", len(kept[0]), rows/4*semanticCoarseCentroidsPerUnit)
	}
}

// searchSemanticUSearchCandidates searches each query token in every shard in
// parallel, validates returned vector keys, and returns the union of unit rows.
// It does not compute the final LateOn MaxSim score.
func searchSemanticUSearchCandidates(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
) ([]uint64, error) {
	result, err := searchSemanticUSearchCandidatesWithMask(ctx, generation, query, nil)
	return result.rows, err
}

func searchSemanticUSearchFilteredCandidates(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	mask *semanticFilterMask,
) ([]uint64, error) {
	if mask == nil || mask.mode != semanticFilterPartial {
		return nil, fmt.Errorf("partial semantic filter mask is missing")
	}
	result, err := searchSemanticUSearchCandidatesWithMask(ctx, generation, query, mask)
	return result.rows, err
}

// semanticUSearchFiltered searches the rows of the files that keep allows, as
// scanNode does with the files that Zoekt selects: it builds the row mask,
// then calls semanticUSearchMasked with no floor. It gives at most limit of the
// best hits. A nil keep allows every row. No allowed row gives no hits.
func semanticUSearchFiltered(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	keep func(path string) bool,
	limit int,
) ([]semanticHit, error) {
	var mask *semanticFilterMask
	if keep != nil {
		built, err := buildSemanticFilterMask(ctx, generation, keep)
		if err != nil {
			return nil, err
		}
		mask = &built
	}
	hits, err := semanticUSearchMasked(ctx, generation, query, mask, noSemanticFloor)
	return firstSemanticHits(hits, limit), err
}

// keepPathPrefix allows the files whose path starts with prefix.
func keepPathPrefix(prefix string) func(path string) bool {
	return func(path string) bool { return strings.HasPrefix(path, prefix) }
}

// dropPathPrefix allows the files whose path does not start with prefix.
func dropPathPrefix(prefix string) func(path string) bool {
	return func(path string) bool { return !strings.HasPrefix(path, prefix) }
}
