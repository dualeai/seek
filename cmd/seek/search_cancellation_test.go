package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// cancellationTestModel is the test model with a build that fails
// (errRerankUnavailable), so no vector exists and only strict hits can print.
// Its query preparation or its scoring, as failureStage says, calls fail and
// returns that error, so a test can cancel the command inside the model call.
// A test with it proves the route after a model failure: the strict fallback
// for an ordinary error, no output and the cancellation for a cancelled
// command. It cannot prove that a cancellation stops a real model call in
// time, or anything about vectors, meaning or rank.
type cancellationTestModel struct {
	hybridTestScorer
	failureStage string
	fail         func() error
}

func (model *cancellationTestModel) PrepareSemanticQuery(
	ctx context.Context,
	text string,
) (*semanticQueryEmbedding, error) {
	if model.failureStage == "prepare" {
		model.prepareCalls.Add(1)
		return nil, model.fail()
	}
	return model.hybridTestScorer.PrepareSemanticQuery(ctx, text)
}

func (model *cancellationTestModel) ScoresWithSemanticQuery(
	context.Context,
	*semanticQueryEmbedding,
	[]rerankDocument,
) ([]float32, error) {
	model.scoreCalls.Add(1)
	return nil, model.fail()
}

func TestRunSearchCommandLexicalModelCancellation(t *testing.T) {
	for _, stage := range []string{"prepare", "score"} {
		for _, cancelled := range []bool{false, true} {
			name := stage + "/ordinary_error"
			if cancelled {
				name = stage + "/cancelled"
			}
			t.Run(name, func(t *testing.T) {
				requireTools(t)
				setTestUserCache(t)
				t.Chdir(t.TempDir())
				strict, relaxed := t.TempDir(), t.TempDir()
				writeFileAt(t, strict, "strict.go", "package sample\n// alpha beta\n")
				writeFileAt(t, relaxed, "relaxed.go", "package sample\n// alpha only\n")
				baseline, err := seek(t, "--lexical-only", "alpha beta", strict, relaxed)
				if err != nil || !strings.Contains(baseline, "strict.go") || strings.Contains(baseline, "relaxed.go") {
					t.Fatalf("strict baseline: output=%q error=%v", baseline, err)
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				model := &cancellationTestModel{
					hybridTestScorer: hybridTestScorer{embedErr: errRerankUnavailable},
					failureStage:     stage,
					fail: func() error {
						if cancelled {
							cancel()
						}
						return errors.New("fake model failure")
					},
				}
				output, err := captureStdout(t, func() error {
					return seekRoot(ctx, t, modelFactory(model), "alpha beta", strict, relaxed)
				})
				// The output shows the route: a cancelled command prints
				// nothing and returns the cancellation; an ordinary model
				// failure falls back to the strict lexical result.
				if cancelled {
					if !errors.Is(err, context.Canceled) || output != "" {
						t.Errorf("cancelled command: output=%q error=%v, want no output and cancellation", output, err)
					}
				} else if err != nil || output != baseline {
					t.Errorf("ordinary fallback: output=%q error=%v, want %q and no error", output, err, baseline)
				}
				wantScores := int32(0)
				if stage == "score" {
					wantScores = 1
				}
				if model.prepareCalls.Load() != 1 || model.scoreCalls.Load() != wantScores || model.closeCalls.Load() != 1 {
					t.Errorf("model calls: prepare=%d score=%d close=%d, want 1/%d/1",
						model.prepareCalls.Load(), model.scoreCalls.Load(), model.closeCalls.Load(), wantScores)
				}
			})
		}
	}
}

type cancellationTestShard struct {
	zoekt.Searcher
	search     func(context.Context) (*zoekt.SearchResult, error)
	closeCalls atomic.Int32
}

func (shard *cancellationTestShard) Search(
	ctx context.Context,
	_ query.Q,
	_ *zoekt.SearchOptions,
) (*zoekt.SearchResult, error) {
	return shard.search(ctx)
}

func (shard *cancellationTestShard) Close() {
	shard.closeCalls.Add(1)
}

func TestSearchLoadedShardsRejectsCancelledPartialResult(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	shard := &cancellationTestShard{search: func(context.Context) (*zoekt.SearchResult, error) {
		cancel()
		return &zoekt.SearchResult{Files: []zoekt.FileMatch{{FileName: "partial.go"}}}, nil
	}}
	files, err := searchLoadedShards(ctx, []zoekt.Searcher{shard}, &query.Const{Value: true}, defaultSearchConfig(), nil)
	if !errors.Is(err, context.Canceled) || len(files) != 0 {
		t.Errorf("cancelled shard: files=%v error=%v, want no files and cancellation", files, err)
	}
	if shard.closeCalls.Load() != 0 {
		t.Errorf("close calls=%d, want 0: the caller closes the shards", shard.closeCalls.Load())
	}
}

// TestSearchLoadedShardsJoinsWorkersBeforeCancellation cancels the search in
// one shard while another shard still runs. The test runs in a synctest
// bubble, where synctest.Wait returns only when every other goroutine is
// blocked. So when synctest.Wait returns, a search that does not wait for its
// last worker has already returned.
func TestSearchLoadedShardsJoinsWorkersBeforeCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		first := &cancellationTestShard{search: func(context.Context) (*zoekt.SearchResult, error) {
			cancel()
			return &zoekt.SearchResult{Files: []zoekt.FileMatch{{FileName: "first.go"}}}, nil
		}}
		started, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseWorker)
		second := &cancellationTestShard{search: func(context.Context) (*zoekt.SearchResult, error) {
			close(started)
			<-release
			return &zoekt.SearchResult{Files: []zoekt.FileMatch{{FileName: "second.go"}}}, nil
		}}
		type result struct {
			files []zoekt.FileMatch
			err   error
		}
		done := make(chan result, 1)
		go func() {
			files, err := searchLoadedShards(ctx, []zoekt.Searcher{first, second}, &query.Const{Value: true}, defaultSearchConfig(), nil)
			done <- result{files: files, err: err}
		}()
		<-started
		<-ctx.Done()
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("search returned before the worker finished: %+v", got)
		default:
		}
		if first.closeCalls.Load() != 0 || second.closeCalls.Load() != 0 {
			t.Error("searchers closed before all workers finished")
		}
		releaseWorker()
		if got := <-done; !errors.Is(got.err, context.Canceled) || len(got.files) != 0 {
			t.Errorf("cancelled shards: files=%v error=%v, want no files and cancellation", got.files, got.err)
		}
		if first.closeCalls.Load() != 0 || second.closeCalls.Load() != 0 {
			t.Errorf("close calls=%d/%d, want 0/0: the caller closes the shards", first.closeCalls.Load(), second.closeCalls.Load())
		}
	})
}
