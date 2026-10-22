//go:build cgo && darwin && arm64

package main

import (
	"context"
	"strings"
	"testing"
)

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
			scorer := &hybridTestScorer{embedErr: errDegenerateSemanticVector}

			// Start with no index. Query preparation and final scoring still
			// work when the vector build fails, so the strict file returns; a
			// relaxed match alone is no candidate.
			output, err := runHybridTestSearch(t, "alpha beta", []string{root}, defaultSearchPolicy(), func(context.Context) (semanticModel, error) {
				return scorer, nil
			})
			if err != nil || !strings.Contains(output, "strict.go") || strings.Contains(output, "relaxed.go") {
				t.Fatalf("embedding fault fallback: output=%q error=%v", output, err)
			}
			if scorer.embedCalls.Load() != 1 || scorer.prepareCalls.Load() != 1 ||
				scorer.scoreCalls.Load() != 1 || scorer.closeCalls.Load() != 1 {
				t.Errorf("model calls: embed=%d prepare=%d score=%d close=%d, want 1/1/1/1",
					scorer.embedCalls.Load(), scorer.prepareCalls.Load(), scorer.scoreCalls.Load(), scorer.closeCalls.Load())
			}
			if record := readLateOnProviderRecord(verdict); record.faults != 1 {
				t.Errorf("provider record=%+v, want one fault without rejection", record)
			}
		})
	}
}
