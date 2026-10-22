package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

type cancellationTestModel struct {
	fixedRerankScorer
	failureStage string
	fail         func() error
	prepareCalls atomic.Int32
	closeCalls   atomic.Int32
}

func (model *cancellationTestModel) PrepareSemanticQuery(
	ctx context.Context,
	text string,
) (*semanticQueryEmbedding, error) {
	model.prepareCalls.Add(1)
	if model.failureStage == "prepare" {
		return nil, model.fail()
	}
	return model.fixedRerankScorer.PrepareSemanticQuery(ctx, text)
}

func (model *cancellationTestModel) ScoresWithSemanticQuery(
	context.Context,
	*semanticQueryEmbedding,
	[]rerankDocument,
) ([]float32, error) {
	model.scoreCalls.Add(1)
	return nil, model.fail()
}

func (model *cancellationTestModel) Close() error {
	model.closeCalls.Add(1)
	return nil
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
				paths := []string{strict, relaxed}
				run := func(ctx context.Context, config searchRunConfig) (string, error) {
					return captureStdout(t, func() error {
						return runSearchCommand(ctx, "alpha beta", paths, 0, 0, defaultSearchConfig(), config)
					})
				}
				baseline, err := run(t.Context(), searchRunConfig{})
				if err != nil || !strings.Contains(baseline, "strict.go") || strings.Contains(baseline, "relaxed.go") {
					t.Fatalf("strict baseline: output=%q error=%v", baseline, err)
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				model := &cancellationTestModel{
					failureStage: stage,
					fail: func() error {
						if cancelled {
							cancel()
						}
						return errors.New("fake model failure")
					},
				}
				output, err := run(ctx, searchRunConfig{
					policy: defaultSearchPolicy(),
					newModel: func(context.Context) (semanticModel, error) {
						return model, nil
					},
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

func TestSearchLoadedShardsJoinsWorkersBeforeCancellation(t *testing.T) {
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
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("second shard did not start")
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first shard did not cancel the search")
	}
	select {
	case got := <-done:
		t.Fatalf("search returned before the worker finished: %+v", got)
	default:
	}
	if first.closeCalls.Load() != 0 || second.closeCalls.Load() != 0 {
		t.Error("searchers closed before all workers finished")
	}
	releaseWorker()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || len(got.files) != 0 {
			t.Errorf("cancelled shards: files=%v error=%v, want no files and cancellation", got.files, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search did not return after all workers finished")
	}
	if first.closeCalls.Load() != 0 || second.closeCalls.Load() != 0 {
		t.Errorf("close calls=%d/%d, want 0/0: the caller closes the shards", first.closeCalls.Load(), second.closeCalls.Load())
	}
}
