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

func ensureFolderCorpusFresh(ctx context.Context, plan corpusPlan) (corpusIndexState, error) {
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
	return runIndexingWithCacheExecution(
		ctx,
		paths,
		cacheDir,
		indexDir,
		state,
		preState,
		testLexicalSearchExecution(),
	)
}

func prepareAndSearchCorpus(
	ctx context.Context,
	plan corpusPlan,
	paths *gitPaths,
	userQ query.Q,
	config searchConfig,
) ([]corpusSearchResult, dirtyFileSet, error) {
	if plan.gitPaths == nil && paths != nil {
		plan.gitPaths = paths
	}
	return strictNode(ctx, plan, queryPlan{strict: userQ}, config, testLexicalSearchExecution())
}

func ensureGitCorpusFresh(
	ctx context.Context,
	plan corpusPlan,
	paths gitPaths,
) (repoState, corpusIndexState, error) {
	return ensureGitCorpusFreshWithExecution(ctx, plan, paths, testLexicalSearchExecution())
}

func ensureCombinedGitCorpus(
	ctx context.Context,
	plan corpusPlan,
	paths gitPaths,
	state repoState,
) (corpusIndexState, error) {
	return ensureCombinedGitCorpusWithExecution(ctx, plan, paths, state, testLexicalSearchExecution())
}
