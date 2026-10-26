package main

// meanMaxSimPasses reports whether a raw LateOn score passes threshold as a
// lift per query token above the empty-document score. The raw score sums one
// maximum similarity per active query token, and emptyScore is that sum for
// the same query against a document with no text. A token with no topic
// matches every document about the same, so it adds about the same to both
// sums, and the lift holds only what the text of the document adds.
// Admission divides the lift by the count of active tokens, with the special
// tokens of each query window (CLS, the query marker, SEP), and uses a strict
// greater-than test (pipelineTauFile gives the cost of that count). The
// values are not relevance probabilities.
//
// A threshold on the score per token alone does not hold for long queries: on
// 11 small files, a sentence of 51 tokens with no topic gave each file 0.742
// to 0.756 per token, more than a short description gave the files that it
// found (0.669 to 0.696). The lifts of that sentence are -0.015 to -0.001.
func meanMaxSimPasses(score, emptyScore float32, activeQueryTokens int, threshold float32) bool {
	return (score-emptyScore)/float32(activeQueryTokens) > threshold
}
