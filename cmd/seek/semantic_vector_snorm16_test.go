package main

import (
	"math"
	"testing"
	"unsafe"
)

// semanticSNORM16MaximumDecodeError is the largest difference between a
// decoded SNORM16 component and code/32767, over every code. The round-trip
// test measures it again and requires this exact value.
const semanticSNORM16MaximumDecodeError = 2.980141289299354e-8

// semanticSourceSquaredNormBounds returns the smallest and the largest exact
// squared norm of a vector that validateNormalizedSemanticVector accepts. The
// check rounds each square to float32 (u32) and sums the 48 squares in
// float64 (gamma47). It accepts a sum within
// semanticNormalizedSquaredNormTolerance of 1. A square can also underflow to
// 0 (underflow).
func semanticSourceSquaredNormBounds() (float64, float64) {
	u32 := math.Ldexp(1, -24)
	u64 := math.Ldexp(1, -53)
	gamma47 := 47 * u64 / (1 - 47*u64)
	underflow := semanticEmbeddingDimensions * math.Ldexp(1, -150)
	lower := (1-semanticNormalizedSquaredNormTolerance)/(1+gamma47) - underflow
	lower /= 1 + u32
	upper := (1+semanticNormalizedSquaredNormTolerance)/(1-gamma47) + underflow
	upper /= 1 - u32
	return lower, upper
}

func TestSemanticSNORM16ComponentCodes(t *testing.T) {
	negativeZero := math.Float32frombits(1 << 31)
	tests := []struct {
		name  string
		value float32
		want  int16
	}{
		{name: "negative one", value: -1, want: -32767},
		{name: "negative half tie", value: -0.5, want: -16384},
		{name: "negative zero", value: negativeZero, want: 0},
		{name: "zero", value: 0, want: 0},
		{name: "positive half tie", value: 0.5, want: 16384},
		{name: "positive one", value: 1, want: 32767},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := encodeSemanticSNORM16Component(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("code=%d, want %d", got, test.want)
			}
		})
	}
	for _, value := range []float32{
		math.Nextafter32(-1, float32(math.Inf(-1))),
		math.Nextafter32(1, float32(math.Inf(1))),
		float32(math.NaN()),
		float32(math.Inf(-1)),
		float32(math.Inf(1)),
	} {
		if _, err := encodeSemanticSNORM16Component(value); err == nil {
			t.Fatalf("invalid value %v was accepted", value)
		}
	}
}

// TestSemanticSNORM16ComponentRoundTripAllCodes decodes every valid code with
// the production decoder and encodes the result again. The float64 quotient
// code/32767 is the exact value that the decoder must round to float32.
func TestSemanticSNORM16ComponentRoundTripAllCodes(t *testing.T) {
	var maximumDecodeError float64
	for value := -32_767; value <= 32_767; value++ {
		code := int16(value)
		decoded := decodeSemanticSNORM16Vector(semanticSNORM16Vector{0: code})[0]
		got, err := encodeSemanticSNORM16Component(decoded)
		if err != nil {
			t.Fatalf("code %d: %v", code, err)
		}
		if got != code {
			t.Fatalf("code %d round-tripped as %d", code, got)
		}
		decodeError := math.Abs(float64(decoded) - float64(code)/32_767)
		maximumDecodeError = max(maximumDecodeError, decodeError)
	}
	if maximumDecodeError != semanticSNORM16MaximumDecodeError {
		t.Fatalf(
			"maximum decoder error=%g, want %g",
			maximumDecodeError,
			semanticSNORM16MaximumDecodeError,
		)
	}
}

func TestSemanticSNORM16StorageErrorBound(t *testing.T) {
	const componentBound = 0.5/semanticSNORM16Scale + semanticSNORM16MaximumDecodeError
	vectorBound := math.Sqrt(semanticEmbeddingDimensions) * componentBound
	for seed := 1; seed <= 256; seed++ {
		var vector semanticVector
		for dimension := range vector {
			vector[dimension] = float32((seed*(dimension+3))%101 - 50)
		}
		if err := normalizeSemanticVector(&vector); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		stored, err := encodeSemanticSNORM16Vector(&vector)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if err := validateSemanticSNORM16Vector(&stored); err != nil {
			t.Fatalf("seed %d stored vector: %v", seed, err)
		}
		decoded := decodeSemanticSNORM16Vector(stored)
		var squaredError float64
		for dimension := range vector {
			componentError := math.Abs(float64(decoded[dimension]) - float64(vector[dimension]))
			if componentError > componentBound {
				t.Fatalf("seed %d dimension %d error=%g, bound=%g", seed, dimension, componentError, componentBound)
			}
			squaredError += componentError * componentError
		}
		if vectorError := math.Sqrt(squaredError); vectorError > vectorBound {
			t.Fatalf("seed %d vector error=%g, bound=%g", seed, vectorError, vectorBound)
		}
	}
}

func TestSemanticSNORM16IntegerNormThreshold(t *testing.T) {
	componentError := 0.5 / float64(semanticSNORM16Scale)
	vectorError := math.Sqrt(semanticEmbeddingDimensions) * componentError
	lowerSourceNorm, upperSourceNorm := semanticSourceSquaredNormBounds()
	upperDecodedError := upperSourceNorm - 1 +
		2*math.Sqrt(upperSourceNorm)*vectorError + vectorError*vectorError
	lowerDecodedError := 1 -
		math.Pow(max(0, math.Sqrt(lowerSourceNorm)-vectorError), 2)
	maximumDecodedError := max(lowerDecodedError, upperDecodedError)
	threshold := float64(semanticSNORM16SquaredNormCodeTolerance) /
		float64(semanticSNORM16SquaredScale)
	if threshold < maximumDecodedError {
		t.Fatalf(
			"stored norm threshold=%g, need at least %g",
			threshold,
			maximumDecodedError,
		)
	}
	// The stored format accepts squared-code sums within 1,301,295 of
	// 32767^2 = 1,073,676,289: floor(0.001212 * 32767^2).
	for _, squaredNorm := range []int64{
		1_072_374_994,
		1_073_676_289,
		1_074_977_584,
	} {
		if !validSemanticSNORM16SquaredNorm(squaredNorm) {
			t.Fatalf("boundary sum %d was rejected", squaredNorm)
		}
	}
	for _, squaredNorm := range []int64{
		1_072_374_993,
		1_074_977_585,
	} {
		if validSemanticSNORM16SquaredNorm(squaredNorm) {
			t.Fatalf("out-of-range sum %d was accepted", squaredNorm)
		}
	}
}

func TestValidateSemanticSNORM16VectorRejectsInvalidCodesAndNorm(t *testing.T) {
	source := semanticVector{0: 1}
	valid, err := encodeSemanticSNORM16Vector(&source)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSemanticSNORM16Vector(&valid); err != nil {
		t.Fatal(err)
	}
	reserved := valid
	reserved[0] = math.MinInt16
	if err := validateSemanticSNORM16Vector(&reserved); err == nil {
		t.Fatal("reserved -32768 code was accepted")
	}
	zero := semanticSNORM16Vector{}
	if err := validateSemanticSNORM16Vector(&zero); err == nil {
		t.Fatal("zero-norm stored vector was accepted")
	}
	tooWide := valid
	tooWide[1] = 1_143
	if err := validateSemanticSNORM16Vector(&tooWide); err == nil {
		t.Fatal("stored vector outside the integer norm threshold was accepted")
	}
}

func TestSemanticSNORM16TypeSizes(t *testing.T) {
	if got, want := unsafe.Sizeof(semanticSNORM16Vector{}), uintptr(semanticEmbeddingDimensions*2); got != want {
		t.Fatalf("vector bytes=%d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(semanticCoarseSNORM16Vectors{}), uintptr(semanticCoarseVectorBytesPerUnit); got != want {
		t.Fatalf("row bytes=%d, want %d", got, want)
	}
}

func BenchmarkSemanticVectorDot(b *testing.B) {
	query := semanticVector{0: 0.6, 1: 0.8}
	document := semanticVector{0: -0.8, 1: 0.6}
	b.Run("float32", func(b *testing.B) {
		for b.Loop() {
			_ = semanticVectorDot(query, document)
		}
	})
}
