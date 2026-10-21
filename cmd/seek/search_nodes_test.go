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

// TestSearchNodesRunsAPhysicalCorpusOnce reaches one corpus through two
// plans: it runs once.
func TestSearchNodesRunsAPhysicalCorpusOnce(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	var calls atomic.Int32
	plans := []corpusPlan{{id: "dup"}, {id: "dup"}, {id: "other"}}
	results, _, err := searchNodes(t.Context(), plans, func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
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
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	logs := captureTestLogs(t, slog.LevelWarn)
	plans := []corpusPlan{{id: "a", displayRoot: "/a"}, {id: "b", displayRoot: "/b"}, {id: "c", displayRoot: "/c"}}
	results, _, err := searchNodes(t.Context(), plans, func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
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
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	logs := captureTestLogs(t, slog.LevelWarn)
	want := errors.New("search: got nil (sub)query")
	_, _, err := searchNodes(t.Context(), []corpusPlan{{id: "a"}, {id: "b", displayRoot: "/b"}}, func(ctx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
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
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	logs := captureTestLogs(t, slog.LevelWarn)
	ctx, cancel := context.WithCancel(t.Context())
	plans := make([]corpusPlan, 8)
	for index := range plans {
		plans[index] = corpusPlan{id: corpusID(fmt.Sprintf("p%d", index)), displayRoot: fmt.Sprintf("/p%d", index)}
	}
	var started atomic.Int32
	_, _, err := searchNodes(ctx, plans, func(nodeCtx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
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
	results, dirty, err := searchNodes(t.Context(), nil, func(context.Context, corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		t.Fatal("no node to search")
		return nil, nil, nil
	})
	if err != nil || len(results) != 0 || len(dirty) != 0 {
		t.Fatalf("results=%d dirty=%d err=%v", len(results), len(dirty), err)
	}
}

// TestBuildSlotsFromReadBudget pins the build slot rule over host memory
// sizes: the slot limit and the window for one node and for all slots, and the
// deadlock proof (2 windows per build + 2 maximum-weight documents <= budget,
// a window of at least one such document) for every build count.
func TestBuildSlotsFromReadBudget(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		gib                  int64
		slots                int
		oneNodeWindowMiB     int64
		allSlotsWindowMinMiB int64
	}{
		{gib: 2, slots: 2, oneNodeWindowMiB: 200, allSlotsWindowMinMiB: 100},
		{gib: 8, slots: 9, oneNodeWindowMiB: 924, allSlotsWindowMinMiB: 100},
		{gib: 64, slots: 80, oneNodeWindowMiB: 8092, allSlotsWindowMinMiB: 100},
		{gib: 256, slots: 326, oneNodeWindowMiB: 32668, allSlotsWindowMinMiB: 100},
	} {
		budget := max(int64(maxInFlightBytes), tc.gib<<30/4)
		if got := buildSlotLimit(budget); got != tc.slots {
			t.Fatalf("%d GiB: slots=%d, want %d", tc.gib, got, tc.slots)
		}
		if got := indexWindowFor(budget, 1) / mib; got != tc.oneNodeWindowMiB {
			t.Fatalf("%d GiB: one-node window=%d MiB, want %d", tc.gib, got, tc.oneNodeWindowMiB)
		}
		if got := indexWindowFor(budget, 10*tc.slots); got != indexWindowFor(budget, tc.slots) || got/mib < tc.allSlotsWindowMinMiB {
			t.Fatalf("%d GiB: window beyond the slots=%d MiB", tc.gib, got/mib)
		}
		for builds := 1; builds <= tc.slots; builds++ {
			window := indexWindowFor(budget, builds)
			if window < maxReadWeightBytes || 2*int64(builds)*window+2*maxReadWeightBytes > budget {
				t.Fatalf("%d GiB, %d builds: window=%d breaks the proof for budget %d", tc.gib, builds, window, budget)
			}
		}
	}
}

// TestCommandWindowFollowsTheGraph sizes the window for a command: one node
// gets the whole budget, many nodes share it by the slot limit, and a build
// outside a command uses indexWindowBytes.
func TestCommandWindowFollowsTheGraph(t *testing.T) {
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()
	// A fixed 2 GiB read budget (the 8 GiB host row of
	// TestBuildSlotsFromReadBudget) gives windows computed by hand: 9 slots
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
// one process: a one-node command's build takes the whole budget, so a build
// of another command waits for it; the windows never pass the budget.
func TestConcurrentCommandsShareTheReadBudget(t *testing.T) {
	one := withIndexWindow(t.Context(), 1)
	release, err := acquireBuildSlot(one)
	if err != nil {
		t.Fatal(err)
	}
	// A failed check gives the slot back, so later slot tests do not wait
	// for it.
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	many := withIndexWindow(t.Context(), 1<<20)
	ctx, cancel := context.WithTimeout(many, 50*time.Millisecond)
	defer cancel()
	if second, err := acquireBuildSlot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if err == nil {
			second()
		}
		t.Fatalf("a second command's build ran beside a whole-budget build: %v", err)
	}
	release()
	released = true
	waitCtx, waitCancel := context.WithTimeout(many, 10*time.Second)
	defer waitCancel()
	second, err := acquireBuildSlot(waitCtx)
	if err != nil {
		t.Fatal(err)
	}
	second()
}

// TestRunSizesTheWindowFromTheGraph runs a search of one node: its build takes
// the whole build budget, so while it runs, the build of a command with many
// nodes waits for a slot.
func TestRunSizesTheWindowFromTheGraph(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("one/a.txt", "zqwindowmarker")
	setTestUserCache(t)
	var ran, waited bool
	folderBuildTestHook = func() {
		ran = true
		ctx, cancel := context.WithTimeout(withIndexWindow(context.Background(), 1<<20), 50*time.Millisecond)
		defer cancel()
		release, err := acquireBuildSlot(ctx)
		if err == nil {
			release()
		}
		waited = errors.Is(err, context.DeadlineExceeded)
	}
	t.Cleanup(func() { folderBuildTestHook = nil })
	if _, err := captureStdout(t, func() error { return run(t.Context(), "zqwindowmarker", []string{f.path("one")}) }); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("the build hook did not run, so the test proves nothing")
	}
	if !waited {
		t.Fatal("a build of another command took a slot beside a one-node build")
	}
}

// TestBuildsWithFewLargeDocumentTokensFinish builds 12 folder nodes at once
// whose files each fill a shard of their own, with 4 large documents per
// window, more readers than that per build, and the indexWindowFor budget: the
// builds finish and give all weight back. Builds share no large-document
// token: with shared tokens, a consumer that waited for a token kept its
// document while the token holders waited for readers that the read budget
// blocked, and the builds could hang under load.
func TestBuildsWithFewLargeDocumentTokensFinish(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	if indexParallelism() <= 4 {
		t.Skip("needs more readers than large-document tokens")
	}
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
	shardMax, largeDocumentParallelism = int(doc/3), 4
	defer func() { shardMax, largeDocumentParallelism = oldShardMax, oldTokens }()
	defer goroutineLeakGuard(t, 60*time.Second)()

	plans := make([]corpusPlan, nodes)
	for index := range plans {
		plans[index] = planSynthCorpus(t, writeRandomFolder(t, 20*doc, doc))
	}
	userQ, err := parseSearchQuery("zzz_no_match_zzz")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, _, err := searchNodes(ctx, plans, func(nodeCtx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		return prepareAndSearchCorpus(nodeCtx, plan, nil, userQ, defaultSearchConfig())
	}); err != nil {
		t.Fatalf("the builds did not finish: %v", err)
	}
	if got := availableWeight(readSemaphore); got != 2*nodes*window+2*doc {
		t.Fatalf("weight leak: %d", got)
	}
}

// TestOpenedNodesKeepsLexicalAfterAFailedUpgrade fails an upgrade to vectors:
// the caller gets the error, and a later route that needs no vectors still
// reads the good preparation.
func TestOpenedNodesKeepsLexicalAfterAFailedUpgrade(t *testing.T) {
	nodes := &openedNodes{}
	plan := corpusPlan{id: "n"}
	good := func() (openedNode, error) { return openedNode{plan: plan}, nil }
	bad := func() (openedNode, error) {
		return openedNode{}, &nodeOpenError{err: errors.New("vector build lock busy")}
	}
	if _, err := nodes.open(t.Context(), plan, false, good); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.open(t.Context(), plan, true, bad); err == nil {
		t.Fatal("the failed upgrade returned no error")
	}
	if _, err := nodes.open(t.Context(), plan, false, func() (openedNode, error) {
		t.Fatal("a third preparation ran")
		return openedNode{}, nil
	}); err != nil {
		t.Fatalf("the lexical route lost its preparation: %v", err)
	}
}

// TestSearchesTakeNoBuildSlot holds every build slot, as builders of other
// nodes would: a search of a ready node still finishes, and a node that must
// build waits for a slot.
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
		if _, err := captureStdout(t, func() error { return run(t.Context(), marker, []string{warm}) }); err != nil {
			t.Fatal(err)
		}
	}
	holdAll := func() {
		// A slot that another test kept fails here, not at the suite timeout.
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		if err := buildWindows.Acquire(ctx, buildWindowBytes); err != nil {
			t.Fatal(err)
		}
	}
	holdAll()
	held := true
	defer func() {
		if held {
			buildWindows.Release(buildWindowBytes)
		}
	}()
	for _, warm := range []string{f.path("warm"), warmRepo} {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		out, err := captureStdout(t, func() error { return run(ctx, marker, []string{warm}) })
		cancel()
		if err != nil || !strings.Contains(out, "a.txt") && !strings.Contains(out, "w.txt") {
			t.Fatalf("the warm search of %s did not finish while the slots were taken: err=%v\n%s", warm, err, out)
		}
	}
	// A cold build waits for a slot and ends only after one is free.
	for _, cold := range []string{f.path("cold"), coldRepo} {
		done := make(chan error, 1)
		go func() {
			_, err := captureStdout(t, func() error { return run(t.Context(), marker, []string{cold}) })
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("a cold build of %s ran with no free slot: %v", cold, err)
		case <-time.After(500 * time.Millisecond):
		}
		buildWindows.Release(buildWindowBytes)
		held = false
		if err := <-done; err != nil {
			t.Fatalf("the cold build of %s failed after a slot was free: %v", cold, err)
		}
		holdAll()
		held = true
	}
}

// TestBuildSlotsLimitConcurrentBuilds takes every build slot with blocked
// builds: one more build waits, and the held builds overlap.
func TestBuildSlotsLimitConcurrentBuilds(t *testing.T) {
	// Outside a command a build takes the two windows of a full set of slots.
	slots := buildSlotLimit(readBudgetBytes)
	gate := make(chan struct{})
	var held sync.WaitGroup
	var active atomic.Int32
	// Every holder gives its slot back, also when a check fails.
	var openGate sync.Once
	defer func() {
		openGate.Do(func() { close(gate) })
		held.Wait()
	}()
	holderCtx, cancelHolders := context.WithCancel(t.Context())
	defer cancelHolders()
	for range slots {
		held.Add(1)
		go func() {
			defer held.Done()
			release, err := acquireBuildSlot(holderCtx)
			if err != nil {
				return
			}
			active.Add(1)
			<-gate
			release()
		}()
	}
	for deadline := time.Now().Add(10 * time.Second); active.Load() != int32(slots); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d builds took a slot", active.Load(), slots)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if extra, err := acquireBuildSlot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if err == nil {
			extra()
		}
		t.Fatalf("a build ran beyond %d slots: %v", slots, err)
	}
}

// TestSearchNodesCollectsEveryNode runs more nodes than build slots: every
// node's results reach the merged list.
func TestSearchNodesCollectsEveryNode(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	plans := make([]corpusPlan, 3*buildSlotLimit(readBudgetBytes)+1)
	for index := range plans {
		plans[index] = corpusPlan{id: corpusID(fmt.Sprintf("plan-%03d", index))}
	}
	results, _, err := searchNodes(t.Context(), plans, func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		return []corpusSearchResult{{corpusID: plan.id}}, nil, nil
	})
	if err != nil || len(results) != len(plans) {
		t.Fatalf("results=%d err=%v, want %d", len(results), err, len(plans))
	}
}

// TestSearchNodesRealIndexingAcrossRotatingWindows builds four folder nodes
// at once whose content crosses the window: each rotates, and the read budget
// gets all its weight back.
func TestSearchNodesRealIndexingAcrossRotatingWindows(t *testing.T) {
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
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
	defer goroutineLeakGuard(t, 120*time.Second)()

	plans := make([]corpusPlan, 4)
	for index := range plans {
		plans[index] = planSynthCorpus(t, writeRandomFolder(t, perCorpusBytes, fileSize))
	}
	userQ, err := parseSearchQuery("zzz_no_match_zzz")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var peak atomic.Int32
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Microsecond):
			}
			// Each build outside a command takes two test windows.
			if held := int32((buildWindowBytes - availableWeight(buildWindows)) / (2 * testWindow)); held > peak.Load() {
				peak.Store(held)
			}
		}
	}()
	_, _, err = searchNodes(ctx, plans, func(nodeCtx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		return prepareAndSearchCorpus(nodeCtx, plan, nil, userQ, defaultSearchConfig())
	})
	close(stop)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak builds=%d, want the four builds to overlap", peak.Load())
	}
	for index, plan := range plans {
		if shards := repositoryShardCount(plan.indexDir, folderRepoName(plan)); shards < 2 {
			t.Fatalf("plan %d: shards=%d, want at least 2 after window rotation", index, shards)
		}
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after the rotating-window builds: got=%d want=%d", got, testBudget)
	}
}

// TestPipelinePreparesEachNodeOnce runs a description over two nodes: the
// strict and the relaxed pass read one preparation per node.
func TestPipelinePreparesEachNodeOnce(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("one/a.txt", "alpha beta\n")
	f.graphFile("one/b.txt", "alpha only\n")
	f.graphFile("two/c.txt", "alpha beta\n")
	f.graphFile("two/d.txt", "beta only\n")
	setTestUserCache(t)
	before := nodePreparations.Load()
	if _, err := runHybridTestSearch(t, "alpha beta", []string{f.path("one"), f.path("two")}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil }); err != nil {
		t.Fatal(err)
	}
	if got := nodePreparations.Load() - before; got != 2 {
		t.Fatalf("preparations=%d, want one per node (2)", got)
	}
}

// TestPipelineOneModelSessionPerCommand runs a description over two nodes:
// one model session serves the command.
func TestPipelineOneModelSessionPerCommand(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	f.graphFile("one/a.txt", "alpha beta\n")
	f.graphFile("two/c.txt", "alpha beta\n")
	setTestUserCache(t)
	var sessions atomic.Int32
	scorer := &hybridTestScorer{}
	if _, err := runHybridTestSearch(t, "alpha beta", []string{f.path("one"), f.path("two")}, defaultSearchPolicy(),
		func(context.Context) (semanticModel, error) {
			sessions.Add(1)
			return scorer, nil
		}); err != nil {
		t.Fatal(err)
	}
	if sessions.Load() != 1 || scorer.prepareCalls.Load() != 1 {
		t.Fatalf("sessions=%d query encodings=%d, want 1 and 1", sessions.Load(), scorer.prepareCalls.Load())
	}
}

// TestCancelDuringGraphSearch cancels when the first node build reads its
// content: the search returns the cancellation once, warns for no node,
// publishes no index state, and leaves no goroutine.
func TestCancelDuringGraphSearch(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	first := f.graphRepo("folder/one", "a.txt", "zqcancelmarker")
	second := f.graphRepo("folder/two", "b.txt", "zqcancelmarker")
	setTestUserCache(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(t.TempDir(), "build-started")
	shimDir := t.TempDir()
	shim := "#!/bin/sh\n" + gitShimSubcommand +
		"if [ \"$subcommand\" = cat-file ]; then\n" +
		"  : > \"$SEEK_TEST_BUILD_STARTED\"\n" +
		"  sleep 30\n" +
		"fi\n" +
		"exec \"$SEEK_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEEK_TEST_REAL_GIT", realGit)
	t.Setenv("SEEK_TEST_BUILD_STARTED", started)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	defer goroutineLeakGuard(t, 30*time.Second)()
	logs := captureTestLogs(t, slog.LevelWarn)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	begin := time.Now()
	err = run(ctx, "zqcancelmarker", []string{f.path("folder")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want the cancellation", err)
	}
	if elapsed := time.Since(begin); elapsed > 20*time.Second {
		t.Fatalf("the cancelled search ran %s", elapsed)
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

// TestPartialFailureContract searches a folder with an unreadable subfolder, a
// nested repository with a bad HEAD, one with a corrupt index, and one whose
// build cannot read its content: the readable content returns,
// each failure warns once, also when the description route reads the nodes
// twice, and the exit is 0.
func TestPartialFailureContract(t *testing.T) {
	requireTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
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
			locked := f.path("tree/locked")
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			// The build of badbuild cannot read its content.
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			shimDir := t.TempDir()
			shim := "#!/bin/sh\n" + gitShimSubcommand +
				"case \"$PWD $*\" in *badbuild*) if [ \"$subcommand\" = cat-file ]; then echo forced-read-failure >&2; exit 3; fi ;; esac\n" +
				"exec \"$SEEK_TEST_REAL_GIT\" \"$@\"\n"
			if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SEEK_TEST_REAL_GIT", realGit)
			t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			setTestUserCache(t)
			logs := captureTestLogs(t, slog.LevelWarn)
			out, err := runHybridTestSearch(t, query, []string{f.path("tree")}, defaultSearchPolicy(),
				func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil })
			if exitCodeForError(err) != 0 || !strings.Contains(out, "readable.txt") {
				t.Fatalf("err=%v output:\n%s", err, out)
			}
			for _, hidden := range []string{"hidden.txt", "head.txt", "index.txt", "build.txt"} {
				if strings.Contains(out, hidden) {
					t.Fatalf("a failed part showed %s:\n%s", hidden, out)
				}
			}
			records := logs.Records()
			if len(records) != 4 || !logsMention(records, "could not be read") ||
				!logsMention(records, badHead) || !logsMention(records, badIndex) || !logsMention(records, badBuild) {
				t.Fatalf("warnings=%v, want one for each failed part", records)
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
			out, err := captureStdout(t, func() error { return run(context.Background(), marker, operands) })
			if err != nil || !strings.Contains(out, "ok.txt") || strings.Contains(out, "secret.env") || strings.Contains(out, "inside.txt") {
				t.Fatalf("err=%v output:\n%s", err, out)
			}
			if records := logs.Records(); len(records) != 1 {
				t.Fatalf("warnings=%v, want one for the one failed node", records)
			}
		})
	}
}

// TestPersistentShardDamageFailsOnce damages an uncommitted shard so that the
// repair cannot fix it, then runs a description over that node and a folder:
// the node fails once in the command, with no preparation per pass.
func TestPersistentShardDamageFailsOnce(t *testing.T) {
	requireTools(t)
	const words = "zqpersistent damage"
	f := newGraphFixture(t)
	repo := f.graphRepo("tree/repo", "a.txt", words)
	writeFixtureFile(t, filepath.Join(repo, "untracked.txt"), words+" dirty\n")
	f.graphFile("plain/b.txt", words)
	setTestUserCache(t)
	operands := []string{repo, f.path("plain")}
	newModel := func(context.Context) (semanticModel, error) { return &hybridTestScorer{}, nil }
	if _, err := runHybridTestSearch(t, words, operands, defaultSearchPolicy(), newModel); err != nil {
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
	logs := captureTestLogs(t, slog.LevelWarn)
	before := nodePreparations.Load()
	out, err := runHybridTestSearch(t, words, operands, defaultSearchPolicy(), newModel)
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
// 32 KiB, then runs one-node commands, whose window is the whole budget: the
// full build writes one window, and 80 KiB deltas stay deltas at the folder,
// the committed Git, and the uncommitted Git admission. A build that
// read the global window would rotate and fall back to full rebuilds. A delta
// adds one shard to the family; a full rebuild in one window writes one shard.
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
		if _, err := captureStdout(t, func() error { return run(t.Context(), marker, []string{operand}) }); err != nil {
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
