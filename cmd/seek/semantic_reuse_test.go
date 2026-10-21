package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	ctags "github.com/sourcegraph/go-ctags"
)

// reuseSearch runs one descriptive search with a fresh test scorer and
// returns the paths that the model embedded.
func reuseSearch(t *testing.T, query string, root string) []string {
	t.Helper()
	scorer := &hybridTestScorer{}
	if _, err := runHybridTestSearch(t, query, []string{root}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil }); err != nil {
		t.Fatal(err)
	}
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	return slices.Clone(scorer.embeddedPaths)
}

// TestCacheKeyIncludesModelInput checks that the key follows the model input:
// the same bytes under two path tails give two keys; the same bytes and tail
// in two repositories give one key.
func TestCacheKeyIncludesModelInput(t *testing.T) {
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	key := func(path string) semanticRowKey {
		units := extractSemanticUnits(path, []byte("package sample\n// same bytes\n"), nil, nil)
		rows, err := windowSemanticUnits(t.Context(), tokenizer, units)
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
	if first := reuseSearch(t, "alpha beta", repo); len(first) < 201 {
		t.Fatalf("the first build embedded %d rows, want every file", len(first))
	}
	writeFileAt(t, repo, "pkg/f100.go", "package sample\n// alpha beta changed file\n")
	gitRunIn(t, repo, "commit", "-q", "-am", "change one")
	second := reuseSearch(t, "alpha beta", repo)
	if len(second) == 0 || slices.ContainsFunc(second, func(path string) bool { return path != "pkg/f100.go" }) {
		t.Fatalf("the second build embedded %v, want pkg/f100.go only", second)
	}
}

// TestOneLineEditReembedsOneUnit edits one line inside the middle function of
// a file: the next build embeds that unit only.
func TestOneLineEditReembedsOneUnit(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	content := "package sample\n\nfunc Alpha() int {\n\treturn 1 // alpha beta\n}\n\n" +
		"func Beta() int {\n\treturn 2 // alpha beta\n}\n\nfunc Gamma() int {\n\treturn 3 // alpha beta\n}\n"
	repo := initGitRepo(t, "three.go", content)
	t.Chdir(t.TempDir())
	if first := reuseSearch(t, "alpha beta", repo); len(first) < 3 {
		t.Fatalf("the first build embedded %d rows", len(first))
	}
	writeFileAt(t, repo, "three.go", strings.Replace(content, "return 2", "return 22", 1))
	gitRunIn(t, repo, "commit", "-q", "-am", "edit Beta")
	if second := reuseSearch(t, "alpha beta", repo); len(second) != 1 {
		t.Fatalf("the second build embedded %d rows (%v), want the edited unit only", len(second), second)
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
	if first := reuseSearch(t, "alpha beta", repo); len(first) == 0 {
		t.Fatal("the first build embedded nothing")
	}
	worktree := filepath.Join(t.TempDir(), "wt")
	gitRunIn(t, repo, "worktree", "add", "-q", "--detach", worktree)
	if second := reuseSearch(t, "alpha beta", worktree); len(second) != 0 {
		t.Fatalf("the linked worktree embedded %v, want nothing", second)
	}
}

// TestRunFolderChangeEmbedsOnlyChangedFile edits one file of a 50-file folder
// so that it describes the query: the search finds it through vectors, and
// the build embeds rows of that file only.
func TestRunFolderChangeEmbedsOnlyChangedFile(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	for i := range 49 {
		writeFileAt(t, folder, fmt.Sprintf("f%02d.go", i), fmt.Sprintf("package sample\n// alpha file %d\n", i))
	}
	t.Chdir(t.TempDir())
	if first := reuseSearch(t, "alpha beta", folder); len(first) < 50 {
		t.Fatalf("the first build embedded %d rows", len(first))
	}
	writeFileAt(t, folder, "f07.go", "package sample\n// SEMANTIC_TARGET dispatches a request to its handler\n")
	scorer := &hybridTestScorer{}
	output, err := runHybridTestSearch(t, "alpha beta", []string{folder}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return scorer, nil })
	if err != nil || !strings.Contains(output, "## "+filepath.Join(folder, "f07.go")) {
		t.Fatalf("output=%q err=%v, want f07.go through vectors", output, err)
	}
	if got := scorer.embeddedPaths; len(got) == 0 || slices.ContainsFunc(got, func(path string) bool { return path != "f07.go" }) {
		t.Fatalf("the change embedded %v, want f07.go only", got)
	}
}

// TestRowsIntegrityCheckedOnceAtBind builds a generation, then opens it for a
// search: the open hashes no rows.bin byte. A touched rows.bin runs the full
// check again.
func TestRowsIntegrityCheckedOnceAtBind(t *testing.T) {
	units, embeddings := testSemanticRowsAndVectors(t)
	dir := filepath.Join(t.TempDir(), "semantic")
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head", units, embeddings); err != nil {
		t.Fatal(err)
	}
	open := func() int64 {
		t.Helper()
		before := semanticRowsHashedBytes.Load()
		generation, err := openSemanticGenerationForSearch(dir, "head")
		if err != nil {
			t.Fatal(err)
		}
		_ = generation.Close()
		return semanticRowsHashedBytes.Load() - before
	}
	if hashed := open(); hashed != 0 {
		t.Fatalf("a search open hashed %d bytes, want 0", hashed)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, semanticRowsFile), later, later); err != nil {
		t.Fatal(err)
	}
	if hashed := open(); hashed == 0 {
		t.Fatal("a touched rows.bin opened with no full check")
	}
}

// TestFileReuseRowsMatchFreshRows builds a generation for one file, then asks
// the reuse index for the same content under another root with the same
// path tail: the copied rows equal the rows that a fresh extraction gives,
// so file reuse changes no stored byte.
func TestFileReuseRowsMatchFreshRows(t *testing.T) {
	requireTools(t)
	content := longSemanticFunction(6_000)
	indexDir := t.TempDir()
	dir := filepath.Join(indexDir, semanticGenerationPrefix+"source")
	documents := make(chan fileContent, 1)
	documents <- makeFileContent(t, "one/x/src/pkg/long.go", content)
	close(documents)
	embedder := semanticBuildTestEmbedder{embed: func(_ context.Context, units []semanticUnit) ([]semanticUnitEmbedding, error) {
		embeddings := make([]semanticUnitEmbedding, len(units))
		for index := range embeddings {
			embeddings[index] = testSemanticUnitEmbedding(semanticVector{0: 1})
		}
		return embeddings, nil
	}}
	if _, err := buildSemanticGeneration(t.Context(), dir, "source", embedder, searchResources{},
		func(context.Context) (<-chan fileContent, func() error) {
			return documents, func() error { return nil }
		},
		nil); err != nil {
		t.Fatal(err)
	}
	reuse := loadSemanticReuse(indexDir, filepath.Join(indexDir, "staging"), "")
	defer reuse.Close()
	const path = "two/x/src/pkg/long.go"
	copied, ok := reuse.fileRows(path, semanticContentID(sha256.Sum256(content)))
	if !ok {
		t.Fatal("the reuse index has no rows for the same tail and content")
	}
	if err := checkCtagsCached(); err != nil {
		t.Fatal(err)
	}
	parser, err := ctags.New(ctags.Options{Bin: os.Getenv("CTAGS_COMMAND")})
	if err != nil {
		t.Fatal(err)
	}
	defer parser.Close()
	entries, parseErr := parser.Parse(path, content)
	tokenizer, err := lateOnTokenizers.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lateOnTokenizers.release(tokenizer)
	fresh, err := windowSemanticUnits(t.Context(), tokenizer,
		extractEligibleSemanticUnits(path, content, entries, parseErr, "Go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != len(fresh) {
		t.Fatalf("copied %d rows, fresh %d", len(copied), len(fresh))
	}
	for index := range fresh {
		got, want := copied[index], fresh[index]
		got.reuse, got.row, want.text, want.modelInput, want.row = nil, 0, nil, nil, 0
		got.text = nil
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("row %d:\ncopied %+v\nfresh  %+v", index, got, want)
		}
	}
}

// TestBranchSwitchEmbedsNothingNew searches branch A, then branch B with 5
// changed files, then branch A again: the third search copies every window
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
	if first := reuseSearch(t, "alpha beta", repo); len(first) < 21 {
		t.Fatalf("branch a embedded %d rows", len(first))
	}
	gitRunIn(t, repo, "checkout", "-q", "b")
	if second := reuseSearch(t, "alpha beta", repo); len(second) != 5 {
		t.Fatalf("branch b embedded %d rows, want the 5 changed files", len(second))
	}
	gitRunIn(t, repo, "checkout", "-q", "a")
	if third := reuseSearch(t, "alpha beta", repo); len(third) != 0 {
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
	runGC(t.Context(), gcOptions{skipThrottle: true}, 0)
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
	if first := reuseSearch(t, "alpha beta", repo); len(first) == 0 {
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
	if second := reuseSearch(t, "alpha beta", repo); !slices.Contains(second, "a.go") {
		t.Fatalf("the build copied a damaged row: embedded %v", second)
	}
}

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
		before := semanticRowsHashedBytes.Load()
		generation, err := openSemanticGenerationForSearch(dir, "head")
		if err != nil {
			t.Fatal(err)
		}
		_ = generation.Close()
		if hashed := semanticRowsHashedBytes.Load() > before; hashed != wantHashed {
			t.Fatalf("open %d hashed=%t, want %t", index, hashed, wantHashed)
		}
	}
}

// TestColdFolderReadsNoOtherCorpus searches a Git repository and a folder,
// then a new folder, all three with the same file: a folder has no lineage,
// so its build reads no generation of another corpus, copies no row from
// one, and embeds the file.
func TestColdFolderReadsNoOtherCorpus(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	const content = "package sample\n// alpha beta one\n"
	repo := initGitRepo(t, "a.go", content)
	first := t.TempDir()
	writeFileAt(t, first, "a.go", content)
	cold := t.TempDir()
	writeFileAt(t, cold, "a.go", content)
	t.Chdir(t.TempDir())
	for _, root := range []string{repo, first, cold} {
		if embedded := reuseSearch(t, "alpha beta", root); !slices.Contains(embedded, "a.go") {
			t.Fatalf("%s embedded %v, want a.go: its build copied rows of another corpus", root, embedded)
		}
	}
}

// TestUnitStoreSegmentsStayFew changes one file in 16 searches in a row: each
// bind archives that file's old window, and the store merges its segments, so
// it holds few segments; an old version still comes back with no new
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
		reuseSearch(t, "alpha beta", folder)
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
	if embedded := reuseSearch(t, "alpha beta", folder); len(embedded) != 0 {
		t.Fatalf("an archived version embedded %v again", embedded)
	}
}
