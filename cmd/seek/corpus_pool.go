package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
)

// corpusPoolResult is one corpus worker's contribution to the merged
// result set. Sent through a buffered channel so the main goroutine can
// drain after errgroup.Wait without ordering constraints on emission.
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
// production performance. Higher-ROI fixes (memoize isOnNFS, cache
// shardsExist, scale indexParallelism) ship first; the bench earns
// its keep once a real regression suspect needs adjudication.
//
// corpusPool drives the bounded errgroup of corpus indexers and exposes
// Enqueue so callers AND in-flight workers can register new plans (the
// dynamic-discovery path used by the folder walker). The pool's
// lifecycle is one invocation of runCorpusPoolWith — no state survives
// between searches.
//
// Concurrency invariants:
//   - Every g.Go happens-before g.Wait returns IFF the spawning
//     goroutine calls Enqueue before returning to errgroup. Dynamic
//     enqueue from a worker body satisfies this.
//   - resultsCh capacity = len(seeds) + maxDiscoveredCorpora so a
//     worker never blocks on send even at the pathological cap of
//     walker-discovered corpora.
type corpusPool struct {
	g         *errgroup.Group
	gctx      context.Context
	resultsCh chan corpusPoolResult
	worker    corpusWorkerFunc
	// seen dedupes by deterministic corpusID. The same physical repo
	// reached via multiple symlinks or via both an explicit operand
	// and walker discovery would otherwise be indexed twice. Empty-
	// struct value keeps LoadOrStore allocation-free on hit.
	seen sync.Map // map[corpusID]struct{}
	// discoveredCount tracks how many UNIQUE walker-discovered corpora
	// have been accepted; bounded by maxDiscoveredCorpora. Cap is
	// checked lock-free so it can be raced slightly past the limit
	// under heavy concurrent discovery (bounded to cap + N concurrent
	// walkers), which is acceptable for opportunistic auto-indexing.
	discoveredCount atomic.Int32
	// capWarned ensures the "discovery cap reached" Warn fires at most
	// once per pool lifetime. A noisy log on every rejected boundary in
	// a monorepo with 100+ submodules would flood stderr.
	capWarned sync.Once
}

// Enqueue registers one plan with the pool. Safe to call from the
// seeding goroutine OR from a worker discovering a nested corpus. The
// underlying errgroup's SetLimit gates concurrent execution.
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
	p.g.Go(func() (err error) {
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
	})
	return true
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
//   - true on fresh accept: a new corpus was enqueued and the cap
//     counter incremented.
//   - true on dedup hit: another worker (typically the previous
//     fingerprint pass during the same ensureFolderCorpusFresh)
//     already enqueued the same physical repo. Content IS covered;
//     walker must NOT descend, otherwise the nested working tree
//     gets double-indexed under the parent folder corpus and
//     leaks gitignored content (e.g. .venv, node_modules).
//   - false on cap exhaustion: cap is full and the boundary cannot
//     be enqueued; walker falls through to plain-folder descent so
//     the subtree's content is not dropped.
//   - false on plan build failure: enqueue is impossible; same
//     fall-through as cap.
//
// Cap is intentionally a soft pre-check: under concurrent discovery
// from multiple walkers, the count can race slightly past
// maxDiscoveredCorpora; the overshoot is bounded by the number of
// concurrent walkers (≤ corpusWorkerCap), which is acceptable for
// opportunistic auto-indexing — the alternative (mutex-guarded
// check-then-increment) would serialize walker callbacks.
func (p *corpusPool) discoverNestedGit(b gitBoundary) bool {
	plan, err := planDiscoveredGitCorpus(b)
	if err != nil {
		slog.Debug("planDiscoveredGitCorpus failed; skipping",
			"root", b.RepoDir, "error", err)
		return false
	}
	if _, loaded := p.seen.Load(plan.id); loaded {
		return true
	}
	if p.discoveredCount.Load() >= maxDiscoveredCorpora {
		if _, loaded := p.seen.Load(plan.id); loaded {
			return true
		}
		// One default-visible Warn per query; per-boundary Debug logs
		// surface individual rejections under -v / SEEK_DEBUG.
		p.capWarned.Do(func() {
			slog.Warn("nested-git discovery cap reached; subsequent nested repos will index under their containing folder corpus rather than as standalone git corpora",
				"cap", maxDiscoveredCorpora,
				"hint", "narrow the search root, or open an issue if your monorepo has > "+strconv.Itoa(maxDiscoveredCorpora)+" submodules")
		})
		slog.Debug("nested git discovery cap reached; skipping",
			"cap", maxDiscoveredCorpora, "root", b.RepoDir)
		return false
	}
	if p.Enqueue(plan) {
		// Newly accepted — count toward the cap.
		p.discoveredCount.Add(1)
		return true
	}
	// Dedup hit: this boundary's content is covered by an already-
	// enqueued corpus (same dev:ino — typically the previous
	// fingerprint pass during the same ensureFolderCorpusFresh, or a
	// repo reachable via two paths). Return true so the walker still
	// SUPPRESSES descent; descending would double-index the nested
	// repo's working tree under the parent folder corpus and leak
	// gitignored content (e.g. .venv, node_modules).
	return true
}

// runCorpusPool replaces the historical serial corpus loop with a
// bounded errgroup-driven worker pool. errgroup's SetLimit gates at
// most corpusWorkerCap concurrent workers; first-error-wins semantics
// and per-plan cancellation are preserved.
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
	g.SetLimit(corpusWorkerCap)
	pool := &corpusPool{
		g:    g,
		gctx: gctx,
		// Capacity covers seeded plans + the cap on walker-discovered
		// plans. A goroutine never blocks on this channel because the
		// drain happens after g.Wait, so over-provisioning is harmless.
		resultsCh: make(chan corpusPoolResult, len(plans)+maxDiscoveredCorpora),
		worker:    worker,
	}
	for _, plan := range plans {
		pool.Enqueue(plan)
	}
	err := g.Wait()
	close(pool.resultsCh)
	if err != nil {
		return nil, nil, err
	}

	var allResults []corpusSearchResult
	dirtyByCorpus := make(dirtyFilesByCorpus)
	for r := range pool.resultsCh {
		if len(r.dirty) > 0 {
			dirtyByCorpus[r.planID] = r.dirty
		}
		allResults = append(allResults, r.results...)
	}
	return allResults, dirtyByCorpus, nil
}
