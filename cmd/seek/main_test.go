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
