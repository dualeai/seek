package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/term"
)

var errGitIndexStateChanged = errors.New("git index state changed")

// version is the release version. The build and release commands set it with
// ldflags (-X main.version=...).
var version = ""

func versionString() string {
	if version != "" {
		return "seek " + version
	}
	// Fall back to the VCS data that go build embeds.
	if info, ok := debug.ReadBuildInfo(); ok {
		v := info.Main.Version
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				v += " (" + s.Value[:7] + ")"
			}
		}
		if v != "" {
			return "seek " + v
		}
	}
	return "seek (unknown)"
}

// main runs the root command and the automatic GC (shouldRunOpportunisticGC),
// then exits with the exit code of the command.
//
// TODO(https://github.com/rs/xid/issues/120): github.com/rs/xid v1.6.0, which
// zoekt/index imports, runs ioreg at package init on macOS. So every seek
// process waits about 20 ms (one ioreg run) before main. The fix
// (https://github.com/rs/xid/pull/121) is on master but in no release yet.
// Upgrade rs/xid when a release after v1.6.0 has it.
func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := newLockWaits()
	ctx = withLockWaits(ctx, waits)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	go watchInterrupts(signals, cancel, waits)

	// Scan the effective arguments for -v and --verbose before Cobra parses
	// them, so a flag error also follows the verbose contract.
	// PersistentPreRunE runs only after pflag parses the flags. Without this
	// scan, `seek -v --unknown foo` would print only the plain `seek: <err>`
	// line, with no structured slog ERROR record.
	root := newRootCmd(newLateOnSemanticModel)
	args := splicePassthroughSeparator(os.Args[1:], root.Flags())
	verbose := hasVerboseArg(args)

	root.SetArgs(args)
	err := root.ExecuteContext(ctx)

	// Help, version, completion, and explicit GC calls do not start automatic
	// GC. A GC command already owns cache maintenance, and its dry-run form
	// must remain read-only. Search calls run automatic GC after their output
	// is flushed.
	if shouldRunOpportunisticGC(args) {
		fireOpportunisticGC(runOpportunisticGC, gcRunTimeout)
	}

	if err != nil {
		code := exitCodeForError(err)
		reportCLIError(os.Stderr, err, verbose)
		os.Exit(code)
	}
}

// shouldRunOpportunisticGC reports whether the command runs the automatic GC
// after it ends. A help or version flag turns it off. A first argument that
// names the help, completion, or GC command turns it off too.
func shouldRunOpportunisticGC(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return true
		}
		for _, name := range []string{"-h", "--help", "--version"} {
			if value, ok := optionalBoolFlag(arg, name); ok && value {
				return false
			}
		}
	}
	for _, arg := range args {
		if arg == "--" {
			return true
		}
		knownBool := false
		for _, name := range []string{"-h", "--help", "--version", "-v", "--verbose"} {
			if _, ok := optionalBoolFlag(arg, name); ok {
				knownBool = true
				break
			}
		}
		if knownBool {
			continue
		}
		switch arg {
		case "help", "completion", "gc", "garbage-collect":
			return false
		}
		return true
	}
	return true
}

// optionalBoolFlag parses arg as the bool flag name, alone or as name=value.
// It returns the flag value and whether arg is that flag with a valid value.
func optionalBoolFlag(arg, name string) (bool, bool) {
	if arg == name {
		return true, true
	}
	value, found := strings.CutPrefix(arg, name+"=")
	if !found {
		return false, false
	}
	parsed, err := strconv.ParseBool(value)
	return parsed, err == nil
}

// hasVerboseArg reports whether the arguments before "--" set -v or
// --verbose; the last one wins. main reads it before Cobra parses the flags,
// so the error of `seek -v --unknown` also gets the verbose record.
func hasVerboseArg(args []string) bool {
	verbose := false
	for _, a := range args {
		if a == "--" {
			break
		}
		for _, name := range []string{"-v", "--verbose"} {
			if value, ok := optionalBoolFlag(a, name); ok {
				verbose = value
				break
			}
		}
	}
	return verbose
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

// runSearchCommand runs one search. It plans the query, expands the path
// operands into the node graph, and prepares and searches every node. For a
// query that gives the model text, the model ranks the results. Then it writes
// the results.
// When it writes no result, it returns errNoMatch, or the user's interrupt
// after an interrupt ended a lock wait (noMatchOrInterrupted).
func runSearchCommand(
	ctx context.Context,
	pattern string,
	pathOperands []string,
	limit int,
	maxMatches int,
	config searchConfig,
	runConfig searchRunConfig,
) error {
	config.contextMatchLimit = maxMatches
	config.contextFileLimit = limit
	plan, err := planQuery(pattern, runConfig.policy)
	if err != nil {
		return err
	}
	// No path operand means the current directory. After this line no code
	// sees the difference.
	if len(pathOperands) == 0 {
		pathOperands = []string{"."}
	}

	// Every node of the command shares one warning registry, so a node that
	// two parts of the graph find gets one warning. Every node needs the cache
	// root: an unusable root ends the command before any node fails on it.
	ctx = withSkippedNodes(ctx)
	if err := checkCacheRoot(); err != nil {
		return err
	}
	plans, err := planCorpora(ctx, pathOperands)
	if err != nil {
		return err
	}
	// The graph is final: size the build window from its nodes.
	ctx = withIndexWindow(ctx, len(plans))
	slog.Debug("Sized the build window", "nodes", len(plans), "window", indexWindow(ctx))

	execution := searchExecution{
		policy:    runConfig.policy,
		resources: liveSearchResources(),
		opened:    &openedNodes{},
		operands:  newDisplayOperands(pathOperands),
	}
	if plan.model != nil && runConfig.newModel != nil {
		// The model stages run: every node builds its vectors before its read,
		// and the model session starts beside the index preparation.
		execution.model = newSemanticModelFuture(runConfig.newModel)
		execution.vectors = true
		execution.model.start(ctx)
		execution.model.startQuery(ctx, plan.model.modelQuery)
		defer func() { _ = execution.model.Close() }()
	} else {
		plan.model = nil
	}

	// The graph is final: stop before any build when the search does not fit
	// the cache budget, and evict unused corpora after it.
	if err := checkCacheBudget(ctx, plans, execution.vectors); err != nil {
		return err
	}
	defer enforceCacheBudget(plans)

	var nameOnly *atomic.Int64
	if !namesOnlyQuery(plan.raw) {
		ctx, nameOnly = withNameOnlyCount(ctx)
	}
	ctx, bm25 := withBM25Command(ctx, len(plans))
	var allResults []corpusSearchResult
	var dirtyByCorpus dirtyFilesByCorpus
	if plan.model == nil {
		allResults, dirtyByCorpus, err = searchNodes(ctx, plans, func(nodeCtx context.Context, node node) ([]corpusSearchResult, dirtyFileSet, error) {
			return strictNode(nodeCtx, node, plan, config, execution)
		})
		if err != nil {
			return err
		}
		rescoreStrictResults(allResults, bm25)
	} else {
		var hits nodeHits
		hits, dirtyByCorpus, err = searchNodes(ctx, plans, func(nodeCtx context.Context, node node) ([]nodeHit, dirtyFileSet, error) {
			return retrieveNode(nodeCtx, node, plan, config, execution)
		})
		if err != nil {
			return err
		}
		rescoreStrictHits(hits, bm25)
		accepted, err := rankModelQuery(ctx, hits, plan, execution)
		if err != nil {
			return err
		}
		allResults = applyRerankDisplayConfig(presentRanked(accepted), config)
	}

	warnUnindexedFileOperands(ctx, plans, execution.opened, pathOperands, allResults, dirtyByCorpus)
	if len(allResults) == 0 {
		return noMatchOrInterrupted(ctx)
	}
	if namesOnlyQuery(plan.raw) {
		keepFileNamesOnly(allResults)
	}
	applyHeaders(allResults, execution.operands)

	// Show corpus context when the graph has more than one node. The graph
	// expansion found every node before the search.
	displayMode := hideCorpusContext
	if len(plans) > 1 {
		displayMode = showCorpusContext
	}
	pal := plainPalette
	if useColor(os.Stdout) {
		pal = ansiPalette
	}
	output := bufio.NewWriter(os.Stdout)
	wrote, err := writeCorpusResultsWithContext(
		output,
		allResults,
		limit,
		maxMatches,
		displayMode,
		pal,
	)
	// One notice per search counts the files indexed by name only, after the
	// results. A search with no result keeps its silent exit 1.
	if err == nil && wrote && nameOnly != nil {
		if files := nameOnly.Load(); files > 0 {
			_, err = output.WriteString(nameOnlyNoticeLine(files))
		}
	}
	if err == nil {
		err = output.Flush()
	}
	if err != nil {
		return fmt.Errorf("write search results: %w", err)
	}
	if !wrote {
		return noMatchOrInterrupted(ctx)
	}
	return nil
}

// useColor reports whether to write ANSI color to f. The first rule that
// applies decides:
//
//   - NO_COLOR is set and not empty: no color.
//   - CLICOLOR_FORCE is set, to any value: color, also through a pipe
//     (`… | less -R`, CI).
//   - Else: color only when TERM is set and is not "dumb", and f is a terminal.
//
// So piped output (agents, CI, `| cat`) is plain with no setting.
func useColor(f *os.File) bool {
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	if _, ok := os.LookupEnv("CLICOLOR_FORCE"); ok {
		return true
	}
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// touchPlanUsed marks the corpus as used: scoped and unscoped searches of one
// repository read one index, so both keep it recent for LRU eviction.
func touchPlanUsed(plan node) {
	touchUsed(plan.cacheDir)
}

func dirtyFileSetFromState(state repoState) dirtyFileSet {
	if len(state.Files) == 0 {
		return nil
	}
	dirtyFiles := make(dirtyFileSet, len(state.Files))
	for _, f := range state.Files {
		dirtyFiles[f] = struct{}{}
	}
	return dirtyFiles
}

func wrapCorpusResults(plan node, files []zoekt.FileMatch) []corpusSearchResult {
	if len(files) == 0 {
		return nil
	}
	var selected map[string]struct{}
	if len(plan.selectedFiles) > 0 {
		selected = make(map[string]struct{}, len(plan.selectedFiles))
		for _, name := range plan.selectedFiles {
			selected[name] = struct{}{}
		}
	}
	results := make([]corpusSearchResult, len(files))
	for i, file := range files {
		displayName := file.FileName
		if _, ok := selected[file.FileName]; ok {
			displayName = filepath.Base(file.FileName)
		}
		results[i] = corpusSearchResult{
			corpusID:    plan.id,
			kind:        plan.kind,
			displayRoot: plan.displayRoot,
			displayName: displayName,
			file:        file,
			modelBase:   plan.modelPathBase(),
		}
	}
	return results
}

// gitNodeState returns the graph expansion's status of a Git node, or a fresh
// status once a route has seen the tree change.
func gitNodeState(ctx context.Context, plan node, paths gitPaths) (repoState, error) {
	if state, ok := plan.gitState.current(); ok {
		return state, nil
	}
	return gitRepoStateIn(ctx, paths.RepoDir)
}

// refresh makes the one combined whole-repo index current. A scoped search
// reads the same index; its scope applies at search time.
func (g *gitNode) refresh(
	ctx context.Context,
	plan node,
	execution searchExecution,
) (repoState, corpusIndexState, error) {
	paths := g.paths
	state, err := gitNodeState(ctx, plan, paths)
	if err != nil {
		return repoState{}, corpusSearchable, gitCorpusError(paths.RepoDir, plan.indexDir, err)
	}
	indexState, err := g.refreshState(ctx, plan, state, execution)
	if err != nil {
		return repoState{}, corpusSearchable, err
	}
	return state, indexState, nil
}

// refreshState refreshes, for the captured state, the whole-repository index
// that serves scoped and unscoped searches. Zoekt combines committed and dirty
// files. When the command reads vectors, a joined semantic generation binds the
// captured commit and the overlay of the dirty files. It returns an update
// failure when no shards exist. With published shards, it warns and leaves
// the prior generation searchable (corpusStale). A peer's build also
// gives corpusStale, with no warning. A node that .empty marks as empty for the
// captured state gives corpusKnownEmpty.
func (g *gitNode) refreshState(
	ctx context.Context,
	plan node,
	state repoState,
	execution searchExecution,
) (corpusIndexState, error) {
	paths := g.paths
	// Check for existing index state. If present, the cache directory
	// exists. Skip MkdirAll on the warm path.
	cachedState := readStateFile(plan.cacheDir)
	if cachedState == "" {
		if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
			return corpusSearchable, fmt.Errorf("create index directory: %w", err)
		}
	}

	currentState := gitCorpusStateHash(paths, state)
	semanticRequired := execution.buildsVectors(state.HeadSHA)
	overlayRequired := execution.buildsOverlay(len(state.Files) > 0)
	baseSource, overlaySource := "", ""
	if semanticRequired {
		baseSource = state.HeadSHA
	}
	if overlayRequired {
		overlaySource = semanticOverlaySource(currentState)
	}
	joinedReady := (!semanticRequired && !overlayRequired) || plan.joinedGenerationsMatch(
		currentState,
		baseSource,
		overlaySource,
	)
	// Use one directory scan so all integrity checks describe the same state.
	sc, scErr := scanFamilyOptional(plan.indexDir)
	hasShards := scErr == nil && sc.familyComplete(plan.indexDir) &&
		plan.committedSnapshotReady(state, sc)
	clearCommitted := state.HeadSHA == "no-head" && sc.hasMember(familyCommitted)
	// A leftover .swapping marker means that a publish was interrupted, or did
	// not move the live family back, and the shards can be torn. Force the
	// build path, so recoverIncompleteSwap runs even when the state looks
	// current.
	swapPending := readCacheFile(plan.cacheDir, swappingMarkerFile) != ""
	needBuild := currentState != cachedState || !hasShards || swapPending || clearCommitted || !joinedReady ||
		missingBlobCheckDue(ctx, paths.RepoDir, plan.cacheDir, state.HeadSHA, semanticRequired)
	if !swapPending && !clearCommitted && cachedState == currentState && !hasShards &&
		readEmptyStateFile(plan.cacheDir) == currentState && joinedReady {
		return corpusKnownEmpty, nil
	}

	if needBuild {
		// Remove a non-directory entry at the index path before MkdirAll.
		if st, statErr := os.Lstat(plan.indexDir); statErr == nil && !st.IsDir() {
			if rmErr := os.Remove(plan.indexDir); rmErr != nil {
				return corpusSearchable, fmt.Errorf("remove non-directory at index path: %w", rmErr)
			}
		}
		if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
			return corpusSearchable, fmt.Errorf("create index directory: %w", err)
		}
		release, err := acquireBuildSlot(ctx)
		if err != nil {
			return corpusSearchable, err
		}
		peer, err := g.build(
			ctx,
			plan.corpusStore,
			state,
			currentState,
			execution,
		)
		release()
		stale := false
		if err != nil {
			if !plan.servesStale(err) {
				return corpusSearchable, err
			}
			stale = true
		}
		if readStateFile(plan.cacheDir) != currentState {
			// The build saw the work tree move and published no state: a
			// later preparation of this command must read the tree again.
			plan.gitState.invalidate()
		}
		if readEmptyStateFile(plan.cacheDir) == currentState && !shardsExist(plan.indexDir) {
			return corpusKnownEmpty, nil
		}
		if stale || peer {
			// A failed build or a peer's build: the index is not current.
			return corpusStale, nil
		}
	}
	return corpusSearchable, nil
}

// watchInterrupts cancels the command on SIGTERM and on SIGINT, with one
// exception. The first SIGINT while a node waits for a lock ends only the
// waits (lockWaits), so the nodes that are ready print. A later SIGINT cancels
// the command. It returns when signals closes.
func watchInterrupts(signals <-chan os.Signal, cancel context.CancelFunc, waits *lockWaits) {
	waitsEnded := false
	for sig := range signals {
		if sig == syscall.SIGINT && !waitsEnded && waits.endRunning() {
			waitsEnded = true
			continue
		}
		cancel()
	}
}

func normalizeCommittedTreeish(headSHA string) string {
	if headSHA == "" || headSHA == "no-head" {
		return "no-head"
	}
	return headSHA
}

// validateCommittedHead returns errGitIndexStateChanged when HEAD no longer
// names the captured commit, or the no-head state. It does not change the
// published cache state.
func validateCommittedHead(ctx context.Context, paths gitPaths, expectedHead string) error {
	currentHead, err := gitHeadTreeish(ctx, paths.RepoDir)
	if err != nil {
		return err
	}
	if normalizeCommittedTreeish(currentHead) != normalizeCommittedTreeish(expectedHead) {
		return errGitIndexStateChanged
	}
	return nil
}

// searchPlannedCorpusParsed runs userQ on one node read of plan. dirty holds
// the dirty files of the node's capture; the read searches only the current
// copy of each file.
func searchPlannedCorpusParsed(
	ctx context.Context,
	plan node,
	dirty dirtyFileSet,
	userQ query.Q,
	config searchConfig,
) ([]zoekt.FileMatch, error) {
	// Hold the shared publish lock while the read lists, opens, and searches
	// the shards: a publisher cannot replace the shard family during that work.
	read, ctx, err := openNodeRead(ctx, plan, dirty)
	if err != nil {
		return nil, err
	}
	defer read.close()
	return read.search(ctx, userQ, config)
}
