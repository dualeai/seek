package main

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"testing"
)

// testContentID returns a content ID whose first byte is seed and whose other
// bytes are zero.
func testContentID(seed byte) semanticContentID {
	var id semanticContentID
	id[0] = seed
	return id
}

// TestMergeHitsIgnoresInputOrder merges the hits of four nodes 20 times, with
// the nodes and the hits of each node shuffled. Each merge gives the same
// hand-worked order and keeps all 24 hits. Each hit has its own content, so
// no hit collapses into another.
func TestMergeHitsIgnoresInputOrder(t *testing.T) {
	var nodes []nodeHits
	for node := range 4 {
		var hits nodeHits
		for file := range 6 {
			hits = append(hits, nodeHit{
				corpus:      corpusID(fmt.Sprintf("corpus-%d", node)),
				displayRoot: fmt.Sprintf("/root/n%d", node),
				path:        fmt.Sprintf("f%d.go", file),
				contentID:   testContentID(byte(node*10 + file + 1)),
				score:       float32(file % 3),
			})
		}
		nodes = append(nodes, hits)
	}
	// The tie key orders by score, then by display path. Score 2 (files 2 and
	// 5) comes first, then 1 (files 1 and 4), then 0 (files 0 and 3). Each
	// score is in node order. No node loses a hit.
	var want []string
	for _, files := range [][2]int{{2, 5}, {1, 4}, {0, 3}} {
		for node := range 4 {
			for _, file := range files {
				want = append(want, fmt.Sprintf("/root/n%d/f%d.go", node, file))
			}
		}
	}
	random := rand.New(rand.NewPCG(3, 4))
	for range 20 {
		shuffled := slices.Clone(nodes)
		random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for index := range shuffled {
			inner := slices.Clone(shuffled[index])
			random.Shuffle(len(inner), func(i, j int) { inner[i], inner[j] = inner[j], inner[i] })
			shuffled[index] = inner
		}
		merged := mergeNodeHits(slices.Concat(shuffled...))
		got := make([]string, len(merged))
		for index, hit := range merged {
			got[index] = hit.displayPath()
		}
		if !slices.Equal(got, want) {
			t.Fatalf("merged order:\ngot  %v\nwant %v", got, want)
		}
	}
}

// TestDuplicateCollapseRepresentative collapses copies with the same content
// and match into the copy with the smallest path. A dirty copy (other
// content) and two name-only files with no content ID stay separate.
func TestDuplicateCollapseRepresentative(t *testing.T) {
	same := testContentID(7)
	hits := nodeHits{
		{corpus: "c", displayRoot: "/z", path: "a.go", contentID: same, match: "m", score: 1},
		{corpus: "b", displayRoot: "/a", path: "a.go", contentID: same, match: "m", score: 1},
		{corpus: "d", displayRoot: "/m", path: "a.go", contentID: same, match: "m", score: 1},
		{corpus: "e", displayRoot: "/b", path: "a.go", contentID: testContentID(8), match: "m", score: 1},
		{corpus: "f", displayRoot: "/n", path: "x.bin", nameOnly: true, score: 1},
		{corpus: "g", displayRoot: "/o", path: "y.bin", nameOnly: true, score: 1},
	}
	merged := mergeNodeHits(hits)
	if len(merged) != 4 {
		t.Fatalf("merged=%+v, want one result for the three copies and three others", merged)
	}
	head := merged[0]
	also := make([]string, len(head.also))
	for index, duplicate := range head.also {
		also[index] = filepath.Join(duplicate.displayRoot, duplicate.path)
	}
	if head.displayPath() != "/a/a.go" || !slices.Equal(also, []string{"/m/a.go", "/z/a.go"}) {
		t.Fatalf("representative=%s also=%v, want /a/a.go with /m/a.go, /z/a.go", head.displayPath(), head.also)
	}
	if merged[1].displayPath() != "/b/a.go" || len(merged[1].also) != 0 {
		t.Fatalf("the copy with other content did not stay separate: %+v", merged[1])
	}
}

// TestAcceptanceIsPerFile scores the same relevant file inside a pool of 128
// and of 4,096 candidates. The verdict for that file is the same, because
// each model-added file passes on its own lift above the empty-document
// score. A strict file passes with a score of 0.
func TestAcceptanceIsPerFile(t *testing.T) {
	// Over 4 tokens above an empty-document score of 3, a noise score of 3.6
	// lifts (3.6 - 3) / 4 = 0.15, under tau_file 0.25. The relevant score of
	// 4.04 lifts (4.04 - 3) / 4 = 0.26, above it.
	const tokens, emptyScore = 4, 3
	verdict := func(poolSize int) bool {
		scored := make(nodeHits, poolSize)
		scores := make([]float32, poolSize)
		for index := range scored {
			scored[index] = nodeHit{corpus: "c", path: fmt.Sprintf("noise%05d.go", index), semantic: true}
			scores[index] = 3.6
		}
		scored[poolSize/2] = nodeHit{corpus: "c", path: "relevant.go", semantic: true}
		scores[poolSize/2] = 4.04
		accepted := acceptFiles(scored, scores, emptyScore, tokens, nil)
		return slices.ContainsFunc(accepted, func(hit nodeHit) bool { return hit.path == "relevant.go" }) &&
			len(accepted) == 1
	}
	if !verdict(128) || !verdict(4096) {
		t.Fatal("the relevant file's verdict depends on the pool size")
	}
	strict := nodeHits{{corpus: "c", path: "strict.go", strict: true}}
	if accepted := acceptFiles(strict, []float32{0}, emptyScore, tokens, nil); len(accepted) != 1 {
		t.Fatalf("a strict all-word file was not admitted: %+v", accepted)
	}
}

// TestCandidateFilesKeepsNameOnlyOutOfTheModel puts strict, semantic and
// name-only hits through the candidate set. The model scores strict and
// semantic files with no limit on their count. A name-only file enters only as
// a strict match and never gets a model score.
func TestCandidateFilesKeepsNameOnlyOutOfTheModel(t *testing.T) {
	hits := nodeHits{
		{path: "strict.go", strict: true},
		{path: "semantic.go", semantic: true},
		{path: "strict.bin", strict: true, nameOnly: true},
	}
	for index := range 300 {
		hits = append(hits, nodeHit{path: fmt.Sprintf("more%03d.go", index), semantic: true})
	}
	scored, nameOnly := candidateFiles(hits)
	if len(scored) != 302 || len(nameOnly) != 1 || nameOnly[0].path != "strict.bin" {
		t.Fatalf("scored=%d nameOnly=%+v", len(scored), nameOnly)
	}
	if slices.ContainsFunc(scored, func(hit nodeHit) bool { return hit.nameOnly }) {
		t.Fatal("a name-only file reached the model")
	}
}
