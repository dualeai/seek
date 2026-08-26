package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// TestPoolEnqueueDedupesByCorpusID covers two plans with the same corpus ID.
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

// TestPoolEnqueueLifecycleInvariant proves that Wait includes child plans that
// a running worker adds.
func TestPoolEnqueueLifecycleInvariant(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()

	var completed int32
	seedPlans := []corpusPlan{{id: corpusID("seed")}}

	// The worker captures the pool to match nested repository discovery.
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

// TestRunCorpusPool_CtxCancelMidFlight proves that a running worker receives
// parent cancellation and the pool returns it.
func TestRunCorpusPool_CtxCancelMidFlight(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	const numPlans = 4
	plans := make([]corpusPlan, numPlans)
	for i := range plans {
		// Explicit plan errors stop the pool.
		plans[i] = corpusPlan{id: corpusID("plan-" + string(rune('A'+i))), userExplicit: true}
	}

	// Cancel only after one worker starts.
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v; want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&observed); got == 0 {
		t.Fatal("no worker observed cancellation from the group context")
	}
}

func TestRunCorpusPool_ParentCancellationCannotReturnSuccess(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	logs := captureTestLogs(t, slog.LevelWarn)
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{}, 2)
	worker := func(workerCtx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		started <- struct{}{}
		<-workerCtx.Done()
		if !plan.userExplicit {
			return nil, nil, errors.New("opaque discovered fallout")
		}
		return nil, nil, nil
	}
	go func() {
		<-started
		<-started
		cancel()
	}()

	plans := []corpusPlan{
		{id: corpusID("explicit"), userExplicit: true},
		{id: corpusID("discovered"), root: "/tmp/discovered", userExplicit: false},
	}
	_, _, err := runCorpusPool(ctx, plans, worker)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want parent context cancellation", err)
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("parent cancellation records=%+v, want no warnings", records)
	}
}

// TestRunCorpusPool_NonExplicitErrorSwallowed — a worker error on a
// discovered (non-explicit) plan must be logged and swallowed so the
// errgroup does not cancel siblings. Other plans complete normally.
func TestRunCorpusPool_NonExplicitErrorSwallowed(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	logs := captureTestLogs(t, slog.LevelDebug)

	plans := []corpusPlan{
		{id: corpusID("good"), userExplicit: true},
		{id: corpusID("bad-discovered"), root: "/tmp/bad-discovered", userExplicit: false},
	}
	var completed int32
	workerErr := errors.New("synthetic broken submodule")
	worker := func(_ context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		if plan.id == "bad-discovered" {
			return nil, nil, workerErr
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
	records := logs.Records()
	if len(records) != 2 {
		t.Fatalf("records=%+v, want one warning and one detail record", records)
	}
	warnRecord, debugRecord := records[0], records[1]
	if warnRecord.Level != slog.LevelWarn || warnRecord.Message != "auto-discovered Git repository failed; results may be incomplete" {
		t.Fatalf("record=%+v, want independent discovered-failure warning", warnRecord)
	}
	attrs := testLogAttrs(warnRecord)
	if attrs["root"] != "/tmp/bad-discovered" {
		t.Fatalf("warning attrs=%+v, want discovered root", attrs)
	}
	if _, found := attrs["error"]; found {
		t.Fatalf("warning leaked technical cause: %+v", attrs)
	}
	if debugRecord.Level != slog.LevelDebug || debugRecord.Message != "auto-discovered Git repository failure details" {
		t.Fatalf("record=%+v, want discovered-failure details", debugRecord)
	}
	debugAttrs := testLogAttrs(debugRecord)
	if debugAttrs["root"] != "/tmp/bad-discovered" || debugAttrs["corpus_id"] != corpusID("bad-discovered") || debugAttrs["error"] != workerErr {
		t.Fatalf("detail attrs=%+v, want root, corpus ID, and original error", debugAttrs)
	}
}

func TestRunCorpusPool_DiscoveredFalloutAfterExplicitFailureDoesNotWarn(t *testing.T) {
	defer goroutineLeakGuard(t, defaultLeakTimeout)()
	logs := captureTestLogs(t, slog.LevelWarn)

	explicitErr := errors.New("explicit corpus failed")
	discoveredStarted := make(chan struct{})
	plans := []corpusPlan{
		{id: corpusID("explicit"), root: "/tmp/explicit", userExplicit: true},
		{id: corpusID("discovered"), root: "/tmp/discovered", userExplicit: false},
	}
	worker := func(ctx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		if !plan.userExplicit {
			close(discoveredStarted)
			<-ctx.Done()
			return nil, nil, errors.New("no loadable shards")
		}
		<-discoveredStarted
		return nil, nil, explicitErr
	}

	_, _, err := runCorpusPool(t.Context(), plans, worker)
	if !errors.Is(err, explicitErr) {
		t.Fatalf("runCorpusPool error=%v, want explicit failure", err)
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("discovered cancellation fallout records=%+v, want no warnings", records)
	}
}

// TestRunCorpusPool_EmptyPlans verifies the empty input contract.
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

func TestRunCorpusPool_LimitsAndOverlapsWorkers(t *testing.T) {
	if corpusWorkerCap < 2 {
		t.Skipf("test requires corpusWorkerCap of at least 2; current=%d", corpusWorkerCap)
	}
	defer goroutineLeakGuard(t, defaultLeakTimeout)()

	plans := make([]corpusPlan, corpusWorkerCap+2)
	for i := range plans {
		plans[i] = corpusPlan{id: corpusID(fmt.Sprintf("plan-%d", i)), userExplicit: true}
	}
	started := make(chan struct{}, len(plans))
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	var active int32
	var peak int32
	worker := func(ctx context.Context, _ corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		current := atomic.AddInt32(&active, 1)
		defer atomic.AddInt32(&active, -1)
		for {
			observed := atomic.LoadInt32(&peak)
			if current <= observed || atomic.CompareAndSwapInt32(&peak, observed, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return nil, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := runCorpusPool(t.Context(), plans, worker)
		done <- err
	}()
	for range corpusWorkerCap {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for all worker slots")
		}
	}
	select {
	case <-started:
		t.Fatal("worker count exceeded corpusWorkerCap")
	default:
	}
	if got := atomic.LoadInt32(&peak); got != int32(corpusWorkerCap) {
		t.Fatalf("peak workers=%d, want %d", got, corpusWorkerCap)
	}
	close(release)
	released = true
	if err := <-done; err != nil {
		t.Fatalf("runCorpusPool: %v", err)
	}
}

// TestRunCorpusPool_RealIndexingAcrossRotatingWindows exercises multiple
// production folder indexers whose content crosses the configured window.
func TestRunCorpusPool_RealIndexingAcrossRotatingWindows(t *testing.T) {
	if corpusWorkerCap < 2 {
		t.Skipf("test requires corpusWorkerCap of at least 2; current=%d", corpusWorkerCap)
	}
	if err := checkCtagsCached(); err != nil {
		t.Skipf("ctags required: %v", err)
	}
	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()

	const testBudget int64 = 8 * 1024 * 1024
	const testWindow int64 = 1 * 1024 * 1024
	const perCorpusBytes int64 = 2 * 1024 * 1024
	const fileSize int64 = 256 * 1024

	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()
	restoreWindow := swapIndexWindowBytesForTest(testWindow)
	defer restoreWindow()
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
		return prepareAndSearchCorpus(wctx, plan, nil, userQ, defaultSearchConfig())
	}
	_, _, err = runCorpusPool(ctx, plans, worker)
	if err != nil {
		t.Fatalf("runCorpusPool err: %v", err)
	}

	for i, p := range plans {
		if shards := repositoryShardCount(p.indexDir, folderRepoName(p)); shards < 2 {
			t.Fatalf("plan %d: shards=%d, want at least 2 after window rotation", i, shards)
		}
	}
	if got := availableWeight(readSemaphore); got != testBudget {
		t.Fatalf("semaphore leak after rotating-window pool run: got=%d want=%d", got, testBudget)
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
