package main

import (
	"context"
	"testing"

	"github.com/sourcegraph/zoekt/query"
)

// testLexicalSearchExecution is the execution of a --lexical-only search: no
// model and no vectors.
func testLexicalSearchExecution() searchExecution {
	return searchExecution{policy: lexicalOnlySearch}
}

// testSemanticSearchExecution offers a model to a corpus build, so the build
// prepares vectors, and closes the future when the test ends.
//
// It sets the model because searchGraph sets it from the query, and a
// test that calls a corpus entry point skips that decision. A test that drives
// a whole search must not use this.
func testSemanticSearchExecution(tb testing.TB, factory semanticModelFactory) searchExecution {
	tb.Helper()
	future := newSemanticModelFuture(factory)
	tb.Cleanup(func() { _ = future.Close() })
	return searchExecution{
		policy: semanticSearch,
		model:  future,
	}
}

// ensureFolderCorpusFresh brings the folder node at plan up to date with a
// lexical execution, as a --lexical-only search does before its read.
func ensureFolderCorpusFresh(ctx context.Context, plan node) (corpusIndexState, error) {
	return ensureFolderCorpusFreshWithExecution(ctx, plan, testLexicalSearchExecution())
}

// runIndexingWithCache builds the Git node of paths into store for state, with
// a lexical execution. preState is the state hash that the build records
// (gitCorpusStateHash).
func runIndexingWithCache(
	ctx context.Context,
	paths gitPaths,
	store corpusStore,
	state repoState,
	preState string,
) error {
	_, err := (&gitNode{paths: paths}).build(ctx, store, state, preState, testLexicalSearchExecution())
	return err
}

// prepareAndSearchCorpus makes the node at plan current and runs userQ on it
// with a lexical execution (strictNode).
func prepareAndSearchCorpus(
	ctx context.Context,
	plan node,
	userQ query.Q,
	config searchConfig,
) ([]corpusSearchResult, dirtyFileSet, error) {
	return strictNode(ctx, plan, queryPlan{strict: userQ}, config, testLexicalSearchExecution())
}

// ensureGitCorpusFresh runs the node refresh of a search (gitNode.prepare) on
// the Git node at plan with a lexical execution. It returns the index state.
func ensureGitCorpusFresh(
	ctx context.Context,
	plan node,
	paths gitPaths,
) (corpusIndexState, error) {
	opened, err := (&gitNode{paths: paths}).prepare(ctx, plan, testLexicalSearchExecution())
	return opened.indexState, err
}

// ensureCombinedGitCorpus refreshes the whole-repository index of plan for
// state (gitNode.refreshState) with a lexical execution. It takes no git
// status of its own.
func ensureCombinedGitCorpus(
	ctx context.Context,
	plan node,
	paths gitPaths,
	state repoState,
) (corpusIndexState, error) {
	return (&gitNode{paths: paths}).refreshState(ctx, plan, state, testLexicalSearchExecution())
}
