package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestCacheBudgetNeverLeavesLessThanFloor checks the default budget formula:
// the budget is at most 10 % of the volume, and filling it leaves at least 5 %
// free.
func TestCacheBudgetNeverLeavesLessThanFloor(t *testing.T) {
	// Exact values: 10 % of the volume, or less to keep 5 % free.
	for _, tc := range []struct{ capacity, free, corpora, want int64 }{
		{1000, 900, 0, 100}, {1000, 60, 20, 30}, {1000, 30, 0, 0}, {1000, 200, 0, 100},
	} {
		if got := budgetFromVolume(tc.capacity, tc.free, tc.corpora); got != tc.want {
			t.Errorf("%+v: budget %d, want %d", tc, got, tc.want)
		}
	}
	for _, tc := range []struct{ capacity, free, corpora int64 }{
		{1000, 900, 0}, {1000, 60, 20}, {1000, 30, 0}, {1000, 50, 400}, {1 << 40, 1 << 30, 1 << 29},
	} {
		budget := budgetFromVolume(tc.capacity, tc.free, tc.corpora)
		if budget < 0 || budget > tc.capacity/10 {
			t.Errorf("%+v: budget %d outside [0, 10 %%]", tc, budget)
		}
		if growth := budget - tc.corpora; growth > 0 && tc.free-growth < tc.capacity/20 {
			t.Errorf("%+v: budget %d leaves %d free, below the 5 %% floor", tc, budget, tc.free-growth)
		}
	}
}

// TestCacheBudgetDefaultBeforeFirstSearch reads the default budget for a cache
// root that does not exist yet: the volume of its nearest ancestor decides.
func TestCacheBudgetDefaultBeforeFirstSearch(t *testing.T) {
	t.Setenv(envCacheBudget, "")
	budget, _, err := cacheBudget(filepath.Join(t.TempDir(), "not", "yet"), func() int64 { return 0 })
	if err != nil || budget <= 0 {
		t.Fatalf("budget=%d err=%v, want a positive default", budget, err)
	}
}

// TestParseByteSize reads sizes with and without a unit and rejects every
// other form.
func TestParseByteSize(t *testing.T) {
	for value, want := range map[string]int64{
		"512": 512, "64K": 64 << 10, "10M": 10 << 20, "20G": 20 << 30, "1T": 1 << 40,
		"1.5G": 3 << 29, "10MB": 10 << 20, "10MiB": 10 << 20, " 2g ": 2 << 30,
	} {
		if got, err := parseByteSize(value); err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "x", "-1", "10Q", "nan", "inf", "1e30", "100000P", "0x1p4", ".5"} {
		if _, err := parseByteSize(value); err == nil {
			t.Errorf("parseByteSize(%q) succeeded", value)
		}
	}
}

// TestCacheBudgetStopsBeforeBuild sets a tiny budget: the search exits 2
// before any build, names the planned size, the budget, and the variable, and
// creates no corpus.
func TestCacheBudgetStopsBeforeBuild(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	writeFixtureFile(t, f.path("folder/a.txt"), strings.Repeat("zqbudgetmarker text\n", 4096))
	setTestUserCache(t)
	t.Setenv(envCacheBudget, "1K")

	err := seekRoot(context.Background(), t, nil, "--lexical-only", "zqbudgetmarker", f.path("folder"))
	if _, ok := errors.AsType[*cacheBudgetError](err); !ok {
		t.Fatalf("err=%v, want a cache budget error", err)
	}
	if code := exitCodeForError(err); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	message := formatCLIError(err)
	for _, want := range []string{"needs about", "cache budget of 1KB", "SEEK_CACHE_BUDGET"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
	root, err := seekUserCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := enumerateCorpusDirs(filepath.Join(root, corporaDir)); len(entries) != 0 {
		t.Fatalf("the stopped search left %d corpora", len(entries))
	}
}

// TestCacheBudgetCountsBinaryAsZero puts a large binary file next to a small
// text file. The index keeps the binary file by name only, so it does not
// count against the budget.
func TestCacheBudgetCountsBinaryAsZero(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("folder/a.txt", "zqbinarybudgetmarker")
	writeFixtureFile(t, f.path("folder/blob.bin"), string(make([]byte, 4<<20)))
	setTestUserCache(t)
	t.Setenv(envCacheBudget, "1M")
	if got := graphSearch(t, "zqbinarybudgetmarker", f.path("folder")); len(got) != 1 {
		t.Fatalf("found %v, want the text file", got)
	}
}

// TestCacheBudgetEvictsLRU fills the cache past its budget with two unused
// corpora. After the search, the least recently used corpus is gone and the
// other stays. The search's own corpus stays with its ledger entry.
func TestCacheBudgetEvictsLRU(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("folder/a.txt", "zqlrumarker")
	setTestUserCache(t)
	root, err := seekUserCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	older := seedCorpus(t, root, fakeCorpusHash(101), time.Now().Add(-48*time.Hour))
	newer := seedCorpus(t, root, fakeCorpusHash(102), time.Now().Add(-24*time.Hour))
	t.Setenv(envCacheBudget, strconv.Itoa(seededCorpusBytes*3/2))

	if got := graphSearch(t, "zqlrumarker", f.path("folder")); len(got) != 1 {
		t.Fatalf("found %v, want 1 file", got)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("the least recently used corpus survived: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatalf("the newer corpus was evicted although the cache fits without it: %v", err)
	}
	plans, err := planCorpora(context.Background(), []string{f.path("folder")})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	data, err := os.ReadFile(filepath.Join(plans[0].cacheDir, corpusSizeFile))
	if size, convErr := strconv.ParseInt(string(data), 10, 64); err != nil || convErr != nil || size <= 0 {
		t.Fatalf("the search's corpus has no ledger entry: %q err=%v", data, err)
	}
}

// TestBytecapFixtureSearchesToTheEnd puts 3 sparse 3.9 GiB files next to a
// text file: no byte or file count stops the search. The sparse files hold
// zeros, so the index keeps them by name only. Under a 1G budget, only the
// head probe of the estimate lets the search pass. A file system without
// sparse files writes the 11.7 GiB.
func TestBytecapFixtureSearchesToTheEnd(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("folder/text.txt", "zqbytecapmarker")
	for i := range 3 {
		path := f.path("folder/sparse" + strconv.Itoa(i) + ".bin")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(int64(39) << 30 / 10); err != nil {
			t.Fatalf("make the 3.9 GiB file %s: %v", path, err)
		}
		_ = file.Close()
	}
	setTestUserCache(t)
	t.Setenv(envCacheBudget, "1G")
	if got := graphSearch(t, "zqbytecapmarker", f.path("folder")); len(got) != 1 {
		t.Fatalf("found %v, want the text file", got)
	}
}

// budgetFolder writes 41 Go files with 109,749 bytes of text into dir.
func budgetFolder(t *testing.T, dir string) {
	t.Helper()
	writeFileAt(t, dir, "strict.go", "package sample\n// alpha beta\n")
	for i := range 40 {
		writeFileAt(t, dir, fmt.Sprintf("f%02d.go", i),
			"package sample\n// alpha lexical candidate "+strings.Repeat(fmt.Sprintf("word%d ", i), 400)+"\n")
	}
}

// budgetGitKind makes the budget folder in dir a node of kind:
//   - "git": a repository with all files in one commit.
//   - "dirty": "git" with one changed file.
//   - "no-head": a repository with no commit.
//
// Any other kind keeps a plain folder.
func budgetGitKind(t *testing.T, dir, kind string) {
	t.Helper()
	switch kind {
	case "git":
		gitInitCommitAll(t, dir)
	case "dirty":
		gitInitCommitAll(t, dir)
		writeFileAt(t, dir, "strict.go", "package sample\n// alpha beta dirty\n")
	case "no-head":
		initEmptyGitRepoNoRemoteAt(t, dir)
	}
}

// TestCacheBudgetCountsVectorBuild warms a node with a lexical search and sets
// the budget just above the node's size. The first search by meaning would
// add vectors, so it stops before any build, as a cold search does. A dirty
// Git node adds the vectors of the commit and of the overlay; a node with no
// commit adds the overlay only.
func TestCacheBudgetCountsVectorBuild(t *testing.T) {
	requireTools(t)
	for _, kind := range []string{"folder", "git", "dirty", "no-head"} {
		t.Run(kind, func(t *testing.T) {
			setTestUserCache(t)
			root := t.TempDir()
			budgetFolder(t, root)
			budgetGitKind(t, root, kind)
			t.Chdir(t.TempDir())
			if _, err := seek(t, "--lexical-only", "alpha beta", root); err != nil {
				t.Fatal(err)
			}
			plans, err := planCorpora(context.Background(), []string{root})
			if err != nil || len(plans) != 1 {
				t.Fatalf("plans=%d err=%v", len(plans), err)
			}
			t.Setenv(envCacheBudget, strconv.FormatInt(corpusDirSize(plans[0].cacheDir)+1<<10, 10))
			_, _, err = stubSearch(t, payloadFar, "alpha beta", root)
			budgetErr, ok := errors.AsType[*cacheBudgetError](err)
			if !ok {
				t.Fatalf("err=%v, want a cache budget error", err)
			}
			// Only the vectors are new: the growth is above 0 and below the
			// planned size.
			if budgetErr.growth <= 0 || budgetErr.growth >= budgetErr.planned {
				t.Fatalf("growth=%d planned=%d, want 0 < growth < planned", budgetErr.growth, budgetErr.planned)
			}
			if dirs := semanticGenerationDirs(t, plans[0].indexDir); len(dirs) != 0 {
				t.Fatalf("the stopped search built %d vector generations", len(dirs))
			}
		})
	}
}

// TestCacheLedgerMatchesDirAfterVectorBind adds vectors to a warm folder: the
// ledger follows the bind, which writes no new lexical state.
func TestCacheLedgerMatchesDirAfterVectorBind(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	budgetFolder(t, folder)
	t.Chdir(t.TempDir())
	if _, err := seek(t, "--lexical-only", "alpha beta", folder); err != nil {
		t.Fatal(err)
	}
	plans, err := planCorpora(context.Background(), []string{folder})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	// Set the time of the text build's state file one second before its
	// ledger entry, so only the vector bind can make the entry stale. A bind
	// in the time step of the entry also makes it stale: ledgerStale counts
	// an equal time.
	ledger, err := os.Stat(filepath.Join(plans[0].cacheDir, corpusSizeFile))
	if err != nil {
		t.Fatalf("no ledger entry after the text build: %v", err)
	}
	textBuilt := ledger.ModTime().Add(-time.Second)
	if err := os.Chtimes(filepath.Join(plans[0].cacheDir, stateFile), textBuilt, textBuilt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", folder); err != nil {
		t.Fatal(err)
	}
	if dirs := semanticGenerationDirs(t, plans[0].indexDir); len(dirs) != 1 {
		t.Fatalf("vector generations=%d, want 1", len(dirs))
	}
	// Read the ledger entry itself: corpusSize measures the directory again
	// when the entry is stale or missing.
	data, err := os.ReadFile(filepath.Join(plans[0].cacheDir, corpusSizeFile))
	if err != nil {
		t.Fatalf("no ledger entry: %v", err)
	}
	if got, want := string(data), strconv.FormatInt(corpusDirSize(plans[0].cacheDir), 10); got != want {
		t.Fatalf("ledger=%s, corpus=%s", got, want)
	}
}

// TestOpportunisticGCKeepsCorporaOfTheCommand runs the GC with a budget of 1
// byte after a search: the corpus that the command used stays.
func TestOpportunisticGCKeepsCorporaOfTheCommand(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("folder/a.txt", "zqgckeepmarker")
	setTestUserCache(t)
	if got := graphSearch(t, "zqgckeepmarker", f.path("folder")); len(got) != 1 {
		t.Fatalf("found %v", got)
	}
	plans, err := planCorpora(context.Background(), []string{f.path("folder")})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	t.Setenv(envCacheBudget, "1")
	runGC(t.Context(), gcOptions{skipThrottle: true})
	if _, err := os.Stat(plans[0].cacheDir); err != nil {
		t.Fatalf("the GC evicted the corpus of this command: %v", err)
	}
}

// TestCacheBudgetCountsCommittedChangeAfterHeadMove moves HEAD to a commit
// with 2 MB of new text: the estimate counts it.
func TestCacheBudgetCountsCommittedChangeAfterHeadMove(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := initGitRepo(t, "seed.go", "package sample\n// zqheadmovemarker\n")
	gitRunIn(t, repo, "checkout", "-q", "-b", "big")
	writeFileAt(t, repo, "big.txt", strings.Repeat("zqheadmovemarker some committed text\n", 56<<10))
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "big")
	gitRunIn(t, repo, "checkout", "-q", "-")
	t.Chdir(t.TempDir())
	if got := budgetSearchFiles(t, "zqheadmovemarker", repo); got != 1 {
		t.Fatalf("found %d files", got)
	}
	plans, err := planCorpora(context.Background(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	t.Setenv(envCacheBudget, strconv.FormatInt(corpusDirSize(plans[0].cacheDir)+64<<10, 10))
	gitRunIn(t, repo, "checkout", "-q", "big")
	err = seekRoot(context.Background(), t, nil, "--lexical-only", "zqheadmovemarker", repo)
	if _, ok := errors.AsType[*cacheBudgetError](err); !ok {
		t.Fatalf("err=%v, want a cache budget error", err)
	}
}

// TestCacheBudgetBinaryCountsZeroEverywhere puts a 4 MB binary file in each
// place that the estimate reads, under a 1 MB budget. The places are a
// committed blob, an untracked file of a warm repository, and a new file of a
// warm folder. The index keeps each by name only, so no search stops.
func TestCacheBudgetBinaryCountsZeroEverywhere(t *testing.T) {
	requireTools(t)
	binary := string(make([]byte, 4<<20))
	for _, row := range []string{"committed", "dirty", "folder delta"} {
		t.Run(row, func(t *testing.T) {
			setTestUserCache(t)
			t.Setenv(envCacheBudget, "1M")
			root := t.TempDir()
			writeFileAt(t, root, "a.txt", "zqbinaryzeromarker\n")
			if row == "committed" {
				writeFileAt(t, root, "blob.bin", binary)
			}
			if row != "folder delta" {
				gitInitCommitAll(t, root)
			}
			t.Chdir(t.TempDir())
			if got := budgetSearchFiles(t, "zqbinaryzeromarker", root); got != 1 {
				t.Fatalf("found %d files", got)
			}
			if row == "committed" {
				return
			}
			writeFileAt(t, root, "blob.bin", binary)
			if got := budgetSearchFiles(t, "zqbinaryzeromarker", root); got != 1 {
				t.Fatalf("after the binary file: found %d files", got)
			}
		})
	}
}

// TestCacheBudgetCountsColdVectorBytes checks the cold estimate of one node,
// with and without vectors, against hand-worked byte counts. budgetFolder
// holds 109,749 bytes, and the dirty strict.go holds 35. The estimate counts
// 2.76 index bytes and 14.4 vector bytes per content byte, rounded down. A
// dirty Git node adds the vectors of the commit and of the overlay; a node
// with no commit adds the overlay only.
func TestCacheBudgetCountsColdVectorBytes(t *testing.T) {
	requireTools(t)
	for _, row := range []struct {
		kind             string
		lexical, vectors int64
	}{
		// 109,749 × 2.76 index bytes; 109,749 × (2.76 + 14.4) with vectors.
		{kind: "folder", lexical: 302_907, vectors: 1_883_292},
		// (109,749 + 35) × 2.76 index bytes; with vectors, add 109,749 × 14.4
		// for the commit and 35 × 14.4 for the overlay.
		{kind: "dirty", lexical: 303_003, vectors: 303_003 + 1_580_385 + 504},
		// 109,749 × 2.76 index bytes; with vectors, add 109,749 × 14.4 for the
		// overlay.
		{kind: "no-head", lexical: 302_907, vectors: 302_907 + 1_580_385},
	} {
		t.Run(row.kind, func(t *testing.T) {
			setTestUserCache(t)
			root := t.TempDir()
			budgetFolder(t, root)
			budgetGitKind(t, root, row.kind)
			plans, err := planCorpora(context.Background(), []string{root})
			if err != nil || len(plans) != 1 {
				t.Fatalf("plans=%d err=%v", len(plans), err)
			}
			lexical, _, err := plans[0].behavior.estimate(t.Context(), plans[0], false, false)
			if err != nil || lexical != row.lexical {
				t.Fatalf("lexical estimate %d (err=%v), want %d", lexical, err, row.lexical)
			}
			vectors, rebuild, err := plans[0].behavior.estimate(t.Context(), plans[0], true, false)
			if err != nil || !rebuild || vectors != row.vectors {
				t.Fatalf("estimate with vectors %d (rebuild=%t err=%v), want %d", vectors, rebuild, err, row.vectors)
			}
		})
	}
}

// TestCacheBudgetMessageWhenFloorBinds checks the message of a stop that the
// 5 % free-space floor sets. The message names the new bytes and the disk,
// not a larger override.
func TestCacheBudgetMessageWhenFloorBinds(t *testing.T) {
	floor := formatCLIError(&cacheBudgetError{planned: 6 << 20, growth: 4 << 10, budget: 4 << 20, freeBound: true})
	for _, want := range []string{"(4KB new)", "nearly full", "SEEK_CACHE_BUDGET"} {
		if !strings.Contains(floor, want) {
			t.Errorf("message %q lacks %q", floor, want)
		}
	}
	if strings.Contains(floor, "for example") {
		t.Errorf("message %q suggests a larger override although the floor binds", floor)
	}
	plain := formatCLIError(&cacheBudgetError{planned: 6 << 20, growth: 6 << 20, budget: 4 << 20})
	if !strings.Contains(plain, "for example SEEK_CACHE_BUDGET=") || strings.Contains(plain, "--lexical-only") {
		t.Errorf("message %q lacks the override example or names --lexical-only", plain)
	}
}

// TestCacheBudgetStopNamesTextSearchWhenItFits searches a folder under a
// budget that fits its text index but not its vectors. The search stops, and
// its hint names --lexical-only, which then runs.
func TestCacheBudgetStopNamesTextSearchWhenItFits(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	// 8,390 bytes of content: its text index is about 23 KB, and with vectors
	// about 144 KB (zoektIndexRatio, semanticBytesPerContentByte).
	for index := range 40 {
		writeFileAt(t, folder, fmt.Sprintf("f%02d.go", index),
			fmt.Sprintf("package p\n// Foo%d alpha beta\n%s", index, strings.Repeat("// filler text line\n", 9)))
	}
	t.Chdir(t.TempDir())
	t.Setenv(envCacheBudget, "60K")
	_, _, err := stubSearch(t, payloadFar, "Foo1", folder)
	if budgetErr, ok := errors.AsType[*cacheBudgetError](err); !ok || !budgetErr.textFits ||
		!strings.Contains(formatCLIError(err), "--lexical-only") {
		t.Fatalf("err=%v, want a budget stop whose hint names --lexical-only", err)
	}
	if _, err := seek(t, "--lexical-only", "Foo1", folder); err != nil {
		t.Fatalf("the text search the hint names failed: %v", err)
	}
}

// TestCacheBudgetHintFollowsTheTextSearch checks the --lexical-only hint
// against what a text search then does. With a budget below the text index of
// a cold folder, the text search stops too, so the hint does not name it. With
// a current text index larger than the budget, a text search builds nothing
// and runs, so the hint names it.
func TestCacheBudgetHintFollowsTheTextSearch(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	for index := range 40 {
		writeFileAt(t, folder, fmt.Sprintf("f%02d.go", index),
			fmt.Sprintf("package p\n// Foo%d alpha beta\n%s", index, strings.Repeat("// filler text line\n", 9)))
	}
	t.Chdir(t.TempDir())
	describe := func() error {
		_, _, err := stubSearch(t, payloadFar, "Foo1", folder)
		return err
	}
	lexical := func() error {
		_, err := seek(t, "--lexical-only", "Foo1", folder)
		return err
	}
	t.Setenv(envCacheBudget, "4K")
	if err := describe(); err == nil || strings.Contains(formatCLIError(err), "--lexical-only") {
		t.Fatalf("cold, 4K: err=%v, want a stop whose hint does not name --lexical-only", err)
	}
	if err := lexical(); err == nil {
		t.Fatal("cold, 4K: the text search did not stop")
	}
	t.Setenv(envCacheBudget, "1G")
	if err := lexical(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envCacheBudget, "4K")
	if err := describe(); err == nil || !strings.Contains(formatCLIError(err), "--lexical-only") {
		t.Fatalf("current text index, 4K: err=%v, want a stop whose hint names --lexical-only", err)
	}
	if err := lexical(); err != nil {
		t.Fatalf("current text index, 4K: the text search the hint names failed: %v", err)
	}
}

// TestGCCmd_DryRunWritesNothing runs `seek gc --dry-run` on a corpus with no
// ledger entry: the dry run writes nothing into the cache.
func TestGCCmd_DryRunWritesNothing(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(201), time.Now().Add(-time.Hour))
	if err := os.Remove(filepath.Join(dir, corpusSizeFile)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return runGCCommandCmd(t.Context(), gcCmdOptions{dryRun: true, sort: "name"})
	})
	if err != nil || !strings.Contains(out, "1 corpora") {
		t.Fatalf("the dry run did not list the corpus: err=%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, corpusSizeFile)); !os.IsNotExist(err) {
		t.Fatalf("the dry run wrote a ledger entry: %v", err)
	}
	if after, err := os.Stat(dir); err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the dry run changed the corpus directory: %v", err)
	}
}

// TestCorpusSizeRemeasuresStaleLedger writes a ledger older than the
// corpus's state: corpusSize measures the directory again.
func TestCorpusSizeRemeasuresStaleLedger(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, dir, corpusSizeFile, "7")
	writeFileAt(t, dir, "index/shard.zoekt", strings.Repeat("x", 4096))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, corpusSizeFile), old, old); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, dir, stateFile, "state")
	// The 4,096-byte shard and the 5-byte state file; the ledger does not
	// count itself.
	if got := corpusSize(dir, false); got != 4096+5 {
		t.Fatalf("size=%d, want the measured %d", got, 4096+5)
	}
}

// TestFolderReaderTransientErrorDoesNotPublish makes one open fail with
// EMFILE: the search fails and publishes no state, and the next search
// indexes the content.
func TestFolderReaderTransientErrorDoesNotPublish(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("folder/a.txt", "zqtransientmarker")
	setTestUserCache(t)
	realOpen := folderOpenFile
	folderOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "a.txt" {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EMFILE}
		}
		return realOpen(path)
	}
	err := seekRoot(context.Background(), t, nil, "--lexical-only", "zqtransientmarker", f.path("folder"))
	folderOpenFile = realOpen
	// EMFILE is no property of the node: the command ends.
	if !errors.Is(err, syscall.EMFILE) {
		t.Fatalf("err=%v, want EMFILE", err)
	}
	plans, planErr := planCorpora(context.Background(), []string{f.path("folder")})
	if planErr != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), planErr)
	}
	if state := readStateFile(plans[0].cacheDir); state != "" {
		t.Fatalf("the failed build published state %q", state)
	}
	if got := graphSearch(t, "zqtransientmarker", f.path("folder")); len(got) != 1 {
		t.Fatalf("the next search found %v, want the content", got)
	}
}

// gitInitCommitAll makes dir a Git repository with all its files in one
// commit.
func gitInitCommitAll(t *testing.T, dir string) {
	t.Helper()
	initEmptyGitRepoNoRemoteAt(t, dir)
	gitRunIn(t, dir, "add", ".")
	gitRunIn(t, dir, "commit", "-q", "-m", "initial")
}

// budgetSearchFiles runs a search and counts the files that it prints.
func budgetSearchFiles(t *testing.T, marker string, operands ...string) int {
	t.Helper()
	out, err := seek(t, slices.Concat([]string{"--lexical-only", marker}, operands)...)
	if err != nil {
		t.Fatalf("run %q: %v\n%s", operands, err, out)
	}
	return strings.Count(out, "## ")
}

// TestCacheBudgetCleanAgainAfterDirtyBuildFits builds vectors, searches the
// repository with a changed file, and then restores the file. The clean search
// binds the generation that exists for HEAD, so it adds nothing and does not
// stop.
func TestCacheBudgetCleanAgainAfterDirtyBuildFits(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	budgetFolder(t, root)
	gitInitCommitAll(t, root)
	t.Chdir(t.TempDir())
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", root); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, root, "strict.go", "package sample\n// alpha beta dirty\n")
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", root); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, root, "checkout", "--", "strict.go")
	plans, err := planCorpora(context.Background(), []string{root})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	t.Setenv(envCacheBudget, strconv.FormatInt(corpusDirSize(plans[0].cacheDir)+1<<10, 10))
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", root); err != nil {
		t.Fatalf("the clean search stopped: %v", err)
	}
}

// TestCacheBudgetCountsDirtyAndFolderDelta adds a new file with 2 MB of text
// to a warm node: a Git repository or a folder. The budget is the corpus size
// plus 64 KiB, so the estimate counts the file and the search stops.
func TestCacheBudgetCountsDirtyAndFolderDelta(t *testing.T) {
	requireTools(t)
	text := strings.Repeat("zqdeltamarker plain text line\n", 72<<10)
	for _, row := range []string{"dirty", "folder delta"} {
		t.Run(row, func(t *testing.T) {
			setTestUserCache(t)
			root := t.TempDir()
			writeFileAt(t, root, "a.txt", "zqdeltamarker\n")
			if row == "dirty" {
				gitInitCommitAll(t, root)
			}
			t.Chdir(t.TempDir())
			if got := budgetSearchFiles(t, "zqdeltamarker", root); got != 1 {
				t.Fatalf("found %d files", got)
			}
			plans, err := planCorpora(context.Background(), []string{root})
			if err != nil || len(plans) != 1 {
				t.Fatalf("plans=%d err=%v", len(plans), err)
			}
			t.Setenv(envCacheBudget, strconv.FormatInt(corpusDirSize(plans[0].cacheDir)+64<<10, 10))
			writeFileAt(t, root, "big.txt", text)
			err = seekRoot(context.Background(), t, nil, "--lexical-only", "zqdeltamarker", root)
			if _, ok := errors.AsType[*cacheBudgetError](err); !ok {
				t.Fatalf("err=%v, want a cache budget error", err)
			}
		})
	}
}

// TestCorpusLedgerKeepsLegacyAge measures a corpus with no .used file: its
// directory time, which is its LRU age, does not change.
func TestCorpusLedgerKeepsLegacyAge(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpus(t, root, fakeCorpusHash(301), time.Now().Add(-48*time.Hour))
	for _, name := range []string{usedFile, corpusSizeFile} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	writeFileAt(t, dir, "index/shard.zoekt", strings.Repeat("x", 4096))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if corporaBytes(root) <= 0 {
		t.Fatal("the corpus was not measured")
	}
	if info, err := os.Stat(dir); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("the ledger write moved the corpus age: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, corpusSizeFile)); err != nil {
		t.Fatalf("no ledger entry: %v", err)
	}
}

// TestBudgetFloorBinds checks the flag that turns the hint to the disk.
func TestBudgetFloorBinds(t *testing.T) {
	if budgetFloorBinds(1000, budgetFromVolume(1000, 900, 0)) {
		t.Error("10 % of the volume is not the floor")
	}
	if !budgetFloorBinds(1000, budgetFromVolume(1000, 60, 20)) {
		t.Error("a budget under 10 % comes from the floor")
	}
}

// TestFolderReaderTransientErrorOnWarmFolder changes a file of a warm folder
// with vectors, and makes its open fail with EMFILE. The lexical delta, the
// full rebuild that follows it, and the vector build publish nothing. The next
// search indexes the content.
func TestFolderReaderTransientErrorOnWarmFolder(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	budgetFolder(t, folder)
	t.Chdir(t.TempDir())
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", folder); err != nil {
		t.Fatal(err)
	}
	plans, err := planCorpora(context.Background(), []string{folder})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	before := readStateFile(plans[0].cacheDir)
	writeFileAt(t, folder, "f07.go", "package sample\n// alpha beta zqwarmtransient\n")
	realOpen := folderOpenFile
	folderOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "f07.go" {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EMFILE}
		}
		return realOpen(path)
	}
	// A warm folder whose update fails serves its old index with a warning.
	_, _, _ = stubSearch(t, payloadFar, "alpha beta", folder)
	folderOpenFile = realOpen
	if state := readStateFile(plans[0].cacheDir); state != before {
		t.Fatalf("the failed build published state %q", state)
	}
	if descriptor, err := readJoinedGeneration(plans[0].cacheDir); err == nil && descriptor.State != before {
		t.Fatalf("the failed build bound vectors for state %q", descriptor.State)
	}
	out, _, err := stubSearch(t, payloadFar, "alpha beta", folder)
	if err != nil || !strings.Contains(out, "zqwarmtransient") {
		t.Fatalf("the next search: err=%v\n%s", err, out)
	}
}

// TestDirtyReaderTransientErrorDoesNotPublish makes the open of a dirty Git
// file fail with EMFILE. EMFILE is no property of the file, so the build stops
// and the live family stays whole. The committed family publishes, and the
// command warns and serves it. The next search indexes the dirty content, not
// the name only.
func TestDirtyReaderTransientErrorDoesNotPublish(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "d.txt", "committed\n")
	writeFixtureFile(t, filepath.Join(repo, "d.txt"), "zqdirtytransient\n")
	setTestUserCache(t)
	realOpen := dirtyOpenFile
	t.Cleanup(func() { dirtyOpenFile = realOpen })
	dirtyOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "d.txt" {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EMFILE}
		}
		return realOpen(path)
	}
	err := seekRoot(context.Background(), t, nil, "--lexical-only", "zqdirtytransient", repo)
	dirtyOpenFile = realOpen
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("err=%v, want the committed family served", err)
	}
	// The committed family published anyway.
	plans, err := planCorpora(context.Background(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plans[0].cacheDir, committedGitStateFile)); err != nil {
		t.Fatalf("the committed state was not published: %v", err)
	}
	if _, headers := searchHeaders(t, "zqdirtytransient", repo); len(headers) != 1 {
		t.Fatalf("the next search found %v, want the dirty content", headers)
	}
}

// TestDirtyFileReadAgainAfterChmod makes a dirty Git file unreadable, then
// readable: the chmod alone changes the state, so the next search reads the
// dirty text. Root, or any user who can open a mode-000 file, reads the file
// in the first search. The test then expects the dirty text in both searches,
// and still requires that the chmod changes the fingerprint of the file.
func TestDirtyFileReadAgainAfterChmod(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "d.txt", "committed\n")
	path := filepath.Join(repo, "d.txt")
	writeFixtureFile(t, path, "zqdirtychmod\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	// The fingerprint of d.txt must change with its mode alone, for any user.
	dirty := repoState{Files: []string{"d.txt"}}
	unreadable := repoStateFingerprint(repo, dirty)
	unreadableHeaders := 0
	if file, err := os.Open(path); err == nil {
		_ = file.Close()
		unreadableHeaders = 1
	}
	setTestUserCache(t)
	if _, headers := searchHeaders(t, "zqdirtychmod", repo); len(headers) != unreadableHeaders {
		t.Fatalf("the mode-000 file gave %v, want %d content matches", headers, unreadableHeaders)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if repoStateFingerprint(repo, dirty) == unreadable {
		t.Fatal("the chmod did not change the fingerprint of d.txt, so a search would not read it again")
	}
	if _, headers := searchHeaders(t, "zqdirtychmod", repo); len(headers) != 1 {
		t.Fatalf("after chmod the search found %v, want the dirty content", headers)
	}
}

// TestDirtyDirectoryReplacedByFile replaces a tracked directory with a file of
// the same name. Lstat of the deleted file gives ENOTDIR: a path that is gone,
// not a read error. So the lexical search and the description keep every
// dirty file. The description runs with a model that accepts every row. So it
// prints each file that has vectors: the new file docs, the edited keep.txt,
// and clean.txt.
func TestDirtyDirectoryReplacedByFile(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "clean.txt", "zqnotdir clean\n")
	writeFixtureFile(t, filepath.Join(repo, "docs/a.md"), "zqnotdir docs\n")
	writeFixtureFile(t, filepath.Join(repo, "keep.txt"), "zqnotdir keep\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "docs")
	if err := os.RemoveAll(filepath.Join(repo, "docs")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "docs"), "zqnotdir now a file\n")
	writeFixtureFile(t, filepath.Join(repo, "keep.txt"), "zqnotdir keep edited\n")
	setTestUserCache(t)
	t.Chdir(repo)
	recorder := captureTestLogs(t, slog.LevelWarn)
	_, headers := searchHeaders(t, "zqnotdir", ".")
	slices.Sort(headers)
	if !slices.Equal(headers, []string{"clean.txt", "docs", "keep.txt"}) {
		t.Fatalf("strict headers=%v", headers)
	}
	output, _, err := stubSearch(t, payloadAccept, "zqnotdir now file", ".")
	if got := headerPaths(output); err != nil || !slices.Equal(got, []string{"clean.txt", "docs", "keep.txt"}) {
		t.Fatalf("description: err=%v, headers %v, want clean.txt, docs and keep.txt:\n%s", err, got, output)
	}
	for _, record := range recorder.Records() {
		t.Errorf("warning: %s", record.Message)
	}
}

// TestDirtyReadErrorKeepsTheLiveFamily makes the read of one changed dirty
// file fail with EIO. The build applies every other change and keeps that file
// by name. So the lexical search and the description show the other dirty
// files, and the notice counts the kept file. The next search reads the
// change.
//
// The description runs with a model that accepts every row, so a vector row
// of the old b.txt would print. The vector build stops at the read error, and
// no vectors bind the state. So the description prints exactly the other
// files, and the next description embeds the change of b.txt.
func TestDirtyReadErrorKeepsTheLiveFamily(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "c.txt", "zqlive alpha beta committed\n")
	for _, name := range []string{"a.txt", "b.txt"} {
		writeFixtureFile(t, filepath.Join(repo, name), "zqlive first\n")
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "files")
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "zqlive alpha beta dirty a\n")
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "zqlive alpha beta dirty b\n")
	writeFixtureFile(t, filepath.Join(repo, "u.txt"), "zqlive alpha beta untracked\n")
	setTestUserCache(t)
	t.Chdir(repo)
	strict := func() (string, []string) {
		t.Helper()
		out, headers := searchHeaders(t, "zqlive", ".")
		slices.Sort(headers)
		return out, headers
	}
	if _, got := strict(); !slices.Equal(got, []string{"a.txt", "b.txt", "c.txt", "u.txt"}) {
		t.Fatalf("first search %v", got)
	}
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "zqlive alpha beta dirty b changed\n")
	realOpen := dirtyOpenFile
	dirtyOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "b.txt" {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EIO}
		}
		return realOpen(path)
	}
	out, got := strict()
	if !slices.Equal(got, []string{"a.txt", "c.txt", "u.txt"}) || !strings.Contains(out, oneNameOnlyNotice) {
		dirtyOpenFile = realOpen
		t.Fatalf("strict search during the failure %v, want the other files and b.txt by name:\n%s", got, out)
	}
	output, _, err := stubSearch(t, payloadAccept, "zqlive alpha beta", ".")
	dirtyOpenFile = realOpen
	if got := headerPaths(output); err != nil || !slices.Equal(got, []string{"a.txt", "c.txt", "u.txt"}) ||
		strings.Contains(output, "zqlive first") || !strings.Contains(output, oneNameOnlyNotice) {
		t.Fatalf("description during the failure printed %v, want a.txt, c.txt and u.txt with the notice: err=%v\n%s", got, err, output)
	}
	plans, err := planCorpora(t.Context(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	if _, err := os.Lstat(filepath.Join(plans[0].cacheDir, joinedGenerationFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the failed vector build bound vectors (err=%v)", err)
	}
	if output, _ := searchHeaders(t, "zqlive changed", "."); !strings.Contains(output, "## b.txt") {
		t.Fatalf("the next search misses the change:\n%s", output)
	}
	_, scorer, err := stubSearch(t, payloadAccept, "zqlive alpha beta", ".")
	if err != nil || !slices.Contains(scorer.unitPaths(), "b.txt") {
		t.Fatalf("the next description embedded %v, want the change of b.txt (err=%v)", scorer.unitPaths(), err)
	}
}

// TestDirtyReadErrorPrintsNoStaleText reverts one dirty file, deletes an
// untracked one, and makes the reads of two newly dirty files fail. The build
// applies the revert and the delete, so no search prints text that no file
// holds. The notice counts the two failed files by name.
func TestDirtyReadErrorPrintsNoStaleText(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "plain committed\n")
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "plain b\n")
	writeFixtureFile(t, filepath.Join(repo, "c.txt"), "zqnewdirty committed\n")
	// A search with no result prints no notice: d.txt gives a result.
	writeFixtureFile(t, filepath.Join(repo, "d.txt"), "zqnewdirty clean\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "files")
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "zqgonetext dirty\n")
	writeFixtureFile(t, filepath.Join(repo, "u.txt"), "zqgonetext untracked\n")
	setTestUserCache(t)
	t.Chdir(repo)
	if _, headers := searchHeaders(t, "zqgonetext", "."); len(headers) != 2 {
		t.Fatalf("first search %v", headers)
	}
	gitRunIn(t, repo, "checkout", "--", "a.txt")
	if err := os.Remove(filepath.Join(repo, "u.txt")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "plain b changed\n")
	writeFixtureFile(t, filepath.Join(repo, "c.txt"), "zqnewdirty edited\n")
	realOpen := dirtyOpenFile
	dirtyOpenFile = func(path string) (*os.File, error) {
		if base := filepath.Base(path); base == "b.txt" || base == "c.txt" {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EIO}
		}
		return realOpen(path)
	}
	defer func() { dirtyOpenFile = realOpen }()
	if out, headers := searchHeaders(t, "zqgonetext", "."); len(headers) != 0 {
		t.Fatalf("stale text printed: %v\n%s", headers, out)
	}
	const twoNameOnly = "\n\n2 name-only files could also match: binary, too large, not on disk, or unreadable\n"
	if out, headers := searchHeaders(t, "zqnewdirty", "."); !slices.Equal(headers, []string{"d.txt"}) || !strings.Contains(out, twoNameOnly) {
		t.Fatalf("headers=%v, want b.txt and c.txt counted by name only:\n%s", headers, out)
	}
}

// TestDirtyLstatErrorKeepsTheName makes the Lstat of a dirty file fail with
// EIO for one search. The build keeps the file by name and writes no state, so
// the next search reads it.
func TestDirtyLstatErrorKeepsTheName(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "zqlstat committed\n")
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "plain\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "files")
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "zqlstat dirty a\n")
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "zqlstat dirty b\n")
	setTestUserCache(t)
	t.Chdir(repo)
	realLstat := dirtyLstat
	dirtyLstat = func(path string, st *unix.Stat_t) error {
		if filepath.Base(path) == "b.txt" {
			return &os.PathError{Op: "lstat", Path: path, Err: syscall.EIO}
		}
		return realLstat(path, st)
	}
	out, headers := searchHeaders(t, "zqlstat", ".")
	dirtyLstat = realLstat
	if !slices.Equal(headers, []string{"a.txt"}) || !strings.Contains(out, oneNameOnlyNotice) {
		t.Fatalf("during the error: headers=%v, want a.txt and b.txt by name:\n%s", headers, out)
	}
	_, headers = searchHeaders(t, "zqlstat", ".")
	slices.Sort(headers)
	if !slices.Equal(headers, []string{"a.txt", "b.txt"}) {
		t.Fatalf("after the error: headers=%v, want both dirty files", headers)
	}
}

// TestUncommittedStageIsSwept leaves a full uncommitted stage in the corpus of
// a searched repository, as a crash does. The stage is a build directory of
// the corpus index, so the GC removes it once it is old. The corpus stays.
func TestUncommittedStageIsSwept(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "zqstagesweep committed\n")
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "zqstagesweep dirty\n")
	setTestUserCache(t)
	if got := budgetSearchFiles(t, "zqstagesweep", repo); got != 1 {
		t.Fatalf("found %d files", got)
	}
	plans, err := planCorpora(t.Context(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	stage, err := buildUncommittedStage(t.Context(), repo, plans[0].indexDir, repoState{Files: []string{"a.txt"}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * buildTmpMaxAge)
	if err := os.Chtimes(stage.dir, old, old); err != nil {
		t.Fatal(err)
	}
	runGC(t.Context(), gcOptions{skipThrottle: true})
	if _, err := os.Stat(stage.dir); !os.IsNotExist(err) {
		t.Fatalf("the GC kept the stage %s: %v", stage.dir, err)
	}
	if _, err := os.Stat(plans[0].cacheDir); err != nil {
		t.Fatalf("the GC removed the corpus: %v", err)
	}
}

// TestKeptFileReadAloneNextTime makes the read of one dirty file fail with
// EIO. The build publishes the rest with a pending manifest. So the next
// search opens only that file (a delta) and does not build the committed
// family again.
func TestKeptFileReadAloneNextTime(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "c.txt", "zqkept committed\n")
	for _, name := range []string{"a.txt", "b.txt", "d.txt"} {
		writeFixtureFile(t, filepath.Join(repo, name), "zqkept dirty "+name+"\n")
	}
	setTestUserCache(t)
	realOpen := dirtyOpenFile
	t.Cleanup(func() { dirtyOpenFile = realOpen })
	var opened []string
	var mu sync.Mutex
	fail := true
	dirtyOpenFile = func(path string) (*os.File, error) {
		mu.Lock()
		opened = append(opened, filepath.Base(path))
		failing := fail && filepath.Base(path) == "b.txt"
		mu.Unlock()
		if failing {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EIO}
		}
		return realOpen(path)
	}
	if _, headers := searchHeaders(t, "zqkept", repo); len(headers) != 3 {
		t.Fatalf("during the error: %v, want a.txt, c.txt, d.txt", headers)
	}
	plans, err := planCorpora(context.Background(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plans[0].cacheDir, committedGitStateFile)); err != nil {
		t.Fatalf("the committed state was not written: %v", err)
	}
	mu.Lock()
	fail, opened = false, nil
	mu.Unlock()
	_, headers := searchHeaders(t, "zqkept", repo)
	for index := range headers {
		headers[index] = filepath.Base(headers[index])
	}
	slices.Sort(headers)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(headers, []string{"a.txt", "b.txt", "c.txt", "d.txt"}) || !slices.Equal(opened, []string{"b.txt"}) {
		t.Fatalf("after the error: headers=%v opened=%v, want b.txt read alone", headers, opened)
	}
}

// TestEMFILEInAFullRebuildKeepsTheLiveFamily forces a full uncommitted
// rebuild of a warm node while EMFILE fails the open of unchanged dirty files.
// The build stops and the live family stays whole. The command warns and
// serves it, and the next search shows the files by content.
func TestEMFILEInAFullRebuildKeepsTheLiveFamily(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "committed\n")
	for _, name := range []string{"a.txt", "b.txt", "d.txt", "e.txt"} {
		writeFixtureFile(t, filepath.Join(repo, name), "zqstorm "+name+"\n")
	}
	setTestUserCache(t)
	t.Chdir(repo)
	want := []string{"a.txt", "b.txt", "d.txt", "e.txt"}
	search := func() []string {
		t.Helper()
		_, headers := searchHeaders(t, "zqstorm", ".")
		slices.Sort(headers)
		return headers
	}
	if got := search(); !slices.Equal(got, want) {
		t.Fatalf("first search %v", got)
	}
	plans, err := planCorpora(context.Background(), []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(plans[0].cacheDir, stateFile)); err != nil {
		t.Fatal(err)
	}
	realOpen := dirtyOpenFile
	t.Cleanup(func() { dirtyOpenFile = realOpen })
	dirtyOpenFile = func(path string) (*os.File, error) {
		if base := filepath.Base(path); base == "b.txt" || base == "d.txt" || base == "e.txt" {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EMFILE}
		}
		return realOpen(path)
	}
	// The update fails, and the search serves the live family.
	got := search()
	dirtyOpenFile = realOpen
	if !slices.Equal(got, want) {
		t.Fatalf("under EMFILE %v, want the live family", got)
	}
	if got := search(); !slices.Equal(got, want) {
		t.Fatalf("after EMFILE %v, want the dirty files by content", got)
	}
}

// TestCacheBudgetEstimatesAnOrphanBranch warms a repository, then moves it to
// an orphan branch. The commit range from the old head to no commit has no
// tree, so the estimate must count it with no Git error.
func TestCacheBudgetEstimatesAnOrphanBranch(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	budgetFolder(t, root)
	gitInitCommitAll(t, root)
	if _, err := seek(t, "--lexical-only", "alpha beta", root); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, root, "checkout", "-q", "--orphan", "fresh")
	plans, err := planCorpora(t.Context(), []string{root})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	for _, vectors := range []bool{false, true} {
		if _, rebuild, err := plans[0].behavior.estimate(t.Context(), plans[0], vectors, false); err != nil || !rebuild {
			t.Fatalf("vectors=%t: rebuild=%t err=%v, want a rebuild with no error", vectors, rebuild, err)
		}
	}
}

// TestCacheBudgetCountsOnlyVectorsForACurrentTextIndex warms a dirty
// repository with a text search, so its text index is current. A first search
// with vectors then adds the vectors of the commit and of the dirty file, and
// no text index bytes.
func TestCacheBudgetCountsOnlyVectorsForACurrentTextIndex(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	budgetFolder(t, root)
	gitInitCommitAll(t, root)
	writeFileAt(t, root, "strict.go", "package sample\n// alpha beta dirty\n")
	if _, err := seek(t, "--lexical-only", "alpha beta", root); err != nil {
		t.Fatal(err)
	}
	plans, err := planCorpora(t.Context(), []string{root})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	bytes, rebuild, err := plans[0].behavior.estimate(t.Context(), plans[0], true, false)
	if err != nil || !rebuild {
		t.Fatalf("rebuild=%t err=%v", rebuild, err)
	}
	// The commit holds the 109,749 bytes of budgetFolder: strict.go (29),
	// f00.go to f09.go (2,443 each), f10.go to f39.go (2,843 each). The dirty
	// strict.go holds 35 bytes. At 14.4 vector bytes per content byte, their
	// vectors are 1,580,385 and 504 bytes.
	const want = 1_580_385 + 504
	if got := bytes - corpusDirSize(plans[0].cacheDir); got != want {
		t.Fatalf("added bytes %d, want %d (vectors of the commit and of the dirty file only)", got, want)
	}
}

// TestCacheBudgetCountsVectorsBehindTheTextIndex builds the vectors of a
// folder and of a Git node, adds a file (in a commit for Git), and runs a text
// search. The text index holds the file, and the vectors do not. The next
// search with vectors must count the vectors of the new file: exactly for Git,
// and at least that for the folder.
func TestCacheBudgetCountsVectorsBehindTheTextIndex(t *testing.T) {
	requireTools(t)
	for _, kind := range []string{"folder", "git"} {
		t.Run(kind, func(t *testing.T) {
			setTestUserCache(t)
			root := t.TempDir()
			budgetFolder(t, root)
			budgetGitKind(t, root, kind)
			if _, _, err := stubSearch(t, payloadFar, "alpha beta", root); err != nil {
				t.Fatal(err)
			}
			added := "package sample\n// alpha " + strings.Repeat("vectorless words ", 9000) + "\n"
			writeFileAt(t, root, "added.go", added)
			if kind == "git" {
				gitRunIn(t, root, "add", "added.go")
				gitRunIn(t, root, "commit", "-q", "-m", "add")
			}
			if _, err := seek(t, "--lexical-only", "alpha beta", root); err != nil {
				t.Fatal(err)
			}
			plans, err := planCorpora(t.Context(), []string{root})
			if err != nil || len(plans) != 1 {
				t.Fatalf("plans=%d err=%v", len(plans), err)
			}
			bytes, rebuild, err := plans[0].behavior.estimate(t.Context(), plans[0], true, false)
			if err != nil || !rebuild {
				t.Fatalf("rebuild=%t err=%v", rebuild, err)
			}
			// added.go holds 153,025 bytes. At 14.4 vector bytes per content
			// byte, its vectors are 2,203,560 bytes.
			const want = 2_203_560
			got := bytes - corpusDirSize(plans[0].cacheDir)
			// Git counts the commit range from the vectors' commit: the new
			// file alone. The folder counts all content (its vectors hold an
			// older state).
			if kind == "git" && got != want {
				t.Fatalf("added bytes %d, want %d (the vectors of the new file)", got, want)
			}
			if got < want {
				t.Fatalf("added bytes %d, want at least %d (the vectors of the new file)", got, want)
			}
		})
	}
}

// TestCacheBudgetCountsTheChangedFilesOfABoundFolder builds a folder's vectors,
// edits one file, and estimates the next search with vectors. The vectors hold
// the published state, so the build adds the text and vectors of the edited
// file only.
func TestCacheBudgetCountsTheChangedFilesOfABoundFolder(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	budgetFolder(t, root)
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", root); err != nil {
		t.Fatal(err)
	}
	const edit = "package sample\n// alpha beta edited\n"
	writeFileAt(t, root, "strict.go", edit)
	plans, err := planCorpora(t.Context(), []string{root})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	bytes, rebuild, err := plans[0].behavior.estimate(t.Context(), plans[0], true, false)
	if err != nil || !rebuild {
		t.Fatalf("rebuild=%t err=%v", rebuild, err)
	}
	// The edit holds 36 bytes: 99 text index bytes at 2.76 per content byte,
	// and 518 vector bytes at 14.4 per content byte.
	const want = 99 + 518
	if got := bytes - corpusDirSize(plans[0].cacheDir); got != want {
		t.Fatalf("added bytes %d, want %d (the text and vectors of the edited file)", got, want)
	}
}

// TestCacheBudgetCountsACommitAfterTheVectors builds a repository's text and
// vectors, then commits a file. The text index and the vectors hold the old
// commit. So the next search with vectors adds the text and vectors of the
// commit range: the new file alone. The estimate runs one git diff-tree,
// because the vector figure reuses the text figure.
func TestCacheBudgetCountsACommitAfterTheVectors(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	root := t.TempDir()
	budgetFolder(t, root)
	gitInitCommitAll(t, root)
	if _, _, err := stubSearch(t, payloadFar, "alpha beta", root); err != nil {
		t.Fatal(err)
	}
	added := "package sample\n// alpha " + strings.Repeat("committed words ", 900) + "\n"
	writeFileAt(t, root, "added.go", added)
	gitRunIn(t, root, "add", "added.go")
	gitRunIn(t, root, "commit", "-q", "-m", "add")
	plans, err := planCorpora(t.Context(), []string{root})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	bytes, rebuild, err := plans[0].behavior.estimate(t.Context(), plans[0], true, false)
	if err != nil || !rebuild {
		t.Fatalf("rebuild=%t err=%v", rebuild, err)
	}
	// added.go holds 14,425 bytes: 39,813 text index bytes at 2.76 per
	// content byte, and 207,720 vector bytes at 14.4 per content byte.
	const want = 39_813 + 207_720
	if got := bytes - corpusDirSize(plans[0].cacheDir); got != want {
		t.Fatalf("added bytes %d, want %d (the text and vectors of the new file)", got, want)
	}
	// The vectors hold the text index's commit, so the estimate reuses the
	// text figure: one git diff-tree for the commit range.
	calls := filepath.Join(t.TempDir(), "calls")
	installGitShim(t, `echo "$subcommand" >> "$SEEK_TEST_GIT_CALLS"
exec "$SEEK_TEST_REAL_GIT" "$@"
`)
	t.Setenv("SEEK_TEST_GIT_CALLS", calls)
	if _, _, err := plans[0].behavior.estimate(t.Context(), plans[0], true, false); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(calls)
	if got := strings.Count(string(logged), "diff-tree\n"); got != 1 {
		t.Fatalf("diff-tree calls=%d, want 1 (the vector figure reuses the text figure): %q", got, logged)
	}
}

// TestChangedFolderCandidatesSkipsRemovedFiles gives the estimate a manifest
// with removed files before, between and after the files that the walk finds.
// Only the new file and the changed file count.
func TestChangedFolderCandidatesSkipsRemovedFiles(t *testing.T) {
	cacheDir := t.TempDir()
	file := func(name string, size int64) folderCandidate {
		return folderCandidate{name: name, path: "/folder/" + name, mode: 0o644, size: size, mtime: 1, ctime: 1, dev: 1, ino: uint64(name[0])}
	}
	published := []folderCandidate{file("a", 1), file("b", 1), file("c", 1), file("d", 1), file("e", 1), file("g", 1)}
	if err := writeFolderManifest(cacheDir, "state", published, 0, make([]int64, len(published))); err != nil {
		t.Fatal(err)
	}
	// a, c and g are removed, d changed, f is new, and b and e stay.
	selected := []folderCandidate{file("b", 1), file("d", 2), file("e", 1), file("f", 1)}
	var changed []string
	for _, candidate := range changedFolderCandidates(cacheDir, "state", selected) {
		changed = append(changed, candidate.name)
	}
	if !slices.Equal(changed, []string{"d", "f"}) {
		t.Fatalf("changed files %v, want [d f]", changed)
	}
}
