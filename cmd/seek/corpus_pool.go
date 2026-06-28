package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"golang.org/x/sync/errgroup"
)

// corpusPoolResult is one corpus worker's contribution to the merged
// result set. A collector goroutine drains the channel while workers run, so
// discovery can enqueue any number of nested corpora without relying on a
// fixed result-buffer cap.
type corpusPoolResult struct {
	planID  corpusID
	results []corpusSearchResult
	dirty   dirtyFileSet
}

// corpusWorkerFunc executes one corpus plan and returns its result.
// Factored out so tests can inject panic-triggering or otherwise
// abnormal worker bodies without coupling to prepareAndSearchCorpus.
type corpusWorkerFunc func(ctx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error)

// TODO(perf/observability): BenchmarkCorpusPool_Sweep + _PeakRSS.
//
// Today's "~50% wall-clock reduction at cap=4" claim in caps.go is
// not pinned by an actual sweep over corpusWorkerCap; only N-way
// functional tests exist. Plan §J' calls out a parameterized bench
// that varies cap ∈ {1, 2, 4, 6, 8} on a representative multi-corpus
// fixture and reports wall-clock + heap-MB via `b.ReportMetric`.
//
// Why deferred: pure observability, not a perf change. Adding the
// bench surfaces ground truth for the sweet spot but does not move
// production performance. Higher-ROI fixes (cache shardsExist, scale
// indexParallelism) ship first; the bench earns its keep once a real
// regression suspect needs adjudication.
//
// corpusPool drives a bounded set of corpus indexers and exposes Enqueue so
// callers AND in-flight workers can register new plans (the dynamic-discovery
// path used by the folder walker). The pool's lifecycle is one invocation of
// runCorpusPoolWith — no state survives between searches.
//
// Concurrency invariants:
//   - Enqueue registers the goroutine with errgroup immediately; workerSlots
//     gates execution without blocking the discovering worker inside g.Go.
//   - resultsCh is drained concurrently with workers, so result sends do not
//     depend on knowing the full discovered-corpus count upfront.
type corpusPool struct {
	g           *errgroup.Group
	gctx        context.Context
	resultsCh   chan corpusPoolResult
	worker      corpusWorkerFunc
	workerSlots chan struct{}
	// seen dedupes by deterministic corpusID. The same physical repo
	// reached via multiple symlinks or via both an explicit operand
	// and walker discovery would otherwise be indexed twice. Empty-
	// struct value keeps LoadOrStore allocation-free on hit.
	seen sync.Map // map[corpusID]struct{}
}

// Enqueue registers one plan with the pool. Safe to call from the seeding
// goroutine and from in-flight workers. runPlan's workerSlots semaphore gates
// concurrent execution.
//
// Returns true if the plan was newly accepted, false if a plan with
// the same corpusID has already been enqueued (dedup hit). The boolean
// lets discovery callers report a more accurate accounting of how many
// unique nested repos were found.
func (p *corpusPool) Enqueue(plan corpusPlan) bool {
	if _, loaded := p.seen.LoadOrStore(plan.id, struct{}{}); loaded {
		return false
	}
	// Folder plans get the dynamic-discovery callback wired in so the
	// walker can enqueue nested git boundaries it finds.
	if plan.kind == corpusKindFolder {
		plan.discover = p.discoverNestedGit
	}
	p.g.Go(p.runPlan(plan))
	return true
}

func (p *corpusPool) runPlan(plan corpusPlan) func() (err error) {
	return func() (err error) {
		select {
		case p.workerSlots <- struct{}{}:
			defer func() { <-p.workerSlots }()
		case <-p.gctx.Done():
			return autoDiscoverySwallow(plan, "failed", p.gctx.Err())
		}
		planCtx, cancelPlan := context.WithCancel(p.gctx)
		defer cancelPlan()
		defer func() {
			if r := recover(); r != nil {
				err = autoDiscoverySwallow(plan, "panic",
					fmt.Errorf("corpus worker panic: %v\n%s", r, debug.Stack()))
			}
		}()
		results, dirty, werr := p.worker(planCtx, plan)
		if werr != nil {
			return autoDiscoverySwallow(plan, "failed", werr)
		}
		p.resultsCh <- corpusPoolResult{planID: plan.id, results: results, dirty: dirty}
		return nil
	}
}

// autoDiscoverySwallow encodes the worker error-vs-panic policy in one
// place: errors and panics from walker-discovered plans (userExplicit
// false) get logged at Warn and converted to nil so the pool keeps
// running the seeded user-explicit plans; the same outcomes from
// user-explicit plans propagate to errgroup so first-error-wins
// cancellation tears down the rest. `kind` distinguishes the log
// message ("auto-discovered corpus failed; skipping" vs "panic;
// skipping"); err carries the underlying value.
func autoDiscoverySwallow(plan corpusPlan, kind string, err error) error {
	if plan.userExplicit {
		return err
	}
	slog.Warn("auto-discovered corpus "+kind+"; skipping",
		"root", plan.root, "corpus_id", plan.id, "error", err)
	return nil
}

// discoverNestedGit is the callback installed on folder plans'
// `discover` field. The walker calls it when detectGitBoundary
// confirms a nested git repo and uses the bool to decide whether to
// SUPPRESS DESCENT (true: content is covered by some pool corpus)
// or DESCEND AS PLAIN FOLDER (false: content is NOT covered).
//
// Return contract:
//   - true on fresh accept: a new corpus was enqueued and owns the subtree.
//   - true on dedup hit: another worker (typically the previous
//     fingerprint pass during the same ensureFolderCorpusFresh)
//     already enqueued the same physical repo. Content IS covered;
//     walker must NOT descend, otherwise the nested working tree
//     gets double-indexed under the parent folder corpus and
//     leaks gitignored content (e.g. .venv, node_modules).
//   - false on plan build failure: enqueue is impossible; same
//     fall-through behavior as any other unowned boundary.
func (p *corpusPool) discoverNestedGit(b gitBoundary) bool {
	return p.discoverNestedGitPaths(b.toGitPaths())
}

func (p *corpusPool) discoverNestedGitPaths(paths gitPaths) bool {
	plans, err := p.planDiscoveredGitSubtree(paths)
	if err != nil {
		slog.Debug("planDiscoveredGitSubtree failed; skipping",
			"root", paths.RepoDir, "error", err)
		return false
	}
	return p.enqueueDiscoveredSubtree(plans)
}

func (p *corpusPool) planDiscoveredGitSubtree(paths gitPaths) ([]corpusPlan, error) {
	plans := make([]corpusPlan, 0, 1)
	plan, err := planDiscoveredGitPaths(paths)
	if err != nil {
		return nil, err
	}
	plans = append(plans, plan)

	groups := make(map[string]*externalGitRoot)
	addVisibleNestedGitOperands(p.gctx, groups, paths, canonicalCorpusPath(paths.RepoDir))
	for _, child := range sortedExternalGitRoots(groups) {
		if canonicalCorpusPath(child.paths.RepoDir) == canonicalCorpusPath(paths.RepoDir) {
			continue
		}
		plan, err := planDiscoveredGitPaths(child.paths)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func (p *corpusPool) enqueueDiscoveredSubtree(plans []corpusPlan) bool {
	if len(plans) == 0 {
		return false
	}
	for _, plan := range plans {
		if _, loaded := p.seen.LoadOrStore(plan.id, struct{}{}); loaded {
			continue
		}
		if plan.kind == corpusKindFolder {
			plan.discover = p.discoverNestedGit
		}
		p.g.Go(p.runPlan(plan))
	}
	// Fresh accepts and dedup hits both mean the boundary is owned by a pool
	// corpus, so the folder walker must suppress descent.
	return true
}

// runCorpusPool replaces the historical serial corpus loop with a bounded
// worker pool. A semaphore gates at most corpusWorkerCap active workers while
// errgroup still provides first-error-wins cancellation.
//
// The worker callback runs one plan and returns its result. Production
// callers build a closure capturing the gitPaths + userQ to invoke
// prepareAndSearchCorpus; tests pass alternate workers to exercise
// panic / cancellation / dedup scenarios without depending on the
// indexer.
//
// Result merge is order-independent because formatCorpusResultsWithContext
// sorts by (score desc, file name, displayRoot, corpusID) — input slice
// order doesn't influence the final output. Each plan's dirtyFileSet is
// keyed by planID so out-of-order completion is also safe for the
// dirty-files merge.
//
// Panic isolation: each worker goroutine wraps its body in defer-recover
// so a panic in one corpus's pipeline becomes a wrapped error rather
// than crashing the process. A single bad nested-git corpus surfaced
// by dynamic discovery must not poison the whole search invocation.
func runCorpusPool(
	ctx context.Context,
	plans []corpusPlan,
	worker corpusWorkerFunc,
) ([]corpusSearchResult, dirtyFilesByCorpus, error) {
	g, gctx := errgroup.WithContext(ctx)
	pool := &corpusPool{
		g:           g,
		gctx:        gctx,
		resultsCh:   make(chan corpusPoolResult, corpusWorkerCap),
		worker:      worker,
		workerSlots: make(chan struct{}, corpusWorkerCap),
	}

	var allResults []corpusSearchResult
	dirtyByCorpus := make(dirtyFilesByCorpus)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for r := range pool.resultsCh {
			if len(r.dirty) > 0 {
				dirtyByCorpus[r.planID] = r.dirty
			}
			allResults = append(allResults, r.results...)
		}
	}()

	for _, plan := range plans {
		pool.Enqueue(plan)
	}
	err := g.Wait()
	close(pool.resultsCh)
	<-drained
	if err != nil {
		return nil, nil, err
	}
	return allResults, dirtyByCorpus, nil
}
