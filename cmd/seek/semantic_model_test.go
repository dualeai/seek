package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
)

// TestSemanticCPUParallelismProvidesCapacityForEffectiveCPU checks
// semanticModelCallLimit on 1 to 256 CPUs. An accelerator gets one call per
// CPU. CPU sessions get enough calls to use every CPU, with less than one
// session of threads to spare.
func TestSemanticCPUParallelismProvidesCapacityForEffectiveCPU(t *testing.T) {
	t.Parallel()
	previousThreads := 0
	for cpus := 1; cpus <= 256; cpus++ {
		if calls := semanticModelCallLimit(cpus, 0); calls != cpus {
			t.Fatalf("cpu count %d: accelerator call limit=%d", cpus, calls)
		}
		threads := lateOnCPUIntraOpThreads(cpus)
		calls := semanticModelCallLimit(cpus, threads)
		if threads < 1 || threads > cpus || threads < previousThreads {
			t.Fatalf("cpu count %d: intra-op threads=%d after %d", cpus, threads, previousThreads)
		}
		if calls < 1 || calls > cpus {
			t.Fatalf("cpu count %d: call limit=%d", cpus, calls)
		}
		capacity := calls * threads
		if capacity < cpus || capacity >= cpus+threads {
			t.Fatalf(
				"cpu count %d: %d calls by %d threads provide capacity %d",
				cpus,
				calls,
				threads,
				capacity,
			)
		}
		previousThreads = threads
	}
}

// scoreWithTestSemanticEmbedder prepares query and scores documents with it,
// as the search pipeline does through the model future.
func scoreWithTestSemanticEmbedder(
	ctx context.Context,
	embedder semanticModel,
	query string,
	documents []rerankDocument,
) ([]float32, error) {
	prepared, err := embedder.PrepareSemanticQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	return embedder.ScoresWithSemanticQuery(ctx, prepared, documents)
}

// TestSemanticModelFutureReusesOneQueryForRetrievalAndRerank prepares one
// query from 9 callers, as the retrieval of each node and the final ranking
// do. The future starts the model once and prepares the query once. Each
// caller gets that model and the same prepared query. Close closes the model
// once.
func TestSemanticModelFutureReusesOneQueryForRetrievalAndRerank(t *testing.T) {
	scorer := observeModel(&hybridTestScorer{})
	future := newSemanticModelFuture(scorer.factory())

	const callers = 8
	prepared := make([]*semanticQueryEmbedding, callers)
	var wait sync.WaitGroup
	for caller := range callers {
		wait.Go(func() {
			model, query, err := future.prepareQuery(t.Context(), "find request handler")
			if err != nil || model != scorer {
				t.Errorf("prepare query: model=%T err=%v", model, err)
				return
			}
			prepared[caller] = query
		})
	}
	wait.Wait()

	model, last, err := future.prepareQuery(t.Context(), "find request handler")
	if err != nil || model != scorer || last.modelQuery != "find request handler" {
		t.Fatalf("prepare query: model=%T query=%+v err=%v", model, last, err)
	}
	for caller, query := range prepared {
		if query != last {
			t.Fatalf("caller %d got another prepared query", caller)
		}
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}
	if got := scorer.starts.Load(); got != 1 {
		t.Fatalf("model factory calls=%d, want 1", got)
	}
	if got := scorer.prepares.Load(); got != 1 {
		t.Fatalf("query model calls=%d, want 1", got)
	}
	if got := scorer.closes.Load(); got != 1 {
		t.Fatalf("model close calls=%d, want 1", got)
	}
}

// waitingQueryScorer is hybridTestScorer with a query preparation that waits
// for release and then returns err. A test with it proves that the
// model future waits for a started query before it closes the model. It also
// proves that the future keeps the query result for later callers. It cannot
// prove that a real model call stops when its context ends.
type waitingQueryScorer struct {
	hybridTestScorer
	release <-chan struct{}
	err     error
}

func (scorer *waitingQueryScorer) PrepareSemanticQuery(
	ctx context.Context,
	query string,
) (*semanticQueryEmbedding, error) {
	<-scorer.release
	prepared, _ := scorer.hybridTestScorer.PrepareSemanticQuery(ctx, query)
	return prepared, scorer.err
}

// TestSemanticModelFutureCloseWaitsForStartedQuery starts a query and closes
// the future before the query ends: on success, on a query failure, and on a
// cancelled context. Close waits for the query, then closes the model once.
// A later caller gets the stored query result.
func TestSemanticModelFutureCloseWaitsForStartedQuery(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		cancel bool
	}{
		{name: "success"},
		{name: "query failure", err: errors.New("query failed")},
		{name: "cancelled native work", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				release := make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				scorer := observeModel(&waitingQueryScorer{release: release, err: test.err})
				future := newSemanticModelFuture(scorer.factory())
				// Close right after startQuery: it must already see the query
				// as started and wait for it.
				future.startQuery(ctx, "find request handler")
				closed := make(chan error, 1)
				go func() { closed <- future.Close() }()
				synctest.Wait()
				if test.cancel {
					cancel()
					synctest.Wait()
				}
				if scorer.closes.Load() != 0 {
					t.Fatal("model closed while the query still used it")
				}
				close(release)
				synctest.Wait()
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
				if scorer.prepares.Load() != 1 || scorer.closes.Load() != 1 {
					t.Fatalf("calls prepare=%d close=%d", scorer.prepares.Load(), scorer.closes.Load())
				}
				// A later caller gets the stored query result.
				if _, _, err := future.prepareQuery(t.Context(), "find request handler"); !errors.Is(err, test.err) {
					t.Fatalf("query error=%v, want %v", err, test.err)
				}
			})
		})
	}
}

func TestSemanticModelFutureCloseAfterFactoryFailure(t *testing.T) {
	want := errors.New("model setup failed")
	future := newSemanticModelFuture(func(context.Context) (semanticModel, error) {
		return nil, want
	})
	future.startQuery(t.Context(), "find request handler")
	if _, _, err := future.prepareQuery(t.Context(), "find request handler"); !errors.Is(err, want) {
		t.Fatalf("query error=%v, want %v", err, want)
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}
}
