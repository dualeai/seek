package main

import (
	"fmt"
	"log/slog"
	"math"
)

const (
	// Calibration record, frozen on 2026-09-14: six repository families gave
	// 72 answerable and 72 wrong-project descriptions. The joined route admitted
	// 65/72 useful queries and 0/71 scorable wrong-project queries. The lexical
	// re-rank route admitted 67/72 and 0/71. One wrong-project query had strict
	// evidence and was not score-tested. A 128-file pool covered 72/72 labelled
	// target files, compared with 70/72 at 20 files.
	//
	// The model asset was rebuilt on 2026-09-14 with shape inference re-run after
	// the fixed dimensions are pinned, which cuts symbolic intermediate shapes
	// from 330 to 171 and so gives Core ML larger partitions. Inference output is
	// bit-identical to the previous asset over an 8-row probe, so this record
	// still holds; only the model hash moved.
	//
	// MeanMaxSim is the raw LateOn score divided by active query-token count.
	// Admission uses a strict greater-than test. These values are not relevance
	// probabilities. Recalibrate after a change to scoring, normalization, model
	// or tokenizer bytes, query or document evidence, pool allocation, route,
	// provider, or platform.
	hybridAcceptanceMinimumMeanMaxSim float32 = 0.6
	rerankAcceptanceMinimumMeanMaxSim float32 = 0.575
	hybridAcceptanceRoute                     = "joined"
	rerankAcceptanceRoute                     = "lexical-rerank"
)

// rerankScoreBatch keeps the score scale metadata with the scores produced by
// one model call. LateOn sums one maximum similarity per active query token,
// so the raw values are not comparable across query lengths.
type rerankScoreBatch struct {
	values            []float32
	activeQueryTokens int
	queryTruncated    bool
}

func newRerankScoreBatch(
	prepared *semanticQueryEmbedding,
	values []float32,
) (rerankScoreBatch, error) {
	if prepared == nil || len(prepared.scoreMask) == 0 {
		return rerankScoreBatch{}, fmt.Errorf("semantic query metadata is invalid")
	}
	active := 0
	for _, keep := range prepared.scoreMask {
		if keep {
			active++
		}
	}
	if active == 0 {
		return rerankScoreBatch{}, fmt.Errorf("semantic query has no active tokens")
	}
	return rerankScoreBatch{
		values:            values,
		activeQueryTokens: active,
		queryTruncated:    prepared.truncated,
	}, nil
}

type rerankAcceptancePolicy struct {
	minimumMeanMaxSim float32
	route             string
}

func newRerankAcceptancePolicy(minimumMeanMaxSim float32) *rerankAcceptancePolicy {
	return &rerankAcceptancePolicy{
		minimumMeanMaxSim: minimumMeanMaxSim,
	}
}

func defaultRerankAcceptancePolicies() rerankAcceptancePolicies {
	// NOTE: Update TestRerankAcceptanceCalibrationContract and recalibrate when
	// an input in the calibration record changes. Do not add a runtime switch
	// that disables expansion.
	return rerankAcceptancePolicies{
		rerank: &rerankAcceptancePolicy{
			minimumMeanMaxSim: rerankAcceptanceMinimumMeanMaxSim,
			route:             rerankAcceptanceRoute,
		},
		hybrid: &rerankAcceptancePolicy{
			minimumMeanMaxSim: hybridAcceptanceMinimumMeanMaxSim,
			route:             hybridAcceptanceRoute,
		},
	}
}

func logRerankExpansionRejection(policy *rerankAcceptancePolicy, reason string) {
	route := "unspecified"
	if policy != nil && policy.route != "" {
		route = policy.route
	}
	slog.Debug(
		"Rejected model expansion",
		"route", route,
		"reason", reason,
	)
}

func (policy *rerankAcceptancePolicy) validate() error {
	if policy == nil {
		return fmt.Errorf("rerank acceptance policy is missing")
	}
	if math.IsNaN(float64(policy.minimumMeanMaxSim)) ||
		math.IsInf(float64(policy.minimumMeanMaxSim), 0) {
		return fmt.Errorf("rerank acceptance threshold is not finite")
	}
	return nil
}

func validateRerankScoreBatch(batch rerankScoreBatch, candidates int) error {
	if len(batch.values) != candidates {
		return fmt.Errorf(
			"invalid semantic score count: got %d, want %d",
			len(batch.values),
			candidates,
		)
	}
	if batch.activeQueryTokens <= 0 {
		return fmt.Errorf("semantic query has no active tokens")
	}
	for _, score := range batch.values {
		if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
			return fmt.Errorf("invalid semantic score")
		}
	}
	return nil
}

func meanMaxSimPasses(score float32, activeQueryTokens int, threshold float32) bool {
	return score/float32(activeQueryTokens) > threshold
}

// applyRerankAcceptance decides if model expansion has enough support before
// reciprocal-rank fusion. A non-truncated strict all-term result bypasses the
// score check and admits the full expansion. Without one, the best model
// candidate must pass the route boundary. Query-wide admission preserves the
// full ranked set. A false return flag keeps only strict BM25 results.
func applyRerankAcceptance(
	candidates []rerankCandidate,
	batch rerankScoreBatch,
	strictRanked []corpusSearchResult,
	policy *rerankAcceptancePolicy,
) ([]rerankCandidate, rerankScoreBatch, bool, error) {
	if err := validateRerankScoreBatch(batch, len(candidates)); err != nil {
		return nil, rerankScoreBatch{}, false, err
	}
	if err := policy.validate(); err != nil {
		return nil, rerankScoreBatch{}, false, err
	}
	route := "unspecified"
	if policy.route != "" {
		route = policy.route
	}

	if !batch.queryTruncated && len(strictRanked) > 0 {
		slog.Debug(
			"Accepted model expansion",
			"route", route,
			"reason", "strict lexical support",
			"candidates", len(candidates),
			"strict_files", len(strictRanked),
		)
		return candidates, batch, true, nil
	}
	if !batch.queryTruncated {
		if len(batch.values) == 0 {
			slog.Debug(
				"Rejected model expansion",
				"route", route,
				"reason", "no candidates",
			)
			batch.values = nil
			return nil, batch, false, nil
		}
		bestScore := batch.values[0]
		for _, score := range batch.values {
			if score > bestScore {
				bestScore = score
			}
		}
		best := bestScore / float32(batch.activeQueryTokens)
		if meanMaxSimPasses(bestScore, batch.activeQueryTokens, policy.minimumMeanMaxSim) {
			slog.Debug(
				"Accepted model expansion",
				"route", route,
				"reason", "score above minimum",
				"best_mean_max_sim", best,
				"minimum_mean_max_sim", policy.minimumMeanMaxSim,
				"candidates", len(candidates),
			)
			return candidates, batch, true, nil
		}
		slog.Debug(
			"Rejected model expansion",
			"route", route,
			"reason", "score not above minimum",
			"best_mean_max_sim", best,
			"minimum_mean_max_sim", policy.minimumMeanMaxSim,
			"candidates", len(candidates),
		)
	} else {
		slog.Debug(
			"Rejected model expansion",
			"route", route,
			"reason", "query truncated",
			"candidates", len(candidates),
			"strict_files", len(strictRanked),
		)
	}

	batch.values = nil
	return nil, batch, false, nil
}
