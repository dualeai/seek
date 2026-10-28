package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"golang.org/x/sys/unix"
)

// TestSearchNodesRunsAPhysicalCorpusOnce reaches one corpus through two
// plans: it runs once.
func TestSearchNodesRunsAPhysicalCorpusOnce(t *testing.T) {
	defer goroutineLeakGuard(t)()
	var calls atomic.Int32
	plans := []node{{id: "dup"}, {id: "dup"}, {id: "other"}}
	results, _, err := searchNodes(t.Context(), plans, func(_ context.Context, plan node) ([]corpusSearchResult, dirtyFileSet, error) {
		calls.Add(1)
		return []corpusSearchResult{{corpusID: plan.id}}, nil, nil
	})
	if err != nil || calls.Load() != 2 || len(results) != 2 {
		t.Fatalf("calls=%d results=%d err=%v", calls.Load(), len(results), err)
	}
}

// TestSearchNodesSkipsAFailedNodeWithOneWarning fails one node of three, the
// start node or a found one alike: one warning, the others answer, no error.
func TestSearchNodesSkipsAFailedNodeWithOneWarning(t *testing.T) {
	defer goroutineLeakGuard(t)()
	logs := captureTestLogs(t, slog.LevelWarn)
	plans := []node{{id: "a", displayRoot: "/a"}, {id: "b", displayRoot: "/b"}, {id: "c", displayRoot: "/c"}}
	results, _, err := searchNodes(t.Context(), plans, func(_ context.Context, plan node) ([]corpusSearchResult, dirtyFileSet, error) {
		if plan.id == "b" {
			return nil, nil, &nodeOpenError{err: errors.New("git status failed: fatal: bad object")}
		}
		return []corpusSearchResult{{corpusID: plan.id}}, nil, nil
	})
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%d err=%v, want the two other nodes", len(results), err)
	}
	records := logs.Records()
	if len(records) != 1 || !logsMention(records, "/b") || !logsMention(records, "bad object") {
		t.Fatalf("warnings=%v, want one naming /b and its cause", records)
	}
}

// TestSearchNodesStopsOnASearchError returns an error that is not a node
// failure (a query error): the search stops with it.
func TestSearchNodesStopsOnASearchError(t *testing.T) {
	defer goroutineLeakGuard(t)()
	logs := captureTestLogs(t, slog.LevelWarn)
	want := errors.New("search: got nil (sub)query")
	_, _, err := searchNodes(t.Context(), []node{{id: "a"}, {id: "b", displayRoot: "/b"}}, func(ctx context.Context, plan node) ([]corpusSearchResult, dirtyFileSet, error) {
		if plan.id == "a" {
			return nil, nil, want
		}
		// The other node fails as fallout of the stop: no warning.
		<-ctx.Done()
		return nil, nil, &nodeOpenError{err: ctx.Err()}
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("fallout of the stop warned: %v", records)
	}
}

// TestSearchNodesCancellationIsSilent cancels while nodes run: the search
// returns the cancellation once, with no warning per node, and cannot
// return success.
func TestSearchNodesCancellationIsSilent(t *testing.T) {
	defer goroutineLeakGuard(t)()
	logs := captureTestLogs(t, slog.LevelWarn)
	ctx, cancel := context.WithCancel(t.Context())
	plans := make([]node, 8)
	for index := range plans {
		plans[index] = node{id: corpusID(fmt.Sprintf("p%d", index)), displayRoot: fmt.Sprintf("/p%d", index)}
	}
	var started atomic.Int32
	_, _, err := searchNodes(ctx, plans, func(nodeCtx context.Context, plan node) ([]corpusSearchResult, dirtyFileSet, error) {
		if started.Add(1) == 3 {
			cancel()
		}
		<-nodeCtx.Done()
		return nil, nil, &nodeOpenError{err: nodeCtx.Err()}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want the cancellation", err)
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("a cancelled search warned: %v", records)
	}
}

// TestSearchNodesEmptyPlans searches no node: no call, no result, no error.
func TestSearchNodesEmptyPlans(t *testing.T) {
	results, dirty, err := searchNodes(t.Context(), nil, func(context.Context, node) ([]corpusSearchResult, dirtyFileSet, error) {
		t.Fatal("no node to search")
		return nil, nil, nil
	})
	if err != nil || len(results) != 0 || len(dirty) != 0 {
		t.Fatalf("results=%d dirty=%d err=%v", len(results), len(dirty), err)
	}
}

// TestBuildSlotsFromReadBudget pins the build slot rule over read budgets. It
// checks the slot limit, and the window for one node and for all slots. For
// every build count, it checks the deadlock proof. 2 windows per build plus 2
// maximum-weight documents fit the budget. A window holds at least one such
// document. The budgets are literal: 600 MiB is the smallest budget, and
// the others are a quarter of 8, 64 and 256 GiB hosts.
func TestBuildSlotsFromReadBudget(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		budgetMiB            int64
		slots                int
		oneNodeWindowMiB     int64
		allSlotsWindowMinMiB int64
	}{
		{budgetMiB: 600, slots: 2, oneNodeWindowMiB: 200, allSlotsWindowMinMiB: 100},
		{budgetMiB: 2 << 10, slots: 9, oneNodeWindowMiB: 924, allSlotsWindowMinMiB: 100},
		{budgetMiB: 16 << 10, slots: 80, oneNodeWindowMiB: 8092, allSlotsWindowMinMiB: 100},
		{budgetMiB: 64 << 10, slots: 326, oneNodeWindowMiB: 32668, allSlotsWindowMinMiB: 100},
	} {
		budget := tc.budgetMiB * mib
		if got := buildSlotLimit(budget); got != tc.slots {
			t.Fatalf("%d MiB: slots=%d, want %d", tc.budgetMiB, got, tc.slots)
		}
		if got := indexWindowFor(budget, 1) / mib; got != tc.oneNodeWindowMiB {
			t.Fatalf("%d MiB: one-node window=%d MiB, want %d", tc.budgetMiB, got, tc.oneNodeWindowMiB)
		}
		if got := indexWindowFor(budget, 10*tc.slots); got != indexWindowFor(budget, tc.slots) || got/mib < tc.allSlotsWindowMinMiB {
			t.Fatalf("%d MiB: window beyond the slots=%d MiB", tc.budgetMiB, got/mib)
		}
		for builds := 1; builds <= tc.slots; builds++ {
			window := indexWindowFor(budget, builds)
			if window < maxReadWeightBytes || 2*int64(builds)*window+2*maxReadWeightBytes > budget {
				t.Fatalf("%d MiB, %d builds: window=%d breaks the proof for budget %d", tc.budgetMiB, builds, window, budget)
			}
		}
	}
}

// TestCommandWindowFollowsTheGraph sizes the window for a command. One node
// gets the whole budget, and many nodes share it by the slot limit. A build
// outside a command uses indexWindowBytes.
func TestCommandWindowFollowsTheGraph(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()
	// A fixed 2 GiB read budget is the 2048 MiB row of
	// TestBuildSlotsFromReadBudget. Its windows are computed by hand: 9 slots
	// share (2048 - 200) MiB, two windows for each build.
	oldBudget := readBudgetBytes
	readBudgetBytes = 2 << 30
	defer func() { readBudgetBytes = oldBudget }()
	const mib = 1 << 20
	for _, tc := range []struct {
		nodes int
		want  int64
	}{
		{nodes: 1, want: 924 * mib},       // 1848 MiB / 2
		{nodes: 2, want: 462 * mib},       // 1848 MiB / 4
		{nodes: 9, want: 107653802},       // 1848 MiB / 18, rounded down
		{nodes: 1 << 20, want: 107653802}, // more nodes than slots: 9 slots
	} {
		if got := indexWindow(withIndexWindow(t.Context(), tc.nodes)); got != tc.want {
			t.Fatalf("%d nodes: window=%d, want %d", tc.nodes, got, tc.want)
		}
	}
	defer swapIndexWindowBytesForTest(32 << 10)()
	if got := indexWindow(t.Context()); got != 32<<10 {
		t.Fatalf("window outside a command=%d, want the global 32 KiB", got)
	}
}

// TestConcurrentCommandsShareTheReadBudget runs the builds of two commands in
// one process. The build of a one-node command takes the whole budget. So a
// build of another command waits for it and starts when it ends, and the
// windows never pass the budget. In the synctest bubble, synctest.Wait returns
// when the second build waits in the slot queue.
func TestConcurrentCommandsShareTheReadBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, err := acquireBuildSlot(withIndexWindow(t.Context(), 1))
		if err != nil {
			t.Fatal(err)
		}
		// A failed check gives the slot back, so later slot tests do not
		// wait for it.
		release = sync.OnceFunc(release)
		defer release()
		type slot struct {
			release func()
			err     error
		}
		second := make(chan slot, 1)
		go func() {
			release, err := acquireBuildSlot(withIndexWindow(t.Context(), 1<<20))
			second <- slot{release: release, err: err}
		}()
		synctest.Wait()
		select {
		case got := <-second:
			if got.err == nil {
				got.release()
			}
			t.Fatalf("a second command's build ran beside a whole-budget build: %v", got.err)
		default:
		}
		release()
		got := <-second
		if got.err != nil {
			t.Fatal(got.err)
		}
		got.release()
	})
}

// awaitBuildSlotWaiter returns with waited true when a build waits for a slot
// in buildWindows. When the search ends first, the build did not wait: it
// returns the search error from ended with waited false. A waiter makes
// TryAcquire(0) fail and changes no weight, so the loop reads the state of
// the semaphore, not a clock. A test whose build runs ctags and Git processes
// uses it: such a build cannot run in a synctest bubble.
func awaitBuildSlotWaiter(ended <-chan error) (waited bool, err error) {
	for {
		select {
		case err = <-ended:
			return false, err
		default:
		}
		if !buildWindows.TryAcquire(0) {
			return true, nil
		}
		runtime.Gosched()
	}
}

// TestRunSizesTheWindowFromTheGraph runs a search of one node. Its build takes
// the whole build budget. So while it runs, the build of a command with many
// nodes cannot take a slot. The test holds a read lock on the node, so the
// build waits to publish while it holds its slot. During that wait, the test
// tries the slot of the other command.
func TestRunSizesTheWindowFromTheGraph(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("one/a.txt", "zqwindowmarker")
	setTestUserCache(t)
	plans, err := planCorpora(t.Context(), []string{f.path("one")})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	release := holdReadLock(t, plans[0].cacheDir)
	_, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitReaders, filepath.Join(plans[0].cacheDir, lockFile))
	// A build takes the two windows of its command (acquireBuildSlot).
	other := 2 * indexWindow(withIndexWindow(context.Background(), 1<<20))
	acted := onLockWait(t, waiting, func() error {
		if buildWindows.TryAcquire(other) {
			buildWindows.Release(other)
			return errors.New("a build of another command took a slot beside a one-node build")
		}
		return nil
	}, release)
	if _, err := seek(t, "--lexical-only", "zqwindowmarker", f.path("one")); err != nil {
		t.Fatal(err)
	}
	lockWaitActed(t, acted)
}

// TestBuildsWithFewLargeDocumentTokensFinish builds 12 folder nodes at once.
// Each file fills a shard of its own, and a window holds 4 large documents.
// Each build has more readers than that, and the read budget is the one that
// the indexWindowFor proof needs. The builds must finish and give all weight
// back. Builds share no large-document token. With shared tokens, a consumer
// that waits for a token keeps its document. The token holders wait for
// readers that the read budget blocks, so the builds can hang under load. The
// test sets GOMAXPROCS, so each build has more readers than tokens on every
// host. A hang runs until the go test timeout, which prints each goroutine.
func TestBuildsWithFewLargeDocumentTokensFinish(t *testing.T) {
	requireTools(t)
	const tokens = 4
	saved := runtime.GOMAXPROCS(2 * tokens)
	t.Cleanup(func() { runtime.GOMAXPROCS(saved) })
	defer goroutineLeakGuard(t)()
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()
	const nodes = 12
	const doc int64 = 96 << 10
	window := doc * 5 / 2
	restore := swapReadSemaphoreForTest(2*nodes*window + 2*doc)
	defer restore()
	restoreWindow := swapIndexWindowBytesForTest(window)
	defer restoreWindow()
	oldShardMax, oldTokens := shardMax, largeDocumentParallelism
	shardMax, largeDocumentParallelism = int(doc/3), tokens
	defer func() { shardMax, largeDocumentParallelism = oldShardMax, oldTokens }()

	plans := make([]node, nodes)
	for index := range plans {
		plans[index] = planSynthCorpus(t, writeRandomFolder(t, 20*doc, doc))
	}
	userQ, err := parseSearchQuery("zzz_no_match_zzz")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := searchNodes(t.Context(), plans, func(nodeCtx context.Context, plan node) ([]corpusSearchResult, dirtyFileSet, error) {
		return prepareAndSearchCorpus(nodeCtx, plan, userQ, defaultSearchConfig())
	}); err != nil {
		t.Fatalf("the builds did not finish: %v", err)
	}
	if got := availableWeight(readSemaphore); got != 2*nodes*window+2*doc {
		t.Fatalf("weight leak: %d", got)
	}
}

// TestSearchesTakeNoBuildSlot holds every build slot, as builders of other
// nodes would. A search of a ready node still finishes. A node that must
// build waits for a slot. A warm search that waits for a slot runs until the
// go test timeout, which prints each goroutine.
func TestSearchesTakeNoBuildSlot(t *testing.T) {
	requireTools(t)
	const marker = "zqslotmarker"
	f := newGraphFixture(t)
	f.graphFile("warm/a.txt", marker)
	f.graphFile("cold/b.txt", marker)
	warmRepo := f.graphRepo("warmrepo", "w.txt", marker)
	coldRepo := f.graphRepo("coldrepo", "c.txt", marker)
	setTestUserCache(t)
	for _, warm := range []string{f.path("warm"), warmRepo} {
		if _, err := seek(t, "--lexical-only", marker, warm); err != nil {
			t.Fatal(err)
		}
	}
	release := holdEveryBuildSlot(t)
	for _, warm := range []string{f.path("warm"), warmRepo} {
		out, err := seek(t, "--lexical-only", marker, warm)
		if err != nil || !strings.Contains(out, "a.txt") && !strings.Contains(out, "w.txt") {
			t.Fatalf("the warm search of %s did not finish while the slots were taken: err=%v\n%s", warm, err, out)
		}
	}
	// A cold build waits for a slot and ends only after one is free.
	for _, cold := range []string{f.path("cold"), coldRepo} {
		done := make(chan error, 1)
		go func() {
			_, err := seek(t, "--lexical-only", marker, cold)
			done <- err
		}()
		if waited, err := awaitBuildSlotWaiter(done); !waited {
			t.Fatalf("a cold build of %s ran with no free slot: %v", cold, err)
		}
		release()
		if err := <-done; err != nil {
			t.Fatalf("the cold build of %s failed after a slot was free: %v", cold, err)
		}
		release = holdEveryBuildSlot(t)
	}
}

// TestBuildSlotsLimitConcurrentBuilds takes every build slot with blocked
// builds: the held builds overlap, and one more build waits until a held
// build ends. In the synctest bubble, a holder that gets no slot makes the
// bubble deadlock at once.
func TestBuildSlotsLimitConcurrentBuilds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Outside a command a build takes the two windows of a full set of
		// slots.
		slots := buildSlotLimit(readBudgetBytes)
		gate := make(chan struct{})
		var held sync.WaitGroup
		// Every holder gives its slot back, also when a check fails.
		openGate := sync.OnceFunc(func() { close(gate) })
		defer func() {
			openGate()
			held.Wait()
		}()
		took := make(chan error, slots)
		for range slots {
			held.Go(func() {
				release, err := acquireBuildSlot(t.Context())
				took <- err
				if err != nil {
					return
				}
				<-gate
				release()
			})
		}
		for range slots {
			if err := <-took; err != nil {
				t.Fatalf("a build of %d did not take a slot: %v", slots, err)
			}
		}
		extra := make(chan error, 1)
		held.Go(func() {
			release, err := acquireBuildSlot(t.Context())
			if err == nil {
				release()
			}
			extra <- err
		})
		synctest.Wait()
		select {
		case err := <-extra:
			t.Fatalf("a build ran beyond %d slots: %v", slots, err)
		default:
		}
		openGate()
		if err := <-extra; err != nil {
			t.Fatalf("the extra build did not take a freed slot: %v", err)
		}
	})
}

// TestSearchNodesCollectsEveryNode runs more nodes than build slots: every
// node's results reach the merged list.
func TestSearchNodesCollectsEveryNode(t *testing.T) {
	defer goroutineLeakGuard(t)()
	plans := make([]node, 3*buildSlotLimit(readBudgetBytes)+1)
	for index := range plans {
		plans[index] = node{id: corpusID(fmt.Sprintf("plan-%03d", index))}
	}
	results, _, err := searchNodes(t.Context(), plans, func(_ context.Context, plan node) ([]corpusSearchResult, dirtyFileSet, error) {
		return []corpusSearchResult{{corpusID: plan.id}}, nil, nil
	})
	if err != nil || len(results) != len(plans) {
		t.Fatalf("results=%d err=%v, want %d", len(results), err, len(plans))
	}
}

// TestSearchNodesRealIndexingBeyondTheWindow builds four folder nodes whose
// content is larger than the window. The builds overlap, each node publishes
// its shards, and the read budget gets all its weight back. The test holds a
// read lock on the first node, so its build waits to publish while it holds
// its slot. The three other builds start only during that wait, and must run
// to their end. If they cannot, the test runs until the go test timeout,
// which prints each goroutine.
func TestSearchNodesRealIndexingBeyondTheWindow(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	// Four builds keep the proof for this budget: 2 x 4 x window + 2 x file
	// (each build has a lexical and a semantic consumer).
	const testBudget int64 = 8*1024*1024 + 512*1024
	const testWindow int64 = 1 * 1024 * 1024
	const perCorpusBytes int64 = 2 * 1024 * 1024
	const fileSize int64 = 256 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	restoreWindow := swapIndexWindowBytesForTest(testWindow)
	defer restoreWindow()
	defer goroutineLeakGuard(t)()

	plans := make([]node, 4)
	for index := range plans {
		plans[index] = planSynthCorpus(t, writeRandomFolder(t, perCorpusBytes, fileSize))
	}
	userQ, err := parseSearchQuery("zzz_no_match_zzz")
	if err != nil {
		t.Fatal(err)
	}
	release := holdReadLock(t, plans[0].cacheDir)
	_, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitReaders, filepath.Join(plans[0].cacheDir, lockFile))
	// The other builds start at the publish wait of the first build. If the
	// first build ends with no wait, they start then, and lockWaitActed fails.
	firstWaits := make(chan struct{})
	startOthers := sync.OnceFunc(func() { close(firstWaits) })
	var others sync.WaitGroup
	others.Add(len(plans) - 1)
	acted := onLockWait(t, waiting, func() error {
		startOthers()
		others.Wait()
		return nil
	}, release)
	_, _, err = searchNodes(t.Context(), plans, func(nodeCtx context.Context, plan node) ([]corpusSearchResult, dirtyFileSet, error) {
		if plan.id == plans[0].id {
			defer startOthers()
		} else {
			defer others.Done()
			<-firstWaits
		}
		return prepareAndSearchCorpus(nodeCtx, plan, userQ, defaultSearchConfig())
	})
	if err != nil {
		t.Fatal(err)
	}
	lockWaitActed(t, acted)
	for index, plan := range plans {
		if shards := repositoryShardCount(plan.indexDir, folderRepoName(plan)); shards < 1 {
			t.Fatalf("plan %d: no shard", index)
		}
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after the builds: got=%d want=%d", got, testBudget)
	}
}

// TestPipelinePreparesEachNodeOnce runs a description over two nodes, with a
// model that accepts every row. The command prepares each node once (2
// preparations). One model session serves the command, with one prepared
// query.
func TestPipelinePreparesEachNodeOnce(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("one/a.txt", "alpha beta\n")
	f.graphFile("one/b.txt", "alpha only\n")
	f.graphFile("two/c.txt", "alpha beta\n")
	f.graphFile("two/d.txt", "beta only\n")
	setTestUserCache(t)
	model := observeModel(&hybridTestScorer{mode: payloadAccept})
	before := nodePreparations.Load()
	if _, err := seekWith(t, model.factory(), "alpha beta", f.path("one"), f.path("two")); err != nil {
		t.Fatal(err)
	}
	if got := nodePreparations.Load() - before; got != 2 {
		t.Fatalf("preparations=%d, want one per node (2)", got)
	}
	if model.starts.Load() != 1 || model.prepares.Load() != 1 {
		t.Fatalf("sessions=%d prepared queries=%d, want 1 and 1", model.starts.Load(), model.prepares.Load())
	}
}

// TestCancelDuringGraphSearch cancels when the first node build reads its
// content. The search returns the cancellation once, warns for no node,
// publishes no index state, and leaves no goroutine. The Git shim signals the
// content read when it opens a FIFO to write. Then it fills the FIFO and waits
// until a signal stops it or the test closes the FIFO. It keeps the output pipe
// of Git open, so the build waits for its output. A search that does not stop
// its build runs until the go test timeout, which prints each goroutine.
func TestCancelDuringGraphSearch(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	first := f.graphRepo("folder/one", "a.txt", "zqcancelmarker")
	second := f.graphRepo("folder/two", "b.txt", "zqcancelmarker")
	setTestUserCache(t)
	started := filepath.Join(t.TempDir(), "build-started")
	if err := unix.Mkfifo(started, 0o600); err != nil {
		t.Fatalf("make the FIFO that signals the content read: %v", err)
	}
	installGitShim(t, "if [ \"$subcommand\" = cat-file ]; then\n"+
		"  exec cat /dev/zero 4>&1 > \"$SEEK_TEST_BUILD_STARTED\"\n"+
		"fi\n"+
		"exec \"$SEEK_TEST_REAL_GIT\" \"$@\"\n")
	t.Setenv("SEEK_TEST_BUILD_STARTED", started)
	defer goroutineLeakGuard(t)()
	logs := captureTestLogs(t, slog.LevelWarn)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opened := make(chan *os.File, 1)
	go func() {
		// The open waits until a shim opens the FIFO to write.
		signal, err := os.Open(started)
		if err == nil {
			cancel()
		}
		opened <- signal
	}()
	// A search that ends before any content read leaves the open waiting:
	// a writer that does not wait ends it. The close of the read end stops a
	// shim that still writes.
	defer func() {
		if writer, err := os.OpenFile(started, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_ = writer.Close()
		}
		if signal := <-opened; signal != nil {
			_ = signal.Close()
		}
	}()
	err := seekRoot(ctx, t, nil, "--lexical-only", "zqcancelmarker", f.path("folder"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want the cancellation", err)
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("a cancelled search warned: %v", records)
	}
	for _, repo := range []string{first, second} {
		paths, err := resolveGitPaths(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := planGitCorpus(paths, nil)
		if err != nil {
			t.Fatal(err)
		}
		if state := readStateFile(plan.cacheDir); state != "" {
			t.Fatalf("%s published state %q after the cancellation", repo, state)
		}
	}
}

// TestPartialFailureContract searches a folder with a one-word and a two-word
// query. The folder has an unreadable subfolder and three nested repositories.
// One has a bad HEAD, one has a corrupt index, and the build of the third
// cannot read its content. The readable content returns, each failure warns
// once, and the exit is 0.
func TestPartialFailureContract(t *testing.T) {
	requireTools(t)
	const words = "zqpartial contract"
	for _, query := range []string{"zqpartial", words} {
		t.Run(query, func(t *testing.T) {
			f := newGraphFixture(t)
			f.graphFile("tree/readable.txt", words)
			f.graphFile("tree/locked/hidden.txt", words)
			badHead := f.graphRepo("tree/badhead", "head.txt", words)
			writeFixtureFile(t, filepath.Join(badHead, ".git", "HEAD"), "not a head\n")
			badIndex := f.graphRepo("tree/badindex", "index.txt", words)
			writeFixtureFile(t, filepath.Join(badIndex, ".git", "index"), "not an index\n")
			badBuild := f.graphRepo("tree/badbuild", "build.txt", words)
			chmodUnreadableOrFatal(t, f.path("tree/locked"), 0)
			hidden := []string{"hidden.txt", "head.txt", "index.txt", "build.txt"}
			// The build of badbuild cannot read its content.
			installGitShim(t, "case \"$PWD $*\" in *badbuild*) if [ \"$subcommand\" = cat-file ]; then echo forced-read-failure >&2; exit 3; fi ;; esac\n"+
				"exec \"$SEEK_TEST_REAL_GIT\" \"$@\"\n")
			setTestUserCache(t)
			logs := captureTestLogs(t, slog.LevelWarn)
			out, _, err := stubSearch(t, payloadFar, query, f.path("tree"))
			if exitCodeForError(err) != 0 || !strings.Contains(out, "readable.txt") {
				t.Fatalf("err=%v output:\n%s", err, out)
			}
			for _, name := range hidden {
				if strings.Contains(out, name) {
					t.Fatalf("a failed part showed %s:\n%s", name, out)
				}
			}
			records := logs.Records()
			if len(records) != len(hidden) || !logsMention(records, "could not be read") ||
				!logsMention(records, badHead) || !logsMention(records, badIndex) || !logsMention(records, badBuild) {
				t.Fatalf("warnings=%v, want one for each of the %d failed parts", records, len(hidden))
			}
		})
	}
}

// TestOneFailedNodeWarnsOnce reaches one failed node from two parts of the
// graph: an operand and a walk, or two walks. The node gets one warning, and a
// repository that Git cannot read shows no Git-ignored file.
func TestOneFailedNodeWarnsOnce(t *testing.T) {
	requireTools(t)
	const marker = "zqwarnoncenode"
	for _, tc := range []struct {
		name  string
		setup func(f *graphFixture) []string
	}{
		{name: "operand and walk", setup: func(f *graphFixture) []string {
			f.graphFile("tree/ok.txt", marker)
			f.graphFile("tree/broken/inside.txt", marker)
			writeFixtureFile(t, f.path("tree/broken/.git"), "gitdir: "+f.path("nowhere/git")+"\n")
			return []string{f.path("tree"), f.path("tree/broken")}
		}},
		{name: "subfolder operand and walk", setup: func(f *graphFixture) []string {
			f.graphFile("tree/ok.txt", marker)
			repo := f.GitRepo("tree/repo", ".gitignore", "sub/secret.env\n")
			writeFixtureFile(t, filepath.Join(repo, "sub", "secret.env"), marker)
			writeFixtureFile(t, filepath.Join(repo, ".git", "index"), "not an index\n")
			return []string{f.path("tree"), filepath.Join(repo, "sub")}
		}},
		{name: "two walks", setup: func(f *graphFixture) []string {
			f.graphFile("tree/ok.txt", marker)
			f.graphFile("tree/inner/ok2.txt", marker)
			f.graphFile("tree/inner/broken/inside.txt", marker)
			writeFixtureFile(t, f.path("tree/inner/broken/.git"), "gitdir: "+f.path("nowhere/git")+"\n")
			return []string{f.path("tree"), f.path("tree/inner")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGraphFixture(t)
			operands := tc.setup(f)
			setTestUserCache(t)
			logs := captureTestLogs(t, slog.LevelWarn)
			out, err := seek(t, slices.Concat([]string{"--lexical-only", marker}, operands)...)
			if err != nil || !strings.Contains(out, "ok.txt") || strings.Contains(out, "secret.env") || strings.Contains(out, "inside.txt") {
				t.Fatalf("err=%v output:\n%s", err, out)
			}
			if records := logs.Records(); len(records) != 1 {
				t.Fatalf("warnings=%v, want one for the one failed node", records)
			}
		})
	}
}

// TestPersistentShardDamageFailsOnce damages an uncommitted shard and holds
// the node's build lock, as a peer builder would, so that the repair cannot
// rebuild it. A description over that node and a folder fails the node once.
// The command has 3 preparations (the repo and its repair, the folder once),
// one damage notice, and one skipped node.
func TestPersistentShardDamageFailsOnce(t *testing.T) {
	requireTools(t)
	const words = "zqpersistent damage"
	f := newGraphFixture(t)
	repo := f.graphRepo("tree/repo", "a.txt", words)
	writeFixtureFile(t, filepath.Join(repo, "untracked.txt"), words+" dirty\n")
	f.graphFile("plain/b.txt", words)
	setTestUserCache(t)
	operands := []string{repo, f.path("plain")}
	newModel := modelFactory(&hybridTestScorer{})
	if _, err := seekWith(t, newModel, slices.Concat([]string{words}, operands)...); err != nil {
		t.Fatal(err)
	}
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planGitCorpus(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	names, err := familyShardNames(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	damaged := 0
	for _, name := range names {
		if !isUncommittedShard(filepath.Base(name)) {
			continue
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		junk := make([]byte, info.Size())
		for index := range junk {
			junk[index] = 0xff
		}
		if err := os.WriteFile(name, junk, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		damaged++
	}
	if damaged == 0 {
		t.Fatal("no uncommitted shard to damage, so the test proves nothing")
	}
	// The repair rebuilds a family that fails its manifest. A peer that
	// holds the build lock keeps the damage for this command.
	holdBuildLock(t, plan.cacheDir)
	logs := captureTestLogs(t, slog.LevelWarn)
	before := nodePreparations.Load()
	out, err := seekWith(t, newModel, slices.Concat([]string{words}, operands)...)
	if err != nil || !strings.Contains(out, "b.txt") {
		t.Fatalf("err=%v output:\n%s", err, out)
	}
	// The repo: the first preparation and the repair; the folder: one.
	if got := nodePreparations.Load() - before; got != 3 {
		t.Fatalf("preparations=%d, want 3", got)
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
	if skipped != 1 || damage != 1 {
		t.Fatalf("warnings=%v, want one damage notice and one skipped node", logs.Records())
	}
}

// TestCommittedShardDamageRepairsOnce damages the committed shards of a clean
// repository with bytes of the same size. The family manifest holds names and
// sizes only, so the warm path trusts it. The read fails, the repair removes
// the manifest and rebuilds, and the second read finds the file. The command
// has two preparations and one damage notice.
func TestCommittedShardDamageRepairsOnce(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.graphRepo("repo", "a.txt", "zqrepair committed")
	setTestUserCache(t)
	if _, err := seek(t, "--lexical-only", "zqrepair", repo); err != nil {
		t.Fatal(err)
	}
	plans, err := planCorpora(t.Context(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	names, err := familyShardNames(plans[0].indexDir)
	if err != nil {
		t.Fatal(err)
	}
	damaged := 0
	for _, name := range names {
		if isUncommittedShard(filepath.Base(name)) {
			continue
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, bytes.Repeat([]byte{0xff}, int(info.Size())), 0o644); err != nil {
			t.Fatal(err)
		}
		damaged++
	}
	if damaged == 0 {
		t.Fatal("no committed shard to damage, so the test proves nothing")
	}
	logs := captureTestLogs(t, slog.LevelWarn)
	before := nodePreparations.Load()
	out, err := seek(t, "--lexical-only", "zqrepair", repo)
	if err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("the repair must find the file: err=%v output:\n%s", err, out)
	}
	// The first preparation, then the one of the repair.
	if got := nodePreparations.Load() - before; got != 2 {
		t.Fatalf("preparations=%d, want 2", got)
	}
	damage := 0
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "Index damage detected") {
			damage++
		}
	}
	if damage != 1 {
		t.Fatalf("damage notices=%d, want 1: %v", damage, logs.Records())
	}
}

// TestDescriptionSearchWhileAPeerBuilds holds the build lock of a Git node
// and a folder node, as a peer builder would. It searches after an edit of
// each, with a model that accepts every row. Each node serves its last
// complete index (corpusStale), so the description reads its text index only.
// The command has one preparation per node, no warning, the old files, and no
// vector hit. a.txt holds no query word. The first search, with no peer,
// prints it through vectors; the second does not.
func TestDescriptionSearchWhileAPeerBuilds(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "committed\n")
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "zqpeer alpha beta\n")
	f.graphFile("plain/c.txt", "zqpeer alpha beta")
	setTestUserCache(t)
	operands := []string{repo, f.path("plain")}
	newModel := modelFactory(&hybridTestScorer{mode: payloadAccept})
	out, err := seekWith(t, newModel, slices.Concat([]string{"zqpeer alpha"}, operands)...)
	if err != nil || !strings.Contains(out, "b.txt") || !strings.Contains(out, "c.txt") || !strings.Contains(out, "a.txt") {
		t.Fatalf("the first search must build both nodes and find a.txt through vectors: err=%v output:\n%s", err, out)
	}
	plans, err := planCorpora(t.Context(), operands)
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	for _, plan := range plans {
		// In one process a second open file description conflicts, as a
		// peer process does.
		holdBuildLock(t, plan.cacheDir)
	}
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "zqpeer alpha beta edited\n")
	f.graphFile("plain/c.txt", "zqpeer alpha beta edited")
	logs := captureTestLogs(t, slog.LevelWarn)
	before := nodePreparations.Load()
	out, err = seekWith(t, newModel, slices.Concat([]string{"zqpeer alpha"}, operands)...)
	if err != nil || !strings.Contains(out, "c.txt") || !strings.Contains(out, "b.txt") || strings.Contains(out, "edited") {
		t.Fatalf("want both nodes' old text indexes: err=%v output:\n%s", err, out)
	}
	if strings.Contains(out, "a.txt") {
		t.Fatalf("the search read vectors while a peer builds:\n%s", out)
	}
	if got := nodePreparations.Load() - before; got != 2 {
		t.Fatalf("preparations=%d, want 1 per node", got)
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("a peer build is no failure, but the search warned: %v", records)
	}
}

// TestDescriptionReadRetriesAfterADirtyFileChanges edits a dirty file after
// the build read it and before the build publishes. The read sees that the
// work tree does not hold the published state now. It prepares the node
// again and finds the new text.
func TestDescriptionReadRetriesAfterADirtyFileChanges(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "committed\n")
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "zqretry alpha\n")
	writeFixtureFile(t, filepath.Join(repo, "b.txt"), "zqretry old\n")
	setTestUserCache(t)
	plans, err := planCorpora(t.Context(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	// The test holds a read lock, so the build waits to publish. Both readers
	// of b.txt in the build (the text index and the overlay) read the old
	// bytes before that wait. The test edits b.txt during the wait.
	release := holdReadLock(t, plans[0].cacheDir)
	_, waiting := captureTestLogsWithSignal(t, slog.LevelWarn, lockWaitReaders, filepath.Join(plans[0].cacheDir, lockFile))
	acted := onLockWait(t, waiting, func() error {
		return os.WriteFile(filepath.Join(repo, "b.txt"), []byte("zqretry fresh\n"), 0o644)
	}, release)
	before := nodePreparations.Load()
	out, _, err := stubSearch(t, payloadFar, "zqretry fresh", repo)
	if err != nil {
		t.Fatal(err)
	}
	lockWaitActed(t, acted)
	// The first preparation, then the one after the read saw the change.
	if got := nodePreparations.Load() - before; got != 2 {
		t.Fatalf("preparations=%d, want 2", got)
	}
	if !strings.Contains(out, "b.txt") || !strings.Contains(out, "zqretry fresh") {
		t.Fatalf("output lacks the edited file:\n%s", out)
	}
}

// TestInterruptedChildIsACancellation stops a child with SIGINT, as a terminal
// Ctrl-C does before Seek cancels its context: the error is a cancellation,
// with no node warning.
func TestInterruptedChildIsACancellation(t *testing.T) {
	err := exec.Command("sh", "-c", "kill -INT $$").Run()
	if !interruptedChild(err) {
		t.Fatalf("err=%v is not an interrupted child", err)
	}
	if exitErr := exec.Command("sh", "-c", "exit 3").Run(); interruptedChild(exitErr) {
		t.Fatalf("a normal exit counts as an interrupt: %v", exitErr)
	}
	logs := captureTestLogs(t, slog.LevelWarn)
	warnSkippedNode(t.Context(), "/n", fmt.Errorf("git status: %w", err))
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("an interrupted child warned: %v", records)
	}
	if got := asNodeFailure(t.Context(), err); !errors.Is(got, context.Canceled) {
		t.Fatalf("asNodeFailure=%v, want the cancellation", got)
	}
}

// TestBuildsUseTheCommandWindow shrinks the window outside a command to
// 32 KiB, then runs one-node commands, whose window is the whole budget. The
// full build writes one shard. 80 KiB deltas stay deltas at the folder, the
// committed Git, and the uncommitted Git admission. A build that reads the
// global window refuses each 80 KiB delta as larger than the window, and
// rebuilds in full. A delta adds one shard to the family; a full rebuild of
// this content writes one shard.
func TestBuildsUseTheCommandWindow(t *testing.T) {
	requireTools(t)
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()
	restore := swapIndexWindowBytesForTest(32 << 10)
	defer restore()
	f := newGraphFixture(t)
	setTestUserCache(t)
	const marker = "zqwindowmarker"
	write := func(dir, version string) {
		t.Helper()
		for index := range 40 {
			body := fmt.Sprintf("%s %s %d\n%s", marker, version, index, strings.Repeat(fmt.Sprintf("line %s %d\n", version, index), 160))
			writeFixtureFile(t, filepath.Join(dir, fmt.Sprintf("f%02d.txt", index)), body)
		}
	}
	search := func(operand string) {
		t.Helper()
		if _, err := seek(t, "--lexical-only", marker, operand); err != nil {
			t.Fatal(err)
		}
	}

	folder := f.path("folder")
	write(folder, "one")
	search(folder)
	plans, err := planCorpora(t.Context(), []string{folder})
	if err != nil {
		t.Fatal(err)
	}
	if shards := repositoryShardCount(plans[0].indexDir, folderRepoName(plans[0])); shards != 1 {
		t.Fatalf("the folder build wrote %d shards, want one window", shards)
	}
	write(folder, "two")
	search(folder)
	if shards := repositoryShardCount(plans[0].indexDir, folderRepoName(plans[0])); shards != 2 {
		t.Fatalf("the folder family has %d shards after the change, want the base and one delta", shards)
	}

	repo := f.GitRepo("repo", "README.md", "readme\n")
	write(repo, "one")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "one")
	search(repo)
	base := gitOutputIn(t, repo, "rev-parse", "HEAD")
	write(repo, "two")
	gitRunIn(t, repo, "commit", "-q", "-am", "two")
	search(repo)
	plans, err = planCorpora(t.Context(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if state, ok := readCommittedGitState(plans[0].cacheDir); !ok || string(state.baseHead) != base {
		t.Fatalf("committed state %+v ok=%t, want a delta on base %s", state, ok, base)
	}

	writeFixtureFile(t, filepath.Join(repo, "f00.txt"), marker+" dirty\n")
	search(repo)
	if shards := repositoryShardCount(plans[0].indexDir, repoUncommitted); shards != 1 {
		t.Fatalf("the uncommitted build wrote %d shards, want one", shards)
	}
	write(repo, "three")
	search(repo)
	if shards := repositoryShardCount(plans[0].indexDir, repoUncommitted); shards != 2 {
		t.Fatalf("the uncommitted family has %d shards after the change, want the base and one delta", shards)
	}
}

// TestPeerBuildPrintsNoStaleTextOfAnEditedFile commits a file, builds, and
// edits it while a peer holds the build lock. The node serves its last
// complete index, which holds only the old committed copy: neither route may
// print that old text.
func TestPeerBuildPrintsNoStaleTextOfAnEditedFile(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "a.txt", "zqstaleedit alpha beta\n")
	setTestUserCache(t)
	newModel := modelFactory(&hybridTestScorer{})
	if _, err := seekWith(t, newModel, "zqstaleedit alpha", repo); err != nil {
		t.Fatal(err)
	}
	plans, err := planCorpora(t.Context(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	holdBuildLock(t, plans[0].cacheDir)
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "edited while a peer builds\n")
	for _, test := range []struct {
		name     string
		args     []string
		newModel semanticModelFactory
	}{
		{"strict", []string{"--lexical-only", "zqstaleedit", repo}, nil},
		{"description", []string{"zqstaleedit alpha", repo}, newModel},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := seekWith(t, test.newModel, test.args...)
			if strings.Contains(out, "zqstaleedit") || err != nil && !errors.Is(err, errNoMatch) {
				t.Fatalf("want no old text of the edited file: err=%v output:\n%s", err, out)
			}
		})
	}
}

// TestPeerBuildUncommittedCopyWinsForARevertedFile edits a committed file,
// builds, and reverts the edit while a peer holds the build lock. The node
// serves its last complete index: its uncommitted shards still hold the edited
// copy, and the capture lists the file as clean. The uncommitted copy wins on
// both routes: the file prints once, as the uncommitted copy, and the
// committed text does not print.
func TestPeerBuildUncommittedCopyWinsForARevertedFile(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	const committed = "zqrevertwin committedtext alpha beta\n"
	repo := f.GitRepo("repo", "a.txt", committed)
	setTestUserCache(t)
	newModel := modelFactory(&hybridTestScorer{})
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), "zqrevertwin editedtext alpha beta\n")
	if _, err := seekWith(t, newModel, "zqrevertwin alpha", repo); err != nil {
		t.Fatal(err)
	}
	plans, err := planCorpora(t.Context(), []string{repo})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	holdBuildLock(t, plans[0].cacheDir)
	writeFixtureFile(t, filepath.Join(repo, "a.txt"), committed)
	if out, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output(); err != nil || len(out) != 0 {
		t.Fatalf("the revert left the work tree dirty: %q err=%v", out, err)
	}
	for _, test := range []struct {
		name     string
		args     []string
		newModel semanticModelFactory
	}{
		{"strict", []string{"--lexical-only", "zqrevertwin", repo}, nil},
		{"description", []string{"zqrevertwin alpha", repo}, newModel},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := seekWith(t, test.newModel, test.args...)
			if err != nil {
				t.Fatalf("search: %v output:\n%s", err, out)
			}
			headers := headerOrder(out)
			if len(headers) != 1 || !strings.HasSuffix(headers[0], "a.txt (Text) [uncommitted]") {
				t.Fatalf("headers=%q, want a.txt once, as the uncommitted copy; output:\n%s", headers, out)
			}
			if !strings.Contains(out, "editedtext") || strings.Contains(out, "committedtext") {
				t.Fatalf("want the uncommitted text only; output:\n%s", out)
			}
		})
	}
}
