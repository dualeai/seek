package main

import "testing"

// TestDirtyFileSetFromState pins the dirty-set as EXACT-membership. The
// formatter suppresses a committed match only when its FileName is in this set,
// so a substring/prefix/whitespace-tolerant set would wrongly hide unrelated
// files. Independent oracle: hand-picked members and non-members.
func TestDirtyFileSetFromState(t *testing.T) {
	if got := dirtyFileSetFromState(repoState{}); got != nil {
		t.Errorf("empty state must yield a nil set, got %v", got)
	}
	set := dirtyFileSetFromState(repoState{Files: []string{"a/b.go", "c.py"}})
	for _, member := range []string{"a/b.go", "c.py"} {
		if !set.contains(member) {
			t.Errorf("exact path %q must be a member", member)
		}
	}
	// Must NOT match by substring, prefix, or trailing whitespace/newline.
	for _, miss := range []string{"a/b", "b.go", "a", "a/b.go\n", "c.py ", ""} {
		if set.contains(miss) {
			t.Errorf("non-member %q matched — set must be exact, not substring", miss)
		}
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
