package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// streamTestUSearchShards runs the graph stream over embeddings with the given
// shard plans. It sends the coarse batches from the last to the first, as the
// model can finish them in any order.
func streamTestUSearchShards(
	tb testing.TB,
	dir string,
	embeddings []semanticUnitEmbedding,
	plans []semanticUSearchShardPlan,
	sources semanticUSearchShardSources,
) semanticUSearchBuildResult {
	tb.Helper()
	batchCount := (len(embeddings) + semanticModelBatchRows - 1) / semanticModelBatchRows
	input := make(chan semanticCoarseBatch, batchCount)
	planInput := make(chan semanticUSearchShardPlan, len(plans))
	done := make(chan semanticUSearchBuildResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go buildSemanticUSearchStream(ctx, dir, input, planInput, sources, cancel, searchResources{}, done)
	for _, plan := range plans {
		planInput <- plan
	}
	close(planInput)
	for sequence := batchCount - 1; sequence >= 0; sequence-- {
		start := sequence * semanticModelBatchRows
		end := min(start+semanticModelBatchRows, len(embeddings))
		coarse := make([]semanticCoarseVectors, end-start)
		for row := start; row < end; row++ {
			coarse[row-start] = embeddings[row].coarse
		}
		input <- semanticCoarseBatch{sequence: sequence, coarse: coarse}
	}
	close(input)
	return <-done
}

// setTestUSearchShardCPUs sets GOMAXPROCS for the test, and restores it
// after, so that the build of a generation of rows rows cuts shards of size
// rows. The build sizes its shards from the row count and GOMAXPROCS
// (semanticUSearchShardRowsFor), as for a user. The helper keeps the host
// count when it gives size. Otherwise it sets rows/size CPUs: each CPU then
// gets from size to 2*size-1 rows, which give size.
func setTestUSearchShardCPUs(tb testing.TB, rows, size uint64) {
	tb.Helper()
	cpus := runtime.GOMAXPROCS(0)
	if semanticUSearchShardRowsFor(rows, cpus) != size {
		cpus = max(1, int(rows/size))
	}
	if got := semanticUSearchShardRowsFor(rows, cpus); got != size {
		tb.Fatalf("%d rows on %d CPUs get shards of %d rows, want %d", rows, cpus, got, size)
	}
	saved := runtime.GOMAXPROCS(cpus)
	tb.Cleanup(func() { runtime.GOMAXPROCS(saved) })
}

// fixedTestUSearchPlans cuts rows rows into shards of size rows, with no file
// boundary. Each plan gets a digest of its start.
func fixedTestUSearchPlans(rows, size int) []semanticUSearchShardPlan {
	var plans []semanticUSearchShardPlan
	for start := 0; start < rows; start += size {
		plan := semanticUSearchShardPlan{start: uint64(start), rows: uint64(min(size, rows-start))}
		binary.LittleEndian.PutUint64(plan.digest[:], uint64(start))
		plans = append(plans, plan)
	}
	return plans
}

// testShardFile is a file of a synthetic generation. version changes the
// keys and vectors of its rows, as an edit does.
type testShardFile struct {
	path    string
	rows    int
	version int
}

// testShardFiles gives count files of 1 to 60 rows, with a few of 300 to
// 9,000 rows, in path order.
func testShardFiles(count int, seed uint64) []testShardFile {
	random := rand.New(rand.NewPCG(seed, 7))
	files := make([]testShardFile, count)
	for index := range files {
		rows := 1 + random.IntN(60)
		if random.IntN(50) == 0 {
			rows = 300 + random.IntN(8_700)
		}
		files[index] = testShardFile{path: fmt.Sprintf("pkg/d%03d/f%05d.go", index/40, index), rows: rows}
	}
	return files
}

// testShardUnits gives the rows of files, in order. The key and unit ID of a
// row come from its file's path and version and its place in the file. So an
// unchanged file gives the same rows in every generation.
func testShardUnits(files []testShardFile) []semanticUnit {
	var units []semanticUnit
	for _, file := range files {
		seed := sha256.Sum256(fmt.Appendf(nil, "%s %d", file.path, file.version))
		for index := range file.rows {
			unit := semanticUnit{row: uint64(len(units)), path: file.path, key: semanticRowKey(seed)}
			binary.LittleEndian.PutUint64(unit.key[:8], binary.LittleEndian.Uint64(seed[:8])^uint64(index))
			unit.id = semanticUnitID(unit.key)
			unit.id[31] ^= 0xff
			units = append(units, unit)
		}
	}
	return units
}

// testShardRows gives the rows of files and their embeddings: four coarse
// vectors that follow the row key.
func testShardRows(files []testShardFile) ([]semanticUnit, []semanticUnitEmbedding) {
	units := testShardUnits(files)
	embeddings := make([]semanticUnitEmbedding, len(units))
	for row, unit := range units {
		base := int(binary.LittleEndian.Uint32(unit.key[:4]) % 1_000_000)
		for centroid := range embeddings[row].coarse {
			embeddings[row].coarse[centroid] = semanticUSearchTestVector(base*semanticCoarseCentroidsPerUnit + centroid)
		}
		for centroid := range embeddings[row].fine {
			embeddings[row].fine[centroid] = embeddings[row].coarse[centroid%semanticCoarseCentroidsPerUnit]
		}
	}
	return units, embeddings
}

// planTestShards runs the planner over units, one file at a time, on the CPU
// count that gives shards of size rows. On len(units)/size CPUs, each CPU gets
// from size to 2*size-1 rows. It fails if the planner cuts another size.
func planTestShards(t *testing.T, units []semanticUnit, size uint64) []semanticUSearchShardPlan {
	t.Helper()
	planner := newSemanticUSearchShardPlanner(max(1, len(units)/int(size)))
	var plans []semanticUSearchShardPlan
	for first := 0; first < len(units); {
		end := first + 1
		for end < len(units) && units[end].path == units[first].path {
			end++
		}
		got, err := planner.addFile(units[first:end])
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, got...)
		first = end
	}
	plans = append(plans, planner.finish()...)
	if planner.shardRows() != size {
		t.Fatalf("%d rows: shards of %d rows, want %d", len(units), planner.shardRows(), size)
	}
	return plans
}

func TestSemanticUSearchShardRowsForSizesFromTheHost(t *testing.T) {
	for _, test := range []struct {
		rows uint64
		cpus int
		want uint64
	}{
		{rows: 0, cpus: 18, want: 256},
		{rows: 5_000, cpus: 18, want: 256},
		{rows: 20_000, cpus: 18, want: 1_024},
		{rows: 73_727, cpus: 18, want: 2_048},
		{rows: 73_728, cpus: 18, want: 4_096},
		{rows: 1_347_164, cpus: 18, want: 4_096},
		{rows: 20_000, cpus: 1, want: 4_096},
		{rows: 20_000, cpus: 0, want: 4_096},
	} {
		if got := semanticUSearchShardRowsFor(test.rows, test.cpus); got != test.want {
			t.Errorf("%d rows on %d CPUs: shard size %d, want %d", test.rows, test.cpus, got, test.want)
		}
	}
}

// TestSemanticUSearchShardPlannerCutsAtFileBoundaries checks the cut of 3,000
// files. The shards follow each other with no gap, hold at most the shard
// size, and end at a file boundary. A file of more rows than the shard size
// gets shards of its own of equal size.
func TestSemanticUSearchShardPlannerCutsAtFileBoundaries(t *testing.T) {
	const size = 1_024
	files := testShardFiles(3_000, 1)
	units := testShardUnits(files)
	plans := planTestShards(t, units, size)
	ends := map[uint64]bool{}
	fileOf := make([]int, len(units))
	row := 0
	for index, file := range files {
		for range file.rows {
			fileOf[row] = index
			row++
		}
		ends[uint64(row)] = true
	}
	next := uint64(0)
	split := 0
	for index, plan := range plans {
		if plan.start != next || plan.rows == 0 || plan.rows > size {
			t.Fatalf("shard %d: start %d rows %d, want start %d and 1 to %d rows", index, plan.start, plan.rows, next, size)
		}
		next += plan.rows
		file := files[fileOf[plan.start]]
		if file.rows > size {
			split++
			pieces := (file.rows + size - 1) / size
			if fileOf[next-1] != fileOf[plan.start] || plan.rows*uint64(pieces) < uint64(file.rows)-uint64(pieces) {
				t.Fatalf("shard %d of a %d-row file has %d rows", index, file.rows, plan.rows)
			}
			continue
		}
		if !ends[next] {
			t.Fatalf("shard %d ends at row %d, inside a file", index, next)
		}
	}
	if next != uint64(len(units)) || split == 0 {
		t.Fatalf("shards cover %d of %d rows; %d shards of large files", next, len(units), split)
	}
}

// TestSemanticUSearchShardPlannerWaitsForTheHostSize gives two CPUs. The
// planner holds the files until the rows reach 8,192. So a small generation
// gets the size of its row count, and a large one gets shards before its end.
func TestSemanticUSearchShardPlannerWaitsForTheHostSize(t *testing.T) {
	small := testShardUnits([]testShardFile{{path: "a.go", rows: 3_000}, {path: "b.go", rows: 2_000}})
	planner := newSemanticUSearchShardPlanner(2)
	for _, file := range [][]semanticUnit{small[:3_000], small[3_000:]} {
		if plans, err := planner.addFile(file); err != nil || len(plans) != 0 {
			t.Fatalf("planner gave %d shards before the size was known (err=%v)", len(plans), err)
		}
	}
	plans := planner.finish()
	if planner.shardRows() != 2_048 || len(plans) != 3 {
		t.Fatalf("5,000 rows on 2 CPUs: size %d and %d shards, want 2,048 and 3", planner.shardRows(), len(plans))
	}

	large := testShardUnits(testShardFiles(1_000, 2))
	planner = newSemanticUSearchShardPlanner(2)
	early := 0
	for first := 0; first < len(large); {
		end := first + 1
		for end < len(large) && large[end].path == large[first].path {
			end++
		}
		plans, err := planner.addFile(large[first:end])
		if err != nil {
			t.Fatal(err)
		}
		early += len(plans)
		first = end
	}
	if len(large) < 2*semanticUSearchMaxShardRows || planner.shardRows() != semanticUSearchMaxShardRows || early == 0 {
		t.Fatalf("%d rows: size %d, %d shards before the end", len(large), planner.shardRows(), early)
	}
}

// TestSemanticUSearchShardPlannerKeepsShardsAfterAnEdit edits one file at a
// time, in 40 places: its rows grow, shrink, or keep their count, with new
// keys. In both trees below, the shards before the file stay, and the cut
// finds the old cuts again at an anchor after the file. A cut every 1,024
// rows would change every shard after the file.
//
//   - Files of 1 to 60 rows, and a few large ones: each edit changes 1 to 3
//     shards.
//   - Files of 2 rows each: every file end can take a cut, so a cut at 15/16
//     of the size with no anchor moves each later cut with the edit. Anchors
//     are sparse here, so an edit can move a cut that is just at 15/16 to the
//     next anchor, and the cuts of a few shards after it. The 40 edits change
//     at most 3 shards for each edit on average.
func TestSemanticUSearchShardPlannerKeepsShardsAfterAnEdit(t *testing.T) {
	const size = 1_024
	small := make([]testShardFile, 10_000)
	for index := range small {
		small[index] = testShardFile{path: fmt.Sprintf("pkg/d%03d/f%05d.go", index/40, index), rows: 2}
	}
	for _, tree := range []struct {
		name  string
		files []testShardFile
		// each: every edit changes at most 3 shards, not only the average.
		each bool
	}{
		{name: "mixed sizes", files: testShardFiles(1_000, 3), each: true},
		{name: "2 rows each", files: small},
	} {
		t.Run(tree.name, func(t *testing.T) {
			files := tree.files
			units := testShardUnits(files)
			before := planTestShards(t, units, size)
			old := map[[sha256.Size]byte]bool{}
			for _, plan := range before {
				old[plan.digest] = true
			}
			random := rand.New(rand.NewPCG(4, 5))
			total := 0
			for edit := range 40 {
				target := random.IntN(len(files))
				firstRow := 0
				for _, file := range files[:target] {
					firstRow += file.rows
				}
				file := files[target]
				file.version++
				file.rows = max(1, file.rows+[]int{0, 1, -1, 3, -3, 20}[edit%6])
				// The rows of a file depend only on the file, so only the
				// edited file gets new rows. The rows after it get new
				// numbers.
				edited := slices.Concat(units[:firstRow], testShardUnits([]testShardFile{file}), units[firstRow+files[target].rows:])
				for row := firstRow; row < len(edited); row++ {
					edited[row].row = uint64(row)
				}
				after := planTestShards(t, edited, size)
				changed := 0
				for _, plan := range after {
					if !old[plan.digest] {
						changed++
					}
				}
				for index, plan := range before {
					if plan.start+plan.rows > uint64(firstRow) {
						break
					}
					if after[index] != plan {
						t.Fatalf("edit %d of file %d changed shard %d before the file", edit, target, index)
					}
				}
				if changed == 0 || tree.each && changed > 3 {
					t.Fatalf("edit %d of file %d (%d rows) changed %d of %d shards", edit, target, files[target].rows, changed, len(after))
				}
				total += changed
			}
			if total > 3*40 {
				t.Fatalf("40 edits changed %d shards of %d, more than 3 for each edit on average", total, len(before))
			}
			t.Logf("40 edits changed %d shards of %d", total, len(before))
		})
	}
}

// TestBuildSemanticUSearchStreamReusesUnchangedShards builds the graphs of a
// generation, then of the same rows, then of the rows after a one-file edit.
// The same rows build no graph: each shard links the file of the first
// build. The edit builds only the shards around the file; the others link.
// A graph that a build reuses is the graph that a new build makes.
func TestBuildSemanticUSearchStreamReusesUnchangedShards(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	const size = 512
	files := testShardFiles(400, 6)
	units, embeddings := testShardRows(files)
	plans := planTestShards(t, units, size)
	first := t.TempDir()
	built := streamTestUSearchShards(t, first, embeddings, plans, nil)
	if built.err != nil || built.built != len(plans) || built.reused != 0 {
		t.Fatalf("first build: built %d reused %d of %d shards, err=%v", built.built, built.reused, len(plans), built.err)
	}
	sources := semanticUSearchShardSources{}
	for _, shard := range built.shards {
		sources[shard.ID] = semanticUSearchShardSource{path: filepath.Join(first, shard.Name), shard: shard, link: true}
	}
	same := func(t *testing.T, left, right string) bool {
		t.Helper()
		leftInfo, leftErr := os.Lstat(left)
		rightInfo, rightErr := os.Lstat(right)
		if leftErr != nil || rightErr != nil {
			t.Fatalf("stat: %v %v", leftErr, rightErr)
		}
		return os.SameFile(leftInfo, rightInfo)
	}

	t.Run("same-rows", func(t *testing.T) {
		before := semanticUSearchShardsBuilt.Load()
		dir := t.TempDir()
		again := streamTestUSearchShards(t, dir, embeddings, plans, sources)
		if again.err != nil || again.built != 0 || again.reused != len(plans) ||
			semanticUSearchShardsBuilt.Load() != before {
			t.Fatalf("built %d reused %d of %d shards, err=%v", again.built, again.reused, len(plans), again.err)
		}
		for index, shard := range again.shards {
			if shard.ID != built.shards[index].ID || shard.semanticArtifact != built.shards[index].semanticArtifact ||
				!same(t, filepath.Join(dir, shard.Name), filepath.Join(first, built.shards[index].Name)) {
				t.Fatalf("shard %d is not the first build's file", index)
			}
			info, err := os.Lstat(filepath.Join(dir, shard.Name))
			if err != nil || semanticRowsCheckOf(info) != shard.Check {
				t.Fatalf("shard %d check does not match its link: %v", index, err)
			}
		}
	})

	t.Run("one-file-edit", func(t *testing.T) {
		edited := slices.Clone(files)
		edited[200].version++
		edited[200].rows += 2
		editedUnits, editedEmbeddings := testShardRows(edited)
		editedPlans := planTestShards(t, editedUnits, size)
		dir := t.TempDir()
		before := semanticUSearchShardsBuilt.Load()
		after := streamTestUSearchShards(t, dir, editedEmbeddings, editedPlans, sources)
		if after.err != nil || after.built == 0 || after.built > 2 || after.built+after.reused != len(editedPlans) ||
			semanticUSearchShardsBuilt.Load()-before != int64(after.built) {
			t.Fatalf("built %d reused %d of %d shards, err=%v", after.built, after.reused, len(editedPlans), after.err)
		}
		fresh := streamTestUSearchShards(t, t.TempDir(), editedEmbeddings, editedPlans, nil)
		if fresh.err != nil {
			t.Fatal(fresh.err)
		}
		linked := 0
		for index, shard := range after.shards {
			if source, ok := sources[shard.ID]; ok {
				if !same(t, filepath.Join(dir, shard.Name), source.path) {
					t.Fatalf("shard %d has a source but is not its file", index)
				}
				linked++
			}
			if shard.semanticArtifact != fresh.shards[index].semanticArtifact || shard.ID != fresh.shards[index].ID {
				t.Fatalf("shard %d differs from a new build of the same rows", index)
			}
		}
		if linked != after.reused {
			t.Fatalf("%d shards link a first-build file, want %d", linked, after.reused)
		}
	})
}

// TestBuildSemanticUSearchStreamRejectsADamagedSource damages one graph file
// of a first build in place, with its length kept. The next build of the same
// rows links or copies its sources. It hashes that file, rejects it, and
// builds the shard again. It reuses every other file.
func TestBuildSemanticUSearchStreamRejectsADamagedSource(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	const size = 512
	units, embeddings := testShardRows(testShardFiles(200, 8))
	plans := planTestShards(t, units, size)
	first := t.TempDir()
	built := streamTestUSearchShards(t, first, embeddings, plans, nil)
	if built.err != nil || len(built.shards) < 3 {
		t.Fatalf("first build: %d shards, err=%v", len(built.shards), built.err)
	}
	damaged := built.shards[1]
	path := filepath.Join(first, damaged.Name)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content[len(content)/2] ^= 0xff
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []bool{true, false} {
		t.Run(fmt.Sprintf("link=%t", link), func(t *testing.T) {
			sources := semanticUSearchShardSources{}
			for _, shard := range built.shards {
				sources[shard.ID] = semanticUSearchShardSource{path: filepath.Join(first, shard.Name), shard: shard, link: link}
			}
			dir := t.TempDir()
			again := streamTestUSearchShards(t, dir, embeddings, plans, sources)
			if again.err != nil || again.built != 1 || again.reused != len(plans)-1 {
				t.Fatalf("built %d reused %d of %d shards, err=%v", again.built, again.reused, len(plans), again.err)
			}
			rebuilt := filepath.Join(dir, again.shards[1].Name)
			if err := validateSemanticArtifact(rebuilt, damaged.semanticArtifact); err != nil {
				t.Fatalf("the rebuilt shard is not the original graph: %v", err)
			}
			info, err := os.Lstat(rebuilt)
			if err != nil {
				t.Fatal(err)
			}
			if source, err := os.Lstat(path); err != nil || os.SameFile(info, source) {
				t.Fatalf("the build reused the damaged file (err=%v)", err)
			}
		})
	}
}

// TestBuildSemanticUSearchStreamLinksNothingWhenItFails reuses every shard of
// a first build, but the rows of the last shard never come. The stream fails
// and links no file. Each source file keeps its recorded check, so the live
// generation hashes none of them at its next search.
func TestBuildSemanticUSearchStreamLinksNothingWhenItFails(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	units, embeddings := testShardRows(testShardFiles(100, 10))
	plans := planTestShards(t, units, 512)
	first := t.TempDir()
	built := streamTestUSearchShards(t, first, embeddings, plans, nil)
	if built.err != nil {
		t.Fatal(built.err)
	}
	sources := semanticUSearchShardSources{}
	for _, shard := range built.shards {
		sources[shard.ID] = semanticUSearchShardSource{path: filepath.Join(first, shard.Name), shard: shard, link: true}
	}
	dir := t.TempDir()
	short := len(embeddings) - semanticModelBatchRows/2
	if failed := streamTestUSearchShards(t, dir, embeddings[:short], plans, sources); failed.err == nil {
		t.Fatal("the stream accepted shards whose rows never came")
	}
	if placed, err := filepath.Glob(filepath.Join(dir, "*"+semanticUSearchFileSuffix)); err != nil || len(placed) != 0 {
		t.Fatalf("the failed stream left %v (err=%v)", placed, err)
	}
	for _, shard := range built.shards {
		info, err := os.Lstat(filepath.Join(first, shard.Name))
		if err != nil || semanticRowsCheckOf(info) != shard.Check {
			t.Fatalf("source %s changed: err=%v", shard.Name, err)
		}
	}
}

// TestBuildSemanticUSearchStreamRebuildsForOtherVectors keeps the keys and
// unit IDs of every row but changes their coarse vectors, as another model
// provider can. No shard matches, and every graph builds again.
func TestBuildSemanticUSearchStreamRebuildsForOtherVectors(t *testing.T) {
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	units, embeddings := testShardRows(testShardFiles(100, 9))
	plans := planTestShards(t, units, 512)
	first := t.TempDir()
	built := streamTestUSearchShards(t, first, embeddings, plans, nil)
	if built.err != nil {
		t.Fatal(built.err)
	}
	sources := semanticUSearchShardSources{}
	for _, shard := range built.shards {
		sources[shard.ID] = semanticUSearchShardSource{path: filepath.Join(first, shard.Name), shard: shard, link: true}
	}
	other := slices.Clone(embeddings)
	for row := range other {
		other[row].coarse[0] = semanticUSearchTestVector(row + 5_000_000)
	}
	again := streamTestUSearchShards(t, t.TempDir(), other, plans, sources)
	if again.err != nil || again.reused != 0 || again.built != len(plans) {
		t.Fatalf("built %d reused %d of %d shards, err=%v", again.built, again.reused, len(plans), again.err)
	}
}

// TestGraphShardsSurviveAOneFileCommit commits a change to one file of 1,200
// in a Git node with 256-row shards. The second build links every shard file
// of the first that the cut keeps, and builds at most two. After the bind
// removes the first generation, each shard of the second still matches its
// recorded check, so the next search hashes no shard.
func TestGraphShardsSurviveAOneFileCommit(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	setTestUSearchShardCPUs(t, 1_201, semanticUSearchMinShardRows)
	repo := initGitRepo(t, "seed.go", "package sample\n// alpha beta seed\n")
	for index := range 1_200 {
		writeFileAt(t, repo, fmt.Sprintf("pkg/d%02d/f%04d.go", index/50, index),
			fmt.Sprintf("package sample\n// alpha beta file %d\n", index))
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "files")
	t.Chdir(t.TempDir())
	reuseSearch(t, repo)
	generation := func() (string, semanticManifest) {
		t.Helper()
		root, err := seekUserCacheRoot()
		if err != nil {
			t.Fatal(err)
		}
		dirs, err := filepath.Glob(filepath.Join(root, "corpora", "*", "index", semanticGenerationPrefix+"*"))
		if err != nil || len(dirs) != 1 {
			t.Fatalf("semantic generations %v (err=%v), want one", dirs, err)
		}
		manifest, err := readSemanticManifestUnchecked(dirs[0])
		if err != nil {
			t.Fatal(err)
		}
		return dirs[0], manifest
	}
	firstDir, firstManifest := generation()
	if len(firstManifest.USearchFiles) < 4 || firstManifest.USearchShardRows != semanticUSearchMinShardRows {
		t.Fatalf("first generation has %d shards of %d rows", len(firstManifest.USearchFiles), firstManifest.USearchShardRows)
	}
	inodes := map[semanticRowsCheck]bool{}
	for _, shard := range firstManifest.USearchFiles {
		info, err := os.Lstat(filepath.Join(firstDir, shard.Name))
		if err != nil {
			t.Fatal(err)
		}
		inodes[semanticRowsCheck{Device: semanticRowsCheckOf(info).Device, Inode: semanticRowsCheckOf(info).Inode}] = true
	}

	writeFileAt(t, repo, "pkg/d12/f0600.go", "package sample\n// alpha beta changed file\n\nfunc Changed() int {\n\treturn 1\n}\n")
	gitRunIn(t, repo, "commit", "-q", "-am", "change one")
	builtBefore := semanticUSearchShardsBuilt.Load()
	reuseSearch(t, repo)
	built := semanticUSearchShardsBuilt.Load() - builtBefore
	secondDir, secondManifest := generation()
	if secondDir == firstDir {
		t.Fatal("the commit did not make a new generation")
	}
	linked := 0
	for _, shard := range secondManifest.USearchFiles {
		info, err := os.Lstat(filepath.Join(secondDir, shard.Name))
		if err != nil {
			t.Fatal(err)
		}
		check := semanticRowsCheckOf(info)
		if inodes[semanticRowsCheck{Device: check.Device, Inode: check.Inode}] {
			linked++
		}
		if check != shard.Check {
			t.Fatalf("shard %s does not match its recorded check after the bind", shard.Name)
		}
	}
	if built == 0 || built > 2 || linked+int(built) != len(secondManifest.USearchFiles) {
		t.Fatalf("the commit built %d and linked %d of %d shards", built, linked, len(secondManifest.USearchFiles))
	}
	hashed := semanticShardsHashed.Load()
	reuseSearch(t, repo)
	if semanticShardsHashed.Load() != hashed {
		t.Fatalf("the search after the bind hashed %d shards", semanticShardsHashed.Load()-hashed)
	}
}

// TestFolderOneFileChangeReusesGraphShards changes one file of a 1,200-file
// folder. The folder build reads its files on several workers, but the vector
// build gets them in walk order. So the rows of the unchanged shards stay the
// same. At most the two shards around the changed file are built, and every
// other shard links its old graph file.
func TestFolderOneFileChangeReusesGraphShards(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	setTestUSearchShardCPUs(t, 1_200, semanticUSearchMinShardRows)
	folder := t.TempDir()
	for index := range 1_200 {
		writeFileAt(t, folder, fmt.Sprintf("pkg/d%02d/f%04d.go", index/50, index),
			fmt.Sprintf("package sample\n// alpha beta file %d\n", index))
	}
	t.Chdir(t.TempDir())
	reuseSearch(t, folder)
	root, err := seekUserCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	glob := filepath.Join(root, "corpora", "*", "index", semanticGenerationPrefix+"*")
	first, _ := filepath.Glob(glob)
	if len(first) != 1 {
		t.Fatalf("generations %v, want one", first)
	}
	firstManifest, err := readSemanticManifestUnchecked(first[0])
	if err != nil {
		t.Fatal(err)
	}
	inodes := map[uint64]bool{}
	for _, shard := range firstManifest.USearchFiles {
		info, err := os.Lstat(filepath.Join(first[0], shard.Name))
		if err != nil {
			t.Fatal(err)
		}
		inodes[semanticRowsCheckOf(info).Inode] = true
	}
	// Set the mtime of the changed file 1 s after its old one, as a later
	// edit would. Its stat then differs from the indexed one on any clock
	// resolution, with no wait.
	changed := filepath.Join(folder, "pkg", "d12", "f0600.go")
	old, err := os.Stat(changed)
	if err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, folder, "pkg/d12/f0600.go", "package sample\n// alpha beta changed file\n\nfunc Changed() int {\n\treturn 1\n}\n")
	mtime := old.ModTime().Add(time.Second)
	if err := os.Chtimes(changed, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	built := semanticUSearchShardsBuilt.Load()
	reuseSearch(t, folder)
	second, _ := filepath.Glob(glob)
	if len(second) != 1 || second[0] == first[0] {
		t.Fatalf("generations %v after the change, first %v", second, first)
	}
	secondManifest, err := readSemanticManifestUnchecked(second[0])
	if err != nil {
		t.Fatal(err)
	}
	linked := 0
	for _, shard := range secondManifest.USearchFiles {
		info, err := os.Lstat(filepath.Join(second[0], shard.Name))
		if err != nil {
			t.Fatal(err)
		}
		if inodes[semanticRowsCheckOf(info).Inode] {
			linked++
		}
	}
	shards := len(secondManifest.USearchFiles)
	if made := semanticUSearchShardsBuilt.Load() - built; shards < 4 || made > 2 || linked < shards-2 {
		t.Fatalf("one-file change: %d shards, built %d, linked %d; want at most 2 built and the rest linked", shards, made, linked)
	}
}

// TestWorktreeGraphSourceIsCopiedNotLinked builds a repository, then a second
// worktree of it. The second build reuses the first worktree's graphs, which
// its bind does not remove, so it copies them. A link would change their
// ctime, and every warm search of the first worktree would then hash them
// again.
func TestWorktreeGraphSourceIsCopiedNotLinked(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	setTestUSearchShardCPUs(t, 601, semanticUSearchMinShardRows)
	repo := initGitRepo(t, "seed.go", "package sample\n// alpha beta seed\n")
	for index := range 600 {
		writeFileAt(t, repo, fmt.Sprintf("pkg/d%02d/f%04d.go", index/50, index),
			fmt.Sprintf("package sample\n// alpha beta file %d\n", index))
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "files")
	t.Chdir(t.TempDir())
	reuseSearch(t, repo)
	worktree := filepath.Join(t.TempDir(), "wt")
	gitRunIn(t, repo, "worktree", "add", "-q", "--detach", worktree)
	built := semanticUSearchShardsBuilt.Load()
	reuseSearch(t, worktree)
	if made := semanticUSearchShardsBuilt.Load() - built; made != 0 {
		t.Fatalf("the second worktree built %d graphs, want every graph reused", made)
	}
	hashed := semanticShardsHashed.Load()
	reuseSearch(t, repo)
	reuseSearch(t, repo)
	if got := semanticShardsHashed.Load() - hashed; got != 0 {
		t.Fatalf("two warm searches of the first worktree hashed %d graph shards", got)
	}
}

// TestRefusedGraphLinkCopiesTheFile places a verified link source whose file
// changed its time after the check, so the placement refuses the link. It
// copies the file instead: the new file has the source bytes, its own inode,
// and the check of what it holds.
func TestRefusedGraphLinkCopiesTheFile(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.usearch")
	writeFixtureFile(t, sourcePath, strings.Repeat("graph bytes\n", 100))
	artifact, err := inspectSemanticArtifact(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	source := semanticUSearchShardSource{
		path:  sourcePath,
		shard: semanticUSearchShard{Name: "source.usearch", semanticArtifact: artifact, Check: semanticRowsCheckOf(info)},
		link:  true,
	}
	reuse, err := verifySemanticUSearchShardSource(source)
	if err != nil {
		t.Fatal(err)
	}
	later := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(sourcePath, later, later); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.usearch")
	check, err := reuse.place(target)
	if err != nil {
		t.Fatalf("a refused link gave %v, want a copy", err)
	}
	placed, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(placed, info) || check != semanticRowsCheckOf(placed) {
		t.Fatalf("placement: same file as the source=%v, check matches=%v; want a copy with its own check",
			os.SameFile(placed, info), check == semanticRowsCheckOf(placed))
	}
	if copied, err := inspectSemanticArtifact(target); err != nil || copied != artifact {
		t.Fatalf("the copy holds other bytes: %v", err)
	}
}

// TestDamagedGraphShardIsRebuiltOnce damages one graph shard of a folder node
// and of a Git node in place. A disk fault that changes the file time can do
// that.
// The next search hashes the shard, keeps its text hits (every file holds the
// query words), and removes the vector generations: one warning.
// The search after it builds the graphs again, and a warm search then hashes
// no shard and builds none.
func TestDamagedGraphShardIsRebuiltOnce(t *testing.T) {
	requireTools(t)
	for _, kind := range []string{"folder", "git"} {
		t.Run(kind, func(t *testing.T) {
			setTestUserCache(t)
			var root string
			if kind == "folder" {
				setTestUSearchShardCPUs(t, 600, semanticUSearchMinShardRows)
				root = t.TempDir()
			} else {
				setTestUSearchShardCPUs(t, 601, semanticUSearchMinShardRows)
				root = initGitRepo(t, "seed.go", "package sample\n// alpha beta seed\n")
			}
			for index := range 600 {
				writeFileAt(t, root, fmt.Sprintf("pkg/d%02d/f%04d.go", index/50, index),
					fmt.Sprintf("package sample\n// alpha beta file %d\n", index))
			}
			if kind == "git" {
				gitRunIn(t, root, "add", ".")
				gitRunIn(t, root, "commit", "-q", "-m", "files")
			}
			t.Chdir(t.TempDir())
			logs := captureTestLogs(t, slog.LevelWarn)
			reuseSearch(t, root)
			cache, err := seekUserCacheRoot()
			if err != nil {
				t.Fatal(err)
			}
			dirs, _ := filepath.Glob(filepath.Join(cache, "corpora", "*", "index", semanticGenerationPrefix+"*"))
			if len(dirs) != 1 {
				t.Fatalf("generations %v, want one", dirs)
			}
			manifest, err := readSemanticManifestUnchecked(dirs[0])
			if err != nil || len(manifest.USearchFiles) < 2 {
				t.Fatalf("generation has %d graph shards (err=%v), want several", len(manifest.USearchFiles), err)
			}
			file, err := os.OpenFile(filepath.Join(dirs[0], manifest.USearchFiles[0].Name), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			offset := int64(manifest.USearchFiles[0].Bytes / 2)
			b := make([]byte, 1)
			if _, err := file.ReadAt(b, offset); err != nil {
				t.Fatal(err)
			}
			b[0] ^= 0xFF
			if _, err := file.WriteAt(b, offset); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}

			from := len(logs.Records())
			hashed := semanticShardsHashed.Load()
			_, output := reuseSearchWith(t, &hybridTestScorer{}, root)
			shards, vectors, skips, next := repairWarnings(logs, from)
			if semanticShardsHashed.Load() == hashed || shards != 0 || vectors != 1 || skips != 0 {
				t.Fatalf("search with the damaged shard: hashed %d shards, warnings %d/%d/%d; want a hash and one vector repair",
					semanticShardsHashed.Load()-hashed, shards, vectors, skips)
			}
			if header := "## " + filepath.Join(root, "pkg", "d00", "f0000.go"); !strings.Contains(output, header) {
				t.Fatalf("the search with the damaged shard printed no %q, want its text hits", header)
			}
			built := semanticUSearchShardsBuilt.Load()
			reuseSearch(t, root)
			if semanticUSearchShardsBuilt.Load() == built {
				t.Fatal("the search after the repair built no graph shard")
			}
			hashed, built = semanticShardsHashed.Load(), semanticUSearchShardsBuilt.Load()
			reuseSearch(t, root)
			shards, vectors, skips, _ = repairWarnings(logs, next)
			if got := semanticShardsHashed.Load() - hashed; got != 0 || semanticUSearchShardsBuilt.Load() != built ||
				shards+vectors+skips != 0 {
				t.Fatalf("warm search: hashed %d shards, built %d, warnings %d/%d/%d; want none",
					got, semanticUSearchShardsBuilt.Load()-built, shards, vectors, skips)
			}
		})
	}
}
