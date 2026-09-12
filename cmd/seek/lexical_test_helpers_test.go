package main

import (
	"context"

	"github.com/sourcegraph/zoekt/query"
)

func testLexicalSearchExecution() searchExecution {
	return searchExecution{policy: lexicalOnlySearchPolicy()}
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
	return prepareAndSearchCorpusWithExecution(
		ctx,
		plan,
		paths,
		userQ,
		config,
		testLexicalSearchExecution(),
	)
}

func ensureGitCorpusFresh(
	ctx context.Context,
	plan *corpusPlan,
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
