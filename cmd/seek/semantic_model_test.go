package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

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

// countingSemanticScorer is a model that counts its query, score and close
// calls. Its build gives no vectors, its query holds only the score mask of
// the test, and every score is 0. A test with it proves how often the model
// future starts the model, prepares the query, scores and closes. It cannot
// prove anything about vectors, scores or what a search prints.
type countingSemanticScorer struct {
	prepareCalls atomic.Int32
	scoreCalls   atomic.Int32
	closeCalls   atomic.Int32
	scoreMask    []bool
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

func (*countingSemanticScorer) EmbedSemanticUnits(
	context.Context,
	[]semanticUnit,
) ([]semanticUnitEmbedding, error) {
	return nil, nil
}

func (scorer *countingSemanticScorer) PrepareSemanticQuery(
	_ context.Context,
	query string,
) (*semanticQueryEmbedding, error) {
	scorer.prepareCalls.Add(1)
	scoreMask := append([]bool(nil), scorer.scoreMask...)
	if len(scoreMask) == 0 {
		scoreMask = []bool{true}
	}
	return &semanticQueryEmbedding{
		modelQuery: query,
		scoreMask:  scoreMask,
	}, nil
}

func (scorer *countingSemanticScorer) ScoresWithSemanticQuery(
	_ context.Context,
	_ *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	scorer.scoreCalls.Add(1)
	return make([]float32, len(documents)), nil
}

func (scorer *countingSemanticScorer) Close() error {
	scorer.closeCalls.Add(1)
	return nil
}

func (*countingSemanticScorer) SemanticCallCPUs(context.Context) int { return 1 }

func TestSemanticModelFutureReusesOneQueryForRetrievalAndRerank(t *testing.T) {
	scorer := &countingSemanticScorer{
		scoreMask: []bool{true, false, true},
	}
	var factoryCalls atomic.Int32
	future := newSemanticModelFuture(func(context.Context) (semanticModel, error) {
		factoryCalls.Add(1)
		return scorer, nil
	})

	const callers = 8
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, prepared, err := future.prepareQuery(t.Context(), "find request handler")
			if err != nil {
				t.Errorf("prepare query: %v", err)
				return
			}
			if prepared.modelQuery != "find request handler" {
				t.Errorf("prepared query=%q", prepared.modelQuery)
			}
		}()
	}
	wait.Wait()

	model, prepared, err := future.prepareQuery(t.Context(), "find request handler")
	if err != nil {
		t.Fatal(err)
	}
	values, err := model.ScoresWithSemanticQuery(t.Context(), prepared, []rerankDocument{{Text: "handler"}})
	if err != nil || len(values) != 1 || len(prepared.scoreMask) != 3 {
		t.Fatalf("values=%v mask=%v err=%v", values, prepared.scoreMask, err)
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("model factory calls=%d, want 1", got)
	}
	if got := scorer.prepareCalls.Load(); got != 1 {
		t.Fatalf("query model calls=%d, want 1", got)
	}
	if got := scorer.scoreCalls.Load(); got != 1 {
		t.Fatalf("final score calls=%d, want 1", got)
	}
	if got := scorer.closeCalls.Load(); got != 1 {
		t.Fatalf("model close calls=%d, want 1", got)
	}
}

// waitingQueryScorer is countingSemanticScorer with a query preparation that
// waits for release and then returns err. A test with it proves that the
// model future waits for a started query before it closes the model, and
// keeps the query result for later callers. It cannot prove that a real model
// call stops when its context ends.
type waitingQueryScorer struct {
	countingSemanticScorer
	release <-chan struct{}
	err     error
}

func (scorer *waitingQueryScorer) PrepareSemanticQuery(
	ctx context.Context,
	query string,
) (*semanticQueryEmbedding, error) {
	<-scorer.release
	prepared, _ := scorer.countingSemanticScorer.PrepareSemanticQuery(ctx, query)
	return prepared, scorer.err
}

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
				scorer := &waitingQueryScorer{release: release, err: test.err}
				future := newSemanticModelFuture(modelFactory(scorer))
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
				if scorer.closeCalls.Load() != 0 {
					t.Fatal("model closed while the query still used it")
				}
				close(release)
				synctest.Wait()
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
				if scorer.prepareCalls.Load() != 1 || scorer.closeCalls.Load() != 1 {
					t.Fatalf("calls prepare=%d close=%d", scorer.prepareCalls.Load(), scorer.closeCalls.Load())
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
