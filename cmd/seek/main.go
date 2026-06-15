package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
)

// errNoMatch is returned by run when the query executed successfully but
// produced zero results. Following the POSIX grep convention, this maps to
// exit code 1 — distinguishing "no match" from both success (0) and error (2).
// This lets callers use seek reliably in shell pipelines and conditionals:
//
//	if seek "TODO"; then … fi       # runs body only when matches exist
//	seek "pattern" || echo "nope"   # "nope" printed only on no-match
var errNoMatch = errors.New("no match")

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
	// Dispatch `seek gc ...` early — before the search flag parser, which
	// would otherwise mistake "gc" for a query token.
	if len(os.Args) >= 2 && os.Args[1] == "gc" {
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
		slog.SetDefault(logger)
		log.SetOutput(io.Discard)
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		if err := runGCCommand(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}

	opts, err := parseCLIArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		writeUsage(os.Stderr, os.Args[0])
		os.Exit(2)
	}

	if opts.showVersion {
		fmt.Println(versionString())
		return
	}

	if opts.query == "" {
		writeUsage(os.Stderr, os.Args[0])
		os.Exit(2)
	}

	// Configure logging: warn+ by default, debug+ with -verbose.
	logLevel := slog.LevelWarn
	if opts.verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	// Silence zoekt's log.Printf output by default; bridge to slog when verbose.
	if opts.verbose {
		log.SetOutput(newSlogWriter(logger))
		log.SetFlags(0)
	} else {
		log.SetOutput(io.Discard)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	runErr := run(ctx, opts.query, opts.paths, opts.limit, opts.maxMatches)

	// Fire opportunistic GC after results are flushed. Wait up to
	// gcRunTimeout so eviction completes before exit (helps tests and
	// observability), but never block longer.
	fireOpportunisticGC(runOpportunisticGC, gcRunTimeout)

	if runErr != nil {
		code := exitCodeForError(runErr)
		if code != 1 {
			slog.Error(runErr.Error())
		}
		os.Exit(code)
	}
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

type cliOptions struct {
	showVersion bool
	verbose     bool
	limit       int
	maxMatches  int
	query       string
	paths       []string
}

func writeUsage(w io.Writer, prog string) {
	_, _ = fmt.Fprintf(w, "Usage: %s [flags] <query> [path...]\n\n", prog)
	_, _ = fmt.Fprintln(w, "Flags:")
	_, _ = fmt.Fprintln(w, "  -v, --verbose              enable debug logging")
	_, _ = fmt.Fprintln(w, "      --version              print version and exit")
	_, _ = fmt.Fprintln(w, "  -n, --limit N              maximum number of files to display")
	_, _ = fmt.Fprintln(w, "  -m, --max-matches N        maximum matches per file")
}

func parseCLIArgs(args []string) (cliOptions, error) {
	var opts cliOptions

	for i := 0; i < len(args); {
		arg := args[i]
		if arg == "--" {
			i++
			if i < len(args) {
				opts.query = args[i]
				opts.paths = cloneStringSlice(args[i+1:])
			}
			return opts, nil
		}

		if !strings.HasPrefix(arg, "-") || arg == "-" {
			opts.query = arg
			opts.paths = cloneStringSlice(args[i+1:])
			return opts, nil
		}

		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "-v", "--verbose", "-verbose":
			if hasValue {
				return opts, fmt.Errorf("%s does not accept a value", name)
			}
			opts.verbose = true
			i++
		case "--version", "-version":
			if hasValue {
				return opts, fmt.Errorf("%s does not accept a value", name)
			}
			opts.showVersion = true
			i++
		case "-n", "--limit", "-limit":
			n, next, err := parseFlagInt(args, i, name, value, hasValue)
			if err != nil {
				return opts, err
			}
			opts.limit = n
			i = next
		case "-m", "--max-matches", "-max-matches":
			n, next, err := parseFlagInt(args, i, name, value, hasValue)
			if err != nil {
				return opts, err
			}
			opts.maxMatches = n
			i = next
		default:
			opts.query = arg
			opts.paths = cloneStringSlice(args[i+1:])
			return opts, nil
		}
	}

	return opts, nil
}

func parseFlagInt(args []string, idx int, name, value string, hasValue bool) (int, int, error) {
	if !hasValue {
		if idx+1 >= len(args) {
			return 0, idx, fmt.Errorf("%s requires a value", name)
		}
		value = args[idx+1]
		idx += 2
	} else {
		idx++
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, idx, fmt.Errorf("%s requires an integer value", name)
	}
	return n, idx, nil
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
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
		return fmt.Errorf("not a git repository: %w", gitErr)
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
	output := formatCorpusResultsWithContext(allResults, dirtyByCorpus, limit, maxMatches, displayMode)
	if output == "" {
		return errNoMatch
	}

	_, _ = os.Stdout.WriteString(output)
	return nil
}

func prepareAndSearchCorpus(
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
		state, readyState, err := ensureGitCorpusFresh(ctx, plan, *planPaths)
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
		touchUsed(plan.cacheDir)
		return nil, dirtyFiles, nil
	}

	files, err := searchPlannedCorpusParsed(ctx, plan, userQ)
	if err != nil {
		return nil, nil, err
	}
	touchUsed(plan.cacheDir)
	return wrapCorpusResults(plan, files), dirtyFiles, nil
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
	results := make([]corpusSearchResult, len(files))
	for i, file := range files {
		results[i] = corpusSearchResult{
			corpusID:    plan.id,
			kind:        plan.kind,
			displayRoot: plan.displayRoot,
			file:        file,
		}
	}
	return results
}

func ensureGitCorpusFresh(ctx context.Context, plan corpusPlan, paths gitPaths) (repoState, corpusIndexState, error) {
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
	searchLockPath := filepath.Join(plan.cacheDir, lockFile)
	searchLockFd, err := os.OpenFile(searchLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open search lock: %w", err)
	}
	defer func() {
		unlockFile(searchLockFd)
		_ = searchLockFd.Close()
	}()
	if err := acquireSearchLock(ctx, plan.indexDir, searchLockFd); err != nil {
		return nil, fmt.Errorf("acquire search lock: %w", err)
	}

	results, err := executeParsedSearchScoped(ctx, plan.indexDir, userQ, plan.scope)
	if err != nil {
		return nil, err
	}
	return results, nil
}
