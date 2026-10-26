package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	_, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitPublish, lockPath)

	var released, acquiredAfterRelease atomic.Bool
	done := make(chan error, 1)

	// Goroutine tries to acquire shared lock — polls until LOCK_EX released
	go func() {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			done <- err
			return
		}
		defer func() {
			unlockFile(f)
			_ = f.Close()
		}()
		err = acquireReadLock(context.Background(), f)
		acquiredAfterRelease.Store(released.Load())
		done <- err
	}()

	// The debug line of the wait shows that LOCK_SH waits while LOCK_EX is
	// held.
	select {
	case <-waiting:
	case err := <-done:
		unlockFile(holder)
		_ = holder.Close()
		t.Fatalf("LOCK_SH should block while LOCK_EX is held (err=%v)", err)
	}

	released.Store(true)
	unlockFile(holder)
	_ = holder.Close()

	if err := <-done; err != nil {
		t.Fatalf("LOCK_SH should have been acquired after LOCK_EX release: %v", err)
	}
	if !acquiredAfterRelease.Load() {
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

	for range iterations {
		wg.Go(func() {
			<-start
			res, err := searchPlannedCorpusForTest(ctx, plan, "always_findable_fix_test")
			if err != nil {
				searchErrors.Add(1)
				return
			}
			if len(res) == 0 {
				missedContent.Add(1)
			}
		})
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

// TestFix_ConcurrentSearchDuringCommittedSwap holds a read of the node while
// a new commit builds. Its publish swaps the committed family: swapFiles moves
// the live family aside, then renames the new files in. The swap is not
// atomic, so the publish must wait for the read (LOCK_EX against LOCK_SH). The
// wait writes its debug line before any shard moves. While the publish waits,
// the held read finds the committed marker. When the read ends, the publish
// runs and the new commit is searchable. The other TestFix_* tests only edit
// the working tree (HEAD stays), so they never test this path.
//
// The publish must also keep its lock through the swap. Two FIFOs stop it
// under that lock: the .swapping marker before the first move, and the family
// manifest after the last move. When the read of the first FIFO ends, the
// publish runs between the two FIFOs, so a shared lock must fail.
func TestFix_ConcurrentSearchDuringCommittedSwap(t *testing.T) {
	requireTools(t)
	dir := initGitRepo(t, "stable.go", "package main\n// always_committed_marker\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(t, dir)

	st := mustGitRepoStateIn(t, ctx, dir)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, st, gitCorpusStateHash(paths, st)); err != nil {
		t.Fatalf("initial committed index: %v", err)
	}
	markerFIFO := filepath.Join(plan.cacheDir, swappingMarkerFile+".tmp")
	manifestFIFO := filepath.Join(plan.indexDir, familyManifestFile+".tmp")
	for _, fifo := range []string{markerFIFO, manifestFIFO} {
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitReaders, filepath.Join(plan.cacheDir, lockFile))
	read, readCtx, err := openNodeRead(ctx, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTrackedFile(t, dir, "c.go", "package main\n// new_commit_marker\n")
	s := mustGitRepoStateIn(t, ctx, dir)
	built := make(chan error, 1)
	go func() {
		built <- runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, s, gitCorpusStateHash(paths, s))
	}()
	select {
	case <-waiting:
	case err := <-built:
		read.close()
		t.Fatalf("the build published while a read held the node (err=%v)", err)
	}
	marker, err := parseSearchQuery("always_committed_marker")
	if err != nil {
		t.Fatal(err)
	}
	files, err := read.search(readCtx, marker, defaultSearchConfig())
	read.close()
	if err != nil || len(files) == 0 {
		t.Fatalf("the held read lost the committed marker while the publish waited: files=%d err=%v", len(files), err)
	}
	if label := readPublishFIFO(t, markerFIFO, built); label != familyCommitted.label() {
		t.Fatalf(".swapping=%q, want %q", label, familyCommitted.label())
	}
	probe, err := os.OpenFile(filepath.Join(plan.cacheDir, lockFile), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	lockErr := lockFileSharedNB(probe)
	_ = probe.Close()
	readPublishFIFO(t, manifestFIFO, built)
	if err := <-built; err != nil {
		t.Fatalf("build after the read: %v", err)
	}
	if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
		t.Fatalf("a shared lock during the swap: %v, want EWOULDBLOCK: the publish freed its lock before the swap", lockErr)
	}
	if files, err := searchPlannedCorpusForTest(ctx, plan, "new_commit_marker"); err != nil || len(files) == 0 {
		t.Fatalf("the new commit is not searchable after the publish: files=%d err=%v", len(files), err)
	}
}

// readPublishFIFO opens the FIFO at path and reads it to its end. A build
// writes the file at path with writeCacheFile, so its open of the FIFO waits
// for this open: the build stops there. When the read ends, the build has
// written the file. A build that ends while the FIFO is still at path did not
// write the file, and the test fails with the build's result. When the build
// ended after its write, its result goes back on built. A build that skips
// the write and stops at a later FIFO makes the test wait until the go test
// timeout.
func readPublishFIFO(t *testing.T, path string, built chan error) string {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	read := make(chan result, 1)
	go func() {
		fifo, err := os.Open(path)
		if err != nil {
			read <- result{err: err}
			return
		}
		data, err := io.ReadAll(fifo)
		_ = fifo.Close()
		read <- result{data: data, err: err}
	}()
	var got result
	select {
	case got = <-read:
	case err := <-built:
		// A build that opened the FIFO renamed it before it ended.
		if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeNamedPipe != 0 {
			// An open to write ends the open of the read goroutine.
			go func() {
				if fifo, openErr := os.OpenFile(path, os.O_WRONLY, 0); openErr == nil {
					_ = fifo.Close()
				}
			}()
			<-read
			t.Fatalf("the build ended before it wrote %s: %v", filepath.Base(path), err)
		}
		built <- err
		got = <-read
	}
	if got.err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), got.err)
	}
	return string(got.data)
}

// TestDescriptionReadDuringRepublish_Stress runs description reads in 8
// readers while commits republish the committed family, one publish per
// commit. Every read must find the strict hit, a committed file. The test
// model makes every row a scan hit, so target.go and dirty.go, which hold no
// query word, are the scan hits of the base and the overlay. A read that
// binds vectors binds the base and the overlay of one state, so it finds both
// scan hits. A read that meets a peer's build reads text only, with neither.
// A read that sees the state move (errGitIndexStateChanged) is counted, not
// failed. Each reader stops after 2 reads, so a publish that waits for the
// readers runs.
//
// After each publish, the test takes the publish lock and moves the committed
// family aside. A swap does the same before it renames the new family in. Then
// two reads start, and a read must wait for the publish lock before it reads,
// which its debug line shows. A read whose prepare builds the node waits to
// publish instead, so the test starts two reads. A read that does not wait
// reads the torn family and finds no committed file. The test fails when that
// wait is missing or no read found both scan hits. 8 commits keep the run near
// 20 s with -race: each publish builds vectors.
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
	_, publishWaits := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitPublish, filepath.Join(plan.cacheDir, lockFile))

	var mu sync.Mutex
	var failures []error
	report := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, err)
	}
	var consistent, moved atomic.Int32
	// read runs one description read, as one command does: it has its own
	// record of prepared nodes.
	read := func() {
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
	for i := range 8 {
		writeTrackedFile(t, dir, fmt.Sprintf("c_%d.go", i), fmt.Sprintf("package main\n// commit_%d\n", i))
		state := mustGitRepoStateIn(t, ctx, dir)
		preState := gitCorpusStateHash(paths, state)
		var readers sync.WaitGroup
		for range 8 {
			readers.Go(func() {
				for range 2 {
					read()
				}
			})
		}
		// A reader's build can hold the build lock, so this build serves the
		// current shards. Build until the commit is published, by this build
		// or a reader's, and wait for the build lock between tries.
		for readStateFile(plan.cacheDir) != preState {
			peer, err := (&gitNode{paths: paths}).build(ctx, plan.corpusStore, state, preState, execution)
			if err != nil {
				t.Fatalf("build %d: %v", i, err)
			}
			if peer {
				waitForBuildLock(t, plan.cacheDir)
			}
		}
		readers.Wait()

		pub, err := acquirePublishLock(ctx, plan.cacheDir)
		if err != nil {
			t.Fatal(err)
		}
		restore := tearFamily(t, plan, familyCommitted)
		// Drop the signal of a read that waited and ended before the tear.
		select {
		case <-publishWaits:
		default:
		}
		ended := make(chan struct{}, 2)
		for range 2 {
			go func() {
				read()
				ended <- struct{}{}
			}()
		}
		pending := 2
		select {
		case <-publishWaits:
		case <-ended:
			pending--
			report(fmt.Errorf("commit %d: a read ended while the test held the publish lock", i))
		}
		restore()
		releaseLock(pub)
		for range pending {
			<-ended
		}
	}
	for _, err := range failures {
		t.Error(err)
	}
	if consistent.Load() == 0 {
		t.Fatalf("consistent reads=%d moved=%d: the test proves nothing", consistent.Load(), moved.Load())
	}
}

// waitForBuildLock waits until no build holds the build lock of the node in
// cacheDir. It takes the lock with a blocking flock, so the wait ends when the
// build that holds the lock ends, and frees it at once.
func waitForBuildLock(t *testing.T, cacheDir string) {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(cacheDir, buildLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = lock.Close()
		t.Fatalf("wait for the build lock: %v", err)
	}
	releaseLock(lock)
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
// each node kind. The test holds a lock that the build waits for, evicts the
// node when the wait writes its debug line, then frees the lock. Git: the
// first search of the node waits for the build lock. The build then makes the
// index directory again, and its publish sees the eviction: the build returns
// nil. The read finds an index with no shard, which is damage. So the repair
// prepares the node again in the same command and finds the file. Folder: the
// publish of an edit waits for a read lock. The publish sees the eviction
// when it checks the index directory under its lock, so it makes no path
// again. The read cannot open its lock, so the node is skipped with one
// warning whose cause is that open. A publish that misses the eviction fails
// the build, and the cause of the skip is then the build's error. The next
// search builds again and finds the edit.
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
			plans, err := planCorpora(t.Context(), []string{root})
			if err != nil || len(plans) != 1 {
				t.Fatalf("plans=%d err=%v", len(plans), err)
			}
			cacheDir := plans[0].cacheDir
			var release func()
			wait, waitLock := lockWaitBuild, filepath.Join(cacheDir, buildLockFile)
			if kind == "git" {
				if err := os.MkdirAll(cacheDir, 0o755); err != nil {
					t.Fatal(err)
				}
				lock, held, err := tryBuildLock(cacheDir)
				if err != nil || !held {
					t.Fatalf("hold the build lock: held=%t err=%v", held, err)
				}
				release = sync.OnceFunc(func() { releaseLock(lock) })
				t.Cleanup(release)
			} else {
				if _, err := search(); err != nil {
					t.Fatal(err)
				}
				writeFileAt(t, root, "a.txt", "zqevict alpha edited\n")
				release = holdReadLock(t, cacheDir)
				wait, waitLock = lockWaitReaders, filepath.Join(cacheDir, lockFile)
			}
			logs, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, wait, waitLock)
			acted := onLockWait(t, waiting, func() error { return os.Rename(cacheDir, cacheDir+".trash") }, release)
			before := nodePreparations.Load()
			out, err := search()
			preparations := nodePreparations.Load() - before
			lockWaitActed(t, acted)
			var skipped, damage int
			var causes []string
			for _, record := range logs.Records() {
				switch {
				case strings.Contains(record.Message, "Skipped a search node"):
					skipped++
					causes = append(causes, fmt.Sprint(testLogAttrs(record)["cause"]))
				case strings.Contains(record.Message, "Index damage detected"):
					damage++
				}
			}
			switch kind {
			case "git":
				if err != nil || !strings.Contains(out, "zqevict alpha") || preparations != 2 || damage != 1 || skipped != 0 {
					t.Fatalf("git: err=%v preparations=%d damage=%d skipped=%d output:\n%s", err, preparations, damage, skipped, out)
				}
				if _, err := os.Stat(filepath.Join(cacheDir, lockFile)); err != nil {
					t.Fatalf("git: the repair must make .lock again: %v", err)
				}
			case "folder":
				_, lockErr := os.Stat(filepath.Join(cacheDir, lockFile))
				if skipped != 1 || !strings.Contains(causes[0], "open search lock") || !os.IsNotExist(lockErr) {
					t.Fatalf("folder: err=%v skipped=%d causes=%q lock=%v output:\n%s", err, skipped, causes, lockErr, out)
				}
				if out, err = search(); err != nil || !strings.Contains(out, "edited") {
					t.Fatalf("the next search must find the edit: err=%v output:\n%s", err, out)
				}
			}
		})
	}
}
