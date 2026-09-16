package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
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

type countingSemanticScorer struct {
	prepareCalls   atomic.Int32
	scoreCalls     atomic.Int32
	closeCalls     atomic.Int32
	scoreMask      []bool
	queryTruncated bool
}

func (*countingSemanticScorer) PackSemanticUnits(
	_ context.Context,
	units []semanticUnit,
) ([]semanticUnit, error) {
	return units, nil
}

func (scorer *countingSemanticScorer) Scores(
	ctx context.Context,
	query string,
	documents []rerankDocument,
) ([]float32, error) {
	return scoreWithTestSemanticEmbedder(ctx, scorer, query, documents)
}

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
		truncated:  scorer.queryTruncated,
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
		scoreMask:      []bool{true, false, true},
		queryTruncated: true,
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

	batch, err := future.scores(
		t.Context(),
		"find request handler",
		[]rerankDocument{{Text: "handler"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if batch.activeQueryTokens != 2 || !batch.queryTruncated || len(batch.values) != 1 {
		t.Fatalf("score metadata=%+v", batch)
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
