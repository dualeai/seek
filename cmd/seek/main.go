package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
		// One combined whole-repo index serves both scoped and unscoped
		// searches; scope is applied at search time. ensureGitCorpusFresh may
		// set plan.scoped* (the over-cap fallback) via the pointer, which the
		// search/lock/validate/touch sites below then target.
		state, readyState, err := ensureGitCorpusFresh(ctx, &plan, *planPaths)
		if err != nil {
			return nil, nil, err
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
	// The over-cap fallback dir is the served layer when it was built; otherwise
	// the combined dir is warmed by BOTH scoped and unscoped searches (so age
	// based GC never evicts the one index every scope depends on).
	if plan.scopedStateHash != "" {
		touchUsed(plan.scopedCacheDir)
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

// ensureGitCorpusFresh makes the one combined whole-repo index current and, for
// a scoped search whose whole repo is over the index caps, builds and selects
// the per-scope over-cap fallback instead (so a huge tracked sibling cannot cap
// a small scope). It mutates *plan to point search/lock/validate at the fallback
// (plan.scopedStateHash != "") when that path is taken.
func ensureGitCorpusFresh(ctx context.Context, plan *corpusPlan, paths gitPaths) (repoState, corpusIndexState, error) {
	state := gitRepoStateIn(ctx, paths.RepoDir)
	treeish := normalizeCommittedTreeish(state.HeadSHA)

	// Scoped over-cap fast path: a cap marker for this HEAD + cap limits records
	// that the whole repo is over budget, so skip the combined build's budget
	// rescan and serve the per-scope fallback directly. The marker self-
	// invalidates on a HEAD change or a cap-limit change.
	if plan.dirtyScope != nil && readGitCapMarker(plan.cacheDir) == gitCapMarkerValue(treeish) {
		return ensureScopedGitCorpusFallback(ctx, plan, paths, state)
	}

	indexState, err := ensureCombinedGitCorpus(ctx, *plan, paths, state)
	if err != nil {
		if errors.Is(err, errGitCapExceeded) && plan.dirtyScope != nil {
			// Confirm HEAD didn't move during the (live-HEAD) budget scan before
			// pinning an over-cap verdict to this treeish, then record the cap
			// (best-effort) and serve the per-scope fallback.
			if verr := validateCommittedHead(ctx, plan.cacheDir, paths, treeish); verr != nil {
				return repoState{}, corpusSearchable, verr
			}
			if markErr := writeGitCapMarker(plan.cacheDir, gitCapMarkerValue(treeish)); markErr != nil {
				// Best-effort optimization cache; a read-only/full cache dir must
				// not fail a search the fallback can serve. Log and continue.
				slog.Warn("Failed to write git cap marker", "error", markErr, "cache_dir", plan.cacheDir)
			}
			return ensureScopedGitCorpusFallback(ctx, plan, paths, state)
		}
		return repoState{}, corpusSearchable, err
	}
	// Under-cap success clears any stale over-cap marker (repo shrank at HEAD).
	if plan.dirtyScope != nil {
		removeGitCapMarker(plan.cacheDir)
	}
	return state, indexState, nil
}

// ensureCombinedGitCorpus refreshes the single combined committed+dirty
// whole-repo index that serves both scoped and unscoped searches. Returns
// errGitCapExceeded when the whole repo is over the index caps.
func ensureCombinedGitCorpus(ctx context.Context, plan corpusPlan, paths gitPaths, state repoState) (corpusIndexState, error) {
	// Check for existing index state. If present, the cache directory
	// exists and one-time setup (ensureUntrackedCache, ensureFSMonitor) was
	// already applied. Skip MkdirAll + setup on the warm path.
	cachedState := readStateFile(plan.cacheDir)
	if cachedState == "" {
		if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
			return corpusSearchable, fmt.Errorf("create index directory: %w", err)
		}
		ensureUntrackedCache(ctx, paths)
		ensureFSMonitor(ctx, paths)
	}

	currentState := gitCorpusStateHash(paths, state)
	hasShards := shardsExist(plan.indexDir)
	// A leftover .swapping marker means a prior publish was interrupted and the
	// shards may be torn; force the build path so recoverIncompleteSwap runs even
	// when state otherwise looks current.
	swapPending := readCacheFile(plan.cacheDir, swappingMarkerFile) != ""
	needBuild := currentState != cachedState || !hasShards || swapPending
	if (currentState != cachedState || !hasShards) && gitCorpusKnownEmpty(ctx, paths, state) {
		if cachedState == currentState && !hasShards {
			return corpusKnownEmpty, nil
		}
		marked, err := markGitCorpusKnownEmpty(ctx, plan, state, currentState)
		if err != nil {
			return corpusSearchable, err
		}
		if marked {
			return corpusKnownEmpty, nil
		}
	}

	if needBuild {
		if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
			return corpusSearchable, fmt.Errorf("create index directory: %w", err)
		}
		if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, currentState); err != nil {
			if errors.Is(err, errGitCapExceeded) {
				return corpusSearchable, err
			}
			if !shardsExist(plan.indexDir) {
				return corpusSearchable, err
			}
			slog.Warn("Indexing failed", "error", err)
		}
	}
	return corpusSearchable, nil
}

// ensureScopedGitCorpusFallback builds (and selects, via plan.scopedStateHash)
// the over-cap fallback: a single per-scope index holding the in-scope committed
// tree (pinned treeish) plus the in-scope dirty working files, in one dir under
// one lock. Only reached when the whole repo is over the index caps, so a huge
// tracked sibling cannot cap a small scope. Rebuilt wholesale whenever HEAD, the
// scope, or the in-scope working tree changes (over-cap is rare; no delta).
func ensureScopedGitCorpusFallback(ctx context.Context, plan *corpusPlan, paths gitPaths, state repoState) (repoState, corpusIndexState, error) {
	if plan.scopedCacheDir == "" || plan.scopedIndexDir == "" {
		return repoState{}, corpusSearchable, fmt.Errorf("scoped git corpus missing fallback paths")
	}
	cacheDir := plan.scopedCacheDir
	indexDir := plan.scopedIndexDir
	treeish := normalizeCommittedTreeish(state.HeadSHA)
	scopedState := repoStateForDirtyScope(state, plan.dirtyScope)
	currentState := scopedFallbackStateHash(paths, plan.dirtyScope, state)

	if readStateFile(cacheDir) == "" {
		ensureUntrackedCache(ctx, paths)
		ensureFSMonitor(ctx, paths)
	}

	// Fast path: state matches and shards (or an empty marker) are present. A
	// leftover .swapping marker means a prior publish was interrupted and the
	// family may be torn — skip the fast path so the build lock + recovery run.
	if readCacheFile(cacheDir, swappingMarkerFile) == "" {
		if st, ok := scopedFallbackCached(cacheDir, indexDir, currentState); ok {
			plan.scopedStateHash = currentState
			return state, st, nil
		}
	}

	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return repoState{}, corpusSearchable, fmt.Errorf("create scoped fallback index directory: %w", err)
	}
	buildFd, acquired, err := acquireBuildLock(ctx, cacheDir, indexDir)
	if err != nil {
		return repoState{}, corpusSearchable, err
	}
	if !acquired {
		// Another builder is active and a usable fallback index exists — serve
		// the currently published generation (route search to the fallback dir
		// by reporting its on-disk state).
		slog.Debug("Another process is indexing; serving current index")
		onDisk := readStateFile(cacheDir)
		if onDisk == "" {
			onDisk = currentState
		}
		plan.scopedStateHash = onDisk
		return state, corpusSearchable, nil
	}
	defer releaseLock(buildFd)
	defer func() { _ = os.Remove(filepath.Join(cacheDir, stateTmpFile)) }()

	recoverIncompleteSwap(cacheDir, indexDir)

	// Re-check under the build lock (another process may have built it).
	if st, ok := scopedFallbackCached(cacheDir, indexDir, currentState); ok {
		plan.scopedStateHash = currentState
		return state, st, nil
	}

	// Wholesale rebuild into a temp dir (committed git-* + uncommitted_*), then
	// publish atomically. The fallback always force-rebuilds uncommitted
	// (cachedState ""), so there is no delta baseline to seed.
	buildDir, err := newBuildDir(indexDir)
	if err != nil {
		return repoState{}, corpusSearchable, err
	}
	defer discardBuildDir(buildDir)

	if treeish != "no-head" {
		_, selected, err := scanGitCommittedScopeBudgetAt(ctx, paths.RepoDir, treeish, plan.dirtyScope, gitCandidateFileLimit, gitCorpusIndexedByteLimit)
		if err != nil {
			deleteStateFiles(cacheDir)
			return repoState{}, corpusSearchable, gitCorpusError(paths.RepoDir, indexDir, err)
		}
		if selected > 0 {
			if err := checkCtagsCached(); err != nil {
				deleteStateFiles(cacheDir)
				return repoState{}, corpusSearchable, gitCorpusError(paths.RepoDir, indexDir, err)
			}
			if _, err := indexScopedCommitted(ctx, paths.RepoDir, buildDir, treeish, plan.dirtyScope, indexParallelism()); err != nil {
				deleteStateFiles(cacheDir)
				return repoState{}, corpusSearchable, gitCorpusError(paths.RepoDir, indexDir, err)
			}
		}
	}

	if len(scopedState.Files) > 0 {
		if err := checkGitDirtyFileBudget(paths.RepoDir, buildDir, scopedState.Files); err != nil {
			deleteStateFiles(cacheDir)
			return repoState{}, corpusSearchable, err
		}
		if err := checkCtagsCached(); err != nil && gitDirtyFilesHaveIndexableDocuments(paths.RepoDir, scopedState.Files) {
			deleteStateFiles(cacheDir)
			return repoState{}, corpusSearchable, gitCorpusError(paths.RepoDir, indexDir, err)
		}
		if err := indexUncommitted(ctx, paths.RepoDir, buildDir, cacheDir, scopedState, "", currentState, indexParallelism()); err != nil {
			deleteStateFiles(cacheDir)
			return repoState{}, corpusSearchable, err
		}
	}

	// validate-before-publish: HEAD must still equal the treeish indexed.
	if err := validateCommittedHead(ctx, cacheDir, paths, treeish); err != nil {
		return repoState{}, corpusSearchable, err // discard (defer)
	}

	// Publish the whole fallback generation atomically — shards + state under one
	// publish-lock hold (see publishGeneration: avoids serving new shards with a
	// stale-matching state label after a crash + content revert).
	if err := publishGeneration(ctx, cacheDir, indexDir, buildDir, familyAll, func() error {
		return writeCommittedLayerState(cacheDir, treeish, currentState, !shardsExist(indexDir))
	}); err != nil {
		if errors.Is(err, errCorpusEvicted) {
			return state, corpusSearchable, nil
		}
		return repoState{}, corpusSearchable, err
	}

	plan.scopedStateHash = currentState
	if !shardsExist(indexDir) {
		return state, corpusKnownEmpty, nil
	}
	return state, corpusSearchable, nil
}

// scopedFallbackCached reports whether the fallback dir already holds the
// requested state (searchable shards, or a recorded empty marker).
func scopedFallbackCached(cacheDir, indexDir, currentState string) (corpusIndexState, bool) {
	if readStateFile(cacheDir) != currentState {
		return corpusSearchable, false
	}
	if shardsExist(indexDir) {
		return corpusSearchable, true
	}
	if readEmptyStateFile(cacheDir) == currentState {
		return corpusKnownEmpty, true
	}
	return corpusSearchable, false
}

// scopedFallbackStateHash keys the over-cap fallback by HEAD treeish, scope, and
// the in-scope working-tree state, so a commit, a scope change, or an in-scope
// edit rebuilds it. repoStateForDirtyScope already encodes head+scope+files.
func scopedFallbackStateHash(paths gitPaths, scope *gitDirtyScope, state repoState) string {
	scoped := repoStateForDirtyScope(state, scope)
	return gitCorpusStateHash(paths, repoState{
		HeadSHA: normalizeCommittedTreeish(state.HeadSHA),
		RawOutput: stringsJoinNUL(
			"git-overcap-fallback-v1",
			scoped.RawOutput,
		),
	})
}

// writeCommittedLayerState persists the head + state (+ optional empty marker)
// for the over-cap per-scope fallback's cache dir.
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

// gitCapMarkerFile records, in the combined corpus cache dir, that the
// whole-repo tree exceeded the index caps. Its payload (gitCapMarkerValue) is
// the committed treeish plus the cap limits in effect. While the marker matches
// the current HEAD *and* the current cap limits, scoped searches skip the
// combined index (and its budget re-scan) and use the per-scope combined
// fallback, so a huge repo cannot cap a small scope. A HEAD change *or* a
// cap-limit change makes the marker stale and the budget is re-evaluated.
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

	buildFd, acquired, err := acquireBuildLock(ctx, plan.cacheDir, plan.indexDir)
	if err != nil {
		return false, err
	}
	if !acquired {
		slog.Debug("Another process is indexing; serving current index")
		return false, nil
	}
	defer releaseLock(buildFd)

	// Clear shards under the publish lock so readers (SH) never see the empty
	// dir mid-clean.
	pub, err := acquirePublishLock(ctx, plan.cacheDir)
	if err != nil {
		if errors.Is(err, errCorpusEvicted) {
			return false, nil
		}
		return false, err
	}
	defer releaseLock(pub)

	cleanAllShards(plan.indexDir)
	// cleanAllShards already removed any torn shards a prior interrupted swap
	// left, so a lingering .swapping marker would only force needless rebuilds
	// of this now-known-empty corpus — clear it.
	removeCacheFile(plan.cacheDir, swappingMarkerFile)
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
	// Hold LOCK_SH (the publish/read lock) across the entire glob+open+search so
	// a concurrent temp-swap publish (brief LOCK_EX) can never interleave and
	// tear the shard set — readers observe exactly the pre- or post-swap
	// generation. A long build holds only the separate .build.lock, so it never
	// blocks a reader; only the ms-scale publish does.
	targets := searchLockTargets(plan)
	locks := make([]*os.File, 0, len(targets))
	for _, target := range targets {
		searchLockPath := filepath.Join(target.cacheDir, lockFile)
		searchLockFd, err := os.OpenFile(searchLockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			closeSearchLocks(locks)
			return nil, fmt.Errorf("open search lock: %w", err)
		}
		if lockErr := acquireReadLock(ctx, target.indexDir, searchLockFd); lockErr != nil {
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
	// The combined index is validated by its own state hash during refresh and
	// served with stale-serve, so it needs no per-search TOCTOU check. Only the
	// over-cap fallback, built and selected just before search, must be
	// confirmed unchanged under the search lock.
	if plan.scopedStateHash == "" {
		return nil
	}
	if !scopedLayerStateMatches(plan.scopedCacheDir, plan.scopedIndexDir, plan.scopedStateHash) {
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
	if plan.scopedStateHash != "" {
		return []searchLockTarget{{cacheDir: plan.scopedCacheDir, indexDir: plan.scopedIndexDir}}
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
	if plan.scopedStateHash != "" {
		return []string{plan.scopedIndexDir}
	}
	return []string{plan.indexDir}
}
