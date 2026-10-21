package main

import (
	"context"
	"sync"
	"sync/atomic"
)

const (
	// These values are the static ONNX input and output shapes. They are model
	// format values, not worker or host resource targets.
	semanticModelBatchRows      = 128
	semanticEmbeddingDimensions = 48
)

// semanticQueryEmbedding holds the token vectors of one query model call: one
// or more 128-token query windows, row after row, so a long query keeps every
// token. The token vectors drive both USearch and LateOn MaxSim.
type semanticQueryEmbedding struct {
	tokens     []float32
	scoreMask  []bool
	modelQuery string
}

// validSemanticQuery reports a query with whole windows and a vector per mask
// slot.
func validSemanticQuery(query *semanticQueryEmbedding) bool {
	return query != nil && len(query.scoreMask) > 0 && len(query.scoreMask)%lateOnSequenceLength == 0 &&
		len(query.tokens) == len(query.scoreMask)*semanticEmbeddingDimensions
}

// semanticModel is the command-wide LateOn model used by indexing, retrieval,
// and final scoring. Implementations must permit concurrent method calls while
// the command is active. The production implementation owns one shared
// inference session. Zoekt and USearch stay outside this boundary.
type semanticModel interface {
	EmbedSemanticUnits(context.Context, []semanticUnit) ([]semanticUnitEmbedding, error)
	PrepareSemanticQuery(context.Context, string) (*semanticQueryEmbedding, error)
	ScoresWithSemanticQuery(context.Context, *semanticQueryEmbedding, []rerankDocument) ([]float32, error)
	// SemanticCallCPUs returns the estimated CPU reservation for one inference
	// call. Zero bypasses the shared CPU gate; it does not mean zero physical CPU
	// use. The call can initialize the provider. A later model call reports any
	// initialization error.
	SemanticCallCPUs(context.Context) int
	Close() error
}

type semanticModelFactory func(context.Context) (semanticModel, error)

func semanticModelCallCPUs(ctx context.Context, model semanticModel) int {
	return max(0, model.SemanticCallCPUs(ctx))
}

// semanticModelCallLimit keeps accelerator calls within the effective CPU
// count even though those calls do not reserve CPU capacity in the shared
// gate. The live memory and throughput controller can select a lower limit.
func semanticModelCallLimit(cpuLimit, callCPUs int) int {
	cpuLimit = max(1, cpuLimit)
	callCPUs = max(1, callCPUs)
	return max(1, (cpuLimit+callCPUs-1)/callCPUs)
}

type semanticModelResult struct {
	model semanticModel
	err   error
}

// semanticModelFuture owns one model session for one command. Index build,
// semantic query, and final reranking borrow the same session.
type semanticModelFuture struct {
	factory semanticModelFactory
	once    sync.Once
	started atomic.Bool
	ready   chan struct{}
	result  semanticModelResult

	queryOnce    sync.Once
	queryStarted atomic.Bool
	queryReady   chan struct{}
	queryText    string
	queryResult  semanticQueryResult
}

func newSemanticModelFuture(factory semanticModelFactory) *semanticModelFuture {
	return &semanticModelFuture{
		factory:    factory,
		ready:      make(chan struct{}),
		queryReady: make(chan struct{}),
	}
}

type semanticQueryResult struct {
	model semanticModel
	query *semanticQueryEmbedding
	err   error
}

func (future *semanticModelFuture) start(ctx context.Context) {
	if future == nil || future.factory == nil {
		return
	}
	future.once.Do(func() {
		future.started.Store(true)
		go func() {
			future.result.model, future.result.err = future.factory(ctx)
			close(future.ready)
		}()
	})
}

func (future *semanticModelFuture) model(ctx context.Context) (semanticModel, error) {
	if future == nil || future.factory == nil {
		return nil, errRerankUnavailable
	}
	future.start(ctx)
	select {
	case <-future.ready:
		return future.result.model, future.result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// startQuery records ownership before returning so Close waits for query work
// even when index preparation returns before the query goroutine starts.
func (future *semanticModelFuture) startQuery(ctx context.Context, query string) {
	if future == nil || query == "" {
		return
	}
	future.queryOnce.Do(func() {
		future.queryText = query
		future.queryStarted.Store(true)
		go func() {
			defer close(future.queryReady)
			model, err := future.model(ctx)
			if err != nil {
				future.queryResult.err = err
				return
			}
			prepared, err := model.PrepareSemanticQuery(ctx, query)
			future.queryResult = semanticQueryResult{
				model: model,
				query: prepared,
				err:   err,
			}
		}()
	})
}

func (future *semanticModelFuture) prepareQuery(
	ctx context.Context,
	query string,
) (semanticModel, *semanticQueryEmbedding, error) {
	if future == nil || query == "" {
		return nil, nil, errRerankUnavailable
	}
	future.startQuery(ctx, query)
	if future.queryText != query {
		return nil, nil, errRerankUnavailable
	}
	select {
	case <-future.queryReady:
		return future.queryResult.model, future.queryResult.query, future.queryResult.err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (future *semanticModelFuture) Close() error {
	if future == nil {
		return nil
	}
	if future.queryStarted.Load() {
		<-future.queryReady
	}
	if !future.started.Load() {
		return nil
	}
	<-future.ready
	if future.result.model == nil {
		return nil
	}
	return future.result.model.Close()
}

type searchExecution struct {
	policy    searchPolicy
	model     *semanticModelFuture
	resources searchResources
	// vectors reports that the command's model stages read vectors: every
	// node builds and binds its generation before its read. A Git node with
	// no commit has no base to build; the overlay adds its files.
	vectors bool
	// opened holds the command's node preparations (openNode). Nil prepares
	// on every call.
	opened *openedNodes
	// operands give the display paths that headers and file: filters read.
	// With nil, file: filters read node-relative paths and headers show
	// absolute paths.
	operands displayOperands
}

// buildsVectors reports whether a node with the commit head builds and binds a
// vector generation: the model stages run, the policy allows semantic search,
// a model exists, and the node has a commit (an empty folder head counts).
func (e searchExecution) buildsVectors(head string) bool {
	return e.vectors && e.policy.semanticEnabled() && e.model != nil && head != "no-head"
}

// buildsOverlay reports whether a Git node with dirty files builds and binds
// the overlay of their work-tree bytes: every file of the node is then in
// vector retrieval, committed or not, with or without a commit.
func (e searchExecution) buildsOverlay(dirty bool) bool {
	return e.vectors && e.policy.semanticEnabled() && e.model != nil && dirty
}
