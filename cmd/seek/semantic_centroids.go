package main

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

const (
	// These values define the stored representation. Each unit puts four
	// once-refined centroids in USearch and stores 20 direct centroids for
	// exact scoring. semanticRepresentationCompatibility includes them all,
	// and semanticUSearchLayout includes the coarse count. So a change cannot
	// reuse an old generation. The coarse count must stay a power of two for
	// the key shift of the bitmap filter (semanticUSearchCentroidShift).
	semanticCoarseCentroidsPerUnit = 4
	semanticFineCentroidsPerUnit   = 20
	semanticCoarseRefinementPasses = 1
	semanticFineRefinementPasses   = 0
)

type semanticCoarseVectors [semanticCoarseCentroidsPerUnit]semanticVector
type semanticFineVectors [semanticFineCentroidsPerUnit]semanticVector

// semanticUnitEmbedding is the stored vectors of one row: the coarse
// centroids for the USearch graph and the fine centroids for exact scoring.
type semanticUnitEmbedding struct {
	coarse semanticCoarseVectors
	fine   semanticFineVectors
}

// semanticCoarseSet keeps only the graph vectors after each fine-vector batch
// is on disk. The USearch stream builds each set from full 128-row batches and
// at most one short last batch. Row 0 of the set is the row at offset in its
// first batch, because a shard starts at a file boundary.
type semanticCoarseSet struct {
	batches [][]semanticCoarseVectors
	offset  int
	rows    int
}

func (set semanticCoarseSet) Len() int {
	return set.rows
}

func (set semanticCoarseSet) At(row int) (*semanticCoarseVectors, bool) {
	if row < 0 || row >= set.rows || set.offset < 0 {
		return nil, false
	}
	batch := (set.offset + row) / semanticModelBatchRows
	offset := (set.offset + row) % semanticModelBatchRows
	if batch >= len(set.batches) || offset >= len(set.batches[batch]) {
		return nil, false
	}
	return &set.batches[batch][offset], true
}

// semanticCentroidWorkspace holds the buffers of one centroid build, so a
// worker reuses them for each row.
type semanticCentroidWorkspace struct {
	tokens  [lateOnSequenceLength]semanticVector
	nearest [lateOnSequenceLength]float32
	sums    [semanticFineCentroidsPerUnit]semanticVector
	members [semanticFineCentroidsPerUnit]int
}

// makeUnitEmbedding builds the centroids of one model output row. The coarse
// centroids start from the first fine centroids and get
// semanticCoarseRefinementPasses passes.
func (workspace *semanticCentroidWorkspace) makeUnitEmbedding(
	embeddings []float32,
	mask []bool,
	row int,
) (semanticUnitEmbedding, error) {
	tokenCount, err := workspace.loadTokens(embeddings, mask, row)
	if err != nil {
		return semanticUnitEmbedding{}, err
	}
	var result semanticUnitEmbedding
	tokens := workspace.tokens[:tokenCount]
	if err := workspace.selectCentroids(tokens, result.fine[:]); err != nil {
		return semanticUnitEmbedding{}, err
	}
	if semanticFineRefinementPasses > 0 {
		if err := workspace.refineCentroids(tokens, result.fine[:], semanticFineRefinementPasses); err != nil {
			return semanticUnitEmbedding{}, err
		}
	}
	copy(result.coarse[:], result.fine[:semanticCoarseCentroidsPerUnit])
	if err := workspace.refineCentroids(tokens, result.coarse[:], semanticCoarseRefinementPasses); err != nil {
		return semanticUnitEmbedding{}, err
	}
	return result, nil
}

// loadTokens copies the normalized vectors of the tokens that mask keeps in
// row into the workspace, and returns their count.
func (workspace *semanticCentroidWorkspace) loadTokens(
	embeddings []float32,
	mask []bool,
	row int,
) (int, error) {
	sequence := len(mask)
	if sequence < 1 || sequence > len(workspace.tokens) || row < 0 ||
		len(embeddings) < (row+1)*sequence*semanticEmbeddingDimensions {
		return 0, fmt.Errorf("LateOn embedding shape is invalid")
	}
	count := 0
	for token, keep := range mask {
		if !keep {
			continue
		}
		offset := (row*sequence + token) * semanticEmbeddingDimensions
		vector := &workspace.tokens[count]
		copy(vector[:], embeddings[offset:offset+semanticEmbeddingDimensions])
		if err := normalizeSemanticVector(vector); err != nil {
			return 0, fmt.Errorf("LateOn token %d: %w", token, err)
		}
		count++
	}
	if count == 0 {
		return 0, fmt.Errorf("LateOn row has no scoreable token")
	}
	return count, nil
}

// selectCentroids keeps the first scoreable token as a fixed anchor. It selects
// the other centroids by farthest-first traversal. Stable token order resolves
// every tie. With fewer tokens than centroids, the selected centroids repeat
// in order to fill destination.
func (workspace *semanticCentroidWorkspace) selectCentroids(
	tokens []semanticVector,
	destination []semanticVector,
) error {
	count := len(destination)
	if len(tokens) == 0 || count < 2 {
		return fmt.Errorf("semantic centroid settings are invalid")
	}
	destination[0] = tokens[0]
	nearest := workspace.nearest[:len(tokens)]
	for token, vector := range tokens {
		nearest[token] = semanticVectorDot(vector, destination[0])
	}
	centroidCount := 1
	for centroidCount < min(count, len(tokens)) {
		selected := 0
		selectedSimilarity := float32(math.Inf(1))
		for token, similarity := range nearest {
			if similarity < selectedSimilarity {
				selected = token
				selectedSimilarity = similarity
			}
		}
		destination[centroidCount] = tokens[selected]
		selectedVector := destination[centroidCount]
		centroidCount++
		for token, vector := range tokens {
			nearest[token] = max(nearest[token], semanticVectorDot(vector, selectedVector))
		}
	}
	for centroidCount < count {
		destination[centroidCount] = destination[centroidCount%len(tokens)]
		centroidCount++
	}
	return nil
}

// refineCentroids runs a number of passes (refinements) on centroids. In each
// pass, every token after the first joins the most similar of the other
// centroids. Then each centroid with members becomes the normalized mean of
// its members. A centroid with no members keeps its value. Centroid zero stays
// on the first scoreable token.
func (workspace *semanticCentroidWorkspace) refineCentroids(
	tokens []semanticVector,
	centroids []semanticVector,
	refinements int,
) error {
	count := len(centroids)
	if len(tokens) == 0 || count < 2 || refinements < 0 {
		return fmt.Errorf("semantic centroid settings are invalid")
	}
	for range refinements {
		sums := workspace.sums[:count]
		members := workspace.members[:count]
		for centroid := range count {
			sums[centroid] = semanticVector{}
			members[centroid] = 0
		}
		// Centroid zero owns only the first scoreable token. This keeps the
		// common model prefix stable and is part of the representation format.
		sums[0] = tokens[0]
		members[0] = 1
		for _, vector := range tokens[1:] {
			best := 1
			bestScore := semanticVectorDot(vector, centroids[best])
			for centroid := best + 1; centroid < count; centroid++ {
				score := semanticVectorDot(vector, centroids[centroid])
				if score > bestScore {
					best = centroid
					bestScore = score
				}
			}
			members[best]++
			for dimension := range semanticEmbeddingDimensions {
				sums[best][dimension] += vector[dimension]
			}
		}
		for centroid := range centroids {
			if members[centroid] == 0 {
				continue
			}
			for dimension := range semanticEmbeddingDimensions {
				sums[centroid][dimension] /= float32(members[centroid])
			}
			if err := normalizeSemanticVector(&sums[centroid]); err != nil {
				return fmt.Errorf("semantic centroid %d: %w", centroid, err)
			}
			centroids[centroid] = sums[centroid]
		}
	}
	return nil
}

func normalizedLateOnSemanticTokens(
	embeddings []float32,
	mask []bool,
	row int,
) ([]semanticVector, error) {
	var workspace semanticCentroidWorkspace
	count, err := workspace.loadTokens(embeddings, mask, row)
	if err != nil {
		return nil, err
	}
	return append([]semanticVector(nil), workspace.tokens[:count]...), nil
}

// errDegenerateSemanticVector marks model output that has no usable
// direction. The run-time provider check tests one row once, so a provider
// fault that depends on the input can still reach the index build. Callers use
// this error to record a provider rejection, not only to fail the build.
var errDegenerateSemanticVector = errors.New("model output is degenerate")

// normalizeSemanticVector scales vector to unit length. A non-finite value or
// a zero norm gives errDegenerateSemanticVector.
func normalizeSemanticVector(vector *semanticVector) error {
	var squaredNorm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("%w: vector has a non-finite value", errDegenerateSemanticVector)
		}
		squaredNorm += float64(value) * float64(value)
	}
	if squaredNorm == 0 || math.IsNaN(squaredNorm) || math.IsInf(squaredNorm, 0) {
		return fmt.Errorf("%w: vector has zero or invalid norm", errDegenerateSemanticVector)
	}
	scale64 := 1 / math.Sqrt(squaredNorm)
	scale32 := float32(scale64)
	if !math.IsInf(float64(scale32), 0) {
		// Normal model output takes the float32 product, which gives the
		// stored bytes. The float64 path handles a scale that float32 cannot
		// hold.
		for dimension := range vector {
			vector[dimension] *= scale32
		}
	} else {
		for dimension, value := range vector {
			vector[dimension] = float32(float64(value) * scale64)
		}
	}
	if err := validateNormalizedSemanticVector(vector); err != nil {
		return fmt.Errorf("normalized vector %w", err)
	}
	return nil
}

func semanticVectorDot(left, right semanticVector) float32 {
	var dot float32
	for dimension := range semanticEmbeddingDimensions {
		dot += left[dimension] * right[dimension]
	}
	return dot
}

// semanticQueryTokenVectors returns the normalized vector of each scoring
// token of query, one 128-token window after another. A window with no scoring
// token adds no vector.
func semanticQueryTokenVectors(query *semanticQueryEmbedding) ([]semanticVector, error) {
	if _, err := semanticQueryTokenCount(query); err != nil {
		return nil, err
	}
	var vectors []semanticVector
	for window := range len(query.scoreMask) / lateOnSequenceLength {
		mask := query.scoreMask[window*lateOnSequenceLength : (window+1)*lateOnSequenceLength]
		if !slices.Contains(mask, true) {
			continue
		}
		tokens, err := normalizedLateOnSemanticTokens(query.tokens, mask, window)
		if err != nil {
			return nil, err
		}
		vectors = append(vectors, tokens...)
	}
	return vectors, nil
}

func semanticQueryTokenCount(query *semanticQueryEmbedding) (int, error) {
	if !validSemanticQuery(query) {
		return 0, fmt.Errorf("semantic query embedding is invalid")
	}
	count := 0
	for _, score := range query.scoreMask {
		if score {
			count++
		}
	}
	if count == 0 {
		return 0, fmt.Errorf("semantic query embedding has no scoring token")
	}
	return count, nil
}
