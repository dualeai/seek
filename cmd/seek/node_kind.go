package main

import "context"

// nodeKind is the behavior of one node kind: *gitNode or *folderNode. The
// node's kind field (corpusKind) is the data tag for display, the corpus ID
// and dedup.
type nodeKind interface {
	// prepare makes the node's index current, or keeps the last complete
	// one after a failed build or during a peer's build (corpusStale), and
	// returns what its read needs.
	prepare(ctx context.Context, plan node, execution searchExecution) (openedNode, error)
	// materialize reads the evidence of the base generation's semantic hits
	// from the source that the generation holds.
	materialize(ctx context.Context, opened openedNode, candidates []semanticFileCandidate, contextLines int) ([]semanticFileCandidate, error)
	// estimate gives the cache bytes of the node's next generation and
	// whether the search rebuilds it (cache_budget.go).
	estimate(ctx context.Context, plan node, vectors, probe bool) (bytes int64, rebuild bool, err error)
}

// gitNode is the kind of a Git work tree.
type gitNode struct{ paths gitPaths }

// folderNode is the kind of a plain folder or a single file.
type folderNode struct{}

// prepare refreshes the Git index from one git status capture.
func (g *gitNode) prepare(ctx context.Context, plan node, execution searchExecution) (openedNode, error) {
	// One combined whole-repo index serves both scoped and unscoped
	// searches; scope is applied at search time.
	state, indexState, err := g.refresh(ctx, plan, execution)
	if err != nil {
		return openedNode{}, err
	}
	return openedNode{
		plan:       plan,
		state:      state,
		indexState: indexState,
		// The capture's dirty files, also for a stale index: the committed
		// copy of a file that is dirty now is old text, so it stays hidden.
		dirty: dirtyFileSetFromState(state),
		// The hash runs at read time: it stats the dirty files again, so a
		// read sees an edit of a dirty file.
		version: func() (string, string, bool) {
			hash := gitCorpusStateHash(g.paths, state)
			if state.HeadSHA == "no-head" {
				// A node with no commit has no base; its overlay holds
				// every file.
				return hash, "", true
			}
			return hash, state.HeadSHA, true
		},
	}, nil
}

// materialize reads the hits from the prepared commit.
func (g *gitNode) materialize(ctx context.Context, opened openedNode, candidates []semanticFileCandidate, contextLines int) ([]semanticFileCandidate, error) {
	return materializeSemanticCandidates(ctx, opened.plan, g.paths, opened.state.HeadSHA, candidates, contextLines)
}

// prepare builds the folder's index. A folder whose update fails while shards
// exist serves them with a warning.
func (*folderNode) prepare(ctx context.Context, plan node, execution searchExecution) (openedNode, error) {
	indexState, err := ensureFolderCorpusFreshWithExecution(ctx, plan, execution)
	if err != nil {
		if !plan.servesStale(err) {
			return openedNode{}, err
		}
		indexState = corpusStale
	}
	capture := plan.folder
	return openedNode{
		plan:       plan,
		indexState: indexState,
		// The walk state is the semantic source; an invalid capture reads
		// text only.
		version: func() (string, string, bool) {
			state, _, ok := capture.current()
			return state, state, ok
		},
	}, nil
}

// materialize reads the hits from the folder's files.
func (*folderNode) materialize(ctx context.Context, opened openedNode, candidates []semanticFileCandidate, contextLines int) ([]semanticFileCandidate, error) {
	return materializeWorkTreeSemanticCandidates(ctx, opened.plan, folderRepoName(opened.plan), candidates, contextLines)
}
