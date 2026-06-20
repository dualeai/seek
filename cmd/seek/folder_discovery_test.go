package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// scanFolderForTest invokes scanFolderCorpus with a discovery callback
// captured into the returned slice. Hides the boilerplate around plan
// construction + temporary cache dir.
func scanFolderForTest(t *testing.T, root string) (string, []gitBoundary) {
	t.Helper()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var discovered []gitBoundary
	plan.discover = func(b gitBoundary) bool {
		mu.Lock()
		discovered = append(discovered, b)
		mu.Unlock()
		return true
	}
	hash, _, _, err := scanFolderCorpus(t.Context(), plan, false)
	if err != nil {
		t.Fatalf("scanFolderCorpus: %v", err)
	}
	return hash, discovered
}

// TestFolderWalkerDiscoversNestedGit — parent folder containing one
// nested git repo: walker emits exactly one boundary via the callback
// and the boundary's RepoDir matches the nested repo path.
func TestFolderWalkerDiscoversNestedGit(t *testing.T) {
	root := canonTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "sub", "repo")
	writeMinimalGitRepo(t, nested)
	if err := os.WriteFile(filepath.Join(nested, "src.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, discovered := scanFolderForTest(t, root)
	if len(discovered) != 1 {
		t.Fatalf("discovered=%d, want 1", len(discovered))
	}
	if discovered[0].RepoDir != nested {
		t.Fatalf("RepoDir=%q, want %q", discovered[0].RepoDir, nested)
	}
}

// TestFolderWalkerDepth3 — nested at a/b/c/repo/.git; discovery must
// find it regardless of depth.
func TestFolderWalkerDepth3(t *testing.T) {
	root := canonTempDir(t)
	deep := filepath.Join(root, "a", "b", "c", "repo")
	writeMinimalGitRepo(t, deep)
	_, discovered := scanFolderForTest(t, root)
	if len(discovered) != 1 {
		t.Fatalf("discovered=%d, want 1 (depth-3 boundary)", len(discovered))
	}
	if discovered[0].RepoDir != deep {
		t.Fatalf("RepoDir=%q, want %q", discovered[0].RepoDir, deep)
	}
}

// TestFolderFingerprintFlipsOnBoundaryAppear — fingerprint of a parent
// changes when a nested .git is added. Defends against regressions in
// emitBoundaryMarker's contribution.
func TestFolderFingerprintFlipsOnBoundaryAppear(t *testing.T) {
	root := canonTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := scanFolderForTest(t, root)

	nested := filepath.Join(root, "newrepo")
	writeMinimalGitRepo(t, nested)
	after, _ := scanFolderForTest(t, root)

	if before == after {
		t.Fatalf("fingerprint unchanged after boundary appeared: %s", before)
	}
}

// TestFolderFingerprintStableUnderNestedCommit — committing inside a
// nested repo must not change the parent's fingerprint because the
// nested subtree is carved out by the boundary marker.
func TestFolderFingerprintStableUnderNestedCommit(t *testing.T) {
	root := canonTempDir(t)
	nested := filepath.Join(root, "repo")
	writeMinimalGitRepo(t, nested)
	if err := os.WriteFile(filepath.Join(nested, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := scanFolderForTest(t, root)

	// Simulate a "commit" inside the nested repo by adding content.
	// The .git entry's dev:ino:mtime is what controls the marker; the
	// working-tree file changes alone must NOT affect the parent's
	// fingerprint contribution.
	if err := os.WriteFile(filepath.Join(nested, "a.go"), []byte("package main\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "b.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := scanFolderForTest(t, root)

	if before != after {
		t.Fatalf("parent fingerprint changed under nested working-tree edit: before=%s after=%s", before, after)
	}
}

// TestFolderWalkerDedupesPhysicalRepo — same repo reached via the
// canonical path and via a symlink must produce ONE pool plan
// (corpusID-based dedup). The discovery callback is called twice but
// the cap counter only fires for genuinely-new repos.
func TestFolderWalkerDedupesPhysicalRepo(t *testing.T) {
	root := canonTempDir(t)
	canonical := filepath.Join(root, "real")
	writeMinimalGitRepo(t, canonical)
	if err := os.Symlink(canonical, filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// Note: detectGitBoundary refuses symlinked `.git`; the symlink
	// here points to the WORKING TREE, not `.git`. Walker should
	// already skip symlinks before reaching detection (per existing
	// walker policy at folder_indexer.go entry-symlink check).
	_, discovered := scanFolderForTest(t, root)
	if len(discovered) != 1 {
		t.Fatalf("discovered=%d, want 1 (symlinked alias should be skipped by walker)", len(discovered))
	}
}

// TestNFSGateDisablesDiscovery — when isOnNFS returns true,
// discoveryEnabledForPlan must short-circuit so the walker descends
// into nested repos rather than emitting boundaries. The test stubs
// the gate at the per-plan level by leaving plan.discover nil
// (equivalent semantics: discovery off).
func TestNFSGateDisablesDiscovery(t *testing.T) {
	root := canonTempDir(t)
	nested := filepath.Join(root, "repo")
	writeMinimalGitRepo(t, nested)
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatal(err)
	}
	// discover=nil simulates the gate: walker treats the nested .git
	// like any other metadata dir (skipped by name) and descends
	// through other content. The nested .git is name-skipped at
	// isFolderMetadataDir, so the working tree files would still be
	// indexed. Asserting absence of discovery callbacks is the
	// contract.
	plan.discover = nil
	hash, _, _, err := scanFolderCorpus(t.Context(), plan, false)
	if err != nil {
		t.Fatalf("scanFolderCorpus: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}
	// Add a file inside the nested repo's working tree; without
	// discovery the walker mixes it into the parent's hash. Mutation
	// → fingerprint MUST flip.
	if err := os.WriteFile(filepath.Join(nested, "extra.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash2, _, _, err := scanFolderCorpus(t.Context(), plan, false)
	if err != nil {
		t.Fatalf("scanFolderCorpus: %v", err)
	}
	if hash == hash2 {
		t.Fatal("nested working-tree edit should flip parent hash when discovery disabled")
	}
}

func TestExplicitFolderExcludeSuppressesDescentWhenDiscoveryDisabled(t *testing.T) {
	root := canonTempDir(t)
	nested := filepath.Join(root, "repo")
	writeMinimalGitRepo(t, nested)
	if err := os.WriteFile(filepath.Join(nested, "leaked.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpusWithExclusions(root, info, []string{nested})
	if err != nil {
		t.Fatal(err)
	}
	plan.discover = nil

	_, selected, _, err := scanFolderCorpus(t.Context(), plan, true)
	if err != nil {
		t.Fatalf("scanFolderCorpus: %v", err)
	}
	for _, candidate := range selected {
		if strings.Contains(candidate.name, "leaked.txt") {
			t.Fatalf("explicit excluded child Git root leaked into parent folder corpus: %q", candidate.name)
		}
	}
}

// TestCapExhaustionFallsBackToPlainDescent — when the discover
// callback rejects a boundary (cap full / dedup / build failure), the
// walker must descend into the subtree as a plain folder rather than
// silently dropping its content. Defends against the pre-fix bug where
// tryDiscoverBoundary emitted a marker + suppressed descent regardless
// of the enqueue result, losing nested content to no corpus.
func TestCapExhaustionFallsBackToPlainDescent(t *testing.T) {
	root := canonTempDir(t)
	nested := filepath.Join(root, "rejected-repo")
	writeMinimalGitRepo(t, nested)
	if err := os.WriteFile(filepath.Join(nested, "marker-file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Discover callback that ALWAYS rejects (simulates cap exhausted).
	rejected := 0
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatal(err)
	}
	plan.discover = func(b gitBoundary) bool {
		rejected++
		return false
	}
	_, selected, _, err := scanFolderCorpus(t.Context(), plan, true)
	if err != nil {
		t.Fatalf("scanFolderCorpus: %v", err)
	}
	if rejected == 0 {
		t.Fatal("discover callback was not invoked; walker is not classifying boundaries")
	}
	// The rejected boundary's content MUST appear in the parent folder
	// corpus's selected files — that's the entire point of the
	// fall-through fix.
	var foundContent bool
	for _, c := range selected {
		if strings.HasSuffix(c.name, "marker-file.txt") {
			foundContent = true
			break
		}
	}
	if !foundContent {
		t.Fatal("rejected boundary's content not indexed under parent; cap-exhaustion would silently drop nested repos")
	}
}

// TestDedupHitMustSuppressDescent — regression guard for the
// double-indexing bug surfaced by a real-world workload (call-center-ai
// .venv content appearing in parent folder corpus results on the
// second consecutive search). The flow:
//
//  1. ensureFolderCorpusFresh's fingerprint pass discovers boundary,
//     pool.Enqueue accepts (sync.Map LoadOrStore stores fresh).
//  2. ensureFolderCorpusFresh's state pass discovers SAME boundary;
//     pool.Enqueue LoadOrStore now returns loaded=true → Enqueue
//     returns false.
//  3. PRE-FIX: discoverNestedGit returned that false →
//     tryDiscoverBoundary descended into the nested repo → parent
//     corpus ate the entire working tree including gitignored content.
//
// Test invokes pool.discoverNestedGit (the real production callback)
// twice with the same boundary, then runs scanFolderCorpus. A leaked
// content file inside the nested repo must NOT appear in the parent's
// selected candidates either time.
func TestDedupHitMustSuppressDescent(t *testing.T) {
	root := canonTempDir(t)
	nested := filepath.Join(root, "nested-repo")
	writeMinimalGitRepo(t, nested)
	if err := os.WriteFile(filepath.Join(nested, "leaked.venv"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(root, info)
	if err != nil {
		t.Fatal(err)
	}

	// Real corpusPool with no-op worker — we only care about the dedup
	// behavior of pool.discoverNestedGit, not actual indexing.
	pool, g := newTestPool(t, noopPoolWorker, maxDiscoveredCorpora+4)
	plan.discover = pool.discoverNestedGit

	// First walk: pool.seen empty → fresh enqueue → walker suppresses
	// descent.
	_, selected1, _, err := scanFolderCorpus(t.Context(), plan, true)
	if err != nil {
		t.Fatalf("first scanFolderCorpus: %v", err)
	}
	for _, c := range selected1 {
		if strings.Contains(c.name, "leaked.venv") {
			t.Fatalf("first pass already leaked nested content: %q", c.name)
		}
	}

	// Second walk: same boundary → pool.seen LoadOrStore returns loaded
	// → Enqueue returns false. Walker MUST still suppress descent.
	_, selected2, _, err := scanFolderCorpus(t.Context(), plan, true)
	if err != nil {
		t.Fatalf("second scanFolderCorpus: %v", err)
	}
	for _, c := range selected2 {
		if strings.Contains(c.name, "leaked.venv") {
			t.Fatalf("dedup-hit caused walker to descend into nested repo and index gitignored content under parent corpus: leaked=%q", c.name)
		}
	}

	// Drain pool so test exits cleanly.
	if err := g.Wait(); err != nil {
		t.Fatalf("pool wait: %v", err)
	}
	close(pool.resultsCh)
}

// TestBrokenSubmoduleGracefulSkip — .git file with a pointer that
// dangles outside scanRoot must NOT enqueue a discovered plan, and
// the walker must continue without error.
func TestBrokenSubmoduleGracefulSkip(t *testing.T) {
	root := canonTempDir(t)
	bad := filepath.Join(root, "bad-sub")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	// gitdir: points to a non-existent path.
	if err := os.WriteFile(filepath.Join(bad, ".git"), []byte("gitdir: "+filepath.Join(root, "missing")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, discovered := scanFolderForTest(t, root)
	if len(discovered) != 0 {
		t.Fatalf("discovered=%d, want 0 (broken submodule must be skipped)", len(discovered))
	}
}
