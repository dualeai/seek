package main

import (
	"math"
	"reflect"
	"testing"
)

func TestLateOnSemanticCentroidsAreStableNormalizedAndAnchored(t *testing.T) {
	mask := make([]bool, lateOnSequenceLength)
	embeddings := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	for token := range 7 {
		mask[token] = true
		embeddings[token*semanticEmbeddingDimensions+token%semanticEmbeddingDimensions] = float32(token + 1)
	}
	var firstWorkspace semanticCentroidWorkspace
	first, err := firstWorkspace.makeUnitEmbedding(embeddings, mask, 0)
	if err != nil {
		t.Fatal(err)
	}
	var secondWorkspace semanticCentroidWorkspace
	second, err := secondWorkspace.makeUnitEmbedding(embeddings, mask, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same semantic tokens produced different centroids")
	}
	if first.coarse[0][0] != 1 || first.fine[0][0] != 1 {
		t.Fatal("first scoreable token did not remain the fixed centroid")
	}
	for _, vectors := range [][]semanticVector{first.coarse[:], first.fine[:]} {
		for index, vector := range vectors {
			var norm float64
			for _, value := range vector {
				norm += float64(value) * float64(value)
			}
			if math.Abs(norm-1) > 1e-5 {
				t.Fatalf("centroid %d norm=%f, want 1", index, norm)
			}
		}
	}
}

func TestLateOnSemanticCentroidsMatchHandCalculatedOrthonormalCase(t *testing.T) {
	mask := make([]bool, lateOnSequenceLength)
	embeddings := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	wantFine := make([]semanticVector, 5)
	for token := range 5 {
		mask[token] = true
		embeddings[token*semanticEmbeddingDimensions+token] = 1
		wantFine[token][token] = 1
	}

	var workspace semanticCentroidWorkspace
	got, err := workspace.makeUnitEmbedding(embeddings, mask, 0)
	if err != nil {
		t.Fatal(err)
	}
	for centroid, want := range wantFine {
		assertSemanticVectorClose(t, got.fine[centroid], want)
	}
	assertSemanticVectorClose(t, got.fine[5], wantFine[0])
	assertSemanticVectorClose(t, got.fine[19], wantFine[4])

	wantCoarse := [semanticCoarseCentroidsPerUnit]semanticVector{
		wantFine[0],
		{1: float32(1 / math.Sqrt2), 4: float32(1 / math.Sqrt2)},
		wantFine[2],
		wantFine[3],
	}
	for centroid, want := range wantCoarse {
		assertSemanticVectorClose(t, got.coarse[centroid], want)
	}
}

func assertSemanticVectorClose(t *testing.T, got, want semanticVector) {
	t.Helper()
	for dimension := range semanticEmbeddingDimensions {
		if math.Abs(float64(got[dimension]-want[dimension])) > 1e-6 {
			t.Fatalf(
				"dimension %d=%g, want %g",
				dimension,
				got[dimension],
				want[dimension],
			)
		}
	}
}

func TestLateOnSemanticCentroidsRejectInvalidRows(t *testing.T) {
	var workspace semanticCentroidWorkspace
	if _, err := workspace.makeUnitEmbedding(nil, nil, 0); err == nil {
		t.Fatal("empty semantic row must fail")
	}
	mask := make([]bool, lateOnSequenceLength)
	mask[0] = true
	embeddings := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	embeddings[0] = float32(math.NaN())
	if _, err := workspace.makeUnitEmbedding(embeddings, mask, 0); err == nil {
		t.Fatal("non-finite semantic row must fail")
	}
}

func TestNormalizeSemanticVectorHandlesFiniteExtremeValues(t *testing.T) {
	t.Run("smallest subnormal", func(t *testing.T) {
		vector := semanticVector{0: math.SmallestNonzeroFloat32}
		if err := normalizeSemanticVector(&vector); err != nil {
			t.Fatal(err)
		}
		if vector[0] != 1 {
			t.Fatalf("first component=%g, want 1", vector[0])
		}
		for dimension, value := range vector[1:] {
			if value != 0 {
				t.Fatalf("component %d=%g, want 0", dimension+1, value)
			}
		}
	})

	t.Run("largest finite", func(t *testing.T) {
		vector := semanticVector{0: math.MaxFloat32, 1: math.MaxFloat32}
		if err := normalizeSemanticVector(&vector); err != nil {
			t.Fatal(err)
		}
		if vector[0] != vector[1] || vector[0] <= 0 ||
			math.Abs(float64(vector[0])-1/math.Sqrt2) > 1e-6 {
			t.Fatalf("first components=%v, want equal positive values near 1/sqrt(2)", vector[:2])
		}
		for dimension, value := range vector[2:] {
			if value != 0 {
				t.Fatalf("component %d=%g, want 0", dimension+2, value)
			}
		}
	})
}

func TestNormalizeSemanticVectorRejectsInvalidInput(t *testing.T) {
	for _, vector := range []semanticVector{
		{},
		{0: float32(math.NaN())},
		{0: float32(math.Inf(-1))},
		{0: float32(math.Inf(1))},
	} {
		if err := normalizeSemanticVector(&vector); err == nil {
			t.Fatalf("invalid vector %v was accepted", vector[:1])
		}
	}
}

func TestNormalizeSemanticVectorPreservesOrdinaryFloat32Result(t *testing.T) {
	vector := semanticVector{0: 0.3, 1: -0.4, 2: 0.5}
	want := semanticVector{
		0: math.Float32frombits(0x3ed93924),
		1: math.Float32frombits(0xbf10d0c3),
		2: math.Float32frombits(0x3f3504f3),
	}
	if err := normalizeSemanticVector(&vector); err != nil {
		t.Fatal(err)
	}
	if vector != want {
		t.Fatalf("normalized bytes changed: got %v, want %v", vector[:3], want[:3])
	}
}

func BenchmarkLateOnSemanticUnitEmbedding(b *testing.B) {
	mask := make([]bool, lateOnSequenceLength)
	embeddings := make([]float32, lateOnSequenceLength*semanticEmbeddingDimensions)
	for token := range lateOnSequenceLength {
		mask[token] = true
		for dimension := range semanticEmbeddingDimensions {
			embeddings[token*semanticEmbeddingDimensions+dimension] = float32((token+dimension)%17 + 1)
		}
	}
	var workspace semanticCentroidWorkspace
	b.ReportAllocs()
	for b.Loop() {
		if _, err := workspace.makeUnitEmbedding(embeddings, mask, 0); err != nil {
			b.Fatal(err)
		}
	}
}
