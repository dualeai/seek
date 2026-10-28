//go:build cgo && darwin && arm64

package main

import (
	"strings"
	"testing"
)

// TestRunSearchCommandEmbeddingFaultRecordsOnce pins the accelerated provider
// and gives a model whose vector build fails with a provider fault
// (errDegenerateSemanticVector). It searches a folder node and a Git node. The
// search still prints the strict file and calls each model stage once. It
// records one provider fault with no rejection.
func TestRunSearchCommandEmbeddingFaultRecordsOnce(t *testing.T) {
	for _, kind := range []string{"folder", "git"} {
		t.Run(kind, func(t *testing.T) {
			requireTools(t)
			setTestUserCache(t)
			t.Chdir(t.TempDir())
			root := t.TempDir()
			if kind == "git" {
				root = initEmptyGitRepo(t)
			}
			writeFileAt(t, root, "strict.go", "package sample\n// alpha beta\n")
			writeFileAt(t, root, "relaxed.go", "package sample\n// alpha only\n")
			if kind == "git" {
				gitRunIn(t, root, "add", ".")
				gitRunIn(t, root, "commit", "-m", "search fixture")
			}
			verdict := pinAcceleratedProviderForTest(t)
			scorer := observeModel(&hybridTestScorer{embedErr: errDegenerateSemanticVector})

			// Start with no index. Query preparation and final scoring still
			// work when the vector build fails, so the strict file returns; a
			// relaxed match alone is no candidate.
			output, err := seekWith(t, scorer.factory(), "alpha beta", root)
			if err != nil || !strings.Contains(output, "strict.go") || strings.Contains(output, "relaxed.go") {
				t.Fatalf("embedding fault fallback: output=%q error=%v", output, err)
			}
			if scorer.embeds.Load() != 1 || scorer.prepares.Load() != 1 ||
				scorer.scores.Load() != 1 || scorer.closes.Load() != 1 {
				t.Errorf("model calls: embed=%d prepare=%d score=%d close=%d, want 1/1/1/1",
					scorer.embeds.Load(), scorer.prepares.Load(), scorer.scores.Load(), scorer.closes.Load())
			}
			if record := readLateOnProviderRecord(verdict); record.faults != 1 {
				t.Errorf("provider record=%+v, want one fault without rejection", record)
			}
		})
	}
}
