package main

// meanMaxSimPasses reports whether a raw LateOn score passes threshold as a
// MeanMaxSim: the raw score sums one maximum similarity per active query
// token, so it is divided by that count. Admission uses a strict greater-than
// test. The values are not relevance probabilities.
func meanMaxSimPasses(score float32, activeQueryTokens int, threshold float32) bool {
	return score/float32(activeQueryTokens) > threshold
}
