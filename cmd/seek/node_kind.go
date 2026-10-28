package main

import "context"

// nodeKind is the behavior of one node kind: *gitNode or *folderNode. The
// node's kind field (corpusKind) is the data tag for display, the corpus ID
// and dedup.
type nodeKind interface {
	// prepare makes the node's index current and returns what its read
	// needs. After a failed build or during a peer's build, it keeps the
	// last complete index (corpusStale).
	prepare(ctx context.Context, plan node, execution searchExecution) (openedNode, error)
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
	// searches; the scope applies at search time.
	state, indexState, err := g.refresh(ctx, plan, execution)
	if err != nil {
		return openedNode{}, err
	}
	return openedNode{
		plan:       plan,
		indexState: indexState,
		// The capture's dirty files, also for a stale index. The committed
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
	// After a tree change, the shared capture is stale, and this prepare
	// walked the folder again. The index's state file then holds the state
	// that it built or checked. Without this state, a node read again after a
	// save during its build would get no vector hit.
	built := ""
	if indexState != corpusStale {
		built = readStateFile(plan.cacheDir)
	}
	return openedNode{
		plan:       plan,
		indexState: indexState,
		// The walk state and the model path base give the semantic source
		// (folderSemanticSource). The read checks the state under the lock
		// (readStateFile in readNodeEvidence).
		version: func() (string, string, bool) {
			if state, _, ok := capture.current(); ok {
				return state, plan.folderSemanticSource(state), true
			}
			return built, plan.folderSemanticSource(built), built != ""
		},
	}, nil
}
