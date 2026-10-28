//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func testSemanticRandomUnit(random *rand.Rand) semanticVector {
	var vector semanticVector
	for dimension := range vector {
		vector[dimension] = float32(random.NormFloat64())
	}
	if err := normalizeSemanticVector(&vector); err != nil {
		panic(err)
	}
	return vector
}

// testSemanticF64Centroids holds the centroids of one row as float64 values.
type testSemanticF64Centroids [semanticFineCentroidsPerUnit][semanticEmbeddingDimensions]float64

// testSemanticMaxSim gives the summed MaxSim of row for query in float64.
func testSemanticMaxSim(row *testSemanticF64Centroids, query [][semanticEmbeddingDimensions]float64) float64 {
	var score float64
	for token := range query {
		maximum := math.Inf(-1)
		for centroid := range row {
			var dot float64
			for dimension := range semanticEmbeddingDimensions {
				dot += query[token][dimension] * row[centroid][dimension]
			}
			maximum = max(maximum, dot)
		}
		score += maximum
	}
	return score
}

// testSemanticNumKongBackends gives every kernel family that Seek can use on
// the host.
func testSemanticNumKongBackends(t *testing.T) []int {
	t.Helper()
	backends, err := semanticNumKongRuntime()
	if err != nil {
		t.Fatal(err)
	}
	return backends
}

// semanticNumKongFamilySpeed ranks each kernel family that Seek can use, from
// the slowest. Serial is the slowest. Then come the base family of the
// architecture and the family with more CPU features.
var semanticNumKongFamilySpeed = map[string]int{
	"serial": 0,
	"neon":   1, "neonfhm": 2,
	"haswell": 1, "skylake": 2,
}

// TestSemanticNumKongBestBackendIsTheFastestSupported checks the families that
// the host supports against semanticNumKongFamilySpeed. The list starts with
// serial and goes from the slowest to the fastest family. The best backend is
// the fastest family of the list.
func TestSemanticNumKongBestBackendIsTheFastestSupported(t *testing.T) {
	backends := testSemanticNumKongBackends(t)
	names := make([]string, len(backends))
	for index, backend := range backends {
		names[index] = semanticNumKongBackendName(backend)
	}
	if len(backends) == 0 || backends[0] != 0 || names[0] != "serial" {
		t.Fatalf("supported families=%v, want serial first", names)
	}
	fastest := names[0]
	for index, name := range names {
		rank, known := semanticNumKongFamilySpeed[name]
		if !known {
			t.Fatalf("supported families=%v: %q has no speed rank", names, name)
		}
		if index > 0 && rank <= semanticNumKongFamilySpeed[names[index-1]] {
			t.Fatalf("supported families=%v, want the slowest first", names)
		}
		if rank > semanticNumKongFamilySpeed[fastest] {
			fastest = name
		}
	}
	best, err := semanticNumKongBackend()
	if err != nil || semanticNumKongBackendName(best) != fastest {
		t.Fatalf("best backend=%q err=%v, want the fastest supported family %q of %v",
			semanticNumKongBackendName(best), err, fastest, names)
	}
	// Every arm64 CPU that Seek supports has NEON.
	if runtime.GOARCH == "arm64" && !slices.Contains(names, "neon") {
		t.Fatalf("supported families=%v, want neon on arm64", names)
	}
	t.Logf("NumKong families: %v, best %s", names, fastest)
}

// TestSemanticNumKongAllowedCapabilitiesNeedEveryTarget gives the gate fake
// CPU features: a family stays only when every compile target of its kernels
// is present.
func TestSemanticNumKongAllowedCapabilitiesNeedEveryTarget(t *testing.T) {
	haswell := semanticNumKongFeatures{avx2: true, fma: true, f16c: true, bmi1: true, bmi2: true}
	skylake := haswell
	skylake.avx512f, skylake.avx512bw, skylake.avx512vl, skylake.avx512dq = true, true, true, true
	serial, neon, fhm := semanticNumKongCapSerial, semanticNumKongCapNeon, semanticNumKongCapNeonFHM
	tests := []struct {
		name     string
		features semanticNumKongFeatures
		want     uint64
	}{
		{"no feature", semanticNumKongFeatures{}, serial},
		{"arm64", semanticNumKongFeatures{arm64: true}, serial | neon},
		{"arm64 with FHM", semanticNumKongFeatures{arm64: true, fhm: true}, serial | neon | fhm},
		{"haswell", haswell, serial | semanticNumKongCapHaswell},
		{"skylake", skylake, serial | semanticNumKongCapHaswell | semanticNumKongCapSkylake},
	}
	for _, drop := range []string{"avx2", "fma", "f16c", "bmi1", "bmi2"} {
		features := skylake
		switch drop {
		case "avx2":
			features.avx2 = false
		case "fma":
			features.fma = false
		case "f16c":
			features.f16c = false
		case "bmi1":
			features.bmi1 = false
		case "bmi2":
			features.bmi2 = false
		}
		tests = append(tests, struct {
			name     string
			features semanticNumKongFeatures
			want     uint64
		}{"skylake without " + drop, features, serial})
	}
	for _, drop := range []string{"avx512f", "avx512bw", "avx512vl", "avx512dq"} {
		features := skylake
		switch drop {
		case "avx512f":
			features.avx512f = false
		case "avx512bw":
			features.avx512bw = false
		case "avx512vl":
			features.avx512vl = false
		case "avx512dq":
			features.avx512dq = false
		}
		tests = append(tests, struct {
			name     string
			features semanticNumKongFeatures
			want     uint64
		}{"skylake without " + drop, features, serial | semanticNumKongCapHaswell})
	}
	for _, test := range tests {
		if got := semanticNumKongAllowedCapabilities(test.features); got != test.want {
			t.Errorf("%s: allowed=%#x, want %#x", test.name, got, test.want)
		}
	}
}

// TestSemanticFineKernelMatchesFloat64Oracle scores random rows with every
// kernel family that the host can run, in whole ranges and in rows with gaps.
// It compares each score with a float64 oracle of the stored codes. The oracle
// and the float32 source score do not change with the family or the row set.
// So the test computes them once for each row and query.
func TestSemanticFineKernelMatchesFloat64Oracle(t *testing.T) {
	random := rand.New(rand.NewPCG(21, 22))
	const rowCount = 700
	vectors := make([]semanticFineF16Vectors, rowCount)
	// The binary16 codes, decoded by the IEEE 754 formula, and the float32
	// source of each code.
	decoded := make([]testSemanticF64Centroids, rowCount)
	source := make([]testSemanticF64Centroids, rowCount)
	for row := range vectors {
		for centroid := range vectors[row] {
			vector := testSemanticRandomUnit(random)
			stored, err := encodeSemanticF16Vector(&vector)
			if err != nil {
				t.Fatal(err)
			}
			vectors[row][centroid] = stored
			for dimension := range semanticEmbeddingDimensions {
				decoded[row][centroid][dimension] = semanticF16OracleValue(stored[dimension])
				source[row][centroid][dimension] = float64(vector[dimension])
			}
		}
	}
	var gapped []uint64
	for row := uint64(0); row < rowCount; row += 3 {
		gapped = append(gapped, row)
	}
	u := math.Ldexp(1, -24)
	ctx := context.Background()
	backends := testSemanticNumKongBackends(t)
	for _, tokens := range []int{1, 2, 13, 32, 128} {
		query := make([]semanticVector, tokens)
		decodedQuery := make([][semanticEmbeddingDimensions]float64, tokens)
		sourceQuery := make([][semanticEmbeddingDimensions]float64, tokens)
		for token := range query {
			query[token] = testSemanticRandomUnit(random)
			stored, err := encodeSemanticF16Vector(&query[token])
			if err != nil {
				t.Fatal(err)
			}
			for dimension := range semanticEmbeddingDimensions {
				decodedQuery[token][dimension] = semanticF16OracleValue(stored[dimension])
				sourceQuery[token][dimension] = float64(query[token][dimension])
			}
		}
		oracle := make([]float64, rowCount)
		sourceScore := make([]float64, rowCount)
		for row := range rowCount {
			oracle[row] = testSemanticMaxSim(&decoded[row], decodedQuery)
			sourceScore[row] = testSemanticMaxSim(&source[row], sourceQuery)
		}
		// Each dot sums 48 exact float32 products in float32, and the
		// score sums the token maxima in float32. Both vectors have a
		// norm of at most sqrt(1.002).
		dotBound := 47 * u / (1 - 47*u) * 1.002
		sumBound := float64(tokens) * u / (1 - float64(tokens)*u) * float64(tokens) * 1.002
		bound := float64(tokens)*dotBound + sumBound
		// The binary16 rounding moves each component by at most 2^-11
		// of its value, so a dot moves by at most about 2^-10.
		codecBound := float64(tokens) * (2*math.Ldexp(1, -11)*1.0011 + math.Ldexp(1, -22))
		for _, backend := range backends {
			t.Run(fmt.Sprintf("%s/tokens-%d", semanticNumKongBackendName(backend), tokens), func(t *testing.T) {
				packed, err := prepareSemanticFineQueryFor(backend, query)
				if err != nil {
					t.Fatal(err)
				}
				var worstCodec float64
				for _, rows := range [][]uint64{nil, gapped} {
					count := rowCount
					if rows != nil {
						count = len(rows)
					}
					hits, err := exactSemanticRange(ctx, vectors, &packed, rows, 0, count, float32(math.Inf(-1)))
					if err != nil {
						t.Fatal(err)
					}
					if len(hits) != count {
						t.Fatalf("hits=%d, want %d", len(hits), count)
					}
					for index, hit := range hits {
						want := uint64(index)
						if rows != nil {
							want = rows[index]
						}
						if hit.row != want {
							t.Fatalf("hit %d row=%d, want %d", index, hit.row, want)
						}
						if difference := math.Abs(float64(hit.score) - oracle[hit.row]); difference > bound {
							t.Fatalf("row %d score=%.9g oracle=%.9g difference=%g bound=%g",
								hit.row, hit.score, oracle[hit.row], difference, bound)
						}
						worstCodec = max(worstCodec, math.Abs(float64(hit.score)-sourceScore[hit.row]))
					}
				}
				if worstCodec > codecBound+bound {
					t.Fatalf("binary16 score change=%g, bound=%g", worstCodec, codecBound+bound)
				}
				t.Logf("largest score change against the float32 source: %.3g (mean MaxSim %.3g)",
					worstCodec, worstCodec/float64(tokens))
			})
		}
	}
}

// TestSemanticFineKernelRejectsDamagedCentroid damages one centroid of row 5
// and checks that every kernel family rejects the row, in a whole range and
// in rows with gaps. A valid vector near the norm bound stays accepted.
func TestSemanticFineKernelRejectsDamagedCentroid(t *testing.T) {
	random := rand.New(rand.NewPCG(23, 24))
	const rowCount = 40
	clean := make([]semanticFineF16Vectors, rowCount)
	for row := range clean {
		for centroid := range clean[row] {
			vector := testSemanticRandomUnit(random)
			stored, err := encodeSemanticF16Vector(&vector)
			if err != nil {
				t.Fatal(err)
			}
			clean[row][centroid] = stored
		}
	}
	var near semanticF16Vector
	for dimension := range 16 {
		near[dimension] = 0x3401
	}
	query := []semanticVector{testSemanticRandomUnit(random), testSemanticRandomUnit(random)}
	damages := map[string]func(*semanticF16Vector){
		"NaN":          func(vector *semanticF16Vector) { vector[9] = 0x7e00 },
		"infinity":     func(vector *semanticF16Vector) { vector[9] = 0xfc00 },
		"zero":         func(vector *semanticF16Vector) { *vector = semanticF16Vector{} },
		"large norm":   func(vector *semanticF16Vector) { vector[9] = semanticF16FromFloat32(0.2) },
		"largest code": func(vector *semanticF16Vector) { vector[9] = 0x7bff },
	}
	ctx := context.Background()
	for _, backend := range testSemanticNumKongBackends(t) {
		packed, err := prepareSemanticFineQueryFor(backend, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, rows := range [][]uint64{nil, {1, 3, 5, 8}} {
			count := rowCount
			if rows != nil {
				count = len(rows)
			}
			accepted := slices.Clone(clean)
			accepted[5][7] = near
			if _, err := exactSemanticRange(ctx, accepted, &packed, rows, 0, count, 0); err != nil {
				t.Fatalf("%s rows=%v: a valid vector near the bound was rejected: %v",
					semanticNumKongBackendName(backend), rows, err)
			}
			for name, damage := range damages {
				damaged := slices.Clone(clean)
				damage(&damaged[5][7])
				_, err := exactSemanticRange(ctx, damaged, &packed, rows, 0, count, 0)
				if err == nil || !strings.Contains(err.Error(), "semantic row 5 centroid 7:") {
					t.Fatalf("%s rows=%v %s: error=%v, want row 5 centroid 7",
						semanticNumKongBackendName(backend), rows, name, err)
				}
			}
		}
	}
}

func TestExactSemanticRangeRejectsRowsOutsideVectors(t *testing.T) {
	vectors := []semanticFineF16Vectors{testSemanticStoredFineVectors(semanticVector{0: 1})}
	packed, err := prepareSemanticFineQuery([]semanticVector{{0: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exactSemanticRange(t.Context(), vectors, &packed, []uint64{0, 1}, 0, 2, 0); err == nil {
		t.Fatal("a row outside the vectors was scored")
	}
	if _, err := exactSemanticRange(t.Context(), vectors, &packed, nil, 0, 2, 0); err == nil {
		t.Fatal("a range outside the vectors was scored")
	}
}

func TestPrepareSemanticFineQueryKeepsTheQuery(t *testing.T) {
	query := []semanticVector{{0: 0.6, 1: 0.8}}
	before := slices.Clone(query)
	if _, err := prepareSemanticFineQuery(query); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(query, before) {
		t.Fatal("query preparation changed its input")
	}
	if _, err := prepareSemanticFineQuery(nil); err == nil {
		t.Fatal("an empty query was accepted")
	}
	if _, err := prepareSemanticFineQuery([]semanticVector{{0: 2}}); err == nil {
		t.Fatal("a query token that is not normalized was accepted")
	}
}

// TestSemanticNumKongDotsF32MatchesFloat64Oracle compares the float32 dot
// products of every kernel family with float64 dot products.
func TestSemanticNumKongDotsF32MatchesFloat64Oracle(t *testing.T) {
	random := rand.New(rand.NewPCG(25, 26))
	const vectorCount, tokens = 300, 13
	vectors := make([]float32, vectorCount*semanticEmbeddingDimensions)
	for index := range vectors {
		vectors[index] = float32(random.NormFloat64())
	}
	query := make([]float32, tokens*semanticEmbeddingDimensions)
	for index := range query {
		query[index] = float32(random.NormFloat64())
	}
	for _, backend := range testSemanticNumKongBackends(t) {
		packed := packSemanticF32Query(backend, query)
		products := make([]float64, vectorCount*tokens)
		semanticNumKongDotsF32(vectors, &packed, products)
		for vector := range vectorCount {
			for token := range tokens {
				var oracle, magnitude float64
				for dimension := range semanticEmbeddingDimensions {
					product := float64(vectors[vector*semanticEmbeddingDimensions+dimension]) *
						float64(query[token*semanticEmbeddingDimensions+dimension])
					oracle += product
					magnitude += math.Abs(product)
				}
				got := products[vector*tokens+token]
				if difference := math.Abs(got - oracle); difference > 1e-12*magnitude {
					t.Fatalf("%s vector %d token %d: dot=%.17g oracle=%.17g",
						semanticNumKongBackendName(backend), vector, token, got, oracle)
				}
			}
		}
	}
}

// TestLateOnMaxSimPreparedMatchesFloat64Oracle compares the final model MaxSim
// with a float64 oracle that applies both masks.
func TestLateOnMaxSimPreparedMatchesFloat64Oracle(t *testing.T) {
	random := rand.New(rand.NewPCG(27, 28))
	const documents = 19
	query := make([]float32, 2*lateOnSequenceLength*semanticEmbeddingDimensions)
	queryMask := make([]bool, 2*lateOnSequenceLength)
	for token := range queryMask {
		queryMask[token] = random.IntN(9) == 0
		vector := testSemanticRandomUnit(random)
		copy(query[token*semanticEmbeddingDimensions:], vector[:])
	}
	vectors := make([]float32, documents*lateOnSequenceLength*semanticEmbeddingDimensions)
	masks := make([][]bool, documents)
	for document := range documents {
		masks[document] = make([]bool, lateOnSequenceLength)
		for token := range lateOnSequenceLength {
			masks[document][token] = random.IntN(4) != 0
			vector := testSemanticRandomUnit(random)
			copy(vectors[(document*lateOnSequenceLength+token)*semanticEmbeddingDimensions:], vector[:])
		}
	}
	scores, err := lateOnMaxSimPrepared(query, queryMask, vectors, masks)
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, keep := range queryMask {
		if keep {
			kept++
		}
	}
	u := math.Ldexp(1, -24)
	bound := float64(kept)*u + float64(kept)*u/(1-float64(kept)*u)*float64(kept)*1.001
	for document := range documents {
		var oracle float64
		for token, keep := range queryMask {
			if !keep {
				continue
			}
			maximum := math.Inf(-1)
			for documentToken, keepDocument := range masks[document] {
				if !keepDocument {
					continue
				}
				var dot float64
				for dimension := range semanticEmbeddingDimensions {
					dot += float64(query[token*semanticEmbeddingDimensions+dimension]) *
						float64(vectors[(document*lateOnSequenceLength+documentToken)*semanticEmbeddingDimensions+dimension])
				}
				maximum = max(maximum, dot)
			}
			oracle += maximum
		}
		if difference := math.Abs(float64(scores[document]) - oracle); difference > bound {
			t.Fatalf("document %d score=%.9g oracle=%.9g difference=%g bound=%g",
				document, scores[document], oracle, difference, bound)
		}
	}
}
