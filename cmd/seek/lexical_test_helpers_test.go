package main

import (
	"context"
	"testing"

	"github.com/sourcegraph/zoekt/query"
)

func testLexicalSearchExecution() searchExecution {
	return searchExecution{policy: lexicalOnlySearchPolicy()}
}

// testSemanticSearchExecution offers a model to a corpus build and asks for
// vector preparation, closing the future when the test ends.
//
// vectors is set here because runSearchCommand decides it in production from
// the query, and a test that calls a corpus entry point directly bypasses
// that decision. A test that drives a whole search must not use this.
func testSemanticSearchExecution(tb testing.TB, factory semanticModelFactory) searchExecution {
	tb.Helper()
	future := newSemanticModelFuture(factory)
	tb.Cleanup(func() { _ = future.Close() })
	return searchExecution{
		policy:  defaultSearchPolicy(),
		model:   future,
		vectors: true,
	}
}

func ensureFolderCorpusFresh(ctx context.Context, plan node) (corpusIndexState, error) {
	return ensureFolderCorpusFreshWithExecution(ctx, plan, testLexicalSearchExecution())
}

func runIndexingWithCache(
	ctx context.Context,
	paths gitPaths,
	cacheDir string,
	indexDir string,
	state repoState,
	preState string,
) error {
	_, err := (&gitNode{paths: paths}).build(
		ctx,
		corpusStore{cacheDir: cacheDir, indexDir: indexDir},
		state,
		preState,
		testLexicalSearchExecution(),
	)
	return err
}

func prepareAndSearchCorpus(
	ctx context.Context,
	plan node,
	paths *gitPaths,
	userQ query.Q,
	config searchConfig,
) ([]corpusSearchResult, dirtyFileSet, error) {
	if plan.behavior == nil && paths != nil {
		plan.behavior = &gitNode{paths: *paths}
	}
	return strictNode(ctx, plan, queryPlan{strict: userQ}, config, testLexicalSearchExecution())
}

func ensureGitCorpusFresh(
	ctx context.Context,
	plan node,
	paths gitPaths,
) (repoState, corpusIndexState, error) {
	return (&gitNode{paths: paths}).refresh(ctx, plan, testLexicalSearchExecution())
}

func ensureCombinedGitCorpus(
	ctx context.Context,
	plan node,
	paths gitPaths,
	state repoState,
) (corpusIndexState, error) {
	return (&gitNode{paths: paths}).refreshState(ctx, plan, state, testLexicalSearchExecution())
}
