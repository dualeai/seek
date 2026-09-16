package main

import (
	"log/slog"
	"math"
	"reflect"
	"testing"
)

func alwaysAcceptRerankPolicyForTest() *rerankAcceptancePolicy {
	policy := newRerankAcceptancePolicy(-math.MaxFloat32)
	policy.route = "test"
	return policy
}

func TestNewRerankScoreBatchKeepsQueryMetadata(t *testing.T) {
	prepared := &semanticQueryEmbedding{
		scoreMask: []bool{true, false, true},
		truncated: true,
	}
	values := []float32{2, 1}
	batch, err := newRerankScoreBatch(prepared, values)
	if err != nil {
		t.Fatal(err)
	}
	if batch.activeQueryTokens != 2 || !batch.queryTruncated ||
		!reflect.DeepEqual(batch.values, values) {
		t.Fatalf("score batch=%+v", batch)
	}
	if _, err := newRerankScoreBatch(nil, values); err == nil {
		t.Fatal("nil query metadata did not fail")
	}
	if _, err := newRerankScoreBatch(
		&semanticQueryEmbedding{scoreMask: []bool{false}},
		values,
	); err == nil {
		t.Fatal("query without active tokens did not fail")
	}
}

func TestMeanMaxSimPassesStrictBoundary(t *testing.T) {
	threshold := rerankAcceptanceMinimumMeanMaxSim
	if meanMaxSimPasses(threshold, 1, threshold) {
		t.Fatal("threshold equality passed")
	}
	if meanMaxSimPasses(math.Nextafter32(threshold, 0), 1, threshold) {
		t.Fatal("the float32 below the threshold passed")
	}
	if !meanMaxSimPasses(math.Nextafter32(threshold, 1), 1, threshold) {
		t.Fatal("the float32 above the threshold did not pass")
	}
}

func TestRerankAcceptanceCalibrationContract(t *testing.T) {
	const wantCompatibility = "lateon-code-edge-proxy-no-symbol-centroids-v5-76339ee2c2f5a452923bf85c0a72d6bb"
	if hybridAcceptanceMinimumMeanMaxSim != 0.600 {
		t.Fatalf("joined minimum=%g, want 0.600", hybridAcceptanceMinimumMeanMaxSim)
	}
	if rerankAcceptanceMinimumMeanMaxSim != 0.575 {
		t.Fatalf("lexical re-rank minimum=%g, want 0.575", rerankAcceptanceMinimumMeanMaxSim)
	}
	if rerankCandidateLimit != 128 {
		t.Fatalf("candidate pool=%d, want calibrated size 128", rerankCandidateLimit)
	}
	if hybridBranchQuota != 64 || hybridSemanticUnitLimit != 1024 {
		t.Fatalf(
			"joined candidate inputs: branch quota=%d semantic units=%d, want 64 and 1024",
			hybridBranchQuota,
			hybridSemanticUnitLimit,
		)
	}
	if hybridAcceptanceRoute != "joined" || rerankAcceptanceRoute != "lexical-rerank" {
		t.Fatalf("routes=%q and %q", hybridAcceptanceRoute, rerankAcceptanceRoute)
	}
	if got := semanticModelCompatibility(); got != wantCompatibility {
		t.Fatalf("bundled compatibility=%q, want %q", got, wantCompatibility)
	}
}

func TestApplyRerankAcceptanceRejectsWholeLowSupportQuery(t *testing.T) {
	logs := captureTestLogs(t, slog.LevelDebug)
	candidates := []rerankCandidate{
		{result: rerankTestResult("equal.go", 2)},
		{result: rerankTestResult("low.go", 1)},
	}
	batch := rerankScoreBatch{
		values:            []float32{1, 0.8},
		activeQueryTokens: 2,
	}

	policy := newRerankAcceptancePolicy(0.6)
	policy.route = hybridAcceptanceRoute
	got, gotBatch, appendRelaxed, err := applyRerankAcceptance(
		candidates,
		batch,
		nil,
		policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(gotBatch.values) != 0 || appendRelaxed {
		t.Fatalf("rejected query kept candidates=%d scores=%v tail=%t", len(got), gotBatch.values, appendRelaxed)
	}
	records := logs.Records()
	if len(records) != 1 || records[0].Message != "Rejected model expansion" {
		t.Fatalf("rejection logs=%v", records)
	}
	attrs := testLogAttrs(records[0])
	if attrs["route"] != hybridAcceptanceRoute ||
		attrs["reason"] != "score not above minimum" ||
		attrs["best_mean_max_sim"] != float64(float32(0.5)) ||
		attrs["minimum_mean_max_sim"] != float64(float32(0.6)) {
		t.Fatalf("rejection log attributes=%v", attrs)
	}
}

func TestApplyRerankAcceptanceAdmitsWholeSupportedQuery(t *testing.T) {
	logs := captureTestLogs(t, slog.LevelDebug)
	candidates := []rerankCandidate{
		{result: rerankTestResult("weak.go", 2)},
		{result: rerankTestResult("strong.go", 1)},
	}
	batch := rerankScoreBatch{
		values:            []float32{0.4, 1.5},
		activeQueryTokens: 2,
	}
	policy := newRerankAcceptancePolicy(0.6)
	policy.route = rerankAcceptanceRoute

	got, gotBatch, appendRelaxed, err := applyRerankAcceptance(
		candidates,
		batch,
		nil,
		policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !appendRelaxed || !reflect.DeepEqual(got, candidates) ||
		!reflect.DeepEqual(gotBatch.values, batch.values) {
		t.Fatalf("accepted query changed candidates=%+v scores=%v tail=%t", got, gotBatch.values, appendRelaxed)
	}
	records := logs.Records()
	if len(records) != 1 || records[0].Message != "Accepted model expansion" {
		t.Fatalf("acceptance logs=%v", records)
	}
	attrs := testLogAttrs(records[0])
	if attrs["best_mean_max_sim"] != float64(float32(0.75)) ||
		attrs["route"] != rerankAcceptanceRoute ||
		attrs["reason"] != "score above minimum" || attrs["candidates"] != int64(2) {
		t.Fatalf("acceptance log attributes=%v", attrs)
	}
}

func TestApplyRerankAcceptanceStrictEvidencePreservesExpansion(t *testing.T) {
	logs := captureTestLogs(t, slog.LevelDebug)
	strict := rerankTestResult("strict.go", 2)
	expanded := rerankTestResult("expanded.go", 1)
	candidates := []rerankCandidate{{result: strict}, {result: expanded}}
	batch := rerankScoreBatch{values: []float32{0.1, 0.1}, activeQueryTokens: 1}

	policy := newRerankAcceptancePolicy(0.6)
	policy.route = hybridAcceptanceRoute
	got, gotBatch, appendRelaxed, err := applyRerankAcceptance(
		candidates,
		batch,
		[]corpusSearchResult{strict},
		policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !appendRelaxed || !reflect.DeepEqual(got, candidates) ||
		!reflect.DeepEqual(gotBatch.values, batch.values) {
		t.Fatalf("strict evidence changed candidates=%+v scores=%v tail=%t", got, gotBatch.values, appendRelaxed)
	}
	records := logs.Records()
	if len(records) != 1 || records[0].Message != "Accepted model expansion" {
		t.Fatalf("strict acceptance logs=%v", records)
	}
	attrs := testLogAttrs(records[0])
	if attrs["route"] != hybridAcceptanceRoute ||
		attrs["reason"] != "strict lexical support" || attrs["strict_files"] != int64(1) {
		t.Fatalf("strict acceptance log attributes=%v", attrs)
	}
}

func TestApplyRerankAcceptanceTruncatedQueryDropsExpansion(t *testing.T) {
	strict := rerankTestResult("strict.go", 10)
	expanded := rerankTestResult("expanded.go", 9)
	candidates := []rerankCandidate{
		{result: strict, lexicalRank: 1},
		{result: expanded, lexicalRank: 2},
	}
	batch := rerankScoreBatch{
		values:            []float32{100, 100},
		activeQueryTokens: 2,
		queryTruncated:    true,
	}
	got, gotBatch, appendRelaxed, err := applyRerankAcceptance(
		candidates,
		batch,
		[]corpusSearchResult{strict},
		newRerankAcceptancePolicy(0.6),
	)
	if err != nil {
		t.Fatal(err)
	}
	if appendRelaxed || len(got) != 0 || len(gotBatch.values) != 0 {
		t.Fatalf("strict-only result=%+v scores=%v tail=%t", got, gotBatch.values, appendRelaxed)
	}
}

func TestApplyRerankAcceptanceMissingPolicyFails(t *testing.T) {
	candidates := []rerankCandidate{{result: rerankTestResult("expanded.go", 1)}}
	batch := rerankScoreBatch{values: []float32{-10}, activeQueryTokens: 1}
	if _, _, _, err := applyRerankAcceptance(candidates, batch, nil, nil); err == nil {
		t.Fatal("missing acceptance policy did not fail")
	}
}

func TestApplyRerankAcceptanceRejectsInvalidInputs(t *testing.T) {
	candidates := []rerankCandidate{{result: rerankTestResult("expanded.go", 1)}}
	validPolicy := newRerankAcceptancePolicy(0.6)
	for _, test := range []struct {
		name   string
		batch  rerankScoreBatch
		policy *rerankAcceptancePolicy
	}{
		{name: "wrong score count", batch: rerankScoreBatch{activeQueryTokens: 1}, policy: validPolicy},
		{name: "no query tokens", batch: rerankScoreBatch{values: []float32{1}}, policy: validPolicy},
		{name: "non-finite score", batch: rerankScoreBatch{values: []float32{float32(math.NaN())}, activeQueryTokens: 1}, policy: validPolicy},
		{name: "non-finite threshold", batch: rerankScoreBatch{values: []float32{1}, activeQueryTokens: 1}, policy: newRerankAcceptancePolicy(float32(math.Inf(1)))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, err := applyRerankAcceptance(candidates, test.batch, nil, test.policy); err == nil {
				t.Fatal("invalid acceptance input did not fail")
			}
		})
	}
}

func TestFuseRerankCandidatesCannotRestoreRejectedRelaxedTail(t *testing.T) {
	rejected := rerankTestResult("rejected.go", 8)
	unscoredTail := rerankTestResult("tail.go", 7)
	candidates := []rerankCandidate{{result: rejected, lexicalRank: 1}}
	batch := rerankScoreBatch{values: []float32{0.6}, activeQueryTokens: 1}
	got, err := fuseRerankCandidates(
		candidates,
		batch,
		nil,
		[]corpusSearchResult{rejected, unscoredTail},
		newRerankAcceptancePolicy(0.6),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("rejected fusion restored %v", got)
	}
}

func TestFuseRerankCandidatesTruncatedQueryAppendsStrictTailOnly(t *testing.T) {
	strict := rerankTestResult("strict.go", 10)
	secondStrict := rerankTestResult("second-strict.go", 8)
	expanded := rerankTestResult("expanded.go", 9)
	unscoredRelaxed := rerankTestResult("relaxed-tail.go", 8)
	candidates := []rerankCandidate{
		{result: strict, lexicalRank: 1},
		{result: expanded, lexicalRank: 2},
		{result: secondStrict, lexicalRank: 3},
	}
	batch := rerankScoreBatch{
		values:            []float32{1, 100, 2},
		activeQueryTokens: 1,
		queryTruncated:    true,
	}
	got, err := fuseRerankCandidates(
		candidates,
		batch,
		[]corpusSearchResult{strict, secondStrict},
		[]corpusSearchResult{strict, expanded, secondStrict, unscoredRelaxed},
		newRerankAcceptancePolicy(0.6),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].file.FileName != "strict.go" ||
		got[1].file.FileName != "second-strict.go" {
		t.Fatalf("strict-only fusion=%+v", got)
	}
}
