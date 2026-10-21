package main

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

func testContentID(seed byte) semanticContentID {
	var id semanticContentID
	id[0] = seed
	return id
}

// TestMergeHitsIgnoresInputOrder merges the same hits in shuffled node orders:
// the output is the same each time. Full ties have different content, since
// equal content collapses.
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
	want := mergeNodeHits(nodes...)
	random := rand.New(rand.NewPCG(3, 4))
	for range 20 {
		shuffled := slices.Clone(nodes)
		random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for index := range shuffled {
			inner := slices.Clone(shuffled[index])
			random.Shuffle(len(inner), func(i, j int) { inner[i], inner[j] = inner[j], inner[i] })
			shuffled[index] = inner
		}
		if got := mergeNodeHits(shuffled...); !reflect.DeepEqual(got, want) {
			t.Fatalf("merge depends on input order:\ngot  %+v\nwant %+v", got, want)
		}
	}
	if len(want) != 24 {
		t.Fatalf("merged %d hits, want all 24 (no per-node cut)", len(want))
	}
}

// TestDuplicateCollapseRepresentative collapses copies with the same content
// and match into the copy with the smallest path; a dirty copy (other
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
		also[index] = duplicate.displayPath()
	}
	if head.displayPath() != "/a/a.go" || !slices.Equal(also, []string{"/m/a.go", "/z/a.go"}) {
		t.Fatalf("representative=%s also=%v, want /a/a.go with /m/a.go, /z/a.go", head.displayPath(), head.also)
	}
	if merged[1].displayPath() != "/b/a.go" || len(merged[1].also) != 0 {
		t.Fatalf("the copy with other content did not stay separate: %+v", merged[1])
	}
}

// TestAcceptanceIsPerFile scores the same relevant file inside a pool of 128
// and of 4,096 candidates: the verdict for that file is the same, because
// each model-added file is accepted on its own score.
func TestAcceptanceIsPerFile(t *testing.T) {
	const tauFile = 0.6
	verdict := func(poolSize int) bool {
		scored := make(nodeHits, poolSize)
		scores := make([]float32, poolSize)
		for index := range scored {
			scored[index] = nodeHit{corpus: "c", path: fmt.Sprintf("noise%05d.go", index), semantic: true}
			scores[index] = 0.3 * 4
		}
		scored[poolSize/2] = nodeHit{corpus: "c", path: "relevant.go", semantic: true}
		scores[poolSize/2] = 0.61 * 4
		accepted, err := acceptFiles(scored, scores, 4, tauFile, nil)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(accepted.hits, func(hit nodeHit) bool { return hit.path == "relevant.go" }) &&
			len(accepted.hits) == 1
	}
	if !verdict(128) || !verdict(4096) {
		t.Fatal("the relevant file's verdict depends on the pool size")
	}
	strict := nodeHits{{corpus: "c", path: "strict.go", strict: true}}
	accepted, err := acceptFiles(strict, []float32{0}, 4, tauFile, nil)
	if err != nil || len(accepted.hits) != 1 {
		t.Fatalf("a strict all-word file was not admitted: %+v %v", accepted, err)
	}
}

// TestCandidateFilesKeepsNameOnlyOutOfTheModel puts strict, relaxed, semantic
// and name-only hits through the candidate set: strict and semantic files are
// scored with no count; a relaxed-only file is not (it cannot pass tau_file);
// a name-only file enters only as a strict match and never gets a model score.
func TestCandidateFilesKeepsNameOnlyOutOfTheModel(t *testing.T) {
	hits := nodeHits{
		{path: "strict.go", strict: true},
		{path: "relaxed.go", lexical: true},
		{path: "semantic.go", semantic: true},
		{path: "strict.bin", strict: true, nameOnly: true},
		{path: "relaxed.bin", lexical: true, nameOnly: true},
	}
	for index := range 300 {
		hits = append(hits, nodeHit{path: fmt.Sprintf("more%03d.go", index), semantic: true})
	}
	scored, nameOnly := candidateFiles(hits)
	if len(scored) != 302 || len(nameOnly) != 1 || nameOnly[0].path != "strict.bin" ||
		slices.ContainsFunc(scored, func(hit nodeHit) bool { return hit.path == "relaxed.go" }) {
		t.Fatalf("scored=%d nameOnly=%+v", len(scored), nameOnly)
	}
	if slices.ContainsFunc(scored, func(hit nodeHit) bool { return hit.nameOnly }) {
		t.Fatal("a name-only file reached the model")
	}
}
