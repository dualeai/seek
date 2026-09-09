package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFamilyScanContiguous pins the guard that decides whether a family may be
// seeded. Zoekt derives the next shard number from the contiguous run it finds,
// so a gapped seed can make a delta replace a later shard. Basename order does
// not group each prefix's shards, so the comparison must use parsed prefixes
// and shard numbers.
func TestFamilyScanContiguous(t *testing.T) {
	scan := func(names ...string) familyScan {
		var sc familyScan
		for _, n := range names {
			m := familyMember{name: n, uncommitted: isUncommittedShard(n), shard: true}
			m.prefix, m.num, m.numbered = parseShardName(n)
			sc.members = append(sc.members, m)
		}
		return sc
	}

	tests := []struct {
		name string
		fam  shardFamily
		scan familyScan
		want bool
	}{
		{"empty", familyAll, scan(), true},
		{"single shard zero", familyAll, scan("r_v16.00000.zoekt"), true},
		{"run of three", familyAll, scan("r_v16.00000.zoekt", "r_v16.00001.zoekt", "r_v16.00002.zoekt"), true},
		{"missing zero", familyAll, scan("r_v16.00001.zoekt", "r_v16.00002.zoekt"), false},
		{"gap in the middle", familyAll, scan("r_v16.00000.zoekt", "r_v16.00002.zoekt"), false},
		{
			// The case the comment calls out: a second family whose whole name
			// sorts between the first family's shard 0 and shard 1. Sorting by
			// basename alone would interleave the two runs and mis-decide.
			name: "two prefixes interleaved by basename",
			fam:  familyAll,
			scan: scan(
				"a_v16.00000.zoekt",
				"a_v16.00001.00000.zoekt",
				"a_v16.00001.zoekt",
			),
			want: true,
		},
		{
			name: "two prefixes, second one gapped",
			fam:  familyAll,
			scan: scan(
				"a_v16.00000.zoekt",
				"b_v16.00001.zoekt",
			),
			want: false,
		},
		{
			// Members arrive in whatever order the caller built them; the
			// comparator must order them, not the input.
			name: "unsorted input still contiguous",
			fam:  familyAll,
			scan: scan("r_v16.00002.zoekt", "r_v16.00000.zoekt", "r_v16.00001.zoekt"),
			want: true,
		},
		{
			// familyCommitted must ignore the uncommitted family entirely: its
			// own numbering says nothing about the committed run.
			name: "committed view skips a gapped uncommitted family",
			fam:  familyCommitted,
			scan: scan(
				"r_v16.00000.zoekt",
				repoUncommitted+"_v16.00003.zoekt",
			),
			want: true,
		},
		{
			name: "all view sees the gapped uncommitted family",
			fam:  familyAll,
			scan: scan(
				"r_v16.00000.zoekt",
				repoUncommitted+"_v16.00003.zoekt",
			),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scan.contiguous(tc.fam); got != tc.want {
				t.Errorf("contiguous(%v) = %v, want %v", tc.fam, got, tc.want)
			}
		})
	}
}

// TestMatchesManifestFamilyScoping checks that a change to the uncommitted
// family does not mark the committed family as damaged.
func TestMatchesManifestFamilyScoping(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed := "git-abc_v16.00000.zoekt"
	uncommitted := repoUncommitted + "_v16.00000.zoekt"
	write(committed, "committed-shard")
	write(uncommitted, "uncommitted")

	if err := writeFamilyManifest(dir, []string{committed, uncommitted}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	scan := func() familyScan {
		sc, err := scanFamily(dir)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return sc
	}

	sc := scan()
	if !sc.matchesManifest(dir) {
		t.Fatal("intact family did not match")
	}
	if !sc.matchesManifestFamily(dir, familyCommitted) {
		t.Fatal("intact committed half did not match")
	}

	// Rewrite only the uncommitted shard, as a dirty edit does.
	write(uncommitted, "uncommitted-but-longer")
	sc = scan()
	if sc.matchesManifest(dir) {
		t.Error("whole family still matched after the uncommitted shard changed")
	}
	if !sc.matchesManifestFamily(dir, familyCommitted) {
		t.Error("committed half reported damaged by an uncommitted-only change")
	}

	// Damage the committed shard: both answers must now be false.
	write(committed, "committed-shard-changed")
	sc = scan()
	if sc.matchesManifestFamily(dir, familyCommitted) {
		t.Error("committed half matched after the committed shard changed")
	}

	// A missing committed member is damage even when sizes elsewhere agree.
	if err := os.Remove(filepath.Join(dir, committed)); err != nil {
		t.Fatal(err)
	}
	sc = scan()
	if sc.matchesManifestFamily(dir, familyCommitted) {
		t.Error("committed half matched after its only shard was removed")
	}
}
