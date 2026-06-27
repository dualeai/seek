package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/term"
)

// errNoMatch is returned by run when the query executed successfully but
// produced zero results. Following the POSIX grep convention, this maps to
// exit code 1 — distinguishing "no match" from both success (0) and error (2).
// This lets callers use seek reliably in shell pipelines and conditionals:
//
//	if seek "TODO"; then … fi       # runs body only when matches exist
//	seek "pattern" || echo "nope"   # "nope" printed only on no-match
var errNoMatch = errors.New("no match")

var errScopedLayerStateChanged = errors.New("scoped git layer state changed")

const maxScopedLayerRefreshRetries = 2

// Set via ldflags (-X main.version=...) by make build / GoReleaser.
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

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Pre-scan os.Args for -v / --verbose so flag-time errors are
	// formatted under the verbose contract too. PersistentPreRunE only
	// fires AFTER pflag parses successfully, so without this pre-scan a
	// `seek -v --unknown foo` would print plain `seek: <err>` instead of
	// the structured slog ERROR record the user asked for.
	verbose := hasVerboseArg(os.Args[1:])

	err := executeCLI(ctx)

	// Fire opportunistic GC after results are flushed. Wait up to
	// gcRunTimeout so eviction completes before exit (helps tests and
	// observability), but never block longer.
	fireOpportunisticGC(runOpportunisticGC, gcRunTimeout)

	if err != nil {
		code := exitCodeForError(err)
		if code != 1 {
			// Plain `seek: <err>` line. The structured slog ERROR
			// output (time=... level=ERROR msg=...) is reserved for
			// verbose mode where the operator wants the full record.
			if verbose {
				slog.Error(err.Error())
			} else {
				fmt.Fprintf(os.Stderr, "seek: %s\n", err)
			}
		}
		os.Exit(code)
	}
}

// hasVerboseArg scans raw os.Args for -v / --verbose without consulting
// Cobra. Used by main() to decide error formatting BEFORE the cobra
// flag parser has run — a `seek -v --unknown` typo still needs the
// verbose contract honoured.
func hasVerboseArg(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-v" || a == "--verbose" || a == "-verbose" {
			return true
		}
	}
	return false
}

func exitCodeForError(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errNoMatch):
		return 1
	default:
		return 2
	}
}

// slogWriter bridges Go's standard log package to slog. Each log.Printf call
// becomes a single slog.Info message.
type slogWriter struct {
	logger *slog.Logger
}

func newSlogWriter(l *slog.Logger) *slogWriter {
	return &slogWriter{logger: l}
}

func (w *slogWriter) Write(p []byte) (int, error) {
	// Trim trailing newline added by log.Printf
	msg := string(p)
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	w.logger.Info(msg)
	return len(p), nil
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func run(ctx context.Context, pattern string, pathOperands []string, limit, maxMatches int) error {
	userQ, err := parseSearchQuery(pattern)
	if err != nil {
		return err
	}

	var paths *gitPaths
	resolvedPaths, gitErr := resolveGitPathsFromCWD(ctx)
	if gitErr == nil {
		paths = &resolvedPaths
	} else if len(pathOperands) == 0 {
		// gitErr from `git rev-parse` typically reads `exit status 128`,
		// which leaks an internal status code. Wrap with a hint at the
		// only useful remedy: pass a path operand.
		return fmt.Errorf("not in a git repository; specify a path to search (e.g. 'seek %q .')", pattern)
	}

	plans, err := planCorpora(ctx, paths, pathOperands)
	if err != nil {
		return err
	}

	worker := func(wctx context.Context, plan corpusPlan) ([]corpusSearchResult, dirtyFileSet, error) {
		return prepareAndSearchCorpus(wctx, plan, paths, userQ)
	}
	allResults, dirtyByCorpus, err := runCorpusPool(ctx, plans, worker)
	if err != nil {
		return err
	}

	if len(allResults) == 0 {
		return errNoMatch
	}

	// Formatting returns "" when all results were stale committed
	// matches for dirty files — treat as no match (exit code 1).
	//
	// Show corpus context when results come from more than one source.
	// Counting seeded plans alone misses corpora that the folder walker
	// discovered dynamically (nested git repos), so users searching a
	// single parent dir would see relative paths with no clue which
	// nested repo each match came from. Count unique corpusIDs in the
	// merged result set instead.
	displayMode := hideCorpusContext
	if len(plans) > 1 || corporaInResults(allResults) > 1 {
		displayMode = showCorpusContext
	}
	pal := plainPalette
	if useColor(os.Stdout) {
		pal = ansiPalette
	}
	output := formatCorpusResultsWithContext(allResults, dirtyByCorpus, limit, maxMatches, displayMode, pal)
	if output == "" {
		return errNoMatch
	}

	_, _ = os.Stdout.WriteString(output)
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

func prepareAndSearchCorpus(
	ctx context.Context,
	plan corpusPlan,
	paths *gitPaths,
	userQ query.Q,
) ([]corpusSearchResult, dirtyFileSet, error) {
	for attempt := 0; ; attempt++ {
		results, dirty, err := prepareAndSearchCorpusOnce(ctx, plan, paths, userQ)
		if errors.Is(err, errScopedLayerStateChanged) && plan.dirtyScope != nil && attempt < maxScopedLayerRefreshRetries {
			slog.Debug("scoped git layer changed during search; retrying", "root", plan.root, "attempt", attempt+1)
			continue
		}
		return results, dirty, err
	}
}

func prepareAndSearchCorpusOnce(
	ctx context.Context,
	plan corpusPlan,
	paths *gitPaths,
	userQ query.Q,
) ([]corpusSearchResult, dirtyFileSet, error) {
	var dirtyFiles dirtyFileSet
	var indexState corpusIndexState

	switch plan.kind {
	case corpusKindGit:
		planPaths := plan.gitPaths
		if planPaths == nil {
			planPaths = paths
		}
		if planPaths == nil {
			return nil, nil, fmt.Errorf("not a git repository")
		}
		var state repoState
		var readyState corpusIndexState
		if plan.dirtyScope != nil {
			var committed committedLayerResolution
			var err error
			state, committed, readyState, err = ensureScopedGitCorpusFresh(ctx, plan, *planPaths)
			if err != nil {
				return nil, nil, err
			}
			// Point the plan at the committed layer that was actually resolved
			// (shared whole-repo index, or the per-scope fallback when the repo
			// is over the index caps) so search/lock/validate use it.
			plan.committedCacheDir = committed.cacheDir
			plan.committedIndexDir = committed.indexDir
			plan.committedStateHash = committed.stateHash
			if len(state.Files) > 0 {
				plan.dirtyStateHash = gitCorpusStateHash(*planPaths, state)
			} else {
				// Clean in-scope tree: ensureScopedGitCorpusFresh elided the
				// dirty layer (no dir created). Drop the dirty paths so the
				// search/lock/validate sites skip the non-existent layer and
				// leave dirtyStateHash empty so validation skips it too.
				plan.dirtyCacheDir = ""
				plan.dirtyIndexDir = ""
			}
		} else {
			var err error
			state, readyState, err = ensureGitCorpusFresh(ctx, plan, *planPaths)
			if err != nil {
				return nil, nil, err
			}
		}
		indexState = readyState
		dirtyFiles = dirtyFileSetFromState(state)
	case corpusKindFolder:
		readyState, err := ensureFolderCorpusFresh(ctx, plan)
		if err != nil {
			if errors.Is(err, errFolderCapExceeded) {
				return nil, nil, err
			}
			if !shardsExist(plan.indexDir) {
				return nil, nil, err
			}
			slog.Warn("Indexing failed", "error", err, "root", plan.root)
		}
		indexState = readyState
	default:
		return nil, nil, fmt.Errorf("unsupported corpus kind: %s", plan.kind)
	}

	if indexState == corpusKnownEmpty {
		touchPlanUsed(plan)
		return nil, dirtyFiles, nil
	}

	files, err := searchPlannedCorpusParsed(ctx, plan, userQ)
	if err != nil {
		return nil, nil, err
	}
	touchPlanUsed(plan)
	return wrapCorpusResults(plan, files), dirtyFiles, nil
}

func touchPlanUsed(plan corpusPlan) {
	if plan.dirtyScope != nil {
		if plan.committedCacheDir != "" {
			touchUsed(plan.committedCacheDir)
		}
		// dirtyCacheDir is "" when the dirty layer was elided (clean in-scope
		// tree); guard so filepath.Join("", ".used") doesn't write in CWD.
		if plan.dirtyCacheDir != "" {
			touchUsed(plan.dirtyCacheDir)
		}
		return
	}
	touchUsed(plan.cacheDir)
}

// corporaInResults counts the distinct corpusIDs represented in the
// merged result set. Used to decide whether to render corpus-context
// prefixes — when discovery surfaces multiple nested git corpora under
// a single seeded folder operand, len(plans) alone reports 1 but the
// user still sees matches from N sources and needs them disambiguated.
func corporaInResults(results []corpusSearchResult) int {
	if len(results) == 0 {
		return 0
	}
	seen := make(map[corpusID]struct{}, 4)
	for _, r := range results {
		seen[r.corpusID] = struct{}{}
	}
	return len(seen)
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

func ensureGitCorpusFresh(ctx context.Context, plan corpusPlan, paths gitPaths) (repoState, corpusIndexState, error) {
	if plan.dirtyScope != nil {
		// The scoped path's committed-layer resolution is consumed directly by
		// prepareAndSearchCorpusOnce; this dispatch is for non-resolution
		// callers and discards it. WARNING: it therefore does NOT repoint the
		// plan's committed layer or zero the dirty paths on a clean-tree
		// elision. A caller that searches the returned plan must do that itself
		// (see prepareAndSearchCorpusOnce); otherwise search/lock would target
		// the resolved-away committed dir or a never-created dirty dir.
		state, _, indexState, err := ensureScopedGitCorpusFresh(ctx, plan, paths)
		return state, indexState, err
	}
	// Check for existing index state. If present, the cache directory
	// exists and one-time setup (ensureUntrackedCache,
	// ensureFSMonitor) was already applied. Skip MkdirAll + setup on the
	// warm path.
	cachedState := readStateFile(plan.cacheDir)
	if cachedState == "" {
		if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
			return repoState{}, corpusSearchable, fmt.Errorf("create index directory: %w", err)
		}
		ensureUntrackedCache(ctx, paths)
		ensureFSMonitor(ctx, paths)
	}

	state := gitRepoStateIn(ctx, paths.RepoDir)
	state = repoStateForDirtyScope(state, plan.dirtyScope)
	currentState := gitCorpusStateHash(paths, state)
	hasShards := shardsExist(plan.indexDir)
	if (currentState != cachedState || !hasShards) && gitCorpusKnownEmpty(ctx, paths, state) {
		if cachedState == currentState && !hasShards {
			return state, corpusKnownEmpty, nil
		}
		marked, err := markGitCorpusKnownEmpty(ctx, plan, state, currentState)
		if err != nil {
			return repoState{}, corpusSearchable, err
		}
		if marked {
			return state, corpusKnownEmpty, nil
		}
	}

	if currentState != cachedState || !hasShards {
		if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
			return repoState{}, corpusSearchable, fmt.Errorf("create index directory: %w", err)
		}
		if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, currentState); err != nil {
			if errors.Is(err, errGitCapExceeded) {
				return repoState{}, corpusSearchable, err
			}
			if !shardsExist(plan.indexDir) {
				return repoState{}, corpusSearchable, err
			}
			slog.Warn("Indexing failed", "error", err)
		}
	}
	return state, corpusSearchable, nil
}

func ensureScopedGitCorpusFresh(ctx context.Context, plan corpusPlan, paths gitPaths) (repoState, committedLayerResolution, corpusIndexState, error) {
	if plan.committedCacheDir == "" || plan.committedIndexDir == "" || plan.dirtyCacheDir == "" || plan.dirtyIndexDir == "" ||
		plan.sharedCommittedCacheDir == "" || plan.sharedCommittedIndexDir == "" {
		return repoState{}, committedLayerResolution{}, corpusSearchable, fmt.Errorf("scoped git corpus missing layer paths")
	}

	if readStateFile(plan.sharedCommittedCacheDir) == "" && readStateFile(plan.committedCacheDir) == "" && readStateFile(plan.dirtyCacheDir) == "" {
		ensureUntrackedCache(ctx, paths)
		ensureFSMonitor(ctx, paths)
	}
	state, err := gitRepoStateInScope(ctx, paths.RepoDir, plan.dirtyScope)
	if err != nil {
		return repoState{}, committedLayerResolution{}, corpusSearchable, err
	}
	committed, committedState, err := resolveCommittedLayer(ctx, plan, paths, state.HeadSHA)
	if err != nil {
		return repoState{}, committedLayerResolution{}, corpusSearchable, err
	}
	// Elide the dirty layer entirely when the in-scope working tree is clean:
	// there is nothing to index, so skip the MkdirAll + state-file writes that
	// would otherwise mint an empty per-scope dirty corpus dir on every scoped
	// search. prepareAndSearchCorpusOnce drops the dirty paths so search/lock
	// skip the non-existent layer.
	dirtyState := corpusKnownEmpty
	if len(state.Files) > 0 {
		dirtyState, err = ensureGitDirtyLayerFresh(ctx, plan, paths, state)
		if err != nil {
			return repoState{}, committedLayerResolution{}, corpusSearchable, err
		}
	}
	if committedState == corpusKnownEmpty && dirtyState == corpusKnownEmpty {
		return state, committed, corpusKnownEmpty, nil
	}
	return state, committed, corpusSearchable, nil
}

// committedLayerResolution names which committed layer a scoped search should
// load: the shared whole-repo index when it fits the caps, otherwise the
// per-scope fallback. The caller searches indexDir and validates stateHash.
type committedLayerResolution struct {
	cacheDir  string
	indexDir  string
	stateHash string
	shared    bool
}

// resolveCommittedLayer resolves the committed layer for a scoped search. It
// prefers the shared whole-repo committed index (one per repo, reused across
// scopes; filtered at search time via plan.scope) and falls back to the
// per-scope committed layer when the whole repo exceeds the index caps — so a
// huge tracked sibling cannot cap a small scope.
func resolveCommittedLayer(ctx context.Context, plan corpusPlan, paths gitPaths, headSHA string) (committedLayerResolution, corpusIndexState, error) {
	treeish := normalizeCommittedTreeish(headSHA)
	// Use the shared index unless a cap marker records that the whole repo is
	// over budget for THIS HEAD *and* the current cap limits; a HEAD change or
	// a cap change invalidates the marker so the budget is re-evaluated.
	if readGitCapMarker(plan.sharedCommittedCacheDir) != gitCapMarkerValue(treeish) {
		state, shared, err := ensureSharedCommittedLayerFresh(ctx, plan, paths, treeish)
		if err != nil {
			return committedLayerResolution{}, corpusSearchable, err
		}
		if shared {
			return committedLayerResolution{
				cacheDir:  plan.sharedCommittedCacheDir,
				indexDir:  plan.sharedCommittedIndexDir,
				stateHash: sharedCommittedLayerStateHash(paths, treeish),
				shared:    true,
			}, state, nil
		}
	}
	state, err := ensureScopedCommittedLayerFresh(ctx, plan, paths, headSHA)
	if err != nil {
		return committedLayerResolution{}, corpusSearchable, err
	}
	return committedLayerResolution{
		cacheDir:  plan.committedCacheDir,
		indexDir:  plan.committedIndexDir,
		stateHash: scopedCommittedLayerStateHash(paths, plan.dirtyScope, treeish),
		shared:    false,
	}, state, nil
}

// ensureSharedCommittedLayerFresh builds/reuses the whole-repo committed index.
// Returns shared=false (after recording a cap marker) when the repo exceeds
// the index caps, signalling the caller to use the per-scope fallback.
func ensureSharedCommittedLayerFresh(ctx context.Context, plan corpusPlan, paths gitPaths, treeish string) (corpusIndexState, bool, error) {
	cacheDir := plan.sharedCommittedCacheDir
	indexDir := plan.sharedCommittedIndexDir
	currentState := sharedCommittedLayerStateHash(paths, treeish)

	cachedState := readStateFile(cacheDir)
	hasShards := shardsExist(indexDir)
	if currentState == cachedState {
		if hasShards {
			return corpusSearchable, true, nil
		}
		if readEmptyStateFile(cacheDir) == currentState {
			return corpusKnownEmpty, true, nil
		}
	}

	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return corpusSearchable, false, fmt.Errorf("create shared committed index directory: %w", err)
	}

	lockPath := filepath.Join(cacheDir, lockFile)
	lockFd, err := acquireLockStrict(ctx, lockPath)
	if err != nil {
		return corpusSearchable, false, err
	}
	defer releaseLock(lockFd)
	defer func() {
		_ = os.Remove(filepath.Join(cacheDir, stateTmpFile))
	}()

	cachedState = readStateFile(cacheDir)
	hasShards = shardsExist(indexDir)
	if currentState == cachedState {
		if hasShards {
			return corpusSearchable, true, nil
		}
		if readEmptyStateFile(cacheDir) == currentState {
			return corpusKnownEmpty, true, nil
		}
	}

	repoName := fallbackGitRepositoryName(paths.RepoDir)
	if treeish == "no-head" {
		if err := validateCommittedHead(ctx, cacheDir, paths, treeish); err != nil {
			return corpusSearchable, false, err
		}
		cleanRepositoryShards(indexDir, repoName)
		if err := writeCommittedLayerState(cacheDir, treeish, currentState, true); err != nil {
			return corpusSearchable, false, err
		}
		return corpusKnownEmpty, true, nil
	}

	if _, err := scanGitCommittedIndexBudget(ctx, paths.RepoDir, gitCandidateFileLimit, gitCorpusIndexedByteLimit); err != nil {
		if errors.Is(err, errGitCapExceeded) {
			// The budget scan reads live HEAD; confirm HEAD still equals the
			// treeish we captured before pinning an over-cap verdict to it. A
			// HEAD move during the scan returns errScopedLayerStateChanged so
			// the refresh retries against the new HEAD instead of recording a
			// cap marker for the wrong treeish (which would persistently lose
			// the shared-index optimization until the next HEAD change).
			if verr := validateCommittedHead(ctx, cacheDir, paths, treeish); verr != nil {
				return corpusSearchable, false, verr
			}
			// Whole repo over budget: clear any stale shared shards, record the
			// cap for this HEAD, and let the caller use the per-scope layer.
			cleanRepositoryShards(indexDir, repoName)
			deleteStateFiles(cacheDir)
			if markErr := writeGitCapMarker(cacheDir, gitCapMarkerValue(treeish)); markErr != nil {
				// The cap marker is a best-effort optimization cache; failing to
				// write it (read-only / full cache dir) must not fail a search
				// the per-scope fallback can serve. Log and fall through.
				slog.Warn("write git cap marker", "error", markErr, "dir", cacheDir)
			}
			return corpusSearchable, false, nil
		}
		return corpusSearchable, false, gitCorpusError(paths.RepoDir, indexDir, err)
	}
	if err := checkCtagsCached(); err != nil {
		deleteStateFiles(cacheDir)
		return corpusSearchable, false, gitCorpusError(paths.RepoDir, indexDir, err)
	}
	if err := indexCommitted(paths.RepoDir, indexDir, indexParallelism()); err != nil {
		deleteStateFiles(cacheDir)
		return corpusSearchable, false, gitCorpusError(paths.RepoDir, indexDir, err)
	}
	if err := validateCommittedHead(ctx, cacheDir, paths, treeish); err != nil {
		return corpusSearchable, false, err
	}
	removeGitCapMarker(cacheDir)
	if !shardsExist(indexDir) {
		if err := writeCommittedLayerState(cacheDir, treeish, currentState, true); err != nil {
			return corpusSearchable, false, err
		}
		return corpusKnownEmpty, true, nil
	}
	if err := writeCommittedLayerState(cacheDir, treeish, currentState, false); err != nil {
		return corpusSearchable, false, err
	}
	return corpusSearchable, true, nil
}

func ensureScopedCommittedLayerFresh(ctx context.Context, plan corpusPlan, paths gitPaths, headSHA string) (corpusIndexState, error) {
	treeish := normalizeCommittedTreeish(headSHA)
	currentState := scopedCommittedLayerStateHash(paths, plan.dirtyScope, treeish)
	cachedState := readStateFile(plan.committedCacheDir)
	hasShards := shardsExist(plan.committedIndexDir)
	if currentState == cachedState {
		if hasShards {
			return corpusSearchable, nil
		}
		if readEmptyStateFile(plan.committedCacheDir) == currentState {
			return corpusKnownEmpty, nil
		}
	}

	if cachedState == "" {
		if err := os.MkdirAll(plan.committedIndexDir, 0o755); err != nil {
			return corpusSearchable, fmt.Errorf("create committed index directory: %w", err)
		}
		ensureUntrackedCache(ctx, paths)
		ensureFSMonitor(ctx, paths)
	}

	if err := os.MkdirAll(plan.committedIndexDir, 0o755); err != nil {
		return corpusSearchable, fmt.Errorf("create committed index directory: %w", err)
	}

	lockPath := filepath.Join(plan.committedCacheDir, lockFile)
	lockFd, err := acquireLockStrict(ctx, lockPath)
	if err != nil {
		return corpusSearchable, err
	}
	defer releaseLock(lockFd)
	defer func() {
		_ = os.Remove(filepath.Join(plan.committedCacheDir, stateTmpFile))
	}()

	cachedState = readStateFile(plan.committedCacheDir)
	hasShards = shardsExist(plan.committedIndexDir)
	if currentState == cachedState {
		if hasShards {
			return corpusSearchable, nil
		}
		if readEmptyStateFile(plan.committedCacheDir) == currentState {
			return corpusKnownEmpty, nil
		}
	}
	if state, reused, err := reuseScopedCommittedLayerAfterOutOfScopeCommit(ctx, plan, paths, cachedState, currentState, treeish, hasShards); err != nil {
		return corpusSearchable, err
	} else if reused {
		return state, nil
	}

	repoName := fallbackGitRepositoryName(paths.RepoDir)
	if treeish == "no-head" {
		if err := validateScopedCommittedHead(ctx, plan, paths, treeish); err != nil {
			return corpusSearchable, err
		}
		cleanRepositoryShards(plan.committedIndexDir, repoName)
		if err := writeCommittedLayerState(plan.committedCacheDir, treeish, currentState, true); err != nil {
			return corpusSearchable, err
		}
		return corpusKnownEmpty, nil
	}

	_, selected, err := scanGitCommittedScopeBudgetAt(ctx, paths.RepoDir, treeish, plan.dirtyScope, gitCandidateFileLimit, gitCorpusIndexedByteLimit)
	if err != nil {
		deleteStateFiles(plan.committedCacheDir)
		return corpusSearchable, gitCorpusError(paths.RepoDir, plan.committedIndexDir, err)
	}
	if selected == 0 {
		if err := validateScopedCommittedHead(ctx, plan, paths, treeish); err != nil {
			return corpusSearchable, err
		}
		cleanRepositoryShards(plan.committedIndexDir, repoName)
		if err := writeCommittedLayerState(plan.committedCacheDir, treeish, currentState, true); err != nil {
			return corpusSearchable, err
		}
		return corpusKnownEmpty, nil
	}
	if err := checkCtagsCached(); err != nil {
		deleteStateFiles(plan.committedCacheDir)
		return corpusSearchable, gitCorpusError(paths.RepoDir, plan.committedIndexDir, err)
	}
	indexedAny, err := indexScopedCommitted(ctx, paths.RepoDir, plan.committedIndexDir, treeish, plan.dirtyScope, indexParallelism())
	if err != nil {
		deleteStateFiles(plan.committedCacheDir)
		return corpusSearchable, gitCorpusError(paths.RepoDir, plan.committedIndexDir, err)
	}
	if err := validateScopedCommittedHead(ctx, plan, paths, treeish); err != nil {
		return corpusSearchable, err
	}
	if !indexedAny {
		if err := writeCommittedLayerState(plan.committedCacheDir, treeish, currentState, true); err != nil {
			return corpusSearchable, err
		}
		return corpusKnownEmpty, nil
	}
	if err := writeCommittedLayerState(plan.committedCacheDir, treeish, currentState, false); err != nil {
		return corpusSearchable, err
	}
	return corpusSearchable, nil
}

func reuseScopedCommittedLayerAfterOutOfScopeCommit(
	ctx context.Context,
	plan corpusPlan,
	paths gitPaths,
	cachedState,
	currentState,
	treeish string,
	hasShards bool,
) (corpusIndexState, bool, error) {
	if cachedState == "" || currentState == cachedState || treeish == "no-head" {
		return corpusSearchable, false, nil
	}
	cachedEmpty := readEmptyStateFile(plan.committedCacheDir) == cachedState
	if !hasShards && !cachedEmpty {
		return corpusSearchable, false, nil
	}
	cachedHead := readHeadFile(plan.committedCacheDir)
	if cachedHead == "" || cachedHead == treeish || cachedHead == "no-head" {
		return corpusSearchable, false, nil
	}
	unchanged, err := scopedCommittedTreeUnchanged(ctx, paths.RepoDir, cachedHead, treeish, plan.dirtyScope)
	if err != nil {
		return corpusSearchable, false, nil
	}
	if !unchanged {
		return corpusSearchable, false, nil
	}
	if err := validateScopedCommittedHead(ctx, plan, paths, treeish); err != nil {
		return corpusSearchable, false, err
	}
	if err := writeCommittedLayerState(plan.committedCacheDir, treeish, currentState, cachedEmpty); err != nil {
		return corpusSearchable, false, err
	}
	if cachedEmpty {
		return corpusKnownEmpty, true, nil
	}
	return corpusSearchable, true, nil
}

func scopedCommittedTreeUnchanged(ctx context.Context, repoDir, oldTreeish, newTreeish string, scope *gitDirtyScope) (bool, error) {
	args := []string{"diff-tree", "--quiet", "-r", oldTreeish, newTreeish, "--"}
	if scope != nil {
		args = append(args, scope.gitIncludePathspecs()...)
	}
	cmd := gitCmd(ctx, args...)
	cmd.Dir = repoDir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg != "" {
		return false, fmt.Errorf("git scoped committed diff-tree: %w: %s", err, msg)
	}
	return false, fmt.Errorf("git scoped committed diff-tree: %w", err)
}

// writeCommittedLayerState persists the head + state (+ optional empty marker)
// for a committed layer's cache dir, shared by the shared whole-repo and the
// per-scope committed builders.
func writeCommittedLayerState(cacheDir, treeish, stateHash string, empty bool) error {
	if treeish == "no-head" {
		_ = os.Remove(filepath.Join(cacheDir, headFile))
		_ = os.Remove(filepath.Join(cacheDir, headFile+".tmp"))
	} else if err := writeHeadFile(cacheDir, treeish); err != nil {
		return fmt.Errorf("write committed head file: %w", err)
	}
	if err := writeStateFile(cacheDir, stateHash); err != nil {
		return fmt.Errorf("write committed state file: %w", err)
	}
	if empty {
		if err := writeEmptyStateFile(cacheDir, stateHash); err != nil {
			return fmt.Errorf("write committed empty marker: %w", err)
		}
		return nil
	}
	deleteEmptyStateFiles(cacheDir)
	return nil
}

func scopedCommittedLayerStateHash(paths gitPaths, scope *gitDirtyScope, headSHA string) string {
	treeish := normalizeCommittedTreeish(headSHA)
	committedState := repoState{
		HeadSHA: treeish,
		RawOutput: stringsJoinNUL(
			"git-scoped-committed-layer-v1",
			"head", treeish,
			"scope", scope.key,
		),
	}
	return gitCorpusStateHash(paths, committedState)
}

// sharedCommittedLayerStateHash keys the whole-repo committed index by HEAD
// only (no scope), so every scope of one repo shares a single committed dir.
func sharedCommittedLayerStateHash(paths gitPaths, headSHA string) string {
	treeish := normalizeCommittedTreeish(headSHA)
	return gitCorpusStateHash(paths, repoState{
		HeadSHA: treeish,
		RawOutput: stringsJoinNUL(
			"git-shared-committed-layer-v1",
			"head", treeish,
		),
	})
}

// gitCapMarkerFile records, in the shared committed cache dir, that the
// whole-repo tree exceeded the index caps. Its payload (gitCapMarkerValue) is
// the committed treeish plus the cap limits in effect. While the marker matches
// the current HEAD *and* the current cap limits, scoped searches skip the
// shared index (and its budget re-scan) and use the per-scope committed layer,
// so a huge repo cannot cap a small scope. A HEAD change *or* a cap-limit
// change makes the marker stale and the budget is re-evaluated.
const gitCapMarkerFile = ".git_cap_exceeded"

func readGitCapMarker(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return readCacheFile(cacheDir, gitCapMarkerFile)
}

func writeGitCapMarker(cacheDir, value string) error {
	return writeCacheFile(cacheDir, gitCapMarkerFile, value)
}

// gitCapMarkerValue is the cap-marker payload: the committed treeish plus the
// cap limits that produced the over-cap verdict. Folding the limits in means a
// later cap change (e.g. a new binary with a larger compiled limit) no longer
// matches a stale marker, so the shared index is re-evaluated instead of
// staying pinned to the per-scope fallback at that HEAD.
func gitCapMarkerValue(treeish string) string {
	return fmt.Sprintf("%s\x00%d\x00%d", treeish, gitCandidateFileLimit, gitCorpusIndexedByteLimit)
}

func removeGitCapMarker(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, gitCapMarkerFile))
}

func normalizeCommittedTreeish(headSHA string) string {
	if headSHA == "" || headSHA == "no-head" || headSHA == "(initial)" {
		return "no-head"
	}
	return headSHA
}

func validateScopedCommittedHead(ctx context.Context, plan corpusPlan, paths gitPaths, expectedTreeish string) error {
	return validateCommittedHead(ctx, plan.committedCacheDir, paths, expectedTreeish)
}

// validateCommittedHead is the TOCTOU guard shared by the per-scope and
// whole-repo committed builds: HEAD must still equal the treeish indexed,
// else the just-written shards are stale and its state files are deleted.
func validateCommittedHead(ctx context.Context, cacheDir string, paths gitPaths, expectedTreeish string) error {
	currentTreeish, err := gitHeadTreeish(ctx, paths.RepoDir)
	if err != nil {
		deleteStateFiles(cacheDir)
		return err
	}
	if normalizeCommittedTreeish(currentTreeish) != normalizeCommittedTreeish(expectedTreeish) {
		deleteStateFiles(cacheDir)
		return errScopedLayerStateChanged
	}
	return nil
}

func ensureGitDirtyLayerFresh(ctx context.Context, plan corpusPlan, paths gitPaths, state repoState) (corpusIndexState, error) {
	currentState := gitCorpusStateHash(paths, state)
	cachedState := readStateFile(plan.dirtyCacheDir)
	hasShards := shardsExist(plan.dirtyIndexDir)
	if currentState == cachedState {
		if !hasShards && readEmptyStateFile(plan.dirtyCacheDir) == currentState {
			return corpusKnownEmpty, nil
		}
		if hasShards {
			return corpusSearchable, nil
		}
	}
	if err := os.MkdirAll(plan.dirtyIndexDir, 0o755); err != nil {
		return corpusSearchable, fmt.Errorf("create dirty index directory: %w", err)
	}
	if err := checkCtagsCached(); err != nil && gitDirtyFilesHaveIndexableDocuments(paths.RepoDir, state.Files) {
		deleteStateFiles(plan.dirtyCacheDir)
		return corpusSearchable, gitCorpusError(paths.RepoDir, plan.dirtyIndexDir, err)
	}

	lockPath := filepath.Join(plan.dirtyCacheDir, lockFile)
	lockFd, err := acquireLockStrict(ctx, lockPath)
	if err != nil {
		return corpusSearchable, err
	}
	defer releaseLock(lockFd)

	cachedState = readStateFile(plan.dirtyCacheDir)
	hasShards = shardsExist(plan.dirtyIndexDir)
	if currentState == cachedState {
		if !hasShards && readEmptyStateFile(plan.dirtyCacheDir) == currentState {
			return corpusKnownEmpty, nil
		}
		if hasShards {
			return corpusSearchable, nil
		}
	}

	if err := checkGitDirtyFileBudget(paths.RepoDir, plan.dirtyIndexDir, state.Files); err != nil {
		deleteStateFiles(plan.dirtyCacheDir)
		return corpusSearchable, err
	}
	if err := indexUncommitted(ctx, paths.RepoDir, plan.dirtyIndexDir, plan.dirtyCacheDir, state, cachedState, currentState, indexParallelism()); err != nil {
		deleteStateFiles(plan.dirtyCacheDir)
		if errors.Is(err, errGitCapExceeded) {
			return corpusSearchable, err
		}
		return corpusSearchable, err
	}
	postRepoState, err := gitRepoStateInScope(ctx, paths.RepoDir, plan.dirtyScope)
	if err != nil {
		deleteStateFiles(plan.dirtyCacheDir)
		return corpusSearchable, err
	}
	postState := gitCorpusStateHash(paths, postRepoState)
	if postState != currentState {
		deleteStateFiles(plan.dirtyCacheDir)
		return corpusSearchable, errScopedLayerStateChanged
	}
	if err := writeStateFile(plan.dirtyCacheDir, currentState); err != nil {
		return corpusSearchable, fmt.Errorf("write dirty state file: %w", err)
	}
	deleteEmptyStateFiles(plan.dirtyCacheDir)
	if !shardsExist(plan.dirtyIndexDir) {
		if err := writeEmptyStateFile(plan.dirtyCacheDir, currentState); err != nil {
			return corpusSearchable, fmt.Errorf("write dirty empty marker: %w", err)
		}
		return corpusKnownEmpty, nil
	}
	return corpusSearchable, nil
}

func gitDirtyFilesHaveIndexableDocuments(repoDir string, files []string) bool {
	for _, name := range files {
		info, err := os.Lstat(filepath.Join(repoDir, name))
		if err == nil && info.Mode().IsRegular() && info.Size() <= maxGitDirtyFileSize {
			return true
		}
	}
	return false
}

func repoStateForDirtyScope(state repoState, scope *gitDirtyScope) repoState {
	if scope == nil {
		return state
	}
	files := make([]string, 0, len(state.Files))
	for _, file := range state.Files {
		if scope.contains(file) {
			files = append(files, file)
		}
	}
	var b strings.Builder
	b.Grow(len(scope.key) + len(state.HeadSHA) + len(files)*32)
	b.WriteString("scoped-git-status-v1\x00head\x00")
	b.WriteString(state.HeadSHA)
	b.WriteString("\x00scope\x00")
	b.WriteString(scope.key)
	for _, file := range files {
		b.WriteString("\x00file\x00")
		b.WriteString(file)
	}
	return repoState{
		HeadSHA:   state.HeadSHA,
		RawOutput: b.String(),
		Files:     files,
	}
}

func gitCorpusKnownEmpty(ctx context.Context, paths gitPaths, state repoState) bool {
	if len(state.Files) > 0 {
		return false
	}
	if state.HeadSHA == "no-head" {
		return true
	}

	cmd := gitCmd(ctx, "rev-parse", "HEAD^{tree}")
	cmd.Dir = paths.RepoDir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == emptyGitTreeSHA
}

func markGitCorpusKnownEmpty(ctx context.Context, plan corpusPlan, state repoState, stateHash string) (bool, error) {
	if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
		return false, fmt.Errorf("create index directory: %w", err)
	}

	lockPath := filepath.Join(plan.cacheDir, lockFile)
	lockFd, acquired, err := acquireLock(ctx, plan.indexDir, lockPath)
	if err != nil {
		return false, err
	}
	if !acquired {
		slog.Warn("Another process is indexing, using existing index")
		return false, nil
	}
	defer releaseLock(lockFd)

	cleanAllShards(plan.indexDir)
	if state.HeadSHA == "no-head" {
		_ = os.Remove(filepath.Join(plan.cacheDir, headFile))
		_ = os.Remove(filepath.Join(plan.cacheDir, headFile+".tmp"))
	} else if err := writeHeadFile(plan.cacheDir, state.HeadSHA); err != nil {
		slog.Warn("Failed to write head file", "error", err)
	}
	if err := writeStateFile(plan.cacheDir, stateHash); err != nil {
		return false, fmt.Errorf("write state file: %w", err)
	}
	return true, nil
}

func searchPlannedCorpusParsed(ctx context.Context, plan corpusPlan, userQ query.Q) ([]zoekt.FileMatch, error) {
	// Execute search with LOCK_SH so concurrent indexers (which hold LOCK_EX)
	// finish before we read shards. Multiple searchers can hold LOCK_SH
	// simultaneously — no contention between readers. Uses non-blocking
	// poll with timeout to prevent indefinite hang if an indexer is stuck.
	targets := searchLockTargets(plan)
	locks := make([]*os.File, 0, len(targets))
	for _, target := range targets {
		searchLockPath := filepath.Join(target.cacheDir, lockFile)
		searchLockFd, err := os.OpenFile(searchLockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			closeSearchLocks(locks)
			return nil, fmt.Errorf("open search lock: %w", err)
		}
		var lockErr error
		if plan.dirtyScope != nil {
			lockErr = acquireSearchLockStrict(ctx, searchLockFd)
		} else {
			lockErr = acquireSearchLock(ctx, target.indexDir, searchLockFd)
		}
		if lockErr != nil {
			_ = searchLockFd.Close()
			closeSearchLocks(locks)
			return nil, fmt.Errorf("acquire search lock: %w", lockErr)
		}
		locks = append(locks, searchLockFd)
	}
	defer closeSearchLocks(locks)

	if err := validateScopedSearchLayerStates(plan); err != nil {
		return nil, err
	}

	results, err := executeParsedSearchScopedDirs(ctx, searchIndexDirs(plan), userQ, plan.scope)
	if err != nil {
		return nil, err
	}
	return results, nil
}

func validateScopedSearchLayerStates(plan corpusPlan) error {
	if plan.dirtyScope == nil {
		return nil
	}
	if plan.committedStateHash != "" && !scopedLayerStateMatches(plan.committedCacheDir, plan.committedIndexDir, plan.committedStateHash) {
		return errScopedLayerStateChanged
	}
	if plan.dirtyStateHash != "" && !scopedLayerStateMatches(plan.dirtyCacheDir, plan.dirtyIndexDir, plan.dirtyStateHash) {
		return errScopedLayerStateChanged
	}
	return nil
}

func scopedLayerStateMatches(cacheDir, indexDir, expected string) bool {
	if expected == "" || readStateFile(cacheDir) != expected {
		return false
	}
	if shardsExist(indexDir) {
		return true
	}
	return readEmptyStateFile(cacheDir) == expected
}

type searchLockTarget struct {
	cacheDir string
	indexDir string
}

func searchLockTargets(plan corpusPlan) []searchLockTarget {
	if plan.dirtyScope != nil {
		targets := make([]searchLockTarget, 0, 2)
		if plan.committedCacheDir != "" && plan.committedIndexDir != "" {
			targets = append(targets, searchLockTarget{cacheDir: plan.committedCacheDir, indexDir: plan.committedIndexDir})
		}
		if plan.dirtyCacheDir != "" && plan.dirtyIndexDir != "" {
			targets = append(targets, searchLockTarget{cacheDir: plan.dirtyCacheDir, indexDir: plan.dirtyIndexDir})
		}
		return targets
	}
	return []searchLockTarget{{cacheDir: plan.cacheDir, indexDir: plan.indexDir}}
}

func closeSearchLocks(locks []*os.File) {
	for _, lock := range locks {
		unlockFile(lock)
		_ = lock.Close()
	}
}

func searchIndexDirs(plan corpusPlan) []string {
	if plan.dirtyScope != nil {
		dirs := make([]string, 0, 2)
		if plan.committedIndexDir != "" {
			dirs = append(dirs, plan.committedIndexDir)
		}
		if plan.dirtyIndexDir != "" {
			dirs = append(dirs, plan.dirtyIndexDir)
		}
		return dirs
	}
	return []string{plan.indexDir}
}
