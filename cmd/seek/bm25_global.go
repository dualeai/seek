package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	grafanaregexp "github.com/grafana/regexp"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/sys/unix"
)

// Zoekt scores BM25 per shard with that shard's average file length
// (index/score.go scoreFileBM25, k = 1.2, b = 0.75). So the strict order
// depends on how the shards and the nodes split the files. Seek rescores every
// strict hit with one average over all the shards that a command searches.
// TODO(https://github.com/sourcegraph/zoekt/issues/1186): drop this rescore
// once Zoekt lets the caller supply the average file length.
//
// The sources:
//   - Zoekt's score is exact in FileMatch.Score.
//   - The term frequencies come from the matches that Seek gets, with the
//     file-name atoms run on the name.
//   - DebugScore gives the low-priority flag and the sum to check.
//   - The shard average is (ContentBytes - file-name bytes) / Documents.
//   - The file length comes from the score.
const (
	bm25K         = 1.2
	bm25B         = 0.75
	bm25StatsFile = ".bm25-v2"
	// Zoekt's terms in low-priority files (tests, vendored, generated) count
	// one fifth (index/score.go lowPriorityFilePenalty).
	bm25LowPriorityPenalty = 5
	// A file-name or symbol match counts five (importantTermBoost).
	bm25ImportantTermBoost = 5
	bm25PartsPrefix        = "seek-bm25 "
)

// shardLengthStats are the counts of one shard that Zoekt's average reads,
// keyed by the shard file's size, mtime and inode.
type shardLengthStats struct {
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	Ino     uint64 `json:"ino"`
	Docs    int64  `json:"docs"`
	Content int64  `json:"content"`
}

// average is the shard's average file length as Zoekt computes it.
func (stats shardLengthStats) average() float64 {
	if stats.Docs == 0 {
		return 0
	}
	average := float64(stats.Content) / float64(stats.Docs)
	if average == 0 {
		average = 1 // Zoekt's guard against division by zero.
	}
	return average
}

type bm25CommandKey struct{}

// bm25Command holds the shards that one command searches. With one node, one
// search sees every shard of the command and gives each hit its final score
// (final). The command rescore then has nothing to do.
type bm25Command struct {
	mu     sync.Mutex
	shards map[string]shardLengthStats
	nodes  int
	final  atomic.Bool
	// rebuilt and kept count the strict hits whose BM25 parts the search
	// rebuilt or kept; only the log of an in-place rescore reads them.
	rebuilt atomic.Int64
	kept    atomic.Int64
}

// withBM25Command returns ctx with a new bm25Command for a command that
// searches nodes nodes.
func withBM25Command(ctx context.Context, nodes int) (context.Context, *bm25Command) {
	command := &bm25Command{shards: map[string]shardLengthStats{}, nodes: nodes}
	return context.WithValue(ctx, bm25CommandKey{}, command), command
}

// set records the counts of one search's shards. They replace every count of
// the same index directories: a repaired node has new shard names.
func (command *bm25Command) set(paths []string, shards []shardLengthStats) {
	dirs := map[string]struct{}{}
	for _, path := range paths {
		dirs[filepath.Dir(path)] = struct{}{}
	}
	command.mu.Lock()
	defer command.mu.Unlock()
	for path := range command.shards {
		if _, ok := dirs[filepath.Dir(path)]; ok {
			delete(command.shards, path)
		}
	}
	for index, path := range paths {
		command.shards[path] = shards[index]
	}
}

// shardsAverage is the average file length over shards, and whether they are
// two or more. With one shard, Zoekt's own average is the global one.
func shardsAverage(shards []shardLengthStats) (float64, bool) {
	var docs, content int64
	for _, stats := range shards {
		docs += stats.Docs
		content += stats.Content
	}
	if len(shards) < 2 || docs == 0 {
		return 0, false
	}
	return shardLengthStats{Docs: docs, Content: content}.average(), true
}

// average is the global average file length, and whether the command searched
// more than one shard. With one shard, Zoekt's own average is the global one.
func (command *bm25Command) average() (float64, bool) {
	command.mu.Lock()
	defer command.mu.Unlock()
	return shardsAverage(slices.Collect(maps.Values(command.shards)))
}

// shardStatsRecord reads and extends the per-node record of shard counts.
type shardStatsRecord struct {
	mu      sync.Mutex
	path    string
	shards  map[string]shardLengthStats
	changed bool
}

// readShardStatsRecord reads the record of the node whose index is indexDir. A
// missing or damaged record reads as empty.
func readShardStatsRecord(indexDir string) *shardStatsRecord {
	record := &shardStatsRecord{path: filepath.Join(filepath.Dir(indexDir), bm25StatsFile), shards: map[string]shardLengthStats{}}
	if data, err := os.ReadFile(record.path); err == nil {
		_ = json.Unmarshal(data, &record.shards)
	}
	return record
}

// stats returns the counts of the shard at path. It reads them from the
// record when the shard file is the same. Else it reads them from the shard
// itself, with one listing of its names.
func (record *shardStatsRecord) stats(ctx context.Context, path string, searcher zoekt.Searcher) (shardLengthStats, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return shardLengthStats{}, err
	}
	stats := shardLengthStats{Size: st.Size, MtimeNS: statMtimeNano(st), Ino: st.Ino}
	name := filepath.Base(path)
	record.mu.Lock()
	cached, ok := record.shards[name]
	record.mu.Unlock()
	if ok && cached.Size == stats.Size && cached.MtimeNS == stats.MtimeNS && cached.Ino == stats.Ino {
		return cached, nil
	}
	list, err := zoektList(ctx, searcher)
	if err != nil {
		return shardLengthStats{}, err
	}
	tombstoned := false
	for _, repo := range list.Repos {
		stats.Docs += int64(repo.Stats.Documents)
		stats.Content += repo.Stats.ContentBytes
		tombstoned = tombstoned || len(repo.Repository.FileTombstones) > 0
	}
	if tombstoned {
		// Zoekt's average counts the tombstoned files, and Search skips
		// them: list the names on a copy that reads no tombstones.
		whole, err := openShardWithoutTombstones(path)
		if err != nil {
			return shardLengthStats{}, err
		}
		defer whole.Close()
		searcher = whole
	}
	opts := searchOpts
	opts.NumContextLines = 0
	names, err := zoektSearch(ctx, searcher, &query.Type{Type: query.TypeFileName, Child: &query.Const{Value: true}}, &opts)
	if err != nil {
		return shardLengthStats{}, err
	}
	for _, file := range names.Files {
		stats.Content -= int64(len(file.FileName))
	}
	record.mu.Lock()
	record.shards[name] = stats
	record.changed = true
	record.mu.Unlock()
	return stats, nil
}

// openShardWithoutTombstones opens the shard at path under a name with no
// ".meta" sidecar. Zoekt reads a shard's tombstones from "<name>.meta"
// (index/read.go), so this copy sees every file that the shard holds.
func openShardWithoutTombstones(path string) (zoekt.Searcher, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// The copy is close-on-exec in the same call, so a Git or ctags child
	// that another goroutine starts never holds it.
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	return openShardFile(os.NewFile(uintptr(fd), path+".seek-no-tombstones"))
}

// save writes the record when it gained shards, with only the live shards; a
// failure costs one listing on the next search.
func (record *shardStatsRecord) save(live []string) {
	record.mu.Lock()
	defer record.mu.Unlock()
	if !record.changed {
		return
	}
	kept := make(map[string]shardLengthStats, len(live))
	for _, path := range live {
		if stats, ok := record.shards[filepath.Base(path)]; ok {
			kept[filepath.Base(path)] = stats
		}
	}
	data, err := json.Marshal(kept)
	if err == nil {
		err = writeSharedCacheFile(filepath.Dir(record.path), filepath.Base(record.path), string(data))
	}
	if err != nil {
		slog.Debug("Failed to write the BM25 shard record", "path", record.path, "error", err)
	}
}

// parseZoektBM25Debug reads the low-priority flag and the summed term
// frequencies from Zoekt's BM25 debug line (index/score.go scoreFileBM25):
// "bm25-score: S (low-priority: B) <- sum-termFrequencies: N, length-ratio: L".
// A boosted score, or any other form, gives false.
func parseZoektBM25Debug(debug string) (lowPriority bool, sum int, ratio float64, ok bool) {
	rest, found := strings.CutPrefix(debug, "bm25-score: ")
	if !found {
		return false, 0, 0, false
	}
	_, rest, found = strings.Cut(rest, " (low-priority: ")
	if !found {
		return false, 0, 0, false
	}
	flag, rest, found := strings.Cut(rest, ") <- sum-termFrequencies: ")
	if !found || (flag != "true" && flag != "false") {
		return false, 0, 0, false
	}
	count, ratioText, found := strings.Cut(rest, ", length-ratio: ")
	if !found || strings.ContainsRune(ratioText, ' ') {
		return false, 0, 0, false
	}
	sum, err := strconv.Atoi(count)
	if err != nil {
		return false, 0, 0, false
	}
	ratio, err = strconv.ParseFloat(ratioText, 64)
	return flag == "true", sum, ratio, err == nil
}

// bm25Parts rebuilds the length and the term frequencies of one hit from shard
// stats, and encodes them in its Debug field for the rescore. Each candidate
// takes Zoekt's term key (bm25Query). The rebuild must give Zoekt's sum of
// frequencies. Solved from the score, it must also give the length ratio that
// Zoekt prints (two decimals). The line score cannot tell some splits of a
// line apart; of those, exactly one value set must fit. Any other hit keeps
// its score (no parts).
func bm25Parts(file *zoekt.FileMatch, bq bm25Query, average float64) bool {
	debug := file.Debug
	file.Debug = ""
	lowPriority, want, printed, ok := parseZoektBM25Debug(debug)
	if !ok || average <= 0 {
		return false
	}
	base := map[string]int{}
	// The multi-line candidates first: each piece takes its candidate's key.
	keys, merges, ok := bq.multiLineMerges(file.LineMatches)
	if !ok {
		return false
	}
	for key, delta := range merges {
		base[key] += delta
	}
	nameLine := false
	// options holds the splits of each line that has two or more.
	var options [][][]bm25Piece
	for _, line := range file.LineMatches {
		if line.FileName {
			// Zoekt's file-name candidates, shown when the file has no
			// content match (a type:file match covers the whole name).
			nameLine = true
			for _, fragment := range line.LineFragments {
				end := fragment.LineOffset + fragment.MatchLength
				if fragment.LineOffset < 0 || end > len(line.Line) {
					return false
				}
				key, ok := bq.nameKey(string(line.Line[fragment.LineOffset:end]), file.FileName)
				if !ok {
					return false
				}
				base[key] += bm25ImportantTermBoost
			}
			continue
		}
		var lineKeys []string
		if keys != nil {
			lineKeys, keys = keys[:len(line.LineFragments)], keys[len(line.LineFragments):]
		}
		splits, ok := bq.lineFrequencies(line, lineKeys)
		if !ok {
			return false
		}
		if len(splits) > 1 {
			options = append(options, splits)
			continue
		}
		for _, piece := range splits[0] {
			base[piece.key] += piece.frequency
		}
	}
	// A file-name match has no fragment when the file has content matches.
	// Then the query's file-name atoms run on the name: Zoekt's file-name
	// candidates, with its overlap rule.
	if !nameLine {
		alternatives, ok := bq.nameCandidates(file.FileName)
		if !ok {
			return false
		}
		if len(alternatives) == 1 {
			for _, key := range alternatives[0] {
				base[key] += bm25ImportantTermBoost
			}
		} else {
			// Zoekt's unstable sort keeps either key of a tied range: the
			// ratio picks.
			splits := make([][]bm25Piece, len(alternatives))
			for index, keys := range alternatives {
				for _, key := range keys {
					splits[index] = append(splits[index], bm25Piece{key: key, frequency: bm25ImportantTermBoost})
				}
			}
			options = append(options, splits)
		}
	}
	// The distinct frequency sets that the splits reach, line by line: many
	// split combinations give one set, so the sets stay few. Lines with the
	// same splits join first.
	options, ok = bm25GroupOptions(options)
	if !ok {
		return false
	}
	states := map[string]map[string]int{"": base}
	for _, splits := range options {
		next := map[string]map[string]int{}
		for _, state := range states {
			for _, split := range splits {
				frequencies := maps.Clone(state)
				for _, piece := range split {
					frequencies[piece.key] += piece.frequency
				}
				next[bm25StateKey(frequencies)] = frequencies
				if len(next) > bm25MaxSplitStates {
					// Too many to try: the hit keeps Zoekt's score. The
					// check runs per state, so the work stays bounded.
					return false
				}
			}
		}
		states = next
	}
	var values []int
	found := false
	for _, frequencies := range states {
		candidate, ok := bm25Fit(frequencies, lowPriority, want, printed, file.Score)
		if !ok {
			continue
		}
		slices.Sort(candidate)
		if found && !slices.Equal(candidate, values) {
			// Two value sets fit: the hit keeps Zoekt's score.
			return false
		}
		values, found = candidate, true
	}
	if !found {
		return false
	}
	length := 0.0
	if len(values) > 0 {
		ratio, ok := solveBM25LengthRatio(values, file.Score)
		if !ok {
			return false
		}
		// Zoekt's length is a whole byte count; its score holds float
		// noise from a map-ordered sum, so two runs could differ.
		length = math.Round(ratio * average)
	}
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = strconv.Itoa(value)
	}
	file.Debug = fmt.Sprintf("%s%.17g %s", bm25PartsPrefix, length, strings.Join(parts, ","))
	return true
}

// bm25MaxSplitStates bounds the distinct frequency sets that one hit tries; a
// hit with more keeps Zoekt's score (no result is cut).
const bm25MaxSplitStates = 4096

// bm25GroupOptions joins the lines whose split lists are equal. k such lines
// reach the sums of k picks among their splits. The compositions of k give
// these sums in one step; a merge line by line costs about k squared.
func bm25GroupOptions(options [][][]bm25Piece) ([][][]bm25Piece, bool) {
	signature := func(splits [][]bm25Piece) string {
		var key strings.Builder
		for _, split := range splits {
			for _, piece := range split {
				key.WriteString(piece.key)
				key.WriteByte(0)
				key.WriteString(strconv.Itoa(piece.frequency))
				key.WriteByte(1)
			}
			key.WriteByte(2)
		}
		return key.String()
	}
	type group struct {
		splits [][]bm25Piece
		count  int
	}
	var order []string
	groups := map[string]*group{}
	for _, splits := range options {
		key := signature(splits)
		if found, ok := groups[key]; ok {
			found.count++
			continue
		}
		groups[key] = &group{splits: splits, count: 1}
		order = append(order, key)
	}
	out := make([][][]bm25Piece, 0, len(order))
	for _, key := range order {
		found := groups[key]
		if found.count == 1 {
			out = append(out, found.splits)
			continue
		}
		parts := len(found.splits)
		// The compositions of count into parts: C(count+parts-1, parts-1).
		compositions := 1.0
		for index := 1; index < parts; index++ {
			compositions = compositions * float64(found.count+index) / float64(index)
		}
		if compositions > bm25MaxSplitStates {
			return nil, false
		}
		sums := map[string][]bm25Piece{}
		times := make([]int, parts)
		var walk func(index, left int)
		walk = func(index, left int) {
			if index == parts-1 {
				times[index] = left
				totals := map[string]int{}
				for split, count := range times {
					for _, piece := range found.splits[split] {
						totals[piece.key] += count * piece.frequency
					}
				}
				pieces := make([]bm25Piece, 0, len(totals))
				for _, term := range slices.Sorted(maps.Keys(totals)) {
					pieces = append(pieces, bm25Piece{key: term, frequency: totals[term]})
				}
				sums[bm25StateKey(totals)] = pieces
				return
			}
			for count := 0; count <= left; count++ {
				times[index] = count
				walk(index+1, left-count)
			}
		}
		walk(0, found.count)
		joined := make([][]bm25Piece, 0, len(sums))
		for _, pieces := range sums {
			joined = append(joined, pieces)
		}
		out = append(out, joined)
	}
	return out, true
}

// bm25StateKey names a frequency set: its sorted terms and counts.
func bm25StateKey(frequencies map[string]int) string {
	keys := slices.Sorted(maps.Keys(frequencies))
	var key strings.Builder
	for _, term := range keys {
		key.WriteString(term)
		key.WriteByte(0)
		key.WriteString(strconv.Itoa(frequencies[term]))
		key.WriteByte(0)
	}
	return key.String()
}

// bm25Fit checks one rebuild against Zoekt's debug line. It checks the sum of
// the frequencies, and a length ratio within the two printed decimals that
// gives the score. The score falls as the ratio grows, so two score calls at
// the ends of the printed bracket check it. A hit with no term has a zero
// score and no ratio to check.
func bm25Fit(frequencies map[string]int, lowPriority bool, want int, printed, score float64) ([]int, bool) {
	sum := 0
	values := make([]int, 0, len(frequencies))
	for _, frequency := range frequencies {
		if frequency < 0 {
			return nil, false
		}
		if lowPriority {
			frequency /= bm25LowPriorityPenalty
		}
		sum += frequency
		if frequency > 0 {
			values = append(values, frequency)
		}
	}
	if sum != want {
		return nil, false
	}
	if len(values) == 0 {
		return values, true
	}
	low, high := max(0, printed-0.005-1e-9), printed+0.005+1e-9
	// A relative tolerance: an absolute one accepts wrong value sets at
	// very large ratios.
	tolerance := 1e-12 * math.Abs(score)
	if bm25Score(low, values) < score-tolerance || bm25Score(high, values) > score+tolerance {
		return nil, false
	}
	return values, true
}

// bm25Query is what the rebuild reads from a strict query: the atoms whose
// candidates Zoekt counts. Zoekt reads no atom under a Not or a type:file
// (index/eval.go gatherMatches). Each atom has Zoekt's term key
// (index/score.go calculateTermFrequency). That key is the lowered pattern of
// a substring atom of three or more runes. It is "" for every regexp, word,
// sym: regexp, or shorter substring atom.
type bm25Query struct {
	names []bm25NameAtom
	// substrings holds the lowered patterns of the content substring atoms,
	// sym: atoms included.
	substrings map[string]struct{}
	// regexps match the whole text of a content regexp candidate.
	regexps []*grafanaregexp.Regexp
	// multiLine is set when a content candidate can span lines: a regexp,
	// or a substring with a newline.
	multiLine bool
}

// bm25NameAtom is one file-name atom of a strict query, compiled once.
type bm25NameAtom struct {
	substring     string
	caseSensitive bool
	regexp        *grafanaregexp.Regexp
	// whole matches the whole text of one regexp candidate.
	whole *grafanaregexp.Regexp
}

// bm25Piece is the frequency of one term on one matched line.
type bm25Piece struct {
	key       string
	frequency int
}

// bm25Candidate is one file-name candidate: its byte range and term key.
type bm25Candidate struct {
	at, end int
	key     string
}

// compileBM25Query collects the atoms of q that the rebuild reads.
func compileBM25Query(q query.Q) bm25Query {
	bq := bm25Query{substrings: map[string]struct{}{}}
	// addRegexp adds an atom that Zoekt searches as a regexp: its candidates
	// share the key "".
	addRegexp := func(pattern string, fileName, caseSensitive bool) {
		flags := ""
		if !caseSensitive {
			flags = "(?i)"
		}
		whole, err := grafanaregexp.Compile(flags + "^(?:" + pattern + ")$")
		if err != nil {
			return
		}
		if !fileName {
			bq.regexps = append(bq.regexps, whole)
			bq.multiLine = true
			return
		}
		if found, err := grafanaregexp.Compile(flags + pattern); err == nil {
			bq.names = append(bq.names, bm25NameAtom{regexp: found, whole: whole})
		}
	}
	// addText adds a substring atom. Zoekt searches one shorter than its
	// ngram size as a regexp (index/matchtree.go newSubstringMatchTree).
	addText := func(pattern string, fileName, caseSensitive bool) {
		if pattern == "" {
			return
		}
		if utf8.RuneCountInString(pattern) < bm25NgramSize {
			addRegexp(grafanaregexp.QuoteMeta(pattern), fileName, caseSensitive)
			return
		}
		if !fileName {
			bq.substrings[strings.ToLower(pattern)] = struct{}{}
			bq.multiLine = bq.multiLine || strings.Contains(pattern, "\n")
			return
		}
		atom := bm25NameAtom{substring: pattern, caseSensitive: caseSensitive}
		if !caseSensitive {
			atom.substring = strings.ToLower(pattern)
		}
		bq.names = append(bq.names, atom)
	}
	var walk func(q query.Q)
	walk = func(q query.Q) {
		switch typed := q.(type) {
		case *query.And:
			for _, child := range typed.Children {
				walk(child)
			}
		case *query.Or:
			for _, child := range typed.Children {
				walk(child)
			}
		case *query.Symbol:
			// A sym: regexp, and a sym: substring shorter than the ngram
			// size, are symbol regexp candidates (index/matchtree.go).
			switch atom := typed.Expr.(type) {
			case *query.Substring:
				addText(atom.Pattern, false, atom.CaseSensitive)
			case *query.Regexp:
				addRegexp(atom.RegexpString(), false, atom.CaseSensitive)
			}
		case *query.Substring:
			addText(typed.Pattern, typed.FileName, typed.CaseSensitive)
		case *query.Regexp:
			// Zoekt searches a regexp that equals a substring tree as that
			// tree (index/eval.go regexpToMatchTreeRecursive).
			if leaves, ok := bm25RegexpSubstrings(typed.Regexp); ok {
				for _, leaf := range leaves {
					addText(leaf.text, typed.FileName, typed.CaseSensitive && leaf.caseSensitive)
				}
				return
			}
			addRegexp(typed.RegexpString(), typed.FileName, typed.CaseSensitive)
		}
		// Not and type:file: Zoekt gathers no candidate there.
	}
	walk(q)
	return bq
}

// bm25NgramSize is Zoekt's ngram size (index/shard_builder.go ngramSize).
const bm25NgramSize = 3

// bm25Leaf is one substring that Zoekt searches for a regexp.
type bm25Leaf struct {
	text          string
	caseSensitive bool
}

// bm25RegexpSubstrings returns the substrings that Zoekt searches in place of
// r, when its match tree equals r (index/eval.go regexpToMatchTreeRecursive).
// Such an r is a literal of bm25NgramSize or more runes, or an alternation of
// such regexps, under captures, x+ and x{1,n}. Zoekt marks the tree of any
// other form as not equal, also a concatenation of two or more parts.
func bm25RegexpSubstrings(r *syntax.Regexp) ([]bm25Leaf, bool) {
	switch r.Op {
	case syntax.OpLiteral:
		if len(r.Rune) >= bm25NgramSize {
			return []bm25Leaf{{text: string(r.Rune), caseSensitive: r.Flags&syntax.FoldCase == 0}}, true
		}
	case syntax.OpCapture, syntax.OpPlus:
		return bm25RegexpSubstrings(r.Sub[0])
	case syntax.OpRepeat:
		if r.Min == 1 {
			return bm25RegexpSubstrings(r.Sub[0])
		}
	case syntax.OpConcat, syntax.OpAlternate:
		if r.Op == syntax.OpConcat && len(r.Sub) != 1 {
			return nil, false
		}
		var leaves []bm25Leaf
		for _, sub := range r.Sub {
			subLeaves, ok := bm25RegexpSubstrings(sub)
			if !ok {
				return nil, false
			}
			leaves = append(leaves, subLeaves...)
		}
		return leaves, len(leaves) > 0
	}
	return nil, false
}

// contentKey returns Zoekt's term key of a content candidate with text. It
// gives false when no atom can make the candidate, or when both a substring
// atom and a regexp atom can.
func (bq bm25Query) contentKey(text string) (string, bool) {
	lowered := strings.ToLower(text)
	_, substring := bq.substrings[lowered]
	regexp := false
	for _, whole := range bq.regexps {
		if whole.MatchString(text) {
			regexp = true
			break
		}
	}
	switch {
	case substring && !regexp:
		return lowered, true
	case regexp && !substring:
		return "", true
	}
	return "", false
}

// nameKey returns the term key of a file-name fragment: a file-name atom's
// key, or the whole name when no atom made it. That last case is Zoekt's
// candidate for a file with no other match, and its key is the name.
func (bq bm25Query) nameKey(text, name string) (string, bool) {
	lowered := strings.ToLower(text)
	keys := map[string]struct{}{}
	for _, atom := range bq.names {
		switch {
		case atom.regexp != nil:
			if atom.whole.MatchString(text) {
				keys[""] = struct{}{}
			}
		case (atom.caseSensitive && text == atom.substring) || (!atom.caseSensitive && lowered == atom.substring):
			keys[strings.ToLower(atom.substring)] = struct{}{}
		}
	}
	switch {
	case len(keys) == 1:
		for key := range keys {
			return key, true
		}
	case len(keys) == 0 && text == name:
		return name, true
	}
	return "", false
}

// nameCandidates returns the key lists that Zoekt's file-name candidates on
// name can give. These are every atom's matches, sorted by offset with the
// longer first, less the overlapping ones (index/eval.go gatherMatches). Two
// atoms with other keys on one range give two lists. Zoekt's unstable sort
// keeps either, and bm25Parts lets the length ratio pick.
func (bq bm25Query) nameCandidates(name string) ([][]string, bool) {
	var candidates []bm25Candidate
	for _, atom := range bq.names {
		found, ok := atom.candidates(name)
		if !ok {
			return nil, false
		}
		candidates = append(candidates, found...)
	}
	return bm25CandidateKeys(candidates)
}

// bm25CandidateKeys applies Zoekt's overlap rule to file-name candidates. It
// returns the key lists that the remaining candidates can give: one per
// choice among the keys of a tied range.
func bm25CandidateKeys(candidates []bm25Candidate) ([][]string, bool) {
	slices.SortStableFunc(candidates, func(left, right bm25Candidate) int {
		if left.at != right.at {
			return left.at - right.at
		}
		return (right.end - right.at) - (left.end - left.at)
	})
	alternatives := [][]string{nil}
	end := -1
	for index := 0; index < len(candidates); {
		candidate := candidates[index]
		// The keys of every candidate on this exact range.
		keys := []string{candidate.key}
		next := index + 1
		for ; next < len(candidates) && candidates[next].at == candidate.at && candidates[next].end == candidate.end; next++ {
			if !slices.Contains(keys, candidates[next].key) {
				keys = append(keys, candidates[next].key)
			}
		}
		index = next
		if candidate.at < end {
			continue
		}
		end = candidate.end
		grown := make([][]string, 0, len(alternatives)*len(keys))
		for _, list := range alternatives {
			for _, key := range keys {
				grown = append(grown, append(slices.Clone(list), key))
			}
		}
		if len(grown) > bm25MaxSplitStates {
			return nil, false
		}
		alternatives = grown
	}
	return alternatives, true
}

// candidates returns the matches of the atom in name with their byte ranges.
func (atom bm25NameAtom) candidates(name string) ([]bm25Candidate, bool) {
	var found []bm25Candidate
	if atom.regexp != nil {
		for _, bounds := range atom.regexp.FindAllStringIndex(name, -1) {
			if bounds[1] > bounds[0] {
				found = append(found, bm25Candidate{at: bounds[0], end: bounds[1]})
			}
		}
		return found, true
	}
	key := strings.ToLower(atom.substring)
	haystack := name
	if !atom.caseSensitive {
		haystack = strings.ToLower(name)
		if !bm25ASCII(name) {
			// Lowercasing can change the byte length of a rune, so the
			// offsets of the lowered name are not the name's: fold case in a
			// regexp. Only a non-ASCII name pays for the compile.
			folded, err := grafanaregexp.Compile("(?i)" + grafanaregexp.QuoteMeta(atom.substring))
			if err != nil {
				return nil, false
			}
			for _, bounds := range folded.FindAllStringIndex(name, -1) {
				found = append(found, bm25Candidate{at: bounds[0], end: bounds[1], key: key})
			}
			return found, true
		}
	}
	for offset := 0; ; {
		at := strings.Index(haystack[offset:], atom.substring)
		if at < 0 {
			break
		}
		found = append(found, bm25Candidate{at: offset + at, end: offset + at + len(atom.substring), key: key})
		offset += at + len(atom.substring)
	}
	return found, true
}

// bm25ASCII reports a name with no byte outside ASCII.
func bm25ASCII(name string) bool {
	for index := 0; index < len(name); index++ {
		if name[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// lineFrequencies returns the splits of one matched line into term frequencies.
// keys holds the term key of each fragment of the line (multiLineMerges); when
// it is nil, the fragments' texts give the keys. A match on a symbol counts
// five, and Zoekt does not mark it on the fragment. So the line's own BM25
// score (index/score.go scoreLineBM25, with the line length over 100) decides
// how many matches of each term were on a symbol. Two or more splits can reach
// the line score; bm25Parts keeps the one that the file's length ratio fits. A
// line of two or more terms that needs more than 1<<14 search steps gives
// false, and the hit keeps Zoekt's score.
func (bq bm25Query) lineFrequencies(line zoekt.LineMatch, keys []string) ([][]bm25Piece, bool) {
	ratio := float64(len(line.Line)) / 100
	counts := map[string]int{}
	for index, fragment := range line.LineFragments {
		if keys != nil {
			counts[keys[index]]++
			continue
		}
		end := fragment.LineOffset + fragment.MatchLength
		if fragment.LineOffset < 0 || end > len(line.Line) {
			return nil, false
		}
		key, ok := bq.contentKey(string(line.Line[fragment.LineOffset:end]))
		if !ok {
			return nil, false
		}
		counts[key]++
	}
	if len(counts) == 0 {
		return nil, false
	}
	// A sized slice: this runs for every matched line.
	terms := make([]string, 0, len(counts))
	for term := range counts {
		terms = append(terms, term)
	}
	slices.Sort(terms)
	matches := func(score float64) bool {
		return math.Abs(score-line.Score) <= 1e-9*max(1, math.Abs(line.Score))
	}
	if len(terms) == 1 {
		// The term score grows with the frequency: one split at most, with
		// no search bound however many matches the line holds.
		count := counts[terms[0]]
		for symbol := 0; symbol <= count; symbol++ {
			frequency := count + (bm25ImportantTermBoost-1)*symbol
			if matches(bm25TermScore(ratio, frequency)) {
				return [][]bm25Piece{{{key: terms[0], frequency: frequency}}}, true
			}
		}
		return nil, false
	}
	symbols := make([]int, len(terms))
	var splits [][]bm25Piece
	tries := 0
	var search func(term int, score float64) bool
	search = func(term int, score float64) bool {
		if tries++; tries > 1<<14 {
			return false
		}
		if term == len(terms) {
			if matches(score) {
				pieces := make([]bm25Piece, len(terms))
				for index, name := range terms {
					pieces[index] = bm25Piece{key: name, frequency: counts[name] + (bm25ImportantTermBoost-1)*symbols[index]}
				}
				splits = append(splits, pieces)
			}
			return true
		}
		for symbol := 0; symbol <= counts[terms[term]]; symbol++ {
			symbols[term] = symbol
			if !search(term+1, score+bm25TermScore(ratio, counts[terms[term]]+(bm25ImportantTermBoost-1)*symbol)) {
				return false
			}
		}
		return true
	}
	if !search(0, 0) || len(splits) == 0 {
		return nil, false
	}
	return splits, true
}

// multiLineMerges finds the multi-line candidates. Zoekt breaks a candidate on
// newlines (index/matchtree.go breakOnNewlines). The first piece ends just
// before its line's newline, and the next starts at column 0. A piece that is
// no candidate alone joins the next pieces, with the newlines between. It
// stops when the text is one candidate, or the text and the newline after it
// is. It returns the key of each content fragment, in line and fragment order
// (a file-name line has none), and the change that makes each candidate count
// once. Each piece of a candidate takes the candidate's key, and each piece
// counts on its line, as in Zoekt's line score. With no atom that can span
// lines, it returns no keys, and lineFrequencies reads them from the texts.
func (bq bm25Query) multiLineMerges(lines []zoekt.LineMatch) ([]string, map[string]int, bool) {
	if !bq.multiLine {
		return nil, nil, true
	}
	count := 0
	for _, line := range lines {
		if !line.FileName {
			count += len(line.LineFragments)
		}
	}
	// The keys serve lineFrequencies, so each fragment gets one contentKey
	// call. Most hits have no piece that needs a join; only then do the
	// pieces matter.
	keys := make([]string, 0, count)
	joins := false
	for _, line := range lines {
		if line.FileName {
			continue
		}
		for _, fragment := range line.LineFragments {
			end := fragment.LineOffset + fragment.MatchLength
			if fragment.LineOffset < 0 || end > len(line.Line) {
				return nil, nil, false
			}
			key, alone := bq.contentKey(string(line.Line[fragment.LineOffset:end]))
			keys = append(keys, key)
			joins = joins || !alone
		}
	}
	if !joins {
		return keys, nil, true
	}
	type piece struct {
		text   string
		offset uint32
		end    uint32
		// at is the position of the piece's key in keys.
		at     int
		alone  bool
		atEnd  bool
		atZero bool
	}
	pieces := make([]piece, 0, count)
	for _, line := range lines {
		if line.FileName {
			continue
		}
		for _, fragment := range line.LineFragments {
			end := fragment.LineOffset + fragment.MatchLength
			text := string(line.Line[fragment.LineOffset:end])
			_, alone := bq.contentKey(text)
			pieces = append(pieces, piece{
				text:   text,
				offset: fragment.Offset,
				end:    fragment.Offset + uint32(fragment.MatchLength),
				at:     len(pieces),
				alone:  alone,
				atEnd:  end == len(line.Line)-1 && line.Line[end] == '\n',
				atZero: fragment.LineOffset == 0,
			})
		}
	}
	merges := map[string]int{}
	slices.SortFunc(pieces, func(left, right piece) int { return int(left.offset) - int(right.offset) })
	for index := 0; index < len(pieces); {
		if pieces[index].alone || !pieces[index].atEnd {
			if !pieces[index].alone {
				return nil, nil, false
			}
			index++
			continue
		}
		// Join the pieces of one candidate until the text is one match; the
		// candidate can also end with the newline after a piece.
		text := pieces[index].text
		var key string
		var joined bool
		next := index
		for {
			if key, joined = bq.contentKey(text + "\n"); joined {
				break
			}
			if next+1 >= len(pieces) || !pieces[next+1].atZero || pieces[next+1].offset <= pieces[next].end {
				break
			}
			text += strings.Repeat("\n", int(pieces[next+1].offset-pieces[next].end)) + pieces[next+1].text
			next++
			if key, joined = bq.contentKey(text); joined {
				break
			}
			if !pieces[next].atEnd {
				break
			}
		}
		if !joined {
			return nil, nil, false
		}
		for _, piece := range pieces[index : next+1] {
			keys[piece.at] = key
		}
		merges[key] -= next - index
		index = next + 1
	}
	return keys, merges, true
}

// bm25TermScore is Zoekt's score of one term (index/score.go tfScore).
func bm25TermScore(ratio float64, frequency int) float64 {
	return ((bm25K + 1) * float64(frequency)) / (bm25K*(1-bm25B+bm25B*ratio) + float64(frequency))
}

// bm25Score is the sum of the term scores at a length ratio.
func bm25Score(ratio float64, frequencies []int) float64 {
	score := 0.0
	for _, frequency := range frequencies {
		score += bm25TermScore(ratio, frequency)
	}
	return score
}

// solveBM25LengthRatio finds the length ratio whose score is score: the score
// falls as the ratio grows, so bisection converges.
func solveBM25LengthRatio(frequencies []int, score float64) (float64, bool) {
	low, high := 0.0, 1.0
	for bm25Score(high, frequencies) > score {
		high *= 2
		if high > 1e12 {
			return 0, false
		}
	}
	if bm25Score(low, frequencies) < score-1e-9 {
		return 0, false
	}
	for range 200 {
		mid := (low + high) / 2
		if bm25Score(mid, frequencies) > score {
			low = mid
		} else {
			high = mid
		}
	}
	return (low + high) / 2, true
}

// bm25Rescore returns the score of a hit under the global average, from the
// hit's Debug field. It returns false for a hit with no parts, and Zoekt's
// score stays.
func bm25Rescore(debug string, average float64) (float64, bool) {
	encoded, ok := strings.CutPrefix(debug, bm25PartsPrefix)
	if !ok {
		return 0, false
	}
	lengthText, valuesText, _ := strings.Cut(encoded, " ")
	length, err := strconv.ParseFloat(lengthText, 64)
	if err != nil || math.IsNaN(length) {
		return 0, false
	}
	var frequencies []int
	if valuesText != "" {
		for _, value := range strings.Split(valuesText, ",") {
			frequency, err := strconv.Atoi(value)
			if err != nil {
				return 0, false
			}
			frequencies = append(frequencies, frequency)
		}
	}
	return bm25Score(length/average, frequencies), true
}

// rescoreAverage returns the global average for a rescore of the strict hits.
// It returns false when no rescore is due. That is when the command has
// counts of fewer than two shards, or when a one-node command rescored its
// hits in place. It logs the rescore in place.
func (command *bm25Command) rescoreAverage() (float64, bool) {
	average, ok := command.average()
	if !command.final.Load() {
		return average, ok
	}
	kept := command.kept.Load()
	slog.Debug("Rescored strict hits with the global average", "hits", command.rebuilt.Load()+kept, "kept", kept, "average", average, "in_place", true)
	return 0, false
}

// rescoreStrictResults gives each strict result its score under the global
// average, before the merge orders them.
func rescoreStrictResults(results []corpusSearchResult, command *bm25Command) {
	average, ok := command.rescoreAverage()
	if !ok {
		return
	}
	kept := 0
	for index := range results {
		if score, ok := bm25Rescore(results[index].file.Debug, average); ok {
			results[index].file.Score = score
		} else {
			kept++
		}
	}
	slog.Debug("Rescored strict hits with the global average", "hits", len(results), "kept", kept, "average", average)
}

// rescoreStrictHits does the same for the strict hits of a model query.
func rescoreStrictHits(hits []nodeHit, command *bm25Command) {
	average, ok := command.rescoreAverage()
	if !ok {
		return
	}
	kept := 0
	for index := range hits {
		switch {
		case hits[index].strict:
			if score, ok := bm25Rescore(hits[index].display.file.Debug, average); ok {
				hits[index].score = float32(score)
				hits[index].display.file.Score = score
			} else {
				kept++
			}
		case hits[index].lexical:
			// A relaxed hit of a model query: its lexical rank takes the
			// same average.
			if score, ok := bm25Rescore(hits[index].lexicalParts, average); ok {
				hits[index].score = float32(score)
			} else {
				kept++
			}
		}
	}
	slog.Debug("Rescored strict hits with the global average", "hits", len(hits), "kept", kept, "average", average)
}
