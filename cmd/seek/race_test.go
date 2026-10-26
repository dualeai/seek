package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ===========================================================================
// Regression tests
//
// These tests verify that the shard swap and LOCK_SH search protection keep
// search results stable across normal reindex boundaries.
// ===========================================================================

// ---------------------------------------------------------------------------
// Locked shard swap: no gap during re-indexing
//
// Planned Git corpus search refreshes dirty content through the normal
// freshness path before reading Zoekt shards, so there is no visible gap where
// the old uncommitted marker disappears without the new one being searchable.
// ---------------------------------------------------------------------------

func TestFix_NoShardGapDuringReindexing(t *testing.T) {
	requireTools(t)

	dir := initGitRepo(t, "app.go", "package main\n// committed_aaa\n")

	// Create uncommitted edit
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// uncommitted_v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	results, err := runSeekInPlannedGitCorpus(ctx, "uncommitted_v1", paths, plan)
	if err != nil {
		t.Fatalf("initial search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("precondition: uncommitted_v1 must be findable after initial indexing")
	}

	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n// uncommitted_v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err = runSeekInPlannedGitCorpus(ctx, "uncommitted_v2", paths, plan)
	if err != nil {
		t.Fatalf("post-reindex search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("FIX FAILED: uncommitted_v2 not findable after re-indexing")
	}

	results, err = runSeekInPlannedGitCorpus(ctx, "uncommitted_v1", paths, plan)
	if err != nil {
		t.Fatalf("old marker search failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("old uncommitted content should be tombstoned, got %d results", len(results))
	}
}

// ---------------------------------------------------------------------------
// acquireReadLock polls until LOCK_EX is released
//
// acquireReadLock is the lock primitive of every node read (openNodeRead).
// The NB shared lock fails while any LOCK_EX is held, so a searcher waits for
// an active publish (acquirePublishLock) to end before it reads shards.
// ---------------------------------------------------------------------------

func TestFix_SharedLockBlocksSearchDuringIndexing(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "test.lock")

	// Acquire LOCK_EX (simulating an indexer)
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(holder); err != nil {
		t.Fatal(err)
	}

	var sharedAcquiredAt atomic.Int64
	done := make(chan struct{})

	// Goroutine tries to acquire shared lock — polls until LOCK_EX released
	go func() {
		defer close(done)
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return
		}
		defer func() {
			unlockFile(f)
			_ = f.Close()
		}()
		// This polls until LOCK_EX is released
		if err := acquireReadLock(context.Background(), f); err != nil {
			return
		}
		sharedAcquiredAt.Store(time.Now().UnixNano())
	}()

	// Wait to confirm LOCK_SH is blocked
	time.Sleep(50 * time.Millisecond)
	if sharedAcquiredAt.Load() != 0 {
		unlockFile(holder)
		_ = holder.Close()
		t.Fatal("LOCK_SH should block while LOCK_EX is held")
	}

	// Release LOCK_EX and record when
	releaseTime := time.Now().UnixNano()
	unlockFile(holder)
	_ = holder.Close()

	// Wait for goroutine to complete
	<-done

	acquiredTime := sharedAcquiredAt.Load()
	if acquiredTime == 0 {
		t.Fatal("LOCK_SH should have been acquired after LOCK_EX release")
	}
	if acquiredTime < releaseTime {
		t.Fatal("LOCK_SH acquired before LOCK_EX was released")
	}
}

// ---------------------------------------------------------------------------
// Concurrent search and reindex stress, with LOCK_SH protection
//
// Searches through the planned-corpus boundary while lower-level reindexing
// updates shards and holds locks.
// ---------------------------------------------------------------------------

func TestFix_ConcurrentSearchDuringReindex_Stress(t *testing.T) {
	requireTools(t)

	const iterations = 20
	dir := initGitRepo(t, "stable.go", "package main\n// always_findable_fix_test\n")
	for iter := range iterations {
		name := fmt.Sprintf("changing_%d.go", iter)
		content := fmt.Sprintf("package main\n// committed_changing_%d\n", iter)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "add changing fixtures")

	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	// Initial index
	state := mustGitRepoStateIn(t, ctx, dir)
	currentState := gitCorpusStateHash(paths, state)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, currentState); err != nil {
		t.Fatalf("initial indexing: %v", err)
	}

	// Verify baseline
	results, err := searchPlannedCorpusForTest(ctx, plan, "always_findable_fix_test")
	if err != nil || len(results) == 0 {
		t.Fatal("precondition: committed content must be findable")
	}

	var missedContent atomic.Int64
	var searchErrors atomic.Int64
	indexErrors := make(chan error, 1)
	recordIndexError := func(err error) {
		select {
		case indexErrors <- err:
		default:
		}
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for iter := range iterations {
			marker := fmt.Sprintf("uncommitted_fix_iter_%d", iter)
			content := fmt.Sprintf("package main\n// %s\n", marker)
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("changing_%d.go", iter)), []byte(content), 0o644); err != nil {
				recordIndexError(fmt.Errorf("write dirty fixture %q: %w", marker, err))
				return
			}
			state, err := gitRepoStateIn(ctx, dir)
			if err != nil {
				recordIndexError(fmt.Errorf("read repository state for %q: %w", marker, err))
				return
			}
			currentState := gitCorpusStateHash(paths, state)
			if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, currentState); err != nil {
				recordIndexError(fmt.Errorf("refresh dirty fixture %q: %w", marker, err))
				return
			}
		}
	}()

	for i := range iterations {
		wg.Add(1)
		go func(iter int) {
			defer wg.Done()
			<-start
			time.Sleep(time.Duration(1+iter%5) * time.Millisecond)
			res, err := searchPlannedCorpusForTest(ctx, plan, "always_findable_fix_test")
			if err != nil {
				searchErrors.Add(1)
				return
			}
			if len(res) == 0 {
				missedContent.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if missedContent.Load() > 0 {
		t.Errorf("FIX REGRESSION: %d/%d searches missed committed content during concurrent reindexing",
			missedContent.Load(), iterations)
	}
	if searchErrors.Load() > 0 {
		t.Errorf("FIX REGRESSION: %d searches returned errors", searchErrors.Load())
	}
	select {
	case err := <-indexErrors:
		t.Errorf("FIX REGRESSION: concurrent indexing failed: %v", err)
	default:
	}

	latestMarker := fmt.Sprintf("uncommitted_fix_iter_%d", iterations-1)
	latestFiles, err := runSeekInPlannedGitCorpus(ctx, latestMarker, paths, plan)
	if err != nil {
		t.Fatalf("final dirty search failed: %v", err)
	}
	if len(latestFiles) == 0 {
		t.Fatalf("latest dirty marker should be searchable after contention settles")
	}
}

// TestFix_ConcurrentSearchDuringCommittedSwap makes the swap of the committed
// family (swapFiles moves the live family aside, then renames the new files
// in) overlap live searches: the builder commits a new file in each
// iteration, so HEAD moves and each committed build swaps the family. Each
// concurrent search must find a stable committed marker: the publish lock
// (LOCK_EX) keeps readers out of the swap, which is not atomic. The other
// TestFix_* tests only edit the working tree (HEAD stays), so they never test
// this path. Each swap stays open for 20 ms and the searches run until the
// last build ends, so a search that does not wait for the publish lock meets
// the torn family. The test fails when no build swapped the family.
func TestFix_ConcurrentSearchDuringCommittedSwap(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "stable.go", "package main\n// always_committed_marker\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	st := mustGitRepoStateIn(t, ctx, dir)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, st, gitCorpusStateHash(paths, st)); err != nil {
		t.Fatalf("initial committed index: %v", err)
	}

	var missed, searchErr atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	built := make(chan struct{})
	var swaps atomic.Int32
	afterAsideBeforeRenameHook = func() {
		swaps.Add(1)
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { afterAsideBeforeRenameHook = nil })

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(built)
		<-start
		for i := 0; i < 20; i++ {
			writeTrackedFile(t, dir, fmt.Sprintf("c_%d.go", i), fmt.Sprintf("package main\n// commit_%d\n", i))
			s, err := gitRepoStateIn(ctx, dir)
			if err != nil {
				t.Errorf("read repository state: %v", err)
				return
			}
			_ = runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, s, gitCorpusStateHash(paths, s))
		}
	}()

	for r := 0; r < 24; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				// Search until the last build ends. The pause leaves moments
				// with no reader, so a swap can take the publish lock.
				select {
				case <-built:
					return
				case <-time.After(100 * time.Millisecond):
				}
				res, err := searchPlannedCorpusForTest(ctx, plan, "always_committed_marker")
				if err != nil {
					searchErr.Add(1)
					return
				}
				if len(res) == 0 {
					missed.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if swaps.Load() == 0 {
		t.Fatal("no build swapped the committed family, so the test proves nothing")
	}
	if missed.Load() > 0 {
		t.Errorf("committed shard gap during swap: %d misses (reader saw no committed shards mid-swap)", missed.Load())
	}
	if searchErr.Load() > 0 {
		t.Errorf("%d search errors during committed swap", searchErr.Load())
	}
}

// TestDescriptionReadDuringRepublish_Stress runs description reads in 8
// readers while commits republish the committed family, one publish per
// commit. Every read must find the strict hit, a committed file. The test
// model makes every row a scan hit, so target.go and dirty.go, which hold no
// query word, are the scan hits of the base and the overlay. A read that binds
// vectors binds the base and the overlay of one state, so it finds both scan
// hits; most reads meet a peer's build and read text only, with neither.
// A read that sees the state move (errGitIndexStateChanged) is counted, not
// failed. The swaps stay open for 20 ms, so a read that does not wait for the
// publish lock meets the torn family. The test fails when no swap ran or no
// read found both scan hits. 8 commits keep the run near 30 s with -race:
// each publish builds vectors.
func TestDescriptionReadDuringRepublish_Stress(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "stable.go", "package main\n// zqstress anchor committed\n")
	writeTrackedFile(t, dir, "target.go", "package main\n// committed\n")
	writeFileAt(t, dir, "dirty.go", "package main\n// dirty\n")
	paths, plan := planGitTestCorpus(t, dir)
	execution := testSemanticSearchExecution(t, modelFactory(&hybridTestScorer{mode: payloadAccept}))
	qp, err := planQuery("zqstress anchor file:go", defaultSearchPolicy())
	if err != nil || qp.model == nil || len(qp.model.filters) == 0 {
		t.Fatalf("the query must plan a filtered model route: err=%v", err)
	}
	ctx := t.Context()
	// found reports the strict hit (stable.go), the base hit (target.go) and
	// the overlay hit (dirty.go).
	found := func(hits []nodeHit) (strict, base, overlay bool) {
		for _, hit := range hits {
			strict = strict || hit.path == "stable.go"
			base = base || hit.path == "target.go"
			overlay = overlay || hit.path == "dirty.go"
		}
		return strict, base, overlay
	}
	hits, _, err := retrieveNode(ctx, plan, qp, defaultSearchConfig(), execution)
	if strict, base, overlay := found(hits); err != nil || !strict || !base || !overlay {
		t.Fatalf("the first read must find the strict and both scan hits: strict=%t base=%t overlay=%t err=%v", strict, base, overlay, err)
	}

	var swaps atomic.Int32
	afterAsideBeforeRenameHook = func() {
		swaps.Add(1)
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { afterAsideBeforeRenameHook = nil })

	errs := make(chan error, 1024)
	// report never blocks: a defect that fails every read must fail the
	// test, not fill the channel and hang it.
	report := func(err error) {
		select {
		case errs <- err:
		default:
		}
	}
	built := make(chan struct{})
	var consistent, moved atomic.Int32
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(built)
		for i := range 8 {
			name := fmt.Sprintf("c_%d.go", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("package main\n// commit_%d\n", i)), 0o644); err != nil {
				report(err)
				return
			}
			for _, args := range [][]string{{"add", name}, {"commit", "-q", "-m", name}} {
				if err := gitRetryIndexLock(dir, args...); err != nil {
					report(err)
					return
				}
			}
			state, err := gitRepoStateIn(ctx, dir)
			if err != nil {
				report(err)
				return
			}
			// A reader's build can hold the build lock, so this build skips;
			// build until the commit is published, by this build or a
			// reader's. The next commit then waits for this publish.
			preState := gitCorpusStateHash(paths, state)
			deadline := time.Now().Add(2 * time.Minute)
			for readStateFile(plan.cacheDir) != preState {
				if time.Now().After(deadline) {
					report(fmt.Errorf("commit %d is never published", i))
					return
				}
				if _, err := (&gitNode{paths: paths}).build(ctx, plan.corpusStore, state, preState, execution); err != nil {
					report(fmt.Errorf("build %d: %w", i, err))
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-built:
					return
				case <-time.After(10 * time.Millisecond):
				}
				// Each read is one command, so it has its own record of
				// prepared nodes. A read that meets a peer's build reads
				// text only.
				reader := execution
				reader.opened = &openedNodes{}
				hits, _, err := retrieveNode(ctx, plan, qp, defaultSearchConfig(), reader)
				switch strict, base, overlay := found(hits); {
				case errors.Is(err, errGitIndexStateChanged):
					moved.Add(1)
				case err != nil:
					report(fmt.Errorf("read: %w", err))
				case !strict:
					// The committed family holds stable.go in every commit.
					report(errors.New("a read found no committed file: it read a torn family"))
				case base && overlay:
					consistent.Add(1)
				case base || overlay:
					report(fmt.Errorf("the read bound one generation of two: base=%t overlay=%t", base, overlay))
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if swaps.Load() == 0 || consistent.Load() == 0 {
		t.Fatalf("swaps=%d consistent reads=%d moved=%d: the test proves nothing", swaps.Load(), consistent.Load(), moved.Load())
	}
}

// gitRetryIndexLock runs git in dir and retries while another git process
// holds index.lock: a reader's git status takes Git's optional index lock.
func gitRetryIndexLock(dir string, args ...string) error {
	for attempt := 0; ; attempt++ {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err == nil {
			return nil
		}
		if attempt == 100 || !strings.Contains(string(out), "index.lock") {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGC_SkipsCorpusWithBuildInProgress verifies that GC honors .build.lock
// while a temp-swap build runs outside the publish lock.
func TestGC_SkipsCorpusWithBuildInProgress(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so the GC would evict the corpus unless the guard
	// under test holds it.
	t.Setenv(envCacheBudget, "1")
	dir := seedCorpus(t, root, fakeCorpusHash(91), time.Now().Add(-30*24*time.Hour))

	// Simulate an in-flight builder holding the build lock.
	bf, err := os.OpenFile(filepath.Join(dir, buildLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unlockFile(bf); _ = bf.Close() }()
	if err := lockFileExclusive(bf); err != nil {
		t.Fatalf("hold build lock: %v", err)
	}

	runGC(context.Background(), gcOptions{})

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("corpus with build in progress must survive gc, got %v", err)
	}
}

// TestEvictionDuringBuildPerKind evicts the node's cache directory during its
// build, as gc does (a rename to a trash name), and checks the result for
// each node kind. Git: the publish sees the eviction and the build returns
// nil; the read finds an index with no shard, which is damage, so the repair
// prepares the node again in the same command and finds the edit. Folder: the
// read cannot open its lock, so the node is skipped with a warning and no path
// is made again; the next search builds again and finds the edit.
func TestEvictionDuringBuildPerKind(t *testing.T) {
	requireTools(t)
	for _, kind := range []string{"git", "folder"} {
		t.Run(kind, func(t *testing.T) {
			setTestUserCache(t)
			root := t.TempDir()
			writeFileAt(t, root, "a.txt", "zqevict alpha\n")
			if kind == "git" {
				gitInitCommitAll(t, root)
			}
			search := func() (string, error) {
				t.Helper()
				return seek(t, "--lexical-only", "zqevict", root)
			}
			if _, err := search(); err != nil {
				t.Fatal(err)
			}
			writeFileAt(t, root, "a.txt", "zqevict alpha edited\n")
			plans, err := planCorpora(t.Context(), []string{root})
			if err != nil || len(plans) != 1 {
				t.Fatalf("plans=%d err=%v", len(plans), err)
			}
			cacheDir := plans[0].cacheDir
			hook := &folderBuildTestHook
			if kind == "git" {
				hook = &missingBlobRecheckTestHook
			}
			var evicted bool
			*hook = func() {
				if !evicted {
					evicted = os.Rename(cacheDir, cacheDir+".trash") == nil
				}
			}
			t.Cleanup(func() { *hook = nil })
			logs := captureTestLogs(t, slog.LevelWarn)
			before := nodePreparations.Load()
			out, err := search()
			preparations := nodePreparations.Load() - before
			*hook = nil
			if !evicted {
				t.Fatal("the hook did not evict the node")
			}
			var skipped, damage int
			for _, record := range logs.Records() {
				switch {
				case strings.Contains(record.Message, "Skipped a search node"):
					skipped++
				case strings.Contains(record.Message, "Index damage detected"):
					damage++
				}
			}
			switch kind {
			case "git":
				if err != nil || !strings.Contains(out, "edited") || preparations != 2 || damage != 1 || skipped != 0 {
					t.Fatalf("git: err=%v preparations=%d damage=%d skipped=%d output:\n%s", err, preparations, damage, skipped, out)
				}
				if _, err := os.Stat(filepath.Join(cacheDir, lockFile)); err != nil {
					t.Fatalf("git: the repair must make .lock again: %v", err)
				}
			case "folder":
				_, lockErr := os.Stat(filepath.Join(cacheDir, lockFile))
				if skipped != 1 || !os.IsNotExist(lockErr) {
					t.Fatalf("folder: err=%v skipped=%d lock=%v output:\n%s", err, skipped, lockErr, out)
				}
				if out, err = search(); err != nil || !strings.Contains(out, "edited") {
					t.Fatalf("the next search must find the edit: err=%v output:\n%s", err, out)
				}
			}
		})
	}
}
