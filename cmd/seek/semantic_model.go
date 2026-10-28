package main

import (
	"context"
	"errors"
	"log/slog"
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
// or more 128-token query windows, row after row. So a long query keeps every
// token, in up to semanticModelBatchRows - 1 windows, because the last row of
// the call holds the empty document. The token vectors drive both USearch and
// LateOn MaxSim.
type semanticQueryEmbedding struct {
	tokens    []float32
	scoreMask []bool
	// emptyScore is the raw MaxSim score of the query against a document with
	// no text: the document prefix and the separator only. Every document row
	// starts with this prefix and ends with this separator. So the value is
	// about the part of a score that the query gets from any document,
	// whatever its text.
	emptyScore float32
}

// validSemanticQuery reports a query with whole windows and a vector per mask
// slot.
func validSemanticQuery(query *semanticQueryEmbedding) bool {
	return query != nil && len(query.scoreMask) > 0 && len(query.scoreMask)%lateOnSequenceLength == 0 &&
		len(query.tokens) == len(query.scoreMask)*semanticEmbeddingDimensions
}

// semanticModel is the command-wide LateOn model used by indexing, retrieval,
// and final scoring. Implementations must permit concurrent method calls while
// the command is active. The production implementation shares one inference
// session, plus a CPU session for the index build when the first one is sized
// for one call. Zoekt and USearch stay outside this boundary.
type semanticModel interface {
	EmbedSemanticUnits(context.Context, []semanticUnit) ([]semanticUnitEmbedding, error)
	PrepareSemanticQuery(context.Context, string) (*semanticQueryEmbedding, error)
	ScoresWithSemanticQuery(context.Context, *semanticQueryEmbedding, []rerankDocument) ([]float32, error)
	// SemanticCallCPUs returns the estimated CPU reservation for one
	// inference call. Zero bypasses the shared CPU gate; it does not mean zero
	// physical CPU use. The call can initialize the provider. A later model
	// call reports any initialization error.
	SemanticCallCPUs(context.Context) int
	Close() error
}

type semanticModelFactory func(context.Context) (semanticModel, error)

// semanticModelCallLimit gives the number of model calls that can run at the
// same time: cpuLimit divided by the CPUs of one call, rounded up. An
// accelerated call (callCPUs 0) reserves no CPU in the shared gate, so the
// limit keeps such calls within the effective CPU count. The live memory and
// throughput controller can select a lower limit.
func semanticModelCallLimit(cpuLimit, callCPUs int) int {
	cpuLimit = max(1, cpuLimit)
	callCPUs = max(1, callCPUs)
	return max(1, (cpuLimit+callCPUs-1)/callCPUs)
}

type semanticModelResult struct {
	model semanticModel
	err   error
}

// semanticModelFuture owns the model of one command. Index build, semantic
// query, and final reranking borrow the same model. runSearchCommand makes the
// future only when the model stages run, always with a factory, and the model
// stages are its only users. So no method accepts a nil future or factory.
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

	faultOnce sync.Once
}

// recordFault records the first provider fault of the command's model and
// ignores later ones. One bad query fails the vector build, the scans, and the
// ranking on the same output. Each record counts toward a demotion of the
// provider, so one command records one fault. An error that is not a provider
// fault records nothing, so a later provider fault is still recorded. A search
// on a built index is the common path. Without this record there, a wrong
// provider would drop every search to text ranking and never record itself.
func (future *semanticModelFuture) recordFault(err error) {
	if !errors.Is(err, errDegenerateSemanticVector) {
		return
	}
	future.faultOnce.Do(func() { rejectLateOnAcceleratedProvider(err.Error()) })
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
	future.once.Do(func() {
		future.started.Store(true)
		go func() {
			future.result.model, future.result.err = future.factory(ctx)
			close(future.ready)
		}()
	})
}

func (future *semanticModelFuture) model(ctx context.Context) (semanticModel, error) {
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
	if query == "" {
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
	if query == "" {
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
	// policy is the search policy of the command. The Git build reads it: a
	// semanticSearch command drops the joined descriptor before a family
	// changes (decideGitBuild).
	policy searchPolicy
	// model is the command's model. It is set only when the model stages run
	// (a query that the semanticSearch policy plans with model text), and then
	// every node builds and binds its vector generation before its read. A Git
	// node with no commit has no base to build; the overlay adds its files.
	model     *semanticModelFuture
	resources searchResources
	// opened records the nodes that the command opened (openNode), for the
	// not-indexed check. Nil records nothing.
	opened *openedNodes
	// operands give the display paths that headers and file: filters read.
	// With no operand, file: filters read node-relative paths and headers
	// show absolute paths.
	operands displayOperands
}

// buildsVectors reports whether a node with the commit head builds and binds a
// vector generation. It does when the model stages run and head is not
// gitNoHead. A folder node passes an empty head.
func (e searchExecution) buildsVectors(head string) bool {
	return e.model != nil && head != gitNoHead
}

// buildsOverlay reports whether a Git node with dirty files builds and binds
// the overlay of their work-tree bytes. Then every file of the node is in
// vector retrieval, committed or not, with or without a commit.
func (e searchExecution) buildsOverlay(dirty bool) bool {
	return e.model != nil && dirty
}

// semanticBuildFailed logs a failed vector build that keeps the text index,
// and records a provider that produced degenerate output, once per command.
func semanticBuildFailed(model *semanticModelFuture, message string, err error) {
	slog.Debug(message, "provider", semanticProviderName(), "error", err)
	model.recordFault(err)
}
