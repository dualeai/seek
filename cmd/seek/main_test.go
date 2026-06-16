package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrNoMatch_IsDistinct(t *testing.T) {
	// errNoMatch must not be confused with generic errors.
	generic := errors.New("something went wrong")
	if errors.Is(generic, errNoMatch) {
		t.Error("generic error should not match errNoMatch")
	}
}

func TestErrNoMatch_WrappedIsDetectable(t *testing.T) {
	// Even when wrapped, errors.Is must still detect errNoMatch so that
	// callers (main) can reliably map it to exit code 1.
	wrapped := fmt.Errorf("search: %w", errNoMatch)
	if !errors.Is(wrapped, errNoMatch) {
		t.Error("wrapped errNoMatch should be detectable via errors.Is")
	}
}

func TestExitCodeForError(t *testing.T) {
	if got := exitCodeForError(nil); got != 0 {
		t.Fatalf("nil error exit code: got %d, want 0", got)
	}
	if got := exitCodeForError(errNoMatch); got != 1 {
		t.Fatalf("no-match exit code: got %d, want 1", got)
	}
	if got := exitCodeForError(fmt.Errorf("wrapped: %w", errNoMatch)); got != 1 {
		t.Fatalf("wrapped no-match exit code: got %d, want 1", got)
	}
	if got := exitCodeForError(errors.New("boom")); got != 2 {
		t.Fatalf("generic error exit code: got %d, want 2", got)
	}
}

// TestCorporaInResults_DistinctIDsCount — display-mode regression
// guard. When folder-walker discovery surfaces nested git corpora,
// len(plans) reports the seed count (often 1) but the merged results
// span N corpora. The displayMode predicate must flip on the actual
// produced-from count so users see source disambiguation.
func TestCorporaInResults_DistinctIDsCount(t *testing.T) {
	if got := corporaInResults(nil); got != 0 {
		t.Errorf("nil results: got=%d, want 0", got)
	}
	if got := corporaInResults([]corpusSearchResult{}); got != 0 {
		t.Errorf("empty results: got=%d, want 0", got)
	}
	one := []corpusSearchResult{
		{corpusID: "a"},
		{corpusID: "a"},
		{corpusID: "a"},
	}
	if got := corporaInResults(one); got != 1 {
		t.Errorf("single corpus, 3 matches: got=%d, want 1", got)
	}
	mixed := []corpusSearchResult{
		{corpusID: "parent-folder"},
		{corpusID: "nested-git-A"},
		{corpusID: "nested-git-A"},
		{corpusID: "nested-git-B"},
	}
	if got := corporaInResults(mixed); got != 3 {
		t.Errorf("3 corpora mixed: got=%d, want 3", got)
	}
}
