package main

import (
	"context"
	"log/slog"
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
type corpusWorkerFunc func(ctx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error)

// corpusPool starts one errgroup task per unique plan. workerSlots limits the
// number of tasks that can call worker at once. A running folder task can add
// nested Git plans before it returns, so the group remains active while Wait
// observes dynamic work.
//
// Concurrency invariants:
//   - Enqueue registers each accepted plan with errgroup immediately;
//     workerSlots blocks inside the new task, not in the discovering task.
//   - resultsCh is drained concurrently with workers, so result sends do not
//     depend on knowing the full discovered-corpus count upfront.
//   - resultsCh closes only after g.Wait, when no producer can send again.
type corpusPool struct {
	g           *errgroup.Group
	gctx        context.Context
	resultsCh   chan corpusPoolResult
	worker      corpusWorkerFunc
	workerSlots chan struct{}
	// seen deduplicates a physical corpus reached through multiple paths.
	seen sync.Map // map[corpusID]struct{}
}

// Enqueue returns false when the pool already contains the corpus.
func (p *corpusPool) Enqueue(plan corpusPlan) bool {
	if _, loaded := p.seen.LoadOrStore(plan.id, struct{}{}); loaded {
		return false
	}
	if plan.kind == corpusKindFolder {
		plan.discover = p.discoverNestedGit
	}
	p.g.Go(p.runPlan(plan))
	return true
}

func (p *corpusPool) runPlan(plan corpusPlan) func() error {
	return func() error {
		select {
		case p.workerSlots <- struct{}{}:
			defer func() { <-p.workerSlots }()
		case <-p.gctx.Done():
			return p.handlePlanError(plan, p.gctx.Err())
		}
		results, dirty, werr := p.worker(p.gctx, plan)
		if werr != nil {
			return p.handlePlanError(plan, werr)
		}
		p.resultsCh <- corpusPoolResult{planID: plan.id, results: results, dirty: dirty}
		return nil
	}
}

// handlePlanError stops the pool for an explicit corpus. An independent
// discovered-corpus failure warns and is swallowed. Failure after group or
// parent cancellation is expected fallout, so it is logged only at debug level.
func (p *corpusPool) handlePlanError(plan corpusPlan, err error) error {
	if plan.userExplicit {
		return err
	}
	if p.gctx.Err() != nil {
		slog.Debug("auto-discovered Git repository failed after cancellation; skipping",
			"root", plan.root, "corpus_id", plan.id, "error", err)
		return nil
	}
	slog.Warn("auto-discovered Git repository failed; results may be incomplete",
		"root", plan.root)
	slog.Debug("auto-discovered Git repository failure details",
		"root", plan.root, "corpus_id", plan.id, "error", err)
	return nil
}

// discoverNestedGit returns true when a pool corpus owns the subtree. A
// duplicate also returns true because another worker already owns it.
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
		p.Enqueue(plan)
	}
	// Fresh accepts and dedup hits both mean the boundary is owned by a pool
	// corpus, so the folder walker must suppress descent.
	return true
}

// runCorpusPool runs at most corpusWorkerCap workers at once. The result order
// does not affect output because the formatter sorts the merged results.
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
	// Workers can swallow cancellation fallout. Preserve cancellation from the
	// caller even when errgroup has no remaining worker error to return.
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, nil, err
	}
	return allResults, dirtyByCorpus, nil
}
