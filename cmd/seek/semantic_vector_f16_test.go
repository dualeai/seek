package main

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"testing"
	"unsafe"
)

// semanticF16One is the binary16 code of 1.
const semanticF16One = 0x3c00

// testSemanticGoldenEmbedding returns the row of
// TestSemanticF16LittleEndianGolden. Fine vector 0 is (-0.5, 0.5, sqrt(0.5)).
// Fine vector c, for c from 1 to 18, is 1 at dimension c. Fine vector 19 is -1
// at dimension 47. The coarse vectors copy the first fine vectors.
func testSemanticGoldenEmbedding() semanticUnitEmbedding {
	var embedding semanticUnitEmbedding
	embedding.fine[0] = semanticVector{0: -0.5, 1: 0.5, 2: float32(math.Sqrt(0.5))}
	for centroid := 1; centroid < 19; centroid++ {
		embedding.fine[centroid][centroid] = 1
	}
	embedding.fine[19][47] = -1
	copy(embedding.coarse[:], embedding.fine[:semanticCoarseCentroidsPerUnit])
	return embedding
}

// semanticF16OracleValue decodes a binary16 code from the IEEE 754 formula in
// float64. It shares no code with the production decoder.
func semanticF16OracleValue(code uint16) float64 {
	sign := 1.0
	if code&0x8000 != 0 {
		sign = -1
	}
	exponent := int(code>>10) & 0x1f
	fraction := float64(code & 0x3ff)
	switch exponent {
	case 0x1f:
		if fraction != 0 {
			return math.NaN()
		}
		return sign * math.Inf(1)
	case 0:
		return sign * math.Ldexp(fraction, -24)
	default:
		return sign * math.Ldexp(1+fraction/1024, exponent-15)
	}
}

// semanticF16OracleTable holds the value of every finite non-negative code, in
// increasing order.
var semanticF16OracleTable = sync.OnceValue(func() []float64 {
	table := make([]float64, 0x7c00)
	for code := range table {
		table[code] = semanticF16OracleValue(uint16(code))
	}
	return table
})

// semanticF16OracleNearest rounds value to binary16 by a search over every
// finite code: the nearest value wins, and a tie goes to the even code. A
// value at or above 65520 is an infinity.
func semanticF16OracleNearest(value float64) uint16 {
	var sign uint16
	if math.Signbit(value) {
		sign = 0x8000
		value = -value
	}
	if value >= 65520 {
		return sign | 0x7c00
	}
	table := semanticF16OracleTable()
	index := sort.SearchFloat64s(table, value)
	if index == len(table) {
		return sign | uint16(len(table)-1)
	}
	if table[index] == value {
		return sign | uint16(index)
	}
	low, high := index-1, index
	below, above := value-table[low], table[high]-value
	switch {
	case below < above:
		return sign | uint16(low)
	case above < below:
		return sign | uint16(high)
	case low%2 == 0:
		return sign | uint16(low)
	default:
		return sign | uint16(high)
	}
}

func TestSemanticF16DecodeMatchesOracleForEveryCode(t *testing.T) {
	for value := range 1 << 16 {
		code := uint16(value)
		got := float64(semanticF16ToFloat32(code))
		want := semanticF16OracleValue(code)
		if math.IsNaN(want) {
			if !math.IsNaN(got) {
				t.Fatalf("code %#04x decoded as %g, want NaN", code, got)
			}
			continue
		}
		if got != want || math.Signbit(got) != math.Signbit(want) {
			t.Fatalf("code %#04x decoded as %g, want %g", code, got, want)
		}
	}
}

// TestSemanticF16EncodeMatchesNearestOracle checks every binary16 value, every
// midpoint between two neighbors (a tie), the float32 values next to each
// midpoint, and random float32 values.
func TestSemanticF16EncodeMatchesNearestOracle(t *testing.T) {
	check := func(value float32) {
		t.Helper()
		if value != value {
			return
		}
		got := semanticF16FromFloat32(value)
		want := semanticF16OracleNearest(float64(value))
		if got != want {
			t.Fatalf("value %g (%#08x) encoded as %#04x, want %#04x",
				value, math.Float32bits(value), got, want)
		}
	}
	table := semanticF16OracleTable()
	for code, exact := range table {
		next := 65536.0
		if code+1 < len(table) {
			next = table[code+1]
		}
		midpoint := float32((exact + next) / 2)
		for _, value := range []float32{
			float32(exact),
			midpoint,
			math.Nextafter32(midpoint, 0),
			math.Nextafter32(midpoint, float32(math.Inf(1))),
		} {
			check(value)
			check(-value)
		}
	}
	random := rand.New(rand.NewPCG(11, 12))
	for range 1 << 20 {
		check(math.Float32frombits(random.Uint32()) / 1e30)
		check(float32(random.Float64()*2 - 1))
		check(math.Float32frombits(random.Uint32() & 0xc7ff_ffff))
	}
	for _, value := range []float32{
		0, math.Float32frombits(1 << 31), float32(math.Inf(1)), float32(math.Inf(-1)),
		math.MaxFloat32, -math.MaxFloat32, math.SmallestNonzeroFloat32, 65504, 65519.996, 65520,
	} {
		check(value)
	}
	if code := semanticF16FromFloat32(float32(math.NaN())); code&0x7c00 != 0x7c00 || code&0x3ff == 0 {
		t.Fatalf("NaN encoded as %#04x, want a NaN code", code)
	}
}

func TestEncodeSemanticFineVectorsLayout(t *testing.T) {
	embedding := testSemanticUnitEmbedding(semanticVector{0: 1})
	buffer := make([]byte, semanticFineVectorBytesPerUnit+16)
	for index := range buffer {
		buffer[index] = 0xa5
	}
	if err := encodeSemanticFineVectors(buffer, &embedding.fine, 7); err != nil {
		t.Fatal(err)
	}
	encoded := buffer[:semanticFineVectorBytesPerUnit]
	const vectorBytes = semanticEmbeddingDimensions * 2
	for centroid := range semanticFineCentroidsPerUnit {
		at := centroid * vectorBytes
		if got := binary.LittleEndian.Uint16(encoded[at : at+2]); got != semanticF16One {
			t.Fatalf("centroid %d first code=%#04x, want %#04x", centroid, got, semanticF16One)
		}
		for dimension := 1; dimension < semanticEmbeddingDimensions; dimension++ {
			componentAt := at + dimension*2
			if got := binary.LittleEndian.Uint16(encoded[componentAt : componentAt+2]); got != 0 {
				t.Fatalf("centroid %d dimension %d code=%#04x, want 0", centroid, dimension, got)
			}
		}
	}
	for index, value := range buffer[semanticFineVectorBytesPerUnit:] {
		if value != 0xa5 {
			t.Fatalf("byte after payload %d changed", index)
		}
	}
	invalid := testSemanticUnitEmbedding(semanticVector{0: 1, 1: 1})
	if err := encodeSemanticFineVectors(buffer, &invalid.fine, 0); err == nil {
		t.Fatal("a vector that is not normalized was accepted")
	}
}

// TestSemanticF16LittleEndianGolden checks every byte of one encoded row:
// -0.5, 0.5, sqrt(0.5), 1, and -1 at their places, and zero elsewhere.
func TestSemanticF16LittleEndianGolden(t *testing.T) {
	const rowBytes = 1_920
	if semanticFineVectorBytesPerUnit != rowBytes {
		t.Fatalf("encoded row has %d bytes, want %d", semanticFineVectorBytesPerUnit, rowBytes)
	}
	encoded := make([]byte, rowBytes)
	golden := testSemanticGoldenEmbedding()
	if err := encodeSemanticFineVectors(encoded, &golden.fine, 0); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, rowBytes)
	for at, code := range map[int]uint16{
		0: 0xb800, 2: 0x3800, 4: 0x39a8, 98: semanticF16One, 1_764: semanticF16One, 1_918: 0xbc00,
	} {
		binary.LittleEndian.PutUint16(want[at:], code)
	}
	for centroid := 1; centroid < 19; centroid++ {
		binary.LittleEndian.PutUint16(want[(centroid*semanticEmbeddingDimensions+centroid)*2:], semanticF16One)
	}
	for at := range want {
		if encoded[at] != want[at] {
			t.Fatalf("byte %d=%#02x, want %#02x", at, encoded[at], want[at])
		}
	}
}

// TestSemanticF16NormToleranceCoversRounding derives the largest squared-norm
// error of a stored vector that the encoder accepts, and checks that the
// tolerance covers it. A vector close to that bound must pass.
func TestSemanticF16NormToleranceCoversRounding(t *testing.T) {
	lowerSource, upperSource := semanticSourceSquaredNormBounds()
	relative := math.Ldexp(1, -11)
	// A subnormal component has an absolute error of at most 2^-25.
	subnormal := semanticEmbeddingDimensions * (2*math.Ldexp(1, -14)*math.Ldexp(1, -25) + math.Ldexp(1, -50))
	storedUpper := upperSource*(1+relative)*(1+relative) + subnormal
	storedLower := lowerSource*(1-relative)*(1-relative) - subnormal
	// The kernel check sums 48 exact float32 products in float32.
	u := math.Ldexp(1, -24)
	sumError := 47 * u / (1 - 47*u)
	need := max(storedUpper*(1+sumError)-1, 1-storedLower*(1-sumError))
	if semanticF16SquaredNormTolerance < need {
		t.Fatalf("stored norm tolerance=%g, need at least %g", semanticF16SquaredNormTolerance, need)
	}
	if need > 0.001982 {
		t.Fatalf("derived bound=%g, the codec comment states 0.001982", need)
	}

	// 16 components of 0.25 * (1 + 2^-11 + 1e-6) have a squared norm within
	// 0.001 of 1. Each rounds up to 0.25 * (1 + 2^-10).
	var near semanticVector
	for dimension := range 16 {
		near[dimension] = float32(0.25 * (1 + math.Ldexp(1, -11) + 1e-6))
	}
	stored, err := encodeSemanticF16Vector(&near)
	if err != nil {
		t.Fatal(err)
	}
	if stored[0] != 0x3401 {
		t.Fatalf("component code=%#04x, want 0x3401", stored[0])
	}
	if err := validateSemanticF16Vector(&stored); err != nil {
		t.Fatalf("a valid vector near the bound was rejected: %v", err)
	}
	for dimension := range 16 {
		stored[dimension] = 0x3402
	}
	if err := validateSemanticF16Vector(&stored); err == nil {
		t.Fatal("a squared norm of (1 + 2^-9)^2 was accepted")
	}

	random := rand.New(rand.NewPCG(13, 14))
	var worst float64
	for range 20_000 {
		var vector semanticVector
		for dimension := range vector {
			vector[dimension] = float32(random.NormFloat64())
		}
		if err := normalizeSemanticVector(&vector); err != nil {
			t.Fatal(err)
		}
		stored, err := encodeSemanticF16Vector(&vector)
		if err != nil {
			t.Fatal(err)
		}
		var squaredNorm float64
		for _, code := range stored {
			value := semanticF16OracleValue(code)
			squaredNorm += value * value
		}
		worst = max(worst, math.Abs(squaredNorm-1))
	}
	if worst > need {
		t.Fatalf("random stored vector error=%g, derived bound=%g", worst, need)
	}
}

func TestValidateSemanticF16VectorRejectsInvalidCodesAndNorm(t *testing.T) {
	source := semanticVector{0: 1}
	valid, err := encodeSemanticF16Vector(&source)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSemanticF16Vector(&valid); err != nil {
		t.Fatal(err)
	}
	for name, code := range map[string]uint16{
		"NaN": 0x7e00, "negative NaN": 0xfe01, "infinity": 0x7c00, "negative infinity": 0xfc00,
	} {
		damaged := valid
		damaged[5] = code
		if err := validateSemanticF16Vector(&damaged); err == nil {
			t.Fatalf("%s component was accepted", name)
		}
	}
	zero := semanticF16Vector{}
	if err := validateSemanticF16Vector(&zero); err == nil {
		t.Fatal("zero-norm stored vector was accepted")
	}
	tooWide := valid
	tooWide[1] = semanticF16FromFloat32(0.05)
	if err := validateSemanticF16Vector(&tooWide); err == nil {
		t.Fatal("stored vector outside the norm tolerance was accepted")
	}
	for _, value := range []float32{float32(math.NaN()), float32(math.Inf(1))} {
		vector := semanticVector{0: 1, 1: value}
		if _, err := encodeSemanticF16Vector(&vector); err == nil {
			t.Fatalf("component %g was encoded", value)
		}
	}
}

func TestSemanticF16TypeSizes(t *testing.T) {
	if got, want := unsafe.Sizeof(semanticF16Vector{}), uintptr(semanticEmbeddingDimensions*2); got != want {
		t.Fatalf("vector bytes=%d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(semanticFineF16Vectors{}), uintptr(semanticFineVectorBytesPerUnit); got != want {
		t.Fatalf("row bytes=%d, want %d", got, want)
	}
}

func TestCheckedSemanticVectorBytesUsesF16Stride(t *testing.T) {
	if got, ok := checkedSemanticVectorBytes(0); !ok || got != 0 {
		t.Fatalf("zero rows: bytes=%d ok=%v", got, ok)
	}
	if got, ok := checkedSemanticVectorBytes(1); !ok || got != 1_920 {
		t.Fatalf("one row: bytes=%d ok=%v, want 1920 and true", got, ok)
	}
	if _, ok := checkedSemanticVectorBytes(math.MaxUint64); ok {
		t.Fatal("overflowing vector size was accepted")
	}
}

// BenchmarkEncodeSemanticFineVectors encodes one model batch, one row at a
// time, as the row writer does.
func BenchmarkEncodeSemanticFineVectors(b *testing.B) {
	const rows = semanticModelBatchRows
	vector := semanticUSearchTestVector(7)
	embeddings := make([]semanticUnitEmbedding, rows)
	for row := range embeddings {
		for centroid := range embeddings[row].fine {
			embeddings[row].fine[centroid] = vector
		}
	}
	buffer := make([]byte, rows*semanticFineVectorBytesPerUnit)
	b.ReportAllocs()
	for b.Loop() {
		for row := range embeddings {
			at := row * semanticFineVectorBytesPerUnit
			if err := encodeSemanticFineVectors(buffer[at:at+semanticFineVectorBytesPerUnit], &embeddings[row].fine, uint64(row)); err != nil {
				b.Fatal(err)
			}
		}
	}
}
