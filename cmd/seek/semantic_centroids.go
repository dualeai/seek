package main

import (
	"fmt"
	"math"
)

const (
	// These values define the stored representation. Each unit puts four
	// once-refined centroids in USearch and stores 20 direct centroids for exact
	// scoring. A change requires new representation and USearch layout IDs.
	semanticCoarseCentroidsPerUnit = 4
	semanticFineCentroidsPerUnit   = 20
	semanticCoarseRefinementPasses = 1
	semanticFineRefinementPasses   = 0
)

type semanticCoarseVectors [semanticCoarseCentroidsPerUnit]semanticVector
type semanticFineVectors [semanticFineCentroidsPerUnit]semanticVector

type semanticUnitEmbedding struct {
	coarse semanticCoarseVectors
	fine   semanticFineVectors
}

// semanticCoarseSet keeps only the graph vectors after each fine-vector batch
// is on disk.
type semanticCoarseSet struct {
	batches [][]semanticCoarseVectors
	rows    int
}

func (set semanticCoarseSet) Len() int {
	return set.rows
}

func (set semanticCoarseSet) At(row int) (*semanticCoarseVectors, bool) {
	if row < 0 || row >= set.rows {
		return nil, false
	}
	batch := row / semanticModelBatchRows
	offset := row % semanticModelBatchRows
	if batch >= len(set.batches) || offset >= len(set.batches[batch]) {
		return nil, false
	}
	return &set.batches[batch][offset], true
}

func (set semanticCoarseSet) validate() error {
	if set.rows < 0 || set.rows > semanticMaxRows {
		return fmt.Errorf("semantic coarse count %d is invalid", set.rows)
	}
	if set.rows == 0 {
		if len(set.batches) != 0 {
			return fmt.Errorf("empty semantic coarse set has %d batches", len(set.batches))
		}
		return nil
	}
	wantBatches := (set.rows + semanticModelBatchRows - 1) / semanticModelBatchRows
	if len(set.batches) != wantBatches {
		return fmt.Errorf("semantic coarse set has %d batches, want %d", len(set.batches), wantBatches)
	}
	for batch := range set.batches {
		wantRows := min(semanticModelBatchRows, set.rows-batch*semanticModelBatchRows)
		if len(set.batches[batch]) != wantRows {
			return fmt.Errorf(
				"semantic coarse batch %d has %d rows, want %d",
				batch,
				len(set.batches[batch]),
				wantRows,
			)
		}
	}
	return nil
}

type semanticCentroidWorkspace struct {
	tokens  [lateOnSequenceLength]semanticVector
	nearest [lateOnSequenceLength]float32
	sums    [semanticFineCentroidsPerUnit]semanticVector
	members [semanticFineCentroidsPerUnit]int
}

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
// every tie.
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

// refineCentroids keeps centroid zero anchored to the first scoreable token.
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

func normalizeSemanticVector(vector *semanticVector) error {
	var squaredNorm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("vector has a non-finite value")
		}
		squaredNorm += float64(value) * float64(value)
	}
	if squaredNorm == 0 || math.IsNaN(squaredNorm) || math.IsInf(squaredNorm, 0) {
		return fmt.Errorf("vector has zero or invalid norm")
	}
	scale := float32(1 / math.Sqrt(squaredNorm))
	for dimension := range vector {
		vector[dimension] *= scale
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

func semanticQueryTokenVectors(query *semanticQueryEmbedding) ([]semanticVector, error) {
	if query == nil || len(query.tokens) != lateOnSequenceLength*semanticEmbeddingDimensions ||
		len(query.scoreMask) != lateOnSequenceLength {
		return nil, fmt.Errorf("semantic query embedding is invalid")
	}
	return normalizedLateOnSemanticTokens(query.tokens, query.scoreMask, 0)
}
