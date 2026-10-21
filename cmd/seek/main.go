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
	"syscall"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/term"
)

var errGitIndexStateChanged = errors.New("git index state changed")

// Set via ldflags (-X main.version=...) by the build and release commands.
var version = ""

func versionString() string {
	if version != "" {
		return "seek " + version
	}
	// Fallback to VCS info embedded by go build.
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

// TODO: github.com/rs/xid v1.6.0, which zoekt/index imports, runs ioreg at
// package init on macOS. Every seek process pays about 20 ms before main.
// Upstream issue https://github.com/rs/xid/issues/120; the fix
// (https://github.com/rs/xid/pull/121) is on master but in no release yet.
// Upgrade rs/xid when a release after v1.6.0 has it.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Pre-scan the effective arguments for -v / --verbose so flag-time errors are
	// formatted under the verbose contract too. PersistentPreRunE only
	// fires AFTER pflag parses successfully, so without this pre-scan a
	// `seek -v --unknown foo` would print plain `seek: <err>` instead of
	// the structured slog ERROR record the user asked for.
	root := newRootCmd()
	args := splicePassthroughSeparator(os.Args[1:], root.Flags())
	verbose := hasVerboseArg(args)

	root.SetArgs(args)
	err := root.ExecuteContext(ctx)

	// Help, completion, and explicit GC calls do not start automatic GC. A GC
	// command already owns cache maintenance, and its dry-run form must remain
	// read-only. Search calls run automatic GC after their output is flushed.
	if shouldRunOpportunisticGC(args) {
		fireOpportunisticGC(runOpportunisticGC, gcRunTimeout)
	}

	if err != nil {
		code := exitCodeForError(err)
		reportCLIError(os.Stderr, err, verbose)
		os.Exit(code)
	}
}

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

// hasVerboseArg scans effective arguments for -v / --verbose without
// consulting Cobra. Used by main() to decide error formatting BEFORE the Cobra
// flag parser has run — a `seek -v --unknown` typo still needs the
// verbose contract honoured.
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

// runSearchCommand runs one search: it plans the query, expands the path
// operands into the node graph, prepares and searches every node, ranks a
// description query with the model, and writes the results. It returns
// errNoMatch when it writes no result.
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

	var nameOnly *nameOnlyCount
	if !namesOnlyQuery(plan.raw) {
		ctx, nameOnly = withNameOnlyCount(ctx)
	}
	ctx, bm25 := withBM25Command(ctx, len(plans))
	var allResults []corpusSearchResult
	var dirtyByCorpus dirtyFilesByCorpus
	if plan.model == nil {
		allResults, dirtyByCorpus, err = searchNodes(ctx, plans, func(nodeCtx context.Context, node corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
			return strictNode(nodeCtx, node, plan, config, execution)
		})
		if err != nil {
			return err
		}
		rescoreStrictResults(allResults, bm25)
	} else {
		var hits nodeHits
		hits, dirtyByCorpus, err = searchNodes(ctx, plans, func(nodeCtx context.Context, node corpusPlan) ([]nodeHit, dirtyFileSet, error) {
			return retrieveNode(nodeCtx, node, plan, config, execution)
		})
		if err != nil {
			return err
		}
		rescoreStrictHits(hits, bm25)
		accepted, err := rankDescription(ctx, hits, plan, execution)
		if err != nil {
			return err
		}
		allResults = applyRerankDisplayConfig(presentRanked(accepted), config)
	}

	warnUnindexedFileOperands(ctx, plans, execution.opened, pathOperands, allResults, dirtyByCorpus)
	if len(allResults) == 0 {
		return errNoMatch
	}
	if namesOnlyQuery(plan.raw) {
		keepFileNamesOnly(allResults)
	}
	applyHeaders(allResults, execution.operands)

	// Formatting writes nothing when all results were stale committed
	// matches for dirty files — treat as no match (exit code 1).
	//
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
		dirtyByCorpus,
		limit,
		maxMatches,
		displayMode,
		pal,
	)
	// One notice per search counts the files indexed by name only, after the
	// results. A search with no result keeps its silent exit 1.
	if err == nil && wrote && nameOnly != nil {
		if files := nameOnly.files.Load(); files > 0 {
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
		return errNoMatch
	}
	return nil
}

// useColor decides whether to emit ANSI color on the given stream. Color is ON
// by default and disabled only by constraints, in precedence order: NO_COLOR
// (present and non-empty) forces off; CLICOLOR_FORCE (present, any value)
// forces on even through a pipe (e.g. `… | less -R`, CI); otherwise color is
// used only when the stream is a real terminal and TERM is set and not "dumb".
// Piped output (agents, CI, `| cat`) is therefore plain with zero config.
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
func touchPlanUsed(plan corpusPlan) {
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

func wrapCorpusResults(plan corpusPlan, files []zoekt.FileMatch) []corpusSearchResult {
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
		}
	}
	return results
}

// gitNodeState returns the graph expansion's status of a Git node, or a fresh
// status once a route has seen the tree change.
func gitNodeState(ctx context.Context, plan corpusPlan, paths gitPaths) (repoState, error) {
	if state, ok := plan.gitState.current(); ok {
		return state, nil
	}
	return gitRepoStateIn(ctx, paths.RepoDir)
}

// ensureGitCorpusFreshWithExecution makes the one combined whole-repo index
// current. A scoped search reads the same index; its scope applies at search
// time.
func ensureGitCorpusFreshWithExecution(
	ctx context.Context,
	plan corpusPlan,
	paths gitPaths,
	execution searchExecution,
) (repoState, corpusIndexState, error) {
	state, err := gitNodeState(ctx, plan, paths)
	if err != nil {
		return repoState{}, corpusSearchable, gitCorpusError(paths.RepoDir, plan.indexDir, err)
	}
	indexState, err := ensureCombinedGitCorpusWithExecution(ctx, plan, paths, state, execution)
	if err != nil {
		return repoState{}, corpusSearchable, err
	}
	return state, indexState, nil
}

// ensureCombinedGitCorpusWithExecution refreshes the whole-repository index
// that serves scoped and unscoped searches. Zoekt combines committed and dirty
// files. When the command reads vectors, a joined semantic generation binds the
// captured commit and the overlay of the dirty files. An update failure is
// returned when no shards exist; with published shards, the function warns and
// leaves the prior generation searchable.
func ensureCombinedGitCorpusWithExecution(
	ctx context.Context,
	plan corpusPlan,
	paths gitPaths,
	state repoState,
	execution searchExecution,
) (corpusIndexState, error) {
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
	joinedReady := (!semanticRequired && !overlayRequired) || joinedGenerationsMatch(
		plan.cacheDir,
		plan.indexDir,
		currentState,
		baseSource,
		overlaySource,
	)
	// Use one directory scan so all integrity checks describe the same state.
	sc, scErr := scanFamilyOptional(plan.indexDir)
	hasShards := scErr == nil && sc.familyComplete(plan.indexDir) &&
		committedSnapshotReady(plan.cacheDir, plan.indexDir, state, sc)
	clearCommitted := state.HeadSHA == "no-head" && sc.hasMember(familyCommitted)
	// A leftover .swapping marker means a prior publish was interrupted and the
	// shards may be torn; force the build path so recoverIncompleteSwap runs even
	// when state otherwise looks current.
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
		err = runIndexingWithCacheExecution(
			ctx,
			paths,
			plan.cacheDir,
			plan.indexDir,
			state,
			currentState,
			execution,
		)
		release()
		stale := false
		if err != nil {
			if !shardsExist(plan.indexDir) {
				return corpusSearchable, err
			}
			reportStaleIndexWarning(err)
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
		if stale {
			return corpusStale, nil
		}
	}
	return corpusSearchable, nil
}

func normalizeCommittedTreeish(headSHA string) string {
	if headSHA == "" || headSHA == "no-head" {
		return "no-head"
	}
	return headSHA
}

// validateCommittedHead reports whether HEAD still equals the captured commit
// OID or no-head sentinel. It does not change published cache state.
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

func searchPlannedCorpusParsed(
	ctx context.Context,
	plan corpusPlan,
	userQ query.Q,
	config searchConfig,
) ([]zoekt.FileMatch, error) {
	// Hold the shared publish lock while listing, opening, and searching shards.
	// A publisher cannot replace the shard family during that work.
	searchLockFd, err := os.OpenFile(filepath.Join(plan.cacheDir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open search lock: %w", err)
	}
	defer func() { _ = searchLockFd.Close() }()
	if err := acquireReadLock(ctx, plan.indexDir, searchLockFd); err != nil {
		return nil, fmt.Errorf("acquire search lock: %w", err)
	}
	defer unlockFile(searchLockFd)
	return executeParsedSearchScopedDirs(withReadLockHeld(ctx), []string{plan.indexDir}, userQ, plan.scope, config)
}

// recordSemanticProviderFault notes a provider that produced unusable output.
// A search on an already-built index is the common path, so without this a wrong
// provider would drop every search to text ranking and never record itself. Any
// other failure says nothing about the provider and is ignored.
func recordSemanticProviderFault(err error) {
	if errors.Is(err, errDegenerateSemanticVector) {
		rejectLateOnAcceleratedProvider(err.Error())
	}
}
