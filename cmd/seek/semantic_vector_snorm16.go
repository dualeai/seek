package main

import (
	"encoding/binary"
	"fmt"
	"math"
)

// The vectors.snorm16 payload has no header or padding. It stores rows in row
// order, then centroids and dimensions within each row. Each input vector must
// have finite components in [-1, 1] and a squared norm within 0.001 of 1. Each
// component is a little-endian int16 equal to
// math.RoundToEven(float64(value) * 32767). Valid codes are -32767 through
// 32767; -32768 is reserved. A stored vector is valid only when its squared-code
// sum passes the integer norm tolerance below. One row contains 20 centroids of
// 48 components and is 1,920 bytes. The file length must equal the row count
// times this stride.
const (
	semanticVectorCodec         = "snorm16-le-rne-s32767-v1"
	semanticSNORM16Scale        = 32767
	semanticSNORM16InverseScale = float32(1.0 / semanticSNORM16Scale)
	semanticSNORM16SquaredScale = semanticSNORM16Scale * semanticSNORM16Scale
	// This tolerance is floor(0.001212 * semanticSNORM16SquaredScale).
	// TestSemanticSNORM16IntegerNormThreshold derives 0.001212 from the accepted
	// float32 norm and the maximum 48-component codec error.
	semanticSNORM16SquaredNormCodeTolerance = 1_301_295
	semanticFineVectorBytesPerUnit          = semanticFineCentroidsPerUnit * semanticEmbeddingDimensions * 2
)

type semanticSNORM16Vector [semanticEmbeddingDimensions]int16
type semanticFineSNORM16Vectors [semanticFineCentroidsPerUnit]semanticSNORM16Vector

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
			stored, err := encodeSemanticSNORM16Vector(&embedding.fine[centroid])
			if err != nil {
				return nil, fmt.Errorf("semantic row %d centroid %d: %w", startRow+row, centroid, err)
			}
			for dimension := range stored {
				code := stored[dimension]
				binary.LittleEndian.PutUint16(encoded[at:at+2], uint16(code))
				at += 2
			}
		}
	}
	return encoded, nil
}

func encodeSemanticSNORM16Vector(vector *semanticVector) (semanticSNORM16Vector, error) {
	if err := validateNormalizedSemanticVector(vector); err != nil {
		return semanticSNORM16Vector{}, err
	}
	var encoded semanticSNORM16Vector
	for dimension := range vector {
		value := vector[dimension]
		// Keep the valid path local. The error helper is too large to inline and
		// would otherwise run once for every stored component.
		if value >= -1 && value <= 1 {
			encoded[dimension] = int16(math.RoundToEven(float64(value) * semanticSNORM16Scale))
			continue
		}
		code, err := encodeSemanticSNORM16Component(value)
		if err != nil {
			return semanticSNORM16Vector{}, fmt.Errorf("component %d %w", dimension, err)
		}
		encoded[dimension] = code
	}
	return encoded, nil
}

func encodeSemanticSNORM16Component(value float32) (int16, error) {
	if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
		return 0, fmt.Errorf("is not finite")
	}
	if value < -1 || value > 1 {
		return 0, fmt.Errorf("is outside [-1, 1]")
	}
	code := math.RoundToEven(float64(value) * semanticSNORM16Scale)
	if code < -semanticSNORM16Scale || code > semanticSNORM16Scale {
		return 0, fmt.Errorf("is outside the codec range")
	}
	return int16(code), nil
}

func validateSemanticSNORM16Vector(vector *semanticSNORM16Vector) error {
	var squaredNorm int64
	for dimension := range vector {
		code := vector[dimension]
		if code == math.MinInt16 {
			return fmt.Errorf("has reserved code at component %d", dimension)
		}
		value := int64(code)
		squaredNorm += value * value
	}
	if !validSemanticSNORM16SquaredNorm(squaredNorm) {
		return fmt.Errorf(
			"has squared-norm code sum %d, want within %d of %d",
			squaredNorm,
			semanticSNORM16SquaredNormCodeTolerance,
			semanticSNORM16SquaredScale,
		)
	}
	return nil
}

func validSemanticSNORM16SquaredNorm(squaredNorm int64) bool {
	difference := squaredNorm - semanticSNORM16SquaredScale
	return difference >= -semanticSNORM16SquaredNormCodeTolerance &&
		difference <= semanticSNORM16SquaredNormCodeTolerance
}

// prepareSemanticSNORM16Query validates and scales an owned normalized query
// buffer in place by 1/32767. The scorer can then use stored int16 codes without
// a decoded vector buffer. If validation fails, the caller must discard query
// because earlier tokens can already be scaled.
func prepareSemanticSNORM16Query(query []semanticVector) ([]semanticVector, error) {
	for token := range query {
		vector := &query[token]
		if err := validateNormalizedSemanticVector(vector); err != nil {
			return nil, fmt.Errorf("semantic query token %d: %w", token, err)
		}
		for dimension := range vector {
			vector[dimension] *= semanticSNORM16InverseScale
		}
	}
	return query, nil
}

func semanticSNORM16VectorDot(scaledQuery *semanticVector, vector *semanticSNORM16Vector) float32 {
	var dot float32
	for dimension := range semanticEmbeddingDimensions {
		dot += scaledQuery[dimension] * float32(vector[dimension])
	}
	return dot
}
