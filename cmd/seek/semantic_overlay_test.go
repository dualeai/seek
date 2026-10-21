package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRunDirtySearchOverlay edits a committed file to drop its meaning and
// adds an untracked file with it: vector retrieval reads the work tree, so
// the untracked file answers as [uncommitted], the edited file does not, and
// only the two dirty files embed.
func TestRunDirtySearchOverlay(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.go", "package a\n// SEMANTIC_TARGET handles the request\n")
	writeFixtureFile(t, filepath.Join(repo, "keep.go"), "package a\n// alpha beta keep\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "keep")
	setTestUserCache(t)
	t.Chdir(repo)
	if output, _, err := describe(t, "alpha beta", "."); err != nil || !strings.Contains(output, "## a.go") {
		t.Fatalf("committed search: err=%v\n%s", err, output)
	}
	writeFixtureFile(t, filepath.Join(repo, "a.go"), "package a\n// nothing to see\n")
	writeFixtureFile(t, filepath.Join(repo, "b.go"), "package b\n// SEMANTIC_TARGET dispatches the call\n")
	output, scorer, err := describe(t, "alpha beta", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "## b.go (Go) [uncommitted]") || strings.Contains(output, "## a.go") {
		t.Fatalf("want the untracked semantic target and not the edited file:\n%s", output)
	}
	scorer.mu.Lock()
	embedded := slices.Clone(scorer.embeddedPaths)
	scorer.mu.Unlock()
	slices.Sort(embedded)
	if embedded = slices.Compact(embedded); !slices.Equal(embedded, []string{"a.go", "b.go"}) {
		t.Fatalf("embedded %v, want only the dirty files a.go and b.go", embedded)
	}
	// The same dirty tree again embeds nothing.
	if _, scorer, err = describe(t, "alpha beta", "."); err != nil || scorer.embedCalls.Load() != 0 {
		t.Fatalf("an unchanged dirty tree embedded %d batches (err=%v)", scorer.embedCalls.Load(), err)
	}
}

// TestNoHeadRepoUsesVectors searches a repository with no commit: its
// overlay holds every file, so a semantic-only file answers.
func TestNoHeadRepoUsesVectors(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.path("fresh")
	gitRunIn(t, f.root, "init", "-q", repo)
	writeFixtureFile(t, filepath.Join(repo, "target.go"), "package fresh\n// SEMANTIC_TARGET routes the request\n")
	writeFixtureFile(t, filepath.Join(repo, "other.go"), "package fresh\n// alpha beta other\n")
	setTestUserCache(t)
	t.Chdir(repo)
	output, scorer, err := describe(t, "alpha beta", ".")
	if err != nil {
		t.Fatal(err)
	}
	if scorer.embedCalls.Load() == 0 || !strings.Contains(output, "## target.go (Go) [uncommitted]") {
		t.Fatalf("want the semantic-only file of a repository with no commit (embeds=%d):\n%s", scorer.embedCalls.Load(), output)
	}
}

// TestOverlayStoreBoundedUnderRepeatedSaves saves one dirty file three times
// with new content: after the last search one overlay generation stays beside
// the base.
func TestOverlayStoreBoundedUnderRepeatedSaves(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.go", "package a\n// alpha beta committed\n")
	setTestUserCache(t)
	t.Chdir(repo)
	for save := range 3 {
		writeFixtureFile(t, filepath.Join(repo, "a.go"), "package a\n// alpha beta save "+strings.Repeat("x", save+1)+"\n")
		if _, _, err := describe(t, "alpha beta", "."); err != nil {
			t.Fatalf("save %d: %v", save, err)
		}
	}
	plans, err := planCorpora(t.Context(), []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(plans[0].indexDir)
	if err != nil {
		t.Fatal(err)
	}
	generations := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), semanticGenerationPrefix) {
			generations++
		}
	}
	if generations != 2 {
		t.Fatalf("generations=%d, want the base and one overlay", generations)
	}
}

// TestOverlayDamageRepairs appends one byte to the overlay rows: the search
// that finds the damage clears the descriptor, so a later search rebuilds the
// overlay and the untracked file answers again.
func TestOverlayDamageRepairs(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.go", "package a\n// alpha beta committed\n")
	writeFixtureFile(t, filepath.Join(repo, "u.go"), "package u\n// SEMANTIC_TARGET routes the request\n")
	setTestUserCache(t)
	t.Chdir(repo)
	if output, _, err := describe(t, "alpha beta", "."); err != nil || !strings.Contains(output, "## u.go (Go) [uncommitted]") {
		t.Fatalf("first search: err=%v\n%s", err, output)
	}
	plans, err := planCorpora(t.Context(), []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := readJoinedGeneration(plans[0].cacheDir)
	if err != nil || descriptor.OverlayKey == "" {
		t.Fatalf("descriptor=%+v err=%v, want an overlay", descriptor, err)
	}
	rows, err := os.OpenFile(filepath.Join(plans[0].indexDir, semanticGenerationPrefix+descriptor.OverlayKey, semanticRowsFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rows.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	_, _, _ = describe(t, "alpha beta", ".")
	if output, _, err := describe(t, "alpha beta", "."); err != nil || !strings.Contains(output, "## u.go (Go) [uncommitted]") {
		t.Fatalf("the overlay was not repaired: err=%v\n%s", err, output)
	}
}

// TestOverlayColdBuildReusesTheStagedBase adds one line to a long committed
// file before the first search: the overlay reuses the windows that the base
// of the same build embedded, so the file embeds about once, not twice.
func TestOverlayColdBuildReusesTheStagedBase(t *testing.T) {
	requireTools(t)
	var body strings.Builder
	body.WriteString("package big\n")
	for index := range 300 {
		fmt.Fprintf(&body, "// alpha beta line %d of the long file\nfunc F%d() int { return %d }\n", index, index, index)
	}
	embeds := func(t *testing.T, edit bool) int {
		t.Helper()
		f := newGraphFixture(t)
		repo := f.GitRepo("repo", "big.go", body.String())
		if edit {
			writeFixtureFile(t, filepath.Join(repo, "big.go"), body.String()+"// one added line\n")
		}
		setTestUserCache(t)
		t.Chdir(repo)
		_, scorer, err := describe(t, "alpha beta", ".")
		if err != nil {
			t.Fatal(err)
		}
		scorer.mu.Lock()
		defer scorer.mu.Unlock()
		count := 0
		for _, path := range scorer.embeddedPaths {
			if path == "big.go" {
				count++
			}
		}
		return count
	}
	clean := embeds(t, false)
	dirty := embeds(t, true)
	if clean < 4 || dirty > clean+2 {
		t.Fatalf("clean embeds %d windows, the cold dirty search %d; want at most %d", clean, dirty, clean+2)
	}
}

// TestOverlayHasNoUSearchGraph builds an overlay: it holds no USearch shard,
// and its search still finds the untracked file.
func TestOverlayHasNoUSearchGraph(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.go", "package a\n// alpha beta committed\n")
	writeFixtureFile(t, filepath.Join(repo, "u.go"), "package u\n// SEMANTIC_TARGET routes the request\n")
	setTestUserCache(t)
	t.Chdir(repo)
	if output, _, err := describe(t, "alpha beta", "."); err != nil || !strings.Contains(output, "## u.go (Go) [uncommitted]") {
		t.Fatalf("err=%v\n%s", err, output)
	}
	plans, err := planCorpora(t.Context(), []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := readJoinedGeneration(plans[0].cacheDir)
	if err != nil || descriptor.OverlayKey == "" || descriptor.SemanticKey == "" {
		t.Fatalf("descriptor=%+v err=%v", descriptor, err)
	}
	for key, want := range map[string]bool{descriptor.SemanticKey: true, descriptor.OverlayKey: false} {
		shards, _ := filepath.Glob(filepath.Join(plans[0].indexDir, semanticGenerationPrefix+key, "*.usearch"))
		if (len(shards) > 0) != want {
			t.Fatalf("generation %s: USearch shards %v, want present=%t", key, shards, want)
		}
	}
}
