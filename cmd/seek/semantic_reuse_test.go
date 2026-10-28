package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// reuseSearch runs the descriptive search "alpha beta" with a fresh test
// model in the far mode and returns the path of each unit that the model
// embedded.
func reuseSearch(t *testing.T, root string) []string {
	t.Helper()
	units, _ := reuseSearchWith(t, &hybridTestScorer{}, root)
	paths := make([]string, len(units))
	for index, unit := range units {
		paths[index] = unit.path
	}
	return paths
}

// reuseSearchWith runs the descriptive search "alpha beta" with model and
// returns a copy of each unit that the model embedded, in call order, and the
// output.
func reuseSearchWith(t *testing.T, model semanticModel, root string) ([]semanticUnit, string) {
	t.Helper()
	observed := observeModel(model)
	output, err := seekWith(t, observed.factory(), "alpha beta", root)
	if err != nil {
		t.Fatal(err)
	}
	observed.mu.Lock()
	defer observed.mu.Unlock()
	return slices.Clone(observed.units), output
}

// TestCacheKeyIncludesModelInput checks that the key follows the model input.
// The same bytes under two path tails give two keys. The same bytes and tail
// in two repositories give one key.
func TestCacheKeyIncludesModelInput(t *testing.T) {
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	key := func(path string) semanticRowKey {
		units := extractSemanticUnits(path, []byte("package sample\n// same bytes\n"), nil, nil)
		rows, err := windowSemanticUnits(t.Context(), tokenizer, units, "")
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows=%d err=%v", len(rows), err)
		}
		return rows[0].key
	}
	if key("one/src/pkg/a.go") == key("one/src/pkg/b.go") {
		t.Error("two path tails gave one key")
	}
	if key("repo1/x/src/pkg/a.go") != key("repo2/x/src/pkg/a.go") {
		t.Error("one tail in two repositories gave two keys")
	}
}

// TestEmbeddingCacheReusesUnchangedFilesAcrossCommits changes 1 of 200 files
// in a new commit: the next build embeds rows of that file only.
func TestEmbeddingCacheReusesUnchangedFilesAcrossCommits(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := initGitRepo(t, "seed.go", "package sample\n// alpha beta seed\n")
	for i := range 200 {
		writeFileAt(t, repo, fmt.Sprintf("pkg/f%03d.go", i), fmt.Sprintf("package sample\n// alpha beta file %d\n", i))
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "files")
	t.Chdir(t.TempDir())
	if first := reuseSearch(t, repo); len(first) < 201 {
		t.Fatalf("the first build embedded %d rows, want every file", len(first))
	}
	writeFileAt(t, repo, "pkg/f100.go", "package sample\n// alpha beta changed file\n")
	gitRunIn(t, repo, "commit", "-q", "-am", "change one")
	second := reuseSearch(t, repo)
	if len(second) == 0 || slices.ContainsFunc(second, func(path string) bool { return path != "pkg/f100.go" }) {
		t.Fatalf("the second build embedded %v, want pkg/f100.go only", second)
	}
}

// TestOneLineEditReembedsOneUnit edits one line inside the middle function of
// a file: the next build embeds that unit only, Beta with its new line.
func TestOneLineEditReembedsOneUnit(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	content := "package sample\n\nfunc Alpha() int {\n\treturn 1 // alpha beta\n}\n\n" +
		"func Beta() int {\n\treturn 2 // alpha beta\n}\n\nfunc Gamma() int {\n\treturn 3 // alpha beta\n}\n"
	repo := initGitRepo(t, "three.go", content)
	t.Chdir(t.TempDir())
	if first := reuseSearch(t, repo); len(first) < 3 {
		t.Fatalf("the first build embedded %d rows", len(first))
	}
	writeFileAt(t, repo, "three.go", strings.Replace(content, "return 2", "return 22", 1))
	gitRunIn(t, repo, "commit", "-q", "-am", "edit Beta")
	second, _ := reuseSearchWith(t, &hybridTestScorer{}, repo)
	if len(second) != 1 || !bytes.HasPrefix(second[0].text, []byte("func Beta() int {\n\treturn 22 ")) {
		var got []string
		for _, unit := range second {
			got = append(got, fmt.Sprintf("%s %q", unit.path, unit.text))
		}
		t.Fatalf("the second build embedded %v, want the edited unit Beta only", got)
	}
}

// TestLinkedWorktreesEmbedNothingNew searches a second linked worktree at the
// same HEAD: it copies every row from the first one.
func TestLinkedWorktreesEmbedNothingNew(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := initGitRepo(t, "a.go", "package sample\n// alpha beta one\n")
	writeFileAt(t, repo, "b.go", "package sample\n// alpha beta two\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "two")
	t.Chdir(t.TempDir())
	if first := reuseSearch(t, repo); len(first) == 0 {
		t.Fatal("the first build embedded nothing")
	}
	worktree := filepath.Join(t.TempDir(), "wt")
	gitRunIn(t, repo, "worktree", "add", "-q", "--detach", worktree)
	if second := reuseSearch(t, worktree); len(second) != 0 {
		t.Fatalf("the linked worktree embedded %v, want nothing", second)
	}
}

// TestRunFolderChangeEmbedsOnlyChangedFile edits one file of a 50-file
// folder: the next build embeds rows of that file only. The model accepts
// every row, so the search prints the file through its vectors, and the
// result holds the new text, not the old text.
func TestRunFolderChangeEmbedsOnlyChangedFile(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	for i := range 49 {
		writeFileAt(t, folder, fmt.Sprintf("f%02d.go", i), fmt.Sprintf("package sample\n// alpha file %d\n", i))
	}
	t.Chdir(t.TempDir())
	if first, _ := reuseSearchWith(t, &hybridTestScorer{mode: payloadAccept}, folder); len(first) < 50 {
		t.Fatalf("the first build embedded %d rows", len(first))
	}
	const edit = "dispatches a request to its handler"
	writeFileAt(t, folder, "f07.go", "package sample\n// "+edit+"\n")
	units, output := reuseSearchWith(t, &hybridTestScorer{mode: payloadAccept}, folder)
	_, block, found := strings.Cut(output, "## "+filepath.Join(folder, "f07.go"))
	block, _, _ = strings.Cut(block, "\n## ")
	if !found || !strings.Contains(block, edit) || strings.Contains(output, "alpha file 7\n") {
		t.Fatalf("output=%q, want the f07.go result with its new text only", output)
	}
	var paths []string
	for _, unit := range units {
		paths = append(paths, unit.path)
	}
	if len(paths) == 0 || slices.ContainsFunc(paths, func(path string) bool { return path != "f07.go" }) {
		t.Fatalf("the change embedded %v, want f07.go only", paths)
	}
}

// TestRowsIntegrityCheckedOnceAtBind builds a generation, then opens it for a
// search: the open runs no full check of rows.bin. A touched rows.bin runs the
// full check again.
func TestRowsIntegrityCheckedOnceAtBind(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	dir := filepath.Join(t.TempDir(), "semantic")
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head", units, embeddings); err != nil {
		t.Fatal(err)
	}
	open := func() int64 {
		t.Helper()
		before := semanticRowsHashed.Load()
		generation, err := openSemanticGenerationForSearch(dir, "head")
		if err != nil {
			t.Fatal(err)
		}
		_ = generation.Close()
		return semanticRowsHashed.Load() - before
	}
	if checks := open(); checks != 0 {
		t.Fatalf("a search open ran %d full checks, want 0", checks)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, semanticRowsFile), later, later); err != nil {
		t.Fatal(err)
	}
	if checks := open(); checks == 0 {
		t.Fatal("a touched rows.bin opened with no full check")
	}
}

// TestFileReuseRowsMatchFreshRows builds a generation for one file, then asks
// the reuse index for the same content under another root with the same
// path tail. A second build, with no reuse index, stores that file under the
// new path. The copied rows equal the rows of that fresh build. Each copied
// row reads the vectors that the fresh build stores for it, so file reuse
// changes no stored byte. The test model gives each window a vector of its own
// model input, so a row that reads the vectors of another row shows.
func TestFileReuseRowsMatchFreshRows(t *testing.T) {
	requireTools(t)
	content := longSemanticFunction(6_000)
	embedder := &hybridTestScorer{mode: payloadAccept}
	// build stores content at path in a generation of indexDir, with no
	// reuse index.
	build := func(indexDir, path string) string {
		t.Helper()
		dir := filepath.Join(indexDir, semanticGenerationPrefix+"source")
		documents := make(chan fileContent, 1)
		documents <- makeFileContent(t, path, content)
		close(documents)
		if err := buildSemanticGeneration(t.Context(), dir, "source", embedder, searchResources{},
			func(context.Context) (<-chan fileContent, func() error) {
				return documents, func() error { return nil }
			},
			&semanticReuseIndex{}); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	indexDir := t.TempDir()
	build(indexDir, "one/x/src/pkg/long.go")
	reuse := loadSemanticReuse(indexDir, filepath.Join(indexDir, "staging"), "", "", nil)
	defer reuse.Close()
	const path = "two/x/src/pkg/long.go"
	copied, ok := reuse.fileRows(path, semanticContentID(sha256.Sum256(content)))
	if !ok {
		t.Fatal("the reuse index has no rows for the same tail and content")
	}
	fresh, err := openSemanticGenerationRows(build(t.TempDir(), path), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if len(copied) < 2 || len(copied) != len(fresh.rows) {
		t.Fatalf("copied %d rows, fresh %d, want the same count above 1", len(copied), len(fresh.rows))
	}
	for index := range fresh.rows {
		got, want := copied[index], fresh.rows[index]
		got.reuse, got.row, got.text, got.modelInput = nil, 0, nil, nil
		want.reuse, want.row, want.text, want.modelInput = nil, 0, nil, nil
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("row %d:\ncopied %+v\nfresh  %+v", index, got, want)
		}
		if copied[index].reuse == nil {
			t.Fatalf("row %d names no vectors to copy", index)
		}
		if fine, coarse := copied[index].reuse.vectors(); *fine != fresh.vectors[index] || *coarse != fresh.coarse[index] {
			t.Fatalf("row %d copies other vectors than the fresh build stores for it", index)
		}
	}
}

// TestBranchSwitchEmbedsNothingNew searches branch A, then branch B with 5
// changed files, then branch A again. The third search copies every window
// from the unit store and embeds nothing.
func TestBranchSwitchEmbedsNothingNew(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := initGitRepo(t, "seed.go", "package sample\n// alpha beta seed\n")
	for i := range 20 {
		writeFileAt(t, repo, fmt.Sprintf("f%02d.go", i), fmt.Sprintf("package sample\n// alpha beta branch a %d\n", i))
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "a")
	gitRunIn(t, repo, "branch", "a")
	gitRunIn(t, repo, "checkout", "-q", "-b", "b")
	for i := range 5 {
		writeFileAt(t, repo, fmt.Sprintf("f%02d.go", i), fmt.Sprintf("package sample\n// alpha beta branch b %d\n", i))
	}
	gitRunIn(t, repo, "commit", "-q", "-am", "b")
	t.Chdir(t.TempDir())
	gitRunIn(t, repo, "checkout", "-q", "a")
	if first := reuseSearch(t, repo); len(first) < 21 {
		t.Fatalf("branch a embedded %d rows", len(first))
	}
	gitRunIn(t, repo, "checkout", "-q", "b")
	if second := reuseSearch(t, repo); len(second) != 5 {
		t.Fatalf("branch b embedded %d rows, want the 5 changed files", len(second))
	}
	gitRunIn(t, repo, "checkout", "-q", "a")
	if third := reuseSearch(t, repo); len(third) != 0 {
		t.Fatalf("back on branch a the search embedded %v, want nothing", third)
	}
}

// TestCacheBudgetEvictsUnusedUnits puts a unit-store segment and a corpus over
// the budget: the segment goes first, since no live generation references it.
func TestCacheBudgetEvictsUnusedUnits(t *testing.T) {
	root := cacheRootForTest(t)
	corpus := seedCorpus(t, root, fakeCorpusHash(401), time.Now().Add(-48*time.Hour))
	dir := semanticUnitsDirFor(root)
	writeFileAt(t, dir, "00000000000000000001-1"+semanticUnitsSegmentExt, strings.Repeat("x", seededCorpusBytes))
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "00000000000000000001-1"+semanticUnitsSegmentExt), old, old)
	t.Setenv(envCacheBudget, strconv.Itoa(seededCorpusBytes*3/2))
	runGC(t.Context(), gcOptions{skipThrottle: true})
	if _, err := os.Stat(filepath.Join(dir, "00000000000000000001-1"+semanticUnitsSegmentExt)); !os.IsNotExist(err) {
		t.Fatalf("the unit segment survived: %v", err)
	}
	if _, err := os.Stat(corpus); err != nil {
		t.Fatalf("the corpus went although evicting the segment made the cache fit: %v", err)
	}
}

// TestDamagedSourceRowIsNotCopied zeroes the vectors of a live generation's
// row: the next build embeds that window instead of copying the damage.
func TestDamagedSourceRowIsNotCopied(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := initGitRepo(t, "a.go", "package sample\n// alpha beta one\n")
	t.Chdir(t.TempDir())
	if first := reuseSearch(t, repo); len(first) == 0 {
		t.Fatal("the first build embedded nothing")
	}
	plans, err := planCorpora(context.Background(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	dirs := semanticGenerationDirs(t, plans[0].indexDir)
	if len(dirs) != 1 {
		t.Fatalf("generations=%d", len(dirs))
	}
	vectors := filepath.Join(dirs[0], semanticVectorsFile)
	if err := os.WriteFile(vectors, make([]byte, semanticFineVectorBytesPerUnit*fileRowCount(t, dirs[0])), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, repo, "b.go", "package sample\n// alpha beta two\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "two")
	if second := reuseSearch(t, repo); !slices.Contains(second, "a.go") {
		t.Fatalf("the build copied a damaged row: embedded %v", second)
	}
}

// fileRowCount returns the row count of the generation in dir, from its
// manifest.
func fileRowCount(t *testing.T, dir string) int {
	t.Helper()
	manifest, err := readSemanticManifestUnchecked(dir)
	if err != nil {
		t.Fatal(err)
	}
	return int(manifest.Rows)
}

// TestRowsCheckRefreshedAfterAFullCheck touches rows.bin: the next open runs
// the full check and records it, so the open after that hashes nothing.
func TestRowsCheckRefreshedAfterAFullCheck(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	dir := filepath.Join(t.TempDir(), "semantic")
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head", units, embeddings); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, semanticRowsFile), later, later); err != nil {
		t.Fatal(err)
	}
	for index, wantHashed := range []bool{true, false} {
		before := semanticRowsHashed.Load()
		generation, err := openSemanticGenerationForSearch(dir, "head")
		if err != nil {
			t.Fatal(err)
		}
		_ = generation.Close()
		if hashed := semanticRowsHashed.Load() > before; hashed != wantHashed {
			t.Fatalf("open %d hashed=%t, want %t", index, hashed, wantHashed)
		}
	}
}

// TestColdFolderReadsNoOtherCorpus searches a Git repository and a folder,
// then a new folder. All three hold the same file at the same path tail, so
// its rows have the same keys in each. A Git lineage shares nothing with a
// folder. A folder whose root neither holds nor sits in another root shares no
// lineage. So each build reads no generation of another corpus, copies no row
// from one, and embeds the file.
func TestColdFolderReadsNoOtherCorpus(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	const name, content = "p/q/r/a.go", "package sample\n// alpha beta one\n"
	repo := initGitRepo(t, "seed.go", "package sample\n// alpha beta seed\n")
	writeFileAt(t, repo, name, content)
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "tail")
	first := t.TempDir()
	writeFileAt(t, first, name, content)
	cold := t.TempDir()
	writeFileAt(t, cold, name, content)
	t.Chdir(t.TempDir())
	for _, root := range []string{repo, first, cold} {
		if embedded := reuseSearch(t, root); !slices.Contains(embedded, name) {
			t.Fatalf("%s embedded %v, want %s: its build copied rows of another corpus", root, embedded, name)
		}
	}
}

// TestReuseIndexesOnlyRowsOfTheNodeTails adds the generation of a folder to
// the reuse index of a node below it. With the node's names, the index holds
// only the rows whose model path tail is the tail of a node file. That holds
// also outside the node's root, because a copy keeps the last 4 path parts.
// With an empty name set, the index holds no row. With no name set (nil), it
// holds every row.
func TestReuseIndexesOnlyRowsOfTheNodeTails(t *testing.T) {
	paths := []string{"other/b.go", "sub/a.go", "a/src/pkg/x/f.go", "b/a.go"}
	for _, row := range []struct {
		names  []string
		want   []string
		ranges int
	}{
		{names: []string{"src/pkg/x/f.go", "a.go"}, want: []string{"a/src/pkg/x/f.go", "b/a.go"}, ranges: 1},
		{names: []string{}, want: nil, ranges: 1},
		{names: nil, want: paths},
	} {
		generation := &semanticGeneration{manifest: semanticManifest{ModelPathBase: "/h"}}
		for index, name := range paths {
			generation.rows = append(generation.rows, semanticUnit{
				path: name, contentID: semanticContentID{byte(index)}, key: semanticRowKey{byte(index)},
			})
		}
		var tails map[string]struct{}
		if row.names != nil {
			tails = map[string]struct{}{}
			for _, name := range row.names {
				tails[semanticModelPathTail("/h/b", name)] = struct{}{}
			}
		}
		index := &semanticReuseIndex{base: "/h/b", rows: map[semanticRowKey]semanticReuseRef{}, files: map[semanticFileReuseKey]semanticFileReuse{}}
		index.add(generation, tails)
		var got []string
		for position, name := range paths {
			_, keyed := index.rows[semanticRowKey{byte(position)}]
			_, filed := index.files[semanticFileReuseKey{
				tail: rerankPathTail(semanticModelPath("/h", name)), contentID: semanticContentID{byte(position)},
			}]
			if keyed != filed {
				t.Fatalf("names %v: %s keyed=%v filed=%v", row.names, name, keyed, filed)
			}
			if keyed {
				got = append(got, name)
			}
		}
		if !slices.Equal(got, row.want) {
			t.Fatalf("names %v indexed %v, want %v", row.names, got, row.want)
		}
		// Skipped rows next to each other make one range.
		if len(index.skipped) != row.ranges {
			t.Fatalf("names %v: skipped ranges %d, want %d", row.names, len(index.skipped), row.ranges)
		}
		// A file that can match another tail indexes every row key of a
		// skipped range, and no file.
		index.markReused([]semanticUnit{{path: "c.go", pathCut: true}})
		if len(index.pendingRows) != len(paths)-len(row.want) || len(index.files) != len(row.want) {
			t.Fatalf("names %v after a lookup: later row keys %d, files %d; want %d, %d", row.names, len(index.pendingRows), len(index.files), len(paths)-len(row.want), len(row.want))
		}
	}
}

// TestSkippedRangesStopAtGapsAndGenerations adds two folder generations for
// a node's tails. A kept row between two skipped rows splits their range. A
// skipped range of the second generation that starts where the first one's
// ends stays apart. The fill then indexes exactly the skipped row keys.
func TestSkippedRangesStopAtGapsAndGenerations(t *testing.T) {
	row := func(path string, key byte) semanticUnit {
		return semanticUnit{path: path, key: semanticRowKey{key}, contentID: semanticContentID{key}}
	}
	first := &semanticGeneration{manifest: semanticManifest{ModelPathBase: "/h"}, rows: []semanticUnit{
		row("other/a.go", 1), row("sub/x.go", 2), row("other/b.go", 3),
	}}
	second := &semanticGeneration{manifest: semanticManifest{ModelPathBase: "/h"}, rows: []semanticUnit{
		row("sub/x.go", 4), row("sub/x.go", 5), row("sub/x.go", 6), row("other/c.go", 7),
	}}
	index := &semanticReuseIndex{base: "/h/sub", rows: map[semanticRowKey]semanticReuseRef{}, files: map[semanticFileReuseKey]semanticFileReuse{}}
	tails := map[string]struct{}{semanticModelPathTail("/h/sub", "x.go"): {}}
	index.add(first, tails)
	index.add(second, tails)
	want := []semanticFileReuse{{generation: first, first: 0, count: 1}, {generation: first, first: 2, count: 1}, {generation: second, first: 3, count: 1}}
	if !slices.Equal(index.skipped, want) {
		t.Fatalf("skipped ranges %+v, want %+v", index.skipped, want)
	}
	index.markReused([]semanticUnit{{path: "y.go", pathCut: true}})
	for _, key := range []byte{1, 3, 7} {
		if _, ok := index.pendingRows[semanticRowKey{key}]; !ok {
			t.Fatalf("skipped key %d is not indexed by the fill", key)
		}
	}
	if len(index.pendingRows) != 3 {
		t.Fatalf("the fill indexed %d keys, want the 3 skipped ones", len(index.pendingRows))
	}
}

// TestGenerationShapeSamplesTailsThatAreNotPlain counts the files of a
// generation of 64 files: one row each, but 3 for every 4th file. It estimates
// the files and rows whose tail is not plain from every 16th file (0, 16, 32
// and 48, of 3 rows each). So it sees the files of those places only.
func TestGenerationShapeSamplesTailsThatAreNotPlain(t *testing.T) {
	for _, row := range []struct {
		name        string
		notPlain    func(int) bool
		files, rows int
	}{
		{name: "plain", notPlain: func(int) bool { return false }},
		{name: "every file", notPlain: func(int) bool { return true }, files: 64, rows: 96},
		{name: "the sampled files", notPlain: func(file int) bool { return file%16 == 0 }, files: 64, rows: 96},
		{name: "half the sampled files", notPlain: func(file int) bool { return file%32 == 0 }, files: 32, rows: 96},
		{name: "files between samples", notPlain: func(file int) bool { return file%16 == 1 }},
	} {
		generation := &semanticGeneration{manifest: semanticManifest{ModelPathBase: "/h"}}
		for file := range 64 {
			name := fmt.Sprintf("d/f%02d.go", file)
			if row.notPlain(file) {
				name = fmt.Sprintf("d/f%02d\u00e9.go", file)
			}
			rows := 1
			if file%4 == 0 {
				rows = 3
			}
			for range rows {
				generation.rows = append(generation.rows, semanticUnit{path: name})
			}
		}
		shape := semanticGenerationShape(generation)
		if shape.files != 64 || shape.notPlainFiles != row.files || shape.notPlainRows != row.rows {
			t.Fatalf("%s: shape %+v, want 64 files, %d not plain, %d rows not plain", row.name, shape, row.files, row.rows)
		}
	}
}

// TestReuseLoadIndexesTheNodeTailsOfOtherCorpora searches a folder of 42
// files of several rows each, then loads the reuse index of nodes in other
// corpora. A file root at its top level indexes the one file by file and by
// row key. It keeps the folder's other row keys pending: a file that can match
// another tail (matchesOtherTails) indexes those others. Names that cover half
// of the folder's files (21) do the same. More names (22, still fewer than
// half of the rows) index every row at once. So do no names, and a node whose
// root holds the folder. The file matches by file through its shallow name.
func TestReuseLoadIndexesTheNodeTailsOfOtherCorpora(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	holder := t.TempDir()
	filler := strings.Repeat("// filler words that give this file a second and a third window\n", 30)
	for index := range 41 {
		writeFileAt(t, holder, fmt.Sprintf("f%02d.go", index), fmt.Sprintf("package x\n// alpha beta file %d\n", index)+filler)
	}
	top := "package x\n// alpha beta top\n" + filler
	writeFileAt(t, holder, "top.go", top)
	t.Chdir(t.TempDir())
	if first := reuseSearch(t, holder); len(first) == 0 {
		t.Fatal("the folder embedded nothing")
	}
	real, err := filepath.EvalSymlinks(holder)
	if err != nil {
		t.Fatal(err)
	}
	names := func(count int) []string {
		names := []string{"top.go"}
		for index := range count - 1 {
			names = append(names, fmt.Sprintf("absent%02d.go", index))
		}
		return names
	}
	fileRoot := semanticFolderLineagePrefix + filepath.ToSlash(filepath.Join(real, "top.go"))
	parent := filepath.Dir(real)
	load := func(lineage, base string, names []string) *semanticReuseIndex {
		indexDir := t.TempDir()
		return loadSemanticReuse(indexDir, filepath.Join(indexDir, "staging"), lineage, filepath.ToSlash(base), names)
	}
	// total and topRows count row keys: windows with the same input share one.
	all := load(fileRoot, real, nil)
	total, rows, topKeys := len(all.rows), len(all.generations[0].rows), map[semanticRowKey]bool{}
	for _, row := range all.generations[0].rows {
		if row.path == "top.go" {
			topKeys[row.key] = true
		}
	}
	all.Close()
	topRows := len(topKeys)
	if topRows < 2 || rows < 2*42 {
		t.Fatalf("top.go has %d row keys and the folder %d rows, want 2 or more for each file", topRows, rows)
	}
	for _, row := range []struct {
		lineage, base, name string
		names               []string
		files, rows, later  int
	}{
		{lineage: fileRoot, base: real, name: "top.go", names: names(1), files: 1, rows: topRows, later: total - topRows},
		{lineage: fileRoot, base: real, name: "top.go", names: names(21), files: 1, rows: topRows, later: total - topRows},
		{lineage: fileRoot, base: real, name: "top.go", names: names(22), files: 42, rows: total},
		{lineage: fileRoot, base: real, name: "top.go", names: nil, files: 42, rows: total},
		{lineage: semanticFolderLineagePrefix + filepath.ToSlash(parent), base: parent,
			name: filepath.Base(real) + "/top.go", names: []string{filepath.Base(real) + "/top.go"}, files: 42, rows: total},
	} {
		reuse := load(row.lineage, row.base, row.names)
		files, rows := len(reuse.files), len(reuse.rows)
		_, matched := reuse.fileRows(row.name, semanticContentID(sha256.Sum256([]byte(top))))
		reuse.markReused([]semanticUnit{{path: row.name, pathCut: true}})
		later := len(reuse.pendingRows)
		reuse.Close()
		if len(reuse.generations) != 1 || files != row.files || rows != row.rows || later != row.later || !matched {
			t.Fatalf("%s, %d names: generations %d, files %d, rows %d, later row keys %d, %s matched %v; want 1, %d, %d, %d, true",
				row.lineage, len(row.names), len(reuse.generations), files, rows, later, row.name, matched, row.files, row.rows, row.later)
		}
	}
}

// TestMarkReusedLooksUpPendingKeysOnlyForOtherTails gives a reuse index one
// skipped range. A file whose header holds its whole plain tail cannot match
// a pending row, whose tail is plain too, so it indexes nothing more. A cut
// path or a tail that is not plain (semanticPlainTail) indexes the pending
// row keys.
func TestMarkReusedLooksUpPendingKeysOnlyForOtherTails(t *testing.T) {
	generation := &semanticGeneration{rows: []semanticUnit{{path: "a.go", key: semanticRowKey{1}}, {path: "b.go", key: semanticRowKey{2}}}}
	for _, row := range []struct {
		unit semanticUnit
		want int
	}{
		{unit: semanticUnit{path: "x.go"}, want: 0},
		{unit: semanticUnit{path: "x.go", pathCut: true}, want: 2},
		{unit: semanticUnit{path: "caf\u00e9.go"}, want: 2},
		{unit: semanticUnit{path: "x[MASK].go"}, want: 2},
		{unit: semanticUnit{path: "x\ny.go"}, want: 2},
		{unit: semanticUnit{path: "notes "}, want: 2},
	} {
		index := &semanticReuseIndex{base: "/h", rows: map[semanticRowKey]semanticReuseRef{}, skipped: []semanticFileReuse{{generation: generation, count: 2}}}
		index.markReused([]semanticUnit{row.unit})
		if len(index.pendingRows) != row.want {
			t.Fatalf("%+v: pending row keys %d, want %d", row.unit, len(index.pendingRows), row.want)
		}
	}
}

// TestSkippedTailsThatAreNotPlainStayIndexed adds a folder generation for a
// node's tails. A row of another tail that is not plain stays indexed by row
// key, because a plain node file can share its key. A plain one waits.
func TestSkippedTailsThatAreNotPlainStayIndexed(t *testing.T) {
	for _, row := range []struct {
		other string
		want  bool
	}{
		{other: "other/b.go", want: false},
		{other: "other/\u212a.go", want: true},
		{other: "other/x [MASK].go", want: true},
		{other: "other/notes ", want: true},
	} {
		generation := &semanticGeneration{manifest: semanticManifest{ModelPathBase: "/h"}, rows: []semanticUnit{
			{path: "sub/a.go", key: semanticRowKey{1}}, {path: row.other, key: semanticRowKey{2}},
		}}
		index := &semanticReuseIndex{base: "/h/sub", rows: map[semanticRowKey]semanticReuseRef{}, files: map[semanticFileReuseKey]semanticFileReuse{}}
		index.add(generation, map[string]struct{}{semanticModelPathTail("/h/sub", "a.go"): {}})
		if _, ok := index.rows[semanticRowKey{2}]; ok != row.want {
			t.Fatalf("%q: indexed %v, want %v", row.other, ok, row.want)
		}
	}
}

// TestMarkReusedRunsInParallel calls markReused from 32 workers, as the build
// does. Files that can match another tail index the pending row keys once,
// while the others read only the first index. Each such file sees its pending
// row key. Run with -race.
func TestMarkReusedRunsInParallel(t *testing.T) {
	generation := &semanticGeneration{}
	for index := range 1_000 {
		generation.rows = append(generation.rows, semanticUnit{path: fmt.Sprintf("d/f%d.go", index), key: semanticRowKey{byte(index), byte(index >> 8), 1}})
	}
	index := &semanticReuseIndex{base: "/h", rows: map[semanticRowKey]semanticReuseRef{{9}: {}}, skipped: []semanticFileReuse{{generation: generation, count: 1_000}}}
	var wait sync.WaitGroup
	for worker := range 32 {
		wait.Go(func() {
			key := generation.rows[worker].key
			rows := []semanticUnit{{path: "x.go", key: key, pathCut: worker%2 == 0}}
			index.markReused(rows)
			if worker%2 == 0 {
				if _, ok := index.pendingRows[key]; !ok {
					t.Errorf("worker %d: its pending row key is not indexed", worker)
				}
			}
		})
	}
	wait.Wait()
}

// TestRenamedLongTailFileReusesItsRows renames a file whose model path tail
// is longer than the window header keeps. Then it searches the subfolder that
// holds the file. No file key matches the new name. But its windows have the
// keys of the old rows (the folder's pending row keys), so the build embeds
// nothing.
func TestRenamedLongTailFileReusesItsRows(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	holder := t.TempDir()
	dir := "b/kubernetes_cluster_autoscaler_cloudprovider_integration/aws_elastic_kubernetes_service_managed_nodegroup_provider/autoscaling_group_instance_template_cache"
	writeFileAt(t, holder, filepath.Join(dir, "one.go"), "package x\n\n// alpha beta gamma\nfunc Run() int {\n\treturn 1\n}\n")
	writeFileAt(t, holder, "b/other.go", "package b\n// alpha beta other\n")
	// Files outside b, so the subfolder's files are at most half of the
	// folder's and its build takes the folder's tails only.
	for index := range 3 {
		writeFileAt(t, holder, fmt.Sprintf("c/f%d.go", index), fmt.Sprintf("package c\n// alpha beta c %d\n", index))
	}
	t.Chdir(t.TempDir())
	if first := reuseSearch(t, holder); len(first) == 0 {
		t.Fatal("the folder embedded nothing")
	}
	if err := os.Rename(filepath.Join(holder, dir, "one.go"), filepath.Join(holder, dir, "two.go")); err != nil {
		t.Fatal(err)
	}
	if embedded := reuseSearch(t, filepath.Join(holder, "b")); len(embedded) != 0 {
		t.Fatalf("the subfolder embedded %v after the rename, want nothing", embedded)
	}
}

// TestSemanticModelPathTailIsTheTailOfTheModelPath compares the tail with the
// last 4 parts of the joined model path. It uses names of 1 to 6 parts and
// every kind of base: none (Git), the root folder, and short and deep
// folders. Odd paths check rerankPathTail alone. The test joins the base and
// the name itself, not with semanticModelPath. So a wrong join, such as
// "//f.go" for the root folder, also fails it.
func TestSemanticModelPathTailIsTheTailOfTheModelPath(t *testing.T) {
	lastParts := func(value string) string {
		parts := strings.Split(value, "/")
		return strings.Join(parts[max(len(parts)-4, 0):], "/")
	}
	// joined is the model path of a clean base and name. With no base, it is
	// the name alone. Else it joins the base and the name with one slash.
	joined := func(base, name string) string {
		if base == "" {
			return name
		}
		return strings.TrimSuffix(base, "/") + "/" + name
	}
	for _, base := range []string{"", "/", "/h", "/h/i", "/a/b/c/d/e"} {
		for _, name := range []string{"f.go", "a/f.go", "a/b/f.go", "a/b/c/f.go", "a/b/c/d/f.go", "a/b/c/d/e/f.go"} {
			if got, want := semanticModelPathTail(base, name), lastParts(joined(base, name)); got != want {
				t.Fatalf("base %q, name %q: tail %q, want %q", base, name, got, want)
			}
		}
	}
	for _, value := range []string{"", "/", "a", "/a", "a/", "a//b/c/d", "a/b/c/d/", "/a/b/c/x", "//a/b/c/d/e"} {
		if got, want := rerankPathTail(value), lastParts(value); got != want {
			t.Fatalf("%q: tail %q, want %q", value, got, want)
		}
	}
}

// TestCopiedOrMovedSubfolderReusesTheFolderRows searches a folder, then copies
// or moves one of its deep subfolders inside it and searches the subfolder.
// Its files keep the last 4 path parts and the content, so the build copies
// their rows from the folder's generation. Only a file whose tail names the
// moved folder embeds again.
func TestCopiedOrMovedSubfolderReusesTheFolderRows(t *testing.T) {
	requireTools(t)
	for _, row := range []struct {
		name string
		move bool
		want []string
	}{
		{name: "copy", want: nil},
		{name: "move", move: true, want: []string{"top.go"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			setTestUserCache(t)
			holder := t.TempDir()
			for index := range 3 {
				writeFileAt(t, holder, fmt.Sprintf("a/src/pkg/x/f%d.go", index), fmt.Sprintf("package x\n// alpha beta file %d\n", index))
			}
			writeFileAt(t, holder, "a/top.go", "package top\n// alpha beta top\n")
			t.Chdir(t.TempDir())
			if first := reuseSearch(t, holder); len(first) == 0 {
				t.Fatal("the folder embedded nothing")
			}
			if row.move {
				if err := os.Rename(filepath.Join(holder, "a"), filepath.Join(holder, "b")); err != nil {
					t.Fatal(err)
				}
			} else {
				for index := range 3 {
					writeFileAt(t, holder, fmt.Sprintf("b/src/pkg/x/f%d.go", index), fmt.Sprintf("package x\n// alpha beta file %d\n", index))
				}
			}
			if embedded := reuseSearch(t, filepath.Join(holder, "b")); !slices.Equal(embedded, row.want) {
				t.Fatalf("the subfolder embedded %v, want %v", embedded, row.want)
			}
		})
	}
}

// TestUnitStoreSegmentsStayFew changes one file in 16 searches in a row. Each
// bind archives that file's old window, and the store merges its segments, so
// it holds few segments. An old version still comes back with no new
// embedding. One segment per bind would grow the store without end.
func TestUnitStoreSegmentsStayFew(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "other.go", "package sample\n// alpha other\n")
	t.Chdir(t.TempDir())
	version := func(n int) string {
		return fmt.Sprintf("package sample\n// alpha changing version %d\n", n)
	}
	for n := range 17 {
		writeFileAt(t, folder, "change.go", version(n))
		reuseSearch(t, folder)
	}
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	segments := semanticUnitsSegments(cacheRoot)
	if len(segments) == 0 || len(segments) > 5 {
		t.Fatalf("segments=%d after 16 archives, want 1 to 5 (about log2 of the records)", len(segments))
	}
	writeFileAt(t, folder, "change.go", version(3))
	if embedded := reuseSearch(t, folder); len(embedded) != 0 {
		t.Fatalf("an archived version embedded %v again", embedded)
	}
}
