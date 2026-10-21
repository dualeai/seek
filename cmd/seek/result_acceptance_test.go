package main

import (
	"math"
	"testing"
)

// TestMeanMaxSimPassesStrictBoundary proves that a score equal to the
// threshold fails and the next float32 above it passes.
func TestMeanMaxSimPassesStrictBoundary(t *testing.T) {
	threshold := pipelineTauFile
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

// TestRerankAcceptanceCalibrationContract pins the inputs of the per-file
// acceptance: tau_file, the retrieval delta, the rank fusion, and the model
// compatibility that they were measured with. Recalibrate when one changes.
func TestRerankAcceptanceCalibrationContract(t *testing.T) {
	const wantCompatibility = "lateon-code-edge-proxy-no-symbol-centroids-v5-76339ee2c2f5a452923bf85c0a72d6bb"
	if pipelineTauFile != 0.6 {
		t.Fatalf("tau_file=%g, want the provisional 0.6", pipelineTauFile)
	}
	if semanticRetrievalDelta != 0.1158 {
		t.Fatalf("delta=%g, want 0.1158", semanticRetrievalDelta)
	}
	if rerankRRFConstant != 60 || rerankLexicalWeight != 2 {
		t.Fatalf("rank fusion k=%g weight=%g, want 60 and 2", rerankRRFConstant, rerankLexicalWeight)
	}
	if got := semanticModelCompatibility(); got != wantCompatibility {
		t.Fatalf("bundled compatibility=%q, want %q", got, wantCompatibility)
	}
}
