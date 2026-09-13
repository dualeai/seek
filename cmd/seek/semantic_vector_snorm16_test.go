package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"testing"
	"unsafe"
)

const semanticSNORM16MaximumDecodeError = 2.980141289299354e-8

func decodeSemanticSNORM16Vector(vector semanticSNORM16Vector) semanticVector {
	var decoded semanticVector
	for dimension, code := range vector {
		decoded[dimension] = float32(code) / float32(semanticSNORM16Scale)
	}
	return decoded
}

func testSemanticSNORM16GoldenEmbedding() semanticUnitEmbedding {
	var embedding semanticUnitEmbedding
	embedding.fine[0] = semanticVector{0: -0.5, 1: 0.5, 2: float32(math.Sqrt(0.5))}
	for centroid := 1; centroid < 19; centroid++ {
		embedding.fine[centroid][centroid] = 1
	}
	embedding.fine[19][47] = -1
	copy(embedding.coarse[:], embedding.fine[:semanticCoarseCentroidsPerUnit])
	return embedding
}

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

func TestSemanticSNORM16ComponentRoundTripAllCodes(t *testing.T) {
	var maximumDecodeError float64
	for value := -semanticSNORM16Scale; value <= semanticSNORM16Scale; value++ {
		code := int16(value)
		decoded := float32(code) / float32(semanticSNORM16Scale)
		got, err := encodeSemanticSNORM16Component(decoded)
		if err != nil {
			t.Fatalf("code %d: %v", code, err)
		}
		if got != code {
			t.Fatalf("code %d round-tripped as %d", code, got)
		}
		decodeError := math.Abs(float64(decoded) - float64(code)/semanticSNORM16Scale)
		maximumDecodeError = max(maximumDecodeError, decodeError)
	}
	if maximumDecodeError != semanticSNORM16MaximumDecodeError {
		t.Fatalf(
			"maximum decoder error=%g, want %g",
			maximumDecodeError,
			semanticSNORM16MaximumDecodeError,
		)
	}
	code := int16(-32672)
	if float32(code)/float32(semanticSNORM16Scale) == float32(code)*semanticSNORM16InverseScale {
		t.Fatal("decoder oracle collapsed into the production pre-scale operation")
	}
}

func TestEncodeSemanticFineVectorsLayout(t *testing.T) {
	embedding := testSemanticUnitEmbedding(semanticVector{0: 1})
	buffer := make([]byte, semanticFineVectorBytesPerUnit+16)
	for index := range buffer {
		buffer[index] = 0xa5
	}
	encoded, err := encodeSemanticFineVectors(buffer, []semanticUnitEmbedding{embedding}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != semanticFineVectorBytesPerUnit {
		t.Fatalf("bytes=%d, want %d", len(encoded), semanticFineVectorBytesPerUnit)
	}
	const vectorBytes = semanticEmbeddingDimensions * 2
	for centroid := range semanticFineCentroidsPerUnit {
		at := centroid * vectorBytes
		if got := int16(binary.LittleEndian.Uint16(encoded[at : at+2])); got != semanticSNORM16Scale {
			t.Fatalf("centroid %d first code=%d, want %d", centroid, got, semanticSNORM16Scale)
		}
		for dimension := 1; dimension < semanticEmbeddingDimensions; dimension++ {
			componentAt := at + dimension*2
			if got := binary.LittleEndian.Uint16(encoded[componentAt : componentAt+2]); got != 0 {
				t.Fatalf("centroid %d dimension %d code=%d, want 0", centroid, dimension, got)
			}
		}
	}
	for index, value := range buffer[semanticFineVectorBytesPerUnit:] {
		if value != 0xa5 {
			t.Fatalf("byte after payload %d changed", index)
		}
	}
	if _, err := encodeSemanticFineVectors(buffer[:semanticFineVectorBytesPerUnit-1], []semanticUnitEmbedding{embedding}, 0); err == nil {
		t.Fatal("short output buffer was accepted")
	}
	if _, err := encodeSemanticFineVectors(buffer, []semanticUnitEmbedding{embedding}, -1); err == nil {
		t.Fatal("negative start row was accepted")
	}
}

func TestSemanticSNORM16LittleEndianGolden(t *testing.T) {
	const rowBytes = 1_920
	embedding := testSemanticSNORM16GoldenEmbedding()
	encoded, err := encodeSemanticFineVectors(
		make([]byte, rowBytes),
		[]semanticUnitEmbedding{embedding},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != rowBytes {
		t.Fatalf("encoded row has %d bytes, want %d", len(encoded), rowBytes)
	}
	checks := []struct {
		at   int
		want int16
	}{
		{at: 0, want: -16_384},
		{at: 2, want: 16_384},
		{at: 4, want: 23_170},
		{at: 98, want: 32_767},
		{at: 1_764, want: 32_767},
		{at: 1_918, want: -32_767},
	}
	for _, check := range checks {
		if got := int16(binary.LittleEndian.Uint16(encoded[check.at : check.at+2])); got != check.want {
			t.Fatalf("code at byte %d=%d, want %d", check.at, got, check.want)
		}
	}
	digest := sha256.Sum256(encoded)
	const wantSHA256 = "cc23605279c5bf152de3c05ee5c2a0733e4d4ff317c3973009f765e9673e69f4"
	if got := hex.EncodeToString(digest[:]); got != wantSHA256 {
		t.Fatalf("encoded row SHA-256=%s, want %s", got, wantSHA256)
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
	for _, squaredNorm := range []int64{
		semanticSNORM16SquaredScale - semanticSNORM16SquaredNormCodeTolerance,
		semanticSNORM16SquaredScale,
		semanticSNORM16SquaredScale + semanticSNORM16SquaredNormCodeTolerance,
	} {
		if !validSemanticSNORM16SquaredNorm(squaredNorm) {
			t.Fatalf("boundary sum %d was rejected", squaredNorm)
		}
	}
	for _, squaredNorm := range []int64{
		semanticSNORM16SquaredScale - semanticSNORM16SquaredNormCodeTolerance - 1,
		semanticSNORM16SquaredScale + semanticSNORM16SquaredNormCodeTolerance + 1,
	} {
		if validSemanticSNORM16SquaredNorm(squaredNorm) {
			t.Fatalf("out-of-range sum %d was accepted", squaredNorm)
		}
	}
}

func TestSemanticSNORM16ScoreMatchesFloat64Oracle(t *testing.T) {
	query := semanticVector{0: 0.6, 1: 0.8}
	document := semanticVector{0: 0.8, 1: 0.6}
	stored, err := encodeSemanticSNORM16Vector(&document)
	if err != nil {
		t.Fatal(err)
	}
	scaled, err := prepareSemanticSNORM16Query([]semanticVector{query})
	if err != nil {
		t.Fatal(err)
	}
	got := semanticSNORM16VectorDot(&scaled[0], &stored)
	decoded := decodeSemanticSNORM16Vector(stored)
	var oracle float64
	for dimension := range query {
		oracle += float64(query[dimension]) * float64(decoded[dimension])
	}
	if difference := math.Abs(float64(got) - oracle); difference > 1e-6 {
		t.Fatalf("score=%g oracle=%g difference=%g", got, oracle, difference)
	}
}

func TestSemanticSNORM16ScoreErrorBound(t *testing.T) {
	const scorerArithmeticBound = 1e-5
	u := math.Ldexp(1, -24)
	// gamma100 overcounts the reciprocal conversion, query pre-scale,
	// component multiply, and float32 accumulation roundings for one dot.
	// A fused multiply-add uses fewer roundings and stays inside this bound.
	gamma100 := 100 * u / (1 - 100*u)
	_, sourceNormLimit := semanticSourceSquaredNormBounds()
	storedNormLimit := math.Sqrt(
		1 + float64(semanticSNORM16SquaredNormCodeTolerance)/semanticSNORM16SquaredScale,
	)
	derivedArithmeticBound := gamma100 *
		math.Sqrt(sourceNormLimit) * storedNormLimit
	if scorerArithmeticBound < derivedArithmeticBound {
		t.Fatalf("scorer bound=%g, need at least %g", scorerArithmeticBound, derivedArithmeticBound)
	}
	storageBound := math.Sqrt(sourceNormLimit) *
		math.Sqrt(semanticEmbeddingDimensions) / (2 * semanticSNORM16Scale)
	for seed := 1; seed <= 4_096; seed++ {
		var query, document semanticVector
		for dimension := range query {
			query[dimension] = float32((seed*(dimension+17))%251 - 125)
			document[dimension] = float32((seed*(dimension+43)+dimension*29)%257 - 128)
		}
		if err := normalizeSemanticVector(&query); err != nil {
			t.Fatalf("query seed %d: %v", seed, err)
		}
		if err := normalizeSemanticVector(&document); err != nil {
			t.Fatalf("document seed %d: %v", seed, err)
		}
		stored, err := encodeSemanticSNORM16Vector(&document)
		if err != nil {
			t.Fatalf("stored seed %d: %v", seed, err)
		}
		scaled, err := prepareSemanticSNORM16Query([]semanticVector{query})
		if err != nil {
			t.Fatalf("scaled seed %d: %v", seed, err)
		}
		got := semanticSNORM16VectorDot(&scaled[0], &stored)
		var storedOracle, sourceOracle float64
		for dimension, code := range stored {
			storedOracle += float64(query[dimension]) * float64(code) / semanticSNORM16Scale
			sourceOracle += float64(query[dimension]) * float64(document[dimension])
		}
		if difference := math.Abs(float64(got) - storedOracle); difference > scorerArithmeticBound {
			t.Fatalf("seed %d scorer difference=%g, bound=%g", seed, difference, scorerArithmeticBound)
		}
		if difference := math.Abs(float64(got) - sourceOracle); difference > storageBound+scorerArithmeticBound {
			t.Fatalf(
				"seed %d total difference=%g, bound=%g",
				seed,
				difference,
				storageBound+scorerArithmeticBound,
			)
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
	if got, want := unsafe.Sizeof(semanticFineSNORM16Vectors{}), uintptr(semanticFineVectorBytesPerUnit); got != want {
		t.Fatalf("row bytes=%d, want %d", got, want)
	}
}

func TestPrepareSemanticSNORM16QueryScalesInPlaceWithoutAllocation(t *testing.T) {
	query := make([]semanticVector, 1)
	var scaled []semanticVector
	var prepareErr error
	allocations := testing.AllocsPerRun(1_000, func() {
		query[0] = semanticVector{0: 1}
		scaled, prepareErr = prepareSemanticSNORM16Query(query)
	})
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	if allocations != 0 {
		t.Fatalf("query preparation allocations=%g, want 0", allocations)
	}
	if &scaled[0] != &query[0] {
		t.Fatal("query preparation copied its owned input")
	}
	const wantScale float32 = 1.0 / 32_767.0
	if scaled[0][0] != wantScale {
		t.Fatalf("scaled component=%g, want %g", scaled[0][0], wantScale)
	}
}

func TestCheckedSemanticVectorBytesUsesSNORM16Stride(t *testing.T) {
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

func TestSemanticSNORM16VectorDotDoesNotAllocate(t *testing.T) {
	query := semanticVector{0: 1}
	stored, err := encodeSemanticSNORM16Vector(&query)
	if err != nil {
		t.Fatal(err)
	}
	scaled, err := prepareSemanticSNORM16Query([]semanticVector{query})
	if err != nil {
		t.Fatal(err)
	}
	if allocations := testing.AllocsPerRun(1_000, func() {
		_ = semanticSNORM16VectorDot(&scaled[0], &stored)
	}); allocations != 0 {
		t.Fatalf("allocations=%g, want 0", allocations)
	}
}

func BenchmarkSemanticVectorDot(b *testing.B) {
	query := semanticVector{0: 0.6, 1: 0.8}
	document := semanticVector{0: -0.8, 1: 0.6}
	stored, err := encodeSemanticSNORM16Vector(&document)
	if err != nil {
		b.Fatal(err)
	}
	scaled, err := prepareSemanticSNORM16Query([]semanticVector{query})
	if err != nil {
		b.Fatal(err)
	}
	b.Run("float32", func(b *testing.B) {
		for b.Loop() {
			_ = semanticVectorDot(query, document)
		}
	})
	b.Run("snorm16", func(b *testing.B) {
		for b.Loop() {
			_ = semanticSNORM16VectorDot(&scaled[0], &stored)
		}
	})
}

var semanticSNORM16BenchmarkBytes []byte

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
	b.ResetTimer()
	for b.Loop() {
		encoded, err := encodeSemanticFineVectors(buffer, embeddings, 0)
		if err != nil {
			b.Fatal(err)
		}
		semanticSNORM16BenchmarkBytes = encoded
	}
}
