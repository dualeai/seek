package main

import "testing"

// TestCapsInvariants mirrors the compile-time byte-budget guards in caps.go
// and reports the values when a dependent constant changes.
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
		// Account for one pending window per consumer and two maximum-sized
		// documents at the in-flight rotation point.
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
