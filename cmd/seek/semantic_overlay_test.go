package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRunDirtySearchOverlay commits a.go, then edits it and adds an
// untracked b.go. The test model accepts every row, so the printed set is the
// set of files whose rows the search binds. Vector retrieval reads the work
// tree. After the edit, a.go answers with its new text and not the committed
// text, and b.go answers as [uncommitted]. Only the two dirty files embed.
// The base rows of a dirty file are no candidate. A committed row would not
// match the work-tree copy, and the search would report index damage.
func TestRunDirtySearchOverlay(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.go", "package a\n// handles the request\n")
	writeFixtureFile(t, filepath.Join(repo, "keep.go"), "package a\n// alpha beta keep\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "keep")
	setTestUserCache(t)
	t.Chdir(repo)
	output, _, err := stubSearch(t, payloadAccept, "alpha beta", ".")
	if got := headerPaths(output); err != nil || !slices.Equal(got, []string{"a.go", "keep.go"}) {
		t.Fatalf("committed search: err=%v headers=%v, want [a.go keep.go]:\n%s", err, got, output)
	}
	writeFixtureFile(t, filepath.Join(repo, "a.go"), "package a\n// nothing to see\n")
	writeFixtureFile(t, filepath.Join(repo, "b.go"), "package b\n// dispatches the call\n")
	logs := captureTestLogs(t, slog.LevelWarn)
	output, scorer, err := stubSearch(t, payloadAccept, "alpha beta", ".")
	if err != nil {
		t.Fatal(err)
	}
	if shards, vectors, skips, _ := repairWarnings(logs, 0); shards+vectors+skips != 0 {
		t.Fatalf("the dirty search reported damage: shard repairs=%d vector repairs=%d skips=%d, want none",
			shards, vectors, skips)
	}
	if got := headerPaths(output); !slices.Equal(got, []string{"a.go", "b.go", "keep.go"}) ||
		!strings.Contains(output, "## b.go (Go) [uncommitted]") || strings.Contains(output, "handles the request") {
		t.Fatalf("headers=%v, want [a.go b.go keep.go] with b.go [uncommitted] and no committed text of a.go:\n%s",
			got, output)
	}
	scorer.mu.Lock()
	embedded := slices.Clone(scorer.embeddedPaths)
	scorer.mu.Unlock()
	slices.Sort(embedded)
	if embedded = slices.Compact(embedded); !slices.Equal(embedded, []string{"a.go", "b.go"}) {
		t.Fatalf("embedded %v, want only the dirty files a.go and b.go", embedded)
	}
	// The same dirty tree again embeds nothing.
	if _, scorer, err = stubSearch(t, payloadAccept, "alpha beta", "."); err != nil || scorer.embedCalls.Load() != 0 {
		t.Fatalf("an unchanged dirty tree embedded %d batches (err=%v)", scorer.embedCalls.Load(), err)
	}
}

// TestNoHeadRepoUsesVectors searches a repository with no commit, so its
// overlay holds every file. The test model accepts every row. Both files
// answer as [uncommitted], and target.go comes from its rows only.
func TestNoHeadRepoUsesVectors(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.path("fresh")
	gitRunIn(t, f.root, "init", "-q", repo)
	writeFixtureFile(t, filepath.Join(repo, "target.go"), "package fresh\n// routes the request\n")
	writeFixtureFile(t, filepath.Join(repo, "other.go"), "package fresh\n// alpha beta other\n")
	setTestUserCache(t)
	t.Chdir(repo)
	output, scorer, err := stubSearch(t, payloadAccept, "alpha beta", ".")
	if err != nil {
		t.Fatal(err)
	}
	if got := headerPaths(output); scorer.embedCalls.Load() == 0 || !slices.Equal(got, []string{"other.go", "target.go"}) ||
		!strings.Contains(output, "## other.go (Go) [uncommitted]") ||
		!strings.Contains(output, "## target.go (Go) [uncommitted]") {
		t.Fatalf("headers=%v (embeds=%d), want other.go and target.go, both [uncommitted]:\n%s",
			got, scorer.embedCalls.Load(), output)
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
		if _, _, err := stubSearch(t, payloadFar, "alpha beta", "."); err != nil {
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

// TestOverlayDamageRepairs appends one byte to the overlay rows. The search
// that finds the damage clears the descriptor. So a later search embeds the
// untracked file again, and the file answers again. The test model accepts
// every row. So u.go, which holds no query word, prints exactly when the
// search binds its overlay rows.
func TestOverlayDamageRepairs(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.go", "package a\n// alpha beta committed\n")
	writeFixtureFile(t, filepath.Join(repo, "u.go"), "package u\n// routes the request\n")
	setTestUserCache(t)
	t.Chdir(repo)
	if output, _, err := stubSearch(t, payloadAccept, "alpha beta", "."); err != nil || !strings.Contains(output, "## u.go (Go) [uncommitted]") {
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
	_, _, _ = stubSearch(t, payloadAccept, "alpha beta", ".")
	output, scorer, err := stubSearch(t, payloadAccept, "alpha beta", ".")
	scorer.mu.Lock()
	embedded := slices.Contains(scorer.embeddedPaths, "u.go")
	scorer.mu.Unlock()
	if err != nil || !embedded || !strings.Contains(output, "## u.go (Go) [uncommitted]") {
		t.Fatalf("the overlay was not repaired: err=%v embedded u.go=%t\n%s", err, embedded, output)
	}
}

// TestOverlayColdBuildReusesTheStagedBase adds one line to a long committed
// file before the first search. The overlay reuses the windows that the base
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
		_, scorer, err := stubSearch(t, payloadFar, "alpha beta", ".")
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

// TestOverlayHasNoUSearchGraph builds an overlay, which holds no USearch
// shard. With a test model that accepts every row, its search still finds the
// untracked file, which holds no query word.
func TestOverlayHasNoUSearchGraph(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.go", "package a\n// alpha beta committed\n")
	writeFixtureFile(t, filepath.Join(repo, "u.go"), "package u\n// routes the request\n")
	setTestUserCache(t)
	t.Chdir(repo)
	if output, _, err := stubSearch(t, payloadAccept, "alpha beta", "."); err != nil || !strings.Contains(output, "## u.go (Go) [uncommitted]") {
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
