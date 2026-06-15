package main

import "testing"

// TestCapsInvariants asserts the byte-budget invariants at runtime as a
// belt-and-suspenders companion to the compile-time guards in caps.go
// (the `const _ = uint(...)` lines that underflow on violation). The
// runtime form makes a violation inspectable in test failure logs and
// catches drift introduced by hand-editing one constant without checking
// its dependents.
func TestCapsInvariants(t *testing.T) {
	t.Run("worker_cap_positive", func(t *testing.T) {
		if corpusWorkerCap < 1 {
			t.Fatalf("corpusWorkerCap=%d must be ≥1", corpusWorkerCap)
		}
	})
	t.Run("single_doc_fits_in_flight", func(t *testing.T) {
		if maxInFlightBytes < maxIndexedDocumentBytes {
			t.Fatalf("single-doc invariant: maxInFlightBytes=%d < maxIndexedDocumentBytes=%d", maxInFlightBytes, maxIndexedDocumentBytes)
		}
	})
	t.Run("nway_windowed_fit", func(t *testing.T) {
		// N concurrent consumers each fully-pending plus one in-rotation
		// reader Acquire. The pre-PR2 formula (window=budget/2) failed
		// this at corpusWorkerCap≥3 because peak in-flight grew as N²
		// while budget grew as N. The current formula keeps the peak at
		// budget/2 + doc, independent of N.
		required := corpusWorkerCap*defaultIndexWindowBytes + 2*maxIndexedDocumentBytes
		if maxInFlightBytes < required {
			t.Fatalf("N-way windowed-fit: maxInFlightBytes=%d < required=%d (N*window=%d + 2*doc=%d)",
				maxInFlightBytes, required, corpusWorkerCap*defaultIndexWindowBytes, 2*maxIndexedDocumentBytes)
		}
	})
	t.Run("window_positive", func(t *testing.T) {
		// Sanity: window must be large enough for forward progress on a
		// max-sized doc. If window < doc, the tipping check
		// (pending >= window) fires immediately on the first doc, leading
		// to per-doc Finish() calls and pathological I/O patterns.
		if defaultIndexWindowBytes < maxIndexedDocumentBytes {
			t.Fatalf("window=%d < maxIndexedDocumentBytes=%d (forward progress requires window ≥ one max doc)",
				defaultIndexWindowBytes, maxIndexedDocumentBytes)
		}
	})
}
