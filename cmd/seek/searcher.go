package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	grafanaregexp "github.com/grafana/regexp"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/sync/errgroup"
	"log/slog"
	"path/filepath"
)

// searchContextLines is the number of context lines included before and after
// each match.
const searchContextLines = 3

// searchOpts are the base Zoekt search options of every search. A package
// variable saves one heap allocation per search.
var searchOpts = zoekt.SearchOptions{
	// MaxDocDisplayCount stays 0 (no limit). The --limit/-n flag limits the
	// display when the formatter runs, after the final sort. A Zoekt display
	// limit would drop low-ranked files before Seek or a pipe (| grep,
	// | head) sees them. The search would then miss them with no sign.
	//
	// Seek searches each shard with Zoekt's per-shard searcher, which reads
	// only ShardMaxMatchCount. It stays at math.MaxInt so that every match in
	// a shard counts: 0 would become Zoekt's default of 100,000. SetDefaults
	// then sets the total match budget to ten times this value, which
	// overflows, but only the search package's aggregator reads that budget.
	ShardMaxMatchCount: math.MaxInt,
	NumContextLines:    searchContextLines,
	UseBM25Scoring:     true,
}

type searchConfig struct {
	// opts holds the Zoekt options. It is never nil.
	opts *zoekt.SearchOptions
	// bm25 marks a lexical search (the strict query, or the relaxed rank of
	// a model query). The command rescores its hits with one global average
	// file length.
	bm25              bool
	afterOnly         bool
	contextMatchLimit int
	contextFileLimit  int
}

// querySyntaxError identifies a query that Zoekt could not parse. The original
// query and wrapped cause let the CLI explain the input and retain Zoekt's
// diagnostic in verbose output.
type querySyntaxError struct {
	query string
	cause error
}

func (e *querySyntaxError) Error() string {
	return fmt.Sprintf("parse query %q: %v", e.query, e.cause)
}

func (e *querySyntaxError) Unwrap() error {
	return e.cause
}

func defaultSearchConfig() searchConfig {
	return searchConfig{opts: &searchOpts}
}

// explicitSearchConfig returns the search config of a context flag
// (--after-context or --context). It limits the context count to
// zoektShardFormatBytes: no document holds more lines than a shard holds
// bytes, so the limit shows the same lines. Zoekt adds the line number to this
// count, and a value near math.MaxInt would overflow and drop every line after
// the match.
func explicitSearchConfig(contextLines int, afterOnly bool) searchConfig {
	opts := searchOpts
	opts.NumContextLines = min(contextLines, zoektShardFormatBytes)
	return searchConfig{opts: &opts, afterOnly: afterOnly}
}

// openShard opens a single .zoekt shard file and returns a Searcher.
func openShard(path string) (zoekt.Searcher, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	iFile, err := index.NewIndexFile(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	s, err := zoektOpen(iFile)
	if err != nil {
		iFile.Close()
		return nil, err
	}
	return s, nil
}

// loadShards opens all .zoekt entries in indexDir and returns individual
// zoekt.Searcher instances. A one-shot CLI search does not need directory
// watchers or ready-channel synchronization. Multiple shards load in parallel.
func loadShards(indexDir string) ([]zoekt.Searcher, error) {
	paths, err := familyShardNames(indexDir)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		// An index dir that holds no shard at all is damage the caller can
		// repair, not a query failure to report.
		return nil, fmt.Errorf("%w: no index shards in %s", errShardUnloadable, indexDir)
	}
	return loadShardPaths(paths)
}

// loadShardPaths opens the shards at paths, which must not be empty. On an
// error it closes the shards that it opened.
func loadShardPaths(paths []string) ([]zoekt.Searcher, error) {
	// The shards load in parallel to overlap mmap and metadata parsing; one
	// shard loads with no goroutine. The searchers keep the path order, which
	// the BM25 rescore reads.
	searchers := make([]zoekt.Searcher, len(paths))
	errs := make([]error, len(paths))
	if len(paths) == 1 {
		searchers[0], errs[0] = openShard(paths[0])
	} else {
		var wg sync.WaitGroup
		for index, path := range paths {
			wg.Go(func() { searchers[index], errs[index] = openShard(path) })
		}
		wg.Wait()
	}
	for index, err := range errs {
		if err != nil {
			closeShards(slices.DeleteFunc(searchers, func(s zoekt.Searcher) bool { return s == nil }))
			return nil, fmt.Errorf("%w: load shard %s: %w", errShardUnloadable, paths[index], err)
		}
	}
	return searchers, nil
}

// errShardUnloadable marks index damage. That is a shard that exists but cannot
// be opened (truncated, unreadable, a directory, or a dangling symlink), or an
// index with no shard. openNode removes the family manifest that the read saw
// (removeDamagedFamilyManifest) and opens the node once more.
//
// The repair covers both families. With no manifest, the committed family
// rebuilds (gitBuildPlan.committed). The uncommitted family then takes no keep
// and no delta (uncommittedDeltaPossible), so it rebuilds in full.
var errShardUnloadable = errors.New("index damage")

// parseQueryHalves parses pattern and expands each bare word into a file-name
// and a content atom, as Zoekt does. halves holds the file-name atoms that the
// expansion made: they keep Zoekt's meaning, so only an explicit file: atom
// reads the header.
func parseQueryHalves(pattern string) (raw, strict query.Q, halves map[query.Q]struct{}, err error) {
	raw, err = query.Parse(pattern)
	if err != nil {
		var ok bool
		raw, ok = parsePlainDescriptionFallback(pattern)
		if !ok {
			return nil, nil, nil, &querySyntaxError{query: pattern, cause: err}
		}
	}
	halves = map[query.Q]struct{}{}
	expanded := query.Map(raw, func(q query.Q) query.Q {
		out := query.ExpandFileContent(q)
		if or, ok := out.(*query.Or); ok && out != q {
			for _, child := range or.Children {
				if fileNameAtom(child) {
					halves[child] = struct{}{}
				}
			}
		}
		return out
	})
	strict = query.Simplify(expanded)
	if err := compileRegexpAtoms(strict); err != nil {
		return nil, nil, nil, &querySyntaxError{query: pattern, cause: err}
	}
	return raw, strict, halves, nil
}

// compileRegexpAtoms compiles each regexp atom of q as Zoekt does
// (index/matchtree.go regexpPattern and MustCompile). Go's parser can reject
// a regexp after Zoekt's simplification, such as a{1,501}. Then it is a query
// error, not a panic in the search.
// TODO(https://github.com/sourcegraph/zoekt/issues/1188): drop this check
// once Zoekt returns the compile error.
func compileRegexpAtoms(q query.Q) error {
	var err error
	check := func(atom *query.Regexp) {
		if err != nil || atom == nil {
			return
		}
		pattern := atom.RegexpString()
		if !atom.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		if _, compileErr := grafanaregexp.Compile(pattern); compileErr != nil {
			// Go's message ends with the whole expanded expression.
			message, _, _ := strings.Cut(compileErr.Error(), ": `")
			err = errors.New(message)
		}
	}
	query.VisitAtoms(q, func(atom query.Q) {
		switch typed := atom.(type) {
		case *query.Regexp:
			check(typed)
		case *query.Symbol:
			if expr, ok := typed.Expr.(*query.Regexp); ok {
				check(expr)
			}
		}
	})
	return err
}

// parsePlainDescriptionFallback keeps programming names such as C++ usable.
// Zoekt parses bare terms as regular expressions, so a valid programming name
// can otherwise fail the whole query. It runs only after Zoekt rejects the
// query. It quotes each programming name (C++, X[], X(), with :: qualifiers)
// and parses the rewritten query again, so filters keep their meaning. A name
// such as at() is also a valid expression, but next to C++ it means the
// literal name. The quoted term parses to the literal name, which the model
// reads.
func parsePlainDescriptionFallback(pattern string) (query.Q, bool) {
	var rewritten strings.Builder
	changed := false
	for at := 0; at < len(pattern); {
		// Zoekt splits words on space and tab; keep every separator as typed.
		end := at
		for end < len(pattern) && pattern[end] != ' ' && pattern[end] != '\t' {
			end++
		}
		term := pattern[at:end]
		if plainProgrammingName(term) {
			// QuoteMeta leaves ':', which would make "content::X()" a filter.
			term = strings.ReplaceAll(regexp.QuoteMeta(term), ":", `\:`)
			changed = true
		}
		rewritten.WriteString(term)
		if end < len(pattern) {
			rewritten.WriteByte(pattern[end])
			end++
		}
		at = end
	}
	if !changed {
		return nil, false
	}
	parsed, err := query.Parse(rewritten.String())
	return parsed, err == nil
}

func plainProgrammingName(term string) bool {
	for _, suffix := range []string{"++", "[]", "()"} {
		if strings.HasSuffix(term, suffix) {
			return plainQualifiedIdentifier(strings.TrimSuffix(term, suffix))
		}
	}
	return false
}

func plainQualifiedIdentifier(name string) bool {
	for _, part := range strings.Split(name, "::") {
		if part == "" {
			return false
		}
		for index, character := range part {
			if character == '_' || unicode.IsLetter(character) ||
				(index > 0 && unicode.IsDigit(character)) {
				continue
			}
			return false
		}
	}
	return true
}

// searchShards runs userQ within scope on one load of shards. paths are in
// the order of searchers; the BM25 rescore reads them. With no searcher it
// returns errShardUnloadable: damage that the caller repairs.
func searchShards(
	ctx context.Context,
	searchers []zoekt.Searcher,
	paths []string,
	userQ query.Q,
	scope query.Q,
	config searchConfig,
) ([]zoekt.FileMatch, error) {
	q := userQ
	if scope != nil {
		// Zoekt gathers no match under a file-name type. So the scope filters
		// the files and adds no file-name term to their BM25 score. A file in
		// the scope scores as in an unscoped search. Unlike a double
		// negation, it keeps Zoekt's shard and file skips.
		q = query.NewAnd(q, &query.Type{Type: query.TypeFileName, Child: scope})
		q = query.Simplify(q)
	}
	if len(searchers) == 0 {
		return nil, fmt.Errorf("%w: no loadable shards", errShardUnloadable)
	}
	parts, err := bm25ShardParts(ctx, searchers, paths, q, &config)
	if err != nil {
		return nil, err
	}
	return searchLoadedShards(ctx, searchers, q, config, parts)
}

// closeShards closes the searchers of one load. The caller calls it after
// every search of them returns: Close unmaps the shard.
func closeShards(searchers []zoekt.Searcher) {
	for _, s := range searchers {
		s.Close()
	}
}

// bm25ShardParts prepares the rescore of a bm25 search. It records each
// shard's counts for the command's average and asks Zoekt for its score debug
// line. It returns the function that rebuilds each hit's parts. Other searches
// get nil.
func bm25ShardParts(ctx context.Context, searchers []zoekt.Searcher, paths []string, q query.Q, config *searchConfig) (func(int, []zoekt.FileMatch), error) {
	command, ok := ctx.Value(bm25CommandKey{}).(*bm25Command)
	if !ok || !config.bm25 || !config.opts.UseBM25Scoring || len(paths) != len(searchers) {
		return nil, nil
	}
	if command.nodes == 1 && len(paths) == 1 {
		// One node of one shard: Zoekt's average is the command's, so
		// Zoekt's scores are final.
		return nil, nil
	}
	// The paths are one node's shards, in its index directory.
	record := readShardStatsRecord(filepath.Dir(paths[0]))
	shards := make([]shardLengthStats, len(paths))
	var group errgroup.Group
	group.SetLimit(runtime.GOMAXPROCS(0))
	for index, path := range paths {
		group.Go(func() error {
			stats, err := record.stats(ctx, path, searchers[index])
			if err != nil {
				return err
			}
			shards[index] = stats
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// A shard whose counts Seek cannot read keeps Zoekt's scores, and
		// the command's average gets none of this search's counts.
		slog.Debug("BM25 shard counts unavailable", "error", err)
		return nil, nil
	}
	// The counts replace those of an earlier search of the same index
	// directory, such as a repaired attempt.
	command.set(paths, shards)
	averages := make([]float64, len(paths))
	for index, stats := range shards {
		averages[index] = stats.average()
	}
	record.save(paths)
	opts := *config.opts
	opts.DebugScore = true
	config.opts = &opts
	// The clone keeps context lines for the best files by score. With one
	// node, these shards are all the shards of the command. So each hit gets
	// its final score here (with one shard, Zoekt's score is final). Else the
	// command rescore can reorder the files after the clone, and every strict
	// file keeps its context.
	global, final := 0.0, false
	if command.nodes == 1 {
		global, final = shardsAverage(shards)
		command.final.Store(final)
	} else {
		config.contextFileLimit = 0
	}
	bq := compileBM25Query(q)
	rebuild := func(file *zoekt.FileMatch, average float64) {
		if !bm25Parts(file, bq, average) {
			command.kept.Add(1)
			return
		}
		command.rebuilt.Add(1)
		if !final {
			return
		}
		if score, ok := bm25Rescore(file.Debug, global); ok {
			file.Score = score
		}
	}
	return func(index int, files []zoekt.FileMatch) {
		// One hit's rebuild reads only that hit. So the hits of one shard
		// spread over the host's CPUs, and the shard with the most ambiguous
		// lines does not hold the search.
		workers := min(runtime.GOMAXPROCS(0), max(1, len(files)/bm25FilesPerWorker))
		var group sync.WaitGroup
		for worker := range workers {
			group.Go(func() {
				for file := worker; file < len(files); file += workers {
					rebuild(&files[file], averages[index])
				}
			})
		}
		group.Wait()
	}, nil
}

// bm25FilesPerWorker is the share of a shard's hits below which one more
// goroutine costs more than it saves. The Kubernetes repository has few hits
// per shard. There, 4 instead of 64 took the rebuild phase of
// `case:yes func Convert` from 5.97 to 2.85 ms (all 26 shards, median of 15
// runs). The end-to-end search time did not change measurably.
const bm25FilesPerWorker = 4

// searchLoadedShards searches the loaded shards and returns clones of the
// hits. It returns only after all its search workers return, and it does not
// close the searchers: one load can serve several searches.
//
// A panic in one shard search becomes that shard's error (zoektSearch); the
// other shards still run.
func searchLoadedShards(
	ctx context.Context,
	searchers []zoekt.Searcher,
	q query.Q,
	config searchConfig,
	parts func(int, []zoekt.FileMatch),
) ([]zoekt.FileMatch, error) {
	searchShard := func(i int) ([]zoekt.FileMatch, error) {
		r, err := zoektSearch(ctx, searchers[i], q, config.opts)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		// The parts read every match, before the display limits drop any.
		if parts != nil {
			parts(i, r.Files)
		}
		roundScores(r.Files)
		return r.Files, nil
	}
	// The shard searches run on GOMAXPROCS goroutines. shardMax gives a
	// corpus of many GiB hundreds of shards, and parallel searches make the
	// query cost the maximum of the shards, not their sum. One shard needs no
	// goroutine. Every shard search runs to its end, and the first failed
	// shard in order gives the error.
	results := make([][]zoekt.FileMatch, len(searchers))
	errs := make([]error, len(searchers))
	if len(searchers) == 1 {
		results[0], errs[0] = searchShard(0)
	} else {
		var group errgroup.Group
		group.SetLimit(runtime.GOMAXPROCS(0))
		for i := range searchers {
			group.Go(func() error {
				results[i], errs[i] = searchShard(i)
				return nil
			})
		}
		_ = group.Wait()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	files := results[0]
	if len(results) > 1 {
		files = slices.Concat(results...)
	}
	return cloneFileMatches(files, config), nil
}

// zoektSearch runs one Zoekt shard search. It is one of five functions that
// recover the panics of Zoekt shard calls:
//   - zoektSearch runs one shard search;
//   - zoektList lists one shard's repositories;
//   - zoektOpen parses one shard file;
//   - zoektReadMetadata reads the repository records of one shard file;
//   - zoektShardCall runs any other Zoekt call that reads shard files (a delta
//     Builder reads the records of the old shards).
//
// Zoekt's index package panics where it could return an error. Its own sharded
// searcher recovers the panic at the shard boundary (search.searchOneShard).
// Seek calls the shards itself, so every Zoekt shard call goes through these
// five functions. A memory fault in a damaged mapping becomes a panic too
// (debug.SetPanicOnFault). They cover only the Zoekt call. A panic in Seek's
// own code still stops the process, so a Seek bug is not taken for index
// damage.
func zoektSearch(ctx context.Context, searcher zoekt.Searcher, q query.Q, opts *zoekt.SearchOptions) (_ *zoekt.SearchResult, err error) {
	defer recoverShardPanic(&err)
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	result, err := searcher.Search(ctx, q, opts)
	return result, shardCallError(err)
}

func zoektList(ctx context.Context, searcher zoekt.Searcher, q query.Q, opts *zoekt.ListOptions) (_ *zoekt.RepoList, err error) {
	defer recoverShardPanic(&err)
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	list, err := searcher.List(ctx, q, opts)
	return list, shardCallError(err)
}

func zoektOpen(file index.IndexFile) (_ zoekt.Searcher, err error) {
	defer recoverShardPanic(&err)
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	searcher, err := index.NewSearcher(file)
	return searcher, shardCallError(err)
}

func zoektShardCall(call func() error) (err error) {
	defer recoverShardPanic(&err)
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	return shardCallError(call())
}

// zoektReadMetadata drops a nil record: Zoekt gives one with no error for a
// .meta file that holds null, a damaged record.
func zoektReadMetadata(path string) (_ []*zoekt.Repository, err error) {
	defer recoverShardPanic(&err)
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	repos, _, err := index.ReadMetadataPath(path)
	repos = slices.DeleteFunc(repos, func(repo *zoekt.Repository) bool { return repo == nil })
	return repos, shardCallError(err)
}

// shardCallError marks as index damage a Zoekt error that names a read past
// the end of the shard file (index.mmapedIndexFile.Read, "out of bounds:").
// Only shard offsets that do not match the file give it.
func shardCallError(err error) error {
	if err != nil && strings.Contains(err.Error(), "out of bounds: ") {
		return fmt.Errorf("%w: %w", errShardUnloadable, err)
	}
	return err
}

// recoverShardPanic sets *err from the panic of a Zoekt shard call
// (shardSearchPanic). It must be deferred directly by that call.
func recoverShardPanic(err *error) {
	if recovered := recover(); recovered != nil {
		*err = shardSearchPanic(recovered)
		slog.Debug("Zoekt shard call panicked", "error", *err, "stack", string(debug.Stack()))
	}
}

// shardSearchPanic turns the panic of one Zoekt shard call into an error.
// openNode repairs index damage with one rebuild. Index damage is one of these
// panics, which shard bytes that do not match their offsets give:
//   - a panic that names a corrupt shard (index.contentProvider.panicCorrupt);
//   - a panic that names a corrupt index (index/eval.go: a damaged subRepos
//     section);
//   - a Go runtime error (an index or slice bound, or a memory fault).
//
// In a census of single-byte changes to one shard, 37 gave the corrupt-shard
// panic and 43 a runtime error. Zoekt's other panics are strings, such as the
// one for type:repo, type:filematch or a negated case:. They fail only this
// node, with one warning.
// TODO(https://github.com/sourcegraph/zoekt/issues/1189): when Zoekt returns
// an error for type:repo and type:filematch, that error names the cause.
func shardSearchPanic(recovered any) error {
	err, ok := recovered.(error)
	if !ok {
		err = fmt.Errorf("%v", recovered)
	}
	var runtimeErr runtime.Error
	message := err.Error()
	if strings.HasPrefix(message, "corrupt shard ") || strings.HasPrefix(message, "corrupt index:") || errors.As(err, &runtimeErr) {
		return fmt.Errorf("%w: %w", errShardUnloadable, err)
	}
	return fmt.Errorf("shard search panicked: %w", err)
}

// roundScores rounds each score once, after the parts read the exact value.
// Zoekt sums a score over a Go map, so equal true scores differ in their last
// bits from run to run. Rounded ties sort by name.
// TODO(https://github.com/sourcegraph/zoekt/issues/1187): drop the rounding
// once Zoekt sums in a fixed order.
func roundScores(files []zoekt.FileMatch) {
	for index := range files {
		files[index].Score = math.Round(files[index].Score*1e9) / 1e9
	}
}

func cloneFileMatches(files []zoekt.FileMatch, config searchConfig) []zoekt.FileMatch {
	if len(files) == 0 {
		return nil
	}
	contextFiles := displayedContextFiles(files, config)
	out := make([]zoekt.FileMatch, len(files))
	for i := range files {
		_, keepContext := contextFiles[i]
		out[i] = cloneFileMatch(
			files[i],
			config.contextMatchLimit,
			config.afterOnly,
			contextFiles == nil || keepContext,
		)
	}
	return out
}

// displayedContextFiles returns the indexes of the best contextFileLimit files
// of this corpus: the files that can still show after the global file limit.
// The node read returns one current copy per file, so a global top N file is
// in its corpus's top N. A nil map means that all context must be copied.
func displayedContextFiles(files []zoekt.FileMatch, config searchConfig) map[int]struct{} {
	limit := config.contextFileLimit
	if limit <= 0 || len(files) <= limit {
		return nil
	}

	candidates := make([]int, len(files))
	for i := range files {
		candidates[i] = i
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := files[candidates[i]]
		right := files[candidates[j]]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		return left.FileName < right.FileName
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	selected := make(map[int]struct{}, len(candidates))
	for _, index := range candidates {
		selected[index] = struct{}{}
	}
	return selected
}

func cloneFileMatch(
	in zoekt.FileMatch,
	contextMatchLimit int,
	afterOnly bool,
	keepFileContext bool,
) zoekt.FileMatch {
	out := in
	out.Branches = cloneStringSlice(in.Branches)
	out.Content = cloneBytes(in.Content)
	out.Checksum = cloneBytes(in.Checksum)

	if len(in.LineMatches) > 0 {
		var contextLines map[int]struct{}
		if keepFileContext {
			contextLines = displayedContextLines(in.LineMatches, contextMatchLimit)
		}
		out.LineMatches = make([]zoekt.LineMatch, len(in.LineMatches))
		for i := range in.LineMatches {
			_, keepContext := contextLines[in.LineMatches[i].LineNumber]
			out.LineMatches[i] = cloneLineMatch(
				in.LineMatches[i],
				keepFileContext && (contextLines == nil || keepContext),
				afterOnly,
			)
		}
	}

	// Seek never asks Zoekt for chunk matches (SearchOptions.ChunkMatches).
	out.ChunkMatches = nil

	return out
}

// displayedContextLines returns the source lines whose context can reach the
// formatter. A nil map means that all context must be copied. Search results
// refer to shard memory, so every displayed byte must be copied before close.
func displayedContextLines(matches []zoekt.LineMatch, limit int) map[int]struct{} {
	if limit <= 0 || len(matches) <= limit {
		return nil
	}
	displayed, _ := normalizeLineMatches(matches, limit)
	lines := make(map[int]struct{}, len(displayed))
	for _, match := range displayed {
		lines[match.LineNumber] = struct{}{}
	}
	return lines
}

func cloneLineMatch(in zoekt.LineMatch, keepContext, afterOnly bool) zoekt.LineMatch {
	out := in
	out.Line = cloneBytes(in.Line)
	if keepContext {
		if afterOnly {
			out.Before = nil
		} else {
			out.Before = cloneBytes(in.Before)
		}
		out.After = cloneBytes(in.After)
	} else {
		out.Before = nil
		out.After = nil
	}
	if len(in.LineFragments) > 0 {
		out.LineFragments = make([]zoekt.LineFragmentMatch, len(in.LineFragments))
		for i := range in.LineFragments {
			out.LineFragments[i] = cloneLineFragmentMatch(in.LineFragments[i])
		}
	}
	return out
}

func cloneLineFragmentMatch(in zoekt.LineFragmentMatch) zoekt.LineFragmentMatch {
	out := in
	if in.SymbolInfo != nil {
		symbol := *in.SymbolInfo
		out.SymbolInfo = &symbol
	}
	return out
}

func cloneBytes(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
