package main

import (
	"fmt"
	"math"
)

// The coarse.snorm16 payload has no header or padding. It stores rows in row
// order, then centroids and dimensions in each row. Each input vector must
// have finite components in [-1, 1] and a squared norm within 0.001 of 1. Each
// component is a little-endian int16 equal to
// math.RoundToEven(float64(value) * 32767). Valid codes are -32767 through
// 32767; -32768 is reserved. A stored vector is valid only when its
// squared-code sum passes the integer norm tolerance below. One row contains 4
// centroids of 48 components and is 384 bytes. The file length must equal the
// row count times this stride.
const (
	semanticSNORM16Scale        = 32767
	semanticSNORM16SquaredScale = semanticSNORM16Scale * semanticSNORM16Scale
	// This tolerance is floor(0.001212 * semanticSNORM16SquaredScale):
	// 0.001212 covers the accepted float32 norm and the maximum 48-component
	// codec error. TestSemanticSNORM16IntegerNormThreshold checks that it is
	// large enough.
	semanticSNORM16SquaredNormCodeTolerance = 1_301_295
)

type semanticSNORM16Vector [semanticEmbeddingDimensions]int16

// semanticCoarseSNORM16Vectors is one row's stored coarse centroids. A build
// makes the USearch shards from their decoded values. So a row that a build
// copies gives the same shard input as a row that the model embeds.
type semanticCoarseSNORM16Vectors [semanticCoarseCentroidsPerUnit]semanticSNORM16Vector

const semanticCoarseVectorBytesPerUnit = semanticCoarseCentroidsPerUnit * semanticEmbeddingDimensions * 2

// decodeSemanticSNORM16Vector returns the float value of a stored vector.
func decodeSemanticSNORM16Vector(stored semanticSNORM16Vector) semanticVector {
	var vector semanticVector
	for dimension, code := range stored {
		vector[dimension] = float32(code) / float32(semanticSNORM16Scale)
	}
	return vector
}

// encodeSemanticSNORM16Vector stores a normalized vector as SNORM16 codes. The
// norm check leaves every component finite, but a component can still be just
// past -1 or 1, which no code holds. A component in [-1, 1] gives a code from
// -32767 through 32767.
func encodeSemanticSNORM16Vector(vector *semanticVector) (semanticSNORM16Vector, error) {
	if err := validateNormalizedSemanticVector(vector); err != nil {
		return semanticSNORM16Vector{}, err
	}
	var encoded semanticSNORM16Vector
	for dimension, value := range vector {
		if !(value >= -1 && value <= 1) {
			return semanticSNORM16Vector{}, fmt.Errorf("component %d is outside [-1, 1]", dimension)
		}
		encoded[dimension] = int16(math.RoundToEven(float64(value) * semanticSNORM16Scale))
	}
	return encoded, nil
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
