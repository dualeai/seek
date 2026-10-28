package main

// gitBuildInputs is what the Git build reads under its build lock, after the
// swap repair, before it acts.
type gitBuildInputs struct {
	head            string // the captured commit; gitNoHead with no commit
	preState        string // the capture's state hash
	cachedState     string // the published .state
	dirty           bool   // the capture has dirty files
	familyIntact    bool   // the whole family matches .family-v1
	committedIntact bool   // the committed family matches .family-v1
	committedMember bool   // the committed family has a member
	committedReady  bool   // committedSnapshotReady: the family holds head
	vectors         bool   // the search builds the base generation
	overlay         bool   // the search builds the overlay generation
	semanticOn      bool   // the policy allows vectors at all
	joinedReady     bool   // the joined descriptor binds what the search reads
	arrival         missingBlobArrival
	// Read only when gitBuildCurrent is false, so a warm build opens no
	// generation and reads the shard metadata once (committedReady).
	publishedHead  string // the published shards' commit; with a commit only
	basePresent    bool   // head's base generation exists; with vectors only
	overlayPresent bool   // preState's overlay exists; with an overlay only
}

// gitBuildPlan is what the Git build does.
type gitBuildPlan struct {
	committed      bool // build the committed family
	clearCommitted bool // drop the committed family of a node with no commit
	semantic       bool // build the base generation
	overlay        bool // build the overlay generation
	dropJoined     bool // the publish first drops the joined descriptor
	baseBound      bool // the live base serves with no build
	overlayBound   bool // the live overlay serves with no build
}

// buildDir reports a build that writes into a build directory.
func (p gitBuildPlan) buildDir() bool {
	return p.committed || p.clearCommitted || p.semantic || p.overlay
}

// snapshot reports a build that reads the captured commit.
func (p gitBuildPlan) snapshot() bool { return p.committed || p.semantic }

// noHeadArtifacts reports a node with no commit that still has committed
// shards: a branch that lost its commits.
func (in gitBuildInputs) noHeadArtifacts() bool {
	return in.head == gitNoHead && in.committedMember
}

// gitBuildCurrent reports a node with nothing to build: the index holds the
// captured state and every family and generation that the search reads.
func gitBuildCurrent(in gitBuildInputs) bool {
	return in.cachedState == in.preState && !in.noHeadArtifacts() && in.committedReady &&
		in.familyIntact && in.joinedReady && !in.arrival.lexical && !in.arrival.semantic
}

// decideGitBuild gives the build of a node that gitBuildCurrent does not
// report current. The decisions that need the build's own results (the delta
// check, the HEAD check, kept files, the activation) stay in the build.
func decideGitBuild(in gitBuildInputs) gitBuildPlan {
	plan := gitBuildPlan{
		// The published shards hold another commit, the committed family no
		// longer matches its manifest, or a missing blob arrived.
		committed: in.head != gitNoHead &&
			(in.publishedHead != in.head || !in.committedIntact || in.arrival.lexical),
		clearCommitted: in.noHeadArtifacts(),
		semantic:       vectorGenerationNeeded(in.vectors, in.basePresent, in.arrival.semantic),
		overlay:        vectorGenerationNeeded(in.overlay, in.overlayPresent, false),
		baseBound:      !in.vectors || (in.basePresent && !in.arrival.semantic),
		overlayBound:   !in.overlay || in.overlayPresent,
	}
	// The old descriptor must not name a family while that family changes.
	plan.dropJoined = in.semanticOn && (plan.committed || plan.clearCommitted || in.dirty || in.arrival.semantic)
	return plan
}

// vectorGenerationNeeded reports a generation that a build makes: the search
// wants it, and the live one is missing or stale. The cache estimate uses the
// same rule.
func vectorGenerationNeeded(want, present, stale bool) bool { return want && (!present || stale) }

// folderBuildInputs is what the folder build reads under its build lock,
// after the state check, before it builds.
type folderBuildInputs struct {
	stateHash    string // the walk state
	currentState string // the published .state
	hasShard     bool   // the family has a shard
	intact       bool   // the family matches .family-v1
	vectors      bool   // the search builds the vector generation
	// semanticPresent: the generation of stateHash exists; with vectors only.
	semanticPresent bool
}

// folderBuildPlan is what the folder build does.
type folderBuildPlan struct {
	lexical bool // build the text family
	delta   bool // as a delta on the published family
	// semantic builds the vector generation.
	semantic bool
	// keepState keeps .state after a failure before the publish: the
	// published family is usable, and .state is its delta base.
	keepState bool
	tornWarn  bool // warn that the family does not match its manifest
}

// decideFolderBuild gives the folder build from its inputs.
func decideFolderBuild(in folderBuildInputs) folderBuildPlan {
	usable := in.hasShard && in.intact
	lexical := in.currentState != in.stateHash || !usable
	return folderBuildPlan{
		lexical:   lexical,
		delta:     lexical && in.currentState != "" && usable,
		semantic:  vectorGenerationNeeded(in.vectors, in.semanticPresent, false),
		keepState: usable,
		tornWarn:  in.hasShard && !in.intact,
	}
}
