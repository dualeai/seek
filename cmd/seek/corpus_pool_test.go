package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// TestPoolEnqueueDedupesByCorpusID — calling Enqueue twice with the
// same corpusID should only spawn one worker. Defends against the
// physical-repo-via-two-paths case once walker discovery lands.
func TestPoolEnqueueDedupesByCorpusID(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()

	var completed int32
	worker := func(_ context.Context, _ corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		atomic.AddInt32(&completed, 1)
		return nil, nil, nil
	}
	pool, g := newTestPool(t, worker, 4)
	plan := corpusPlan{id: corpusID("dup"), userExplicit: true}
	if !pool.Enqueue(plan) {
		t.Fatal("first Enqueue must accept (returned false)")
	}
	if pool.Enqueue(plan) {
		t.Fatal("second Enqueue with same id must reject (returned true)")
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("g.Wait: %v", err)
	}
	if got := atomic.LoadInt32(&completed); got != 1 {
		t.Fatalf("completed=%d, want 1 (dedup must drop the second enqueue)", got)
	}
}

// TestPoolEnqueueLifecycleInvariant — a worker that mid-flight enqueues
// child plans must have those children awaited by g.Wait. Regression
// guard for dynamic discovery (PR3 commit 4+): if g.Wait could return
// before children registered via Enqueue completed, the result channel
// would close out from under live producers.
func TestPoolEnqueueLifecycleInvariant(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()

	var completed int32
	seedPlans := []corpusPlan{{id: corpusID("seed")}}

	// Build pool manually so the worker can capture it and call Enqueue
	// from inside its own body — exactly the dynamic-discovery pattern.
	var pool *corpusPool
	worker := func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		atomic.AddInt32(&completed, 1)
		if plan.id == "seed" {
			pool.Enqueue(corpusPlan{id: corpusID("child-1")})
			pool.Enqueue(corpusPlan{id: corpusID("child-2")})
		}
		return nil, nil, nil
	}

	var g *errgroup.Group
	pool, g = newTestPool(t, worker, 16)
	for _, p := range seedPlans {
		pool.Enqueue(p)
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("g.Wait: %v", err)
	}

	if got := atomic.LoadInt32(&completed); got != 3 {
		t.Fatalf("completed=%d, want 3 (1 seed + 2 children)", got)
	}
}

// TestRunCorpusPool_CtxCancelMidFlight — cancelling the parent context
// while workers are mid-flight must propagate to per-plan contexts so
// the workers observe Done() and the pool returns context.Canceled.
// Regression guard for the per-plan WithCancel(gctx) wiring: if the
// child ctx were derived from background instead of gctx, cancellation
// would not flow through and workers would hang past Wait.
func TestRunCorpusPool_CtxCancelMidFlight(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	const numPlans = 4
	plans := make([]corpusPlan, numPlans)
	for i := range plans {
		// userExplicit=true so the cancellation error propagates out
		// instead of being swallowed under the discovered-plan policy.
		plans[i] = corpusPlan{id: corpusID("plan-" + string(rune('A'+i))), userExplicit: true}
	}

	// started is signalled once when ANY worker enters its select;
	// cancel fires after that. Avoids the time.Sleep race where -race
	// or a slow runner could fire cancel BEFORE any worker started,
	// turning the test into "cancel-before-spawn" rather than the
	// "cancel-mid-flight" contract we want to assert.
	started := make(chan struct{}, numPlans)
	var observed int32
	worker := func(wctx context.Context, _ corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-wctx.Done():
			atomic.AddInt32(&observed, 1)
			return nil, nil, wctx.Err()
		case <-time.After(5 * time.Second):
			return nil, nil, nil
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	_, _, err := runCorpusPool(ctx, plans, worker)
	if err == nil {
		t.Fatal("err=nil; want context.Canceled or wrapped variant")
	}
	if !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("err=%v; want cancellation-related", err)
	}
	if got := atomic.LoadInt32(&observed); got == 0 {
		t.Fatal("no worker observed cancellation; per-plan ctx not derived from gctx?")
	}
}

// TestRunCorpusPool_NonExplicitErrorSwallowed — a worker error on a
// discovered (non-explicit) plan must be logged and swallowed so the
// errgroup does not cancel siblings. Other plans complete normally.
func TestRunCorpusPool_NonExplicitErrorSwallowed(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	plans := []corpusPlan{
		{id: corpusID("good"), userExplicit: true},
		{id: corpusID("bad-discovered"), userExplicit: false},
	}
	var completed int32
	worker := func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		if plan.id == "bad-discovered" {
			return nil, nil, fmt.Errorf("synthetic broken submodule")
		}
		atomic.AddInt32(&completed, 1)
		return nil, nil, nil
	}
	_, _, err := runCorpusPool(t.Context(), plans, worker)
	if err != nil {
		t.Fatalf("err=%v, want nil (discovered failure must not poison search)", err)
	}
	if got := atomic.LoadInt32(&completed); got != 1 {
		t.Fatalf("completed=%d, want 1 (good plan must run)", got)
	}
}

// TestRunCorpusPool_NonExplicitPanicSwallowed — panic in a discovered
// plan also swallowed. Mirror of the error case above.
func TestRunCorpusPool_NonExplicitPanicSwallowed(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	plans := []corpusPlan{
		{id: corpusID("good"), userExplicit: true},
		{id: corpusID("panic-discovered"), userExplicit: false},
	}
	var completed int32
	worker := func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		if plan.id == "panic-discovered" {
			panic("synthetic discovered panic")
		}
		atomic.AddInt32(&completed, 1)
		return nil, nil, nil
	}
	_, _, err := runCorpusPool(t.Context(), plans, worker)
	if err != nil {
		t.Fatalf("err=%v, want nil (discovered panic must not poison search)", err)
	}
	if got := atomic.LoadInt32(&completed); got != 1 {
		t.Fatalf("completed=%d, want 1 (good plan must run)", got)
	}
}

// TestRunCorpusPool_PanicIsolation — a worker that panics must not
// crash the process; the pool must return a wrapped error and other
// workers must complete normally.
func TestRunCorpusPool_PanicIsolation(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	// All plans userExplicit=true so the panic propagates (per PR3
	// commit 2 policy, non-explicit panics are logged + swallowed —
	// covered by TestRunCorpusPool_NonExplicitPanicSwallowed).
	plans := []corpusPlan{
		{id: corpusID("plan-good-1"), userExplicit: true},
		{id: corpusID("plan-panic"), userExplicit: true},
		{id: corpusID("plan-good-2"), userExplicit: true},
	}

	var completed int32
	worker := func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		if plan.id == "plan-panic" {
			panic("synthetic worker panic")
		}
		atomic.AddInt32(&completed, 1)
		return nil, nil, nil
	}

	_, _, err := runCorpusPool(t.Context(), plans, worker)
	if err == nil {
		t.Fatal("err=nil, want wrapped panic")
	}
	if !strings.Contains(err.Error(), "corpus worker panic") {
		t.Fatalf("err=%v, want wrapped panic message", err)
	}
	if !strings.Contains(err.Error(), "synthetic worker panic") {
		t.Fatalf("err=%v, want original panic value", err)
	}
	// errgroup's first non-nil error cancels gctx; whether plan-good-2
	// observed the cancellation in time is racy. Assert at least one
	// good worker completed (panic didn't crash plan-good-1 either).
	if atomic.LoadInt32(&completed) == 0 {
		t.Fatal("no good worker completed; panic crashed siblings")
	}
}

// TestRunCorpusPool_EmptyPlans — sanity guard for the boundary case.
// Zero plans must return without spawning workers, without blocking on
// the result channel (len=0 makes it harmless), and without an error.
func TestRunCorpusPool_EmptyPlans(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	noop := func(context.Context, corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		return nil, nil, nil
	}
	results, dirty, err := runCorpusPool(t.Context(), nil, noop)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if len(results) != 0 {
		t.Fatalf("results=%v, want empty", results)
	}
	if len(dirty) != 0 {
		t.Fatalf("dirty=%v, want empty", dirty)
	}
}

// TestRunCorpusPool_NWayConcurrent — exercise the cap>1 hot path with
// N synthetic corpora running through the pool. This is the deadlock
// regression test for the corrected windowed-fit invariant: each
// corpus's content is sized at defaultIndexWindowBytes - some slack so
// every worker fully pends its window before rotating; a regression of
// the formula at corpusWorkerCap≥3 would leave the pool wedged.
func TestRunCorpusPool_NWayConcurrent(t *testing.T) {
	if corpusWorkerCap < 2 {
		t.Skipf("test requires corpusWorkerCap≥2; current=%d", corpusWorkerCap)
	}
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	// Budget sized like production: each worker can hold up to its
	// window-share concurrently. Use a small absolute budget to keep
	// test wall-clock low while preserving the budget topology.
	const testBudget int64 = 8 * 1024 * 1024
	const perCorpusBytes int64 = 2 * 1024 * 1024
	const fileSize int64 = 256 * 1024

	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	defer goroutineLeakGuard(t, 120*time.Second)()

	n := corpusWorkerCap
	plans := make([]corpusPlan, n)
	for i := range n {
		plans[i] = planSynthCorpus(t, writeRandomFolder(t, perCorpusBytes, fileSize))
	}

	userQ, err := parseSearchQuery("zzz_no_match_zzz")
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	worker := func(wctx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		return prepareAndSearchCorpus(wctx, plan, nil, userQ)
	}
	_, _, err = runCorpusPool(ctx, plans, worker)
	if err != nil {
		t.Fatalf("runCorpusPool err: %v", err)
	}

	for i, p := range plans {
		if shards := repositoryShardCount(p.indexDir, folderRepoName(p)); shards == 0 {
			t.Fatalf("plan %d: no shards (pool silently skipped under concurrency?)", i)
		}
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after N-way pool run: got=%d want=%d", got, testBudget)
	}
}

func TestDiscoveryAcceptsManyNestedGitCorpora(t *testing.T) {
	const repoCount = corpusWorkerCap*2 + 1
	pool, g := newTestPool(t, noopPoolWorker, repoCount)
	defer func() { _ = g.Wait(); close(pool.resultsCh) }()
	root := canonTempDir(t)
	accepted := 0
	for i := 0; i < repoCount; i++ {
		repo := filepath.Join(root, fmt.Sprintf("r%d", i))
		writeMinimalGitRepo(t, repo)
		b := gitBoundary{
			RepoDir:   repo,
			GitDir:    filepath.Join(repo, ".git"),
			CommonDir: filepath.Join(repo, ".git"),
			Mode:      rootTypeDirectory,
		}
		if pool.discoverNestedGit(b) {
			accepted++
		}
	}
	if accepted != repoCount {
		t.Fatalf("accepted=%d, want %d", accepted, repoCount)
	}
}

func TestRunCorpusPoolDrainsResultsWhileWorkersRun(t *testing.T) {
	const n = corpusWorkerCap*3 + 1
	plans := make([]corpusPlan, n)
	for i := range plans {
		plans[i] = corpusPlan{id: corpusID(fmt.Sprintf("plan-%02d", i)), userExplicit: true}
	}
	worker := func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		return []corpusSearchResult{{corpusID: plan.id}}, nil, nil
	}
	results, _, err := runCorpusPool(t.Context(), plans, worker)
	if err != nil {
		t.Fatalf("runCorpusPool: %v", err)
	}
	if len(results) != n {
		t.Fatalf("results=%d, want %d", len(results), n)
	}
}

func TestDiscoverNestedGitDedupSuppressesDescentWhenAlreadyCovered(t *testing.T) {
	pool, g := newTestPool(t, noopPoolWorker, 2)
	defer func() { _ = g.Wait(); close(pool.resultsCh) }()

	repo := filepath.Join(canonTempDir(t), "repo")
	writeMinimalGitRepo(t, repo)
	b := gitBoundary{
		RepoDir:   repo,
		GitDir:    filepath.Join(repo, ".git"),
		CommonDir: filepath.Join(repo, ".git"),
		Mode:      rootTypeDirectory,
	}
	plan, err := planDiscoveredGitCorpus(b)
	if err != nil {
		t.Fatalf("planDiscoveredGitCorpus: %v", err)
	}
	if !pool.Enqueue(plan) {
		t.Fatal("initial explicit-equivalent enqueue must accept")
	}

	if !pool.discoverNestedGit(b) {
		t.Fatal("covered boundary must suppress descent")
	}
}

// TestRunCorpusPool_DiscoveredPanicSwallowed — end-to-end through
// runCorpusPool: a discovered (non-explicit) plan panics in the
// worker; the seeded plan must complete and the pool must return no
// error (per the swallow policy at corpus_pool.go:Enqueue's defer-
// recover).
func TestRunCorpusPool_DiscoveredPanicSwallowed(t *testing.T) {
	good := corpusPlan{id: corpusID("seed-explicit"), userExplicit: true}
	bad := corpusPlan{id: corpusID("discovered-bad"), userExplicit: false}
	var completed int32
	worker := func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		if plan.id == "discovered-bad" {
			panic("synthetic discovered crash")
		}
		atomic.AddInt32(&completed, 1)
		return nil, nil, nil
	}
	_, _, err := runCorpusPool(t.Context(), []corpusPlan{good, bad}, worker)
	if err != nil {
		t.Fatalf("err=%v, want nil (discovered panic must not poison search)", err)
	}
	if atomic.LoadInt32(&completed) != 1 {
		t.Fatalf("good plan did not complete")
	}
}
