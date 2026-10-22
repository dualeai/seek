package main

import (
	"encoding/binary"
	"fmt"
	"math"
)

// The vectors.f16 payload has no header or padding. It stores rows in row
// order, then centroids and dimensions within each row. Each component is a
// little-endian IEEE 754 binary16 value, rounded from float32 to nearest with
// ties to even. One row contains 20 centroids of 48 components and is 1,920
// bytes. The file length must equal the row count times this stride.
//
// The fine scorer reads these values directly with NumKong's binary16 dot
// products (semantic_numkong.go), and it rounds the query tokens to binary16
// too. Against float64 scores of the float32 vectors, this rounding moved a
// fine MeanMaxSim by at most 5.3e-5 at 13 query tokens and by at most 3.4e-5
// at 32 tokens, in a measurement on 20,000 rows of random unit vectors.
const (
	semanticVectorCodec            = "f16-le-rne-v1"
	semanticFineVectorBytesPerUnit = semanticFineCentroidsPerUnit * semanticEmbeddingDimensions * 2
	// semanticF16SquaredNormTolerance limits the squared norm of a stored
	// vector. It covers the accepted input norm (0.001), the binary16 rounding
	// (a relative error of at most 2^-11 for each component, so at most
	// 0.000978 more), and the float32 sum of the check. The worst valid vector
	// stays below 0.001982. TestSemanticF16NormToleranceCoversRounding checks
	// the bound.
	semanticF16SquaredNormTolerance = 0.002
)

type semanticF16Vector [semanticEmbeddingDimensions]uint16

type semanticFineF16Vectors [semanticFineCentroidsPerUnit]semanticF16Vector

// semanticF16FromFloat32 rounds value to the nearest binary16 value, with ties
// to even. A value out of range becomes an infinity, and a NaN stays a NaN.
func semanticF16FromFloat32(value float32) uint16 {
	bits := math.Float32bits(value)
	sign := uint16(bits>>16) & 0x8000
	exponent := int32(bits>>23) & 0xff
	mantissa := bits & 0x7f_ffff
	if exponent == 0xff {
		if mantissa != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	}
	// halfExponent is the biased binary16 exponent of a normal result.
	halfExponent := exponent - 127 + 15
	if halfExponent >= 0x1f {
		return sign | 0x7c00
	}
	if halfExponent <= 0 {
		// The result is subnormal or zero: value / 2^-24, rounded.
		if halfExponent < -10 {
			return sign
		}
		significand := mantissa | 0x80_0000
		shift := uint32(14 - halfExponent)
		rounded := significand >> shift
		remainder := significand & (1<<shift - 1)
		halfway := uint32(1) << (shift - 1)
		if remainder > halfway || (remainder == halfway && rounded&1 == 1) {
			rounded++
		}
		return sign | uint16(rounded)
	}
	// A carry out of the mantissa increments the exponent, which is correct:
	// it can give the next power of two or an infinity.
	rounded := uint32(halfExponent)<<10 | mantissa>>13
	remainder := mantissa & 0x1fff
	if remainder > 0x1000 || (remainder == 0x1000 && rounded&1 == 1) {
		rounded++
	}
	return sign | uint16(rounded)
}

// semanticF16ToFloat32 gives the exact float32 value of a binary16 code.
func semanticF16ToFloat32(code uint16) float32 {
	sign := uint32(code&0x8000) << 16
	exponent := uint32(code>>10) & 0x1f
	mantissa := uint32(code) & 0x3ff
	switch exponent {
	case 0x1f:
		return math.Float32frombits(sign | 0x7f80_0000 | mantissa<<13)
	case 0:
		// A subnormal value is mantissa * 2^-24. Both factors are exact in
		// float32, so the product is exact.
		return math.Float32frombits(sign | math.Float32bits(float32(mantissa)*0x1p-24))
	default:
		return math.Float32frombits(sign | (exponent+127-15)<<23 | mantissa<<13)
	}
}

// encodeSemanticF16Vector stores a normalized vector as binary16 codes.
func encodeSemanticF16Vector(vector *semanticVector) (semanticF16Vector, error) {
	if err := validateNormalizedSemanticVector(vector); err != nil {
		return semanticF16Vector{}, err
	}
	var encoded semanticF16Vector
	for dimension := range vector {
		encoded[dimension] = semanticF16FromFloat32(vector[dimension])
	}
	return encoded, nil
}

// validateSemanticF16Vector accepts a stored vector when its components are
// finite and its squared norm is within semanticF16SquaredNormTolerance of 1.
// The fine scorer applies the same rule in C before it scores a row.
func validateSemanticF16Vector(vector *semanticF16Vector) error {
	var squaredNorm float64
	for dimension := range vector {
		value := float64(semanticF16ToFloat32(vector[dimension]))
		squaredNorm += value * value
	}
	if !(math.Abs(squaredNorm-1) <= semanticF16SquaredNormTolerance) {
		return fmt.Errorf("has squared norm %.6f, want within %g of 1", squaredNorm, semanticF16SquaredNormTolerance)
	}
	return nil
}

// encodeSemanticFineVectors writes the fine centroids of embeddings to buffer
// in the vectors.f16 layout and returns the written bytes.
func encodeSemanticFineVectors(
	buffer []byte,
	embeddings []semanticUnitEmbedding,
	startRow int,
) ([]byte, error) {
	wantBytes := len(embeddings) * semanticFineVectorBytesPerUnit
	if startRow < 0 || len(buffer) < wantBytes {
		return nil, fmt.Errorf("semantic vector buffer is too small")
	}
	encoded := buffer[:wantBytes]
	at := 0
	// Use indexes so range does not copy a 4,608-byte embedding or a 192-byte
	// vector.
	for row := range embeddings {
		embedding := &embeddings[row]
		for centroid := range embedding.fine {
			stored, err := encodeSemanticF16Vector(&embedding.fine[centroid])
			if err != nil {
				return nil, fmt.Errorf("semantic row %d centroid %d: %w", startRow+row, centroid, err)
			}
			for dimension := range stored {
				binary.LittleEndian.PutUint16(encoded[at:at+2], stored[dimension])
				at += 2
			}
		}
	}
	return encoded, nil
}
