package main

import "testing"

// TestDirtyFileSetFromState checks that the dirty set holds each path of the
// state exactly. A node read hides the committed copy of each file in this
// set (currentCopies), and semantic retrieval skips the rows of each such
// file (nodeRowFilter). So a set that matched a substring, a prefix, or a
// path with added white space would hide other files. The members and
// non-members are hand-picked.
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
	// No substring, prefix, or path with a trailing space or newline matches.
	for _, miss := range []string{"a/b", "b.go", "a", "a/b.go\n", "c.py ", ""} {
		if set.contains(miss) {
			t.Errorf("non-member %q matched; the set must hold exact paths only", miss)
		}
	}
}
