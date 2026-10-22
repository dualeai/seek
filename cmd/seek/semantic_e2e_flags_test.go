package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// e2eFlagQuery is a description of two words that no fixture file or file
// name holds, so every result of it comes from vector search, unless a test
// writes a text file that holds both words.
const e2eFlagQuery = "zqalpha zqbeta"

var (
	e2eFlagNearMark  = regexp.MustCompile(`zqnear(\d\d)`)
	e2eFlagModelMark = regexp.MustCompile(`zqmodel(\d+)`)
)

// e2eFlagVector is the test vector of a unit. A unit with the marker zqnearNN
// has the cosine 0.NN to the query. A unit with SEMANTIC_TARGET has the query
// vector. Every other unit is orthogonal to the query.
func e2eFlagVector(t *testing.T) func(semanticUnit) semanticVector {
	return func(unit semanticUnit) semanticVector {
		if match := e2eFlagNearMark.FindSubmatch(unit.text); match != nil {
			near, err := strconv.Atoi(string(match[1]))
			if err != nil {
				t.Error(err)
			}
			cosine := float64(near) / 100
			vector := semanticVector{0: float32(cosine)}
			// Each level has its own second axis, which keeps the HNSW
			// graph sound.
			vector[2+near%(semanticEmbeddingDimensions-2)] = float32(math.Sqrt(1 - cosine*cosine))
			if err := normalizeSemanticVector(&vector); err != nil {
				t.Error(err)
			}
			return vector
		}
		if bytes.Contains(unit.text, []byte("SEMANTIC_TARGET")) {
			return semanticVector{0: 1}
		}
		return semanticVector{1: 1}
	}
}

// e2eFlagScorer is the test model with graded scores. It keeps the vectors
// and the call records of hybridTestScorer. A document with the marker
// zqmodelN scores N, a document with SEMANTIC_TARGET and no such marker
// scores 100, and every other document scores 0. The acceptance threshold is
// 0.6, so zqmodel0 rejects a file.
type e2eFlagScorer struct {
	*hybridTestScorer
}

// e2eFlagNewScorer returns a graded test model with the vectors of
// e2eFlagVector.
func e2eFlagNewScorer(t *testing.T) *e2eFlagScorer {
	return &e2eFlagScorer{&hybridTestScorer{vectorForUnit: e2eFlagVector(t)}}
}

func (scorer *e2eFlagScorer) ScoresWithSemanticQuery(
	ctx context.Context,
	query *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	if _, err := scorer.hybridTestScorer.ScoresWithSemanticQuery(ctx, query, documents); err != nil {
		return nil, err
	}
	scores := make([]float32, len(documents))
	for index, document := range documents {
		if match := e2eFlagModelMark.FindStringSubmatch(document.Text); match != nil {
			score, err := strconv.Atoi(match[1])
			if err != nil {
				return nil, err
			}
			scores[index] = float32(score)
		} else if strings.Contains(document.Text, "SEMANTIC_TARGET") {
			scores[index] = 100
		}
	}
	return scores, nil
}

// scoredNames returns the base name of each document that the model scored
// in its last call, sorted.
func (scorer *e2eFlagScorer) scoredNames() []string {
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	names := make([]string, 0, len(scorer.documents))
	for _, document := range scorer.documents {
		names = append(names, filepath.Base(document.Path))
	}
	slices.Sort(names)
	return names
}

// e2eFlagModel returns a model factory that gives scorer and counts its
// calls in calls, when calls is not nil.
func e2eFlagModel(scorer semanticModel, calls *atomic.Int32) semanticModelFactory {
	return func(context.Context) (semanticModel, error) {
		if calls != nil {
			calls.Add(1)
		}
		return scorer, nil
	}
}

// e2eFlagRunRoot runs the root command with args, as main does: the splice,
// Cobra's flag parse, PersistentPreRunE, PreRunE, and RunE. A non-nil newModel
// replaces the bundled model of the search; nil keeps it.
func e2eFlagRunRoot(t *testing.T, newModel semanticModelFactory, args ...string) (string, error) {
	t.Helper()
	logger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(logger) })
	if newModel == nil {
		newModel = newLateOnSemanticModel
	}
	root := newRootCmd(newModel)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(splicePassthroughSeparator(args, root.Flags()))
	return captureStdout(t, func() error { return root.ExecuteContext(t.Context()) })
}

// e2eFlagColdWarm runs search on an empty cache, where it builds every index
// and every vector (cold), then once more (warm). Both runs must print the
// same and end with the same error. It returns the cold output and error.
func e2eFlagColdWarm(t *testing.T, search func() (string, error)) (string, error) {
	t.Helper()
	setTestUserCache(t)
	cold, coldErr := search()
	warm, warmErr := search()
	if cold != warm || fmt.Sprint(coldErr) != fmt.Sprint(warmErr) {
		t.Fatalf("cold and warm searches differ:\ncold (err=%v):\n%s\nwarm (err=%v):\n%s", coldErr, cold, warmErr, warm)
	}
	return cold, coldErr
}

// e2eFlagHeaders returns the path of each result header, in output order.
func e2eFlagHeaders(output string) []string {
	var paths []string
	for line := range strings.Lines(output) {
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			paths = append(paths, strings.Fields(rest)[0])
		}
	}
	return paths
}

// e2eFlagBlocks splits output into one block per result header: the header
// path maps to the lines that follow it, up to the next header.
func e2eFlagBlocks(output string) map[string]string {
	blocks := make(map[string]string)
	var current string
	for line := range strings.Lines(output) {
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			current = strings.Fields(rest)[0]
			blocks[current] = ""
			continue
		}
		if current != "" && line != "\n" {
			blocks[current] += line
		}
	}
	return blocks
}

// e2eFlagContextFile is one fixture file of the context tests: its lines and
// the line where its vector unit starts. The output prints that line as the
// match line.
type e2eFlagContextFile struct {
	lines []string
	match int
}

// content returns the file bytes. A file with noNewline has no newline after
// its last line.
func (file e2eFlagContextFile) content(noNewline bool) string {
	text := strings.Join(file.lines, "\n")
	if !noNewline {
		text += "\n"
	}
	return text
}

// span returns the block that the output prints for the lines first to last
// of the file: each line with its number.
func (file e2eFlagContextFile) span(first, last int) string {
	first = max(first, 1)
	last = min(last, len(file.lines))
	var block strings.Builder
	for number := first; number <= last; number++ {
		fmt.Fprintf(&block, "%d %s\n", number, file.lines[number-1])
	}
	return block.String()
}

// e2eFlagContextShapes returns the three file shapes of the context tests.
// Each shape holds tag, the name of the file, so no two files have the same
// content. The vector unit starts on line 2 of 6 in "start", line 7 of 16 in
// "mid", and line 7 of 7 in "end".
func e2eFlagContextShapes(tag string) map[string]e2eFlagContextFile {
	return map[string]e2eFlagContextFile{
		"start": {match: 2, lines: []string{
			"package p // " + tag,
			"func zqStart() {",
			"\t// SEMANTIC_TARGET start " + tag,
			"\t_ = 4",
			"}",
			"// tail 6",
		}},
		"mid": {match: 7, lines: []string{
			"package p",
			"// " + tag + " line 2",
			"// line 3",
			"// line 4",
			"// line 5",
			"// line 6",
			"func zqMid() {",
			"\t// SEMANTIC_TARGET mid " + tag,
			"\t_ = 9",
			"\t_ = 10",
			"\t_ = 11",
			"}",
			"",
			"func zqTail() {",
			"\t_ = 15",
			"}",
		}},
		"end": {match: 7, lines: []string{
			"package p",
			"// " + tag + " line 2",
			"// line 3",
			"// line 4",
			"// line 5",
			"// line 6",
			"func zqEnd() { _ = \"SEMANTIC_TARGET end " + tag + "\" }",
		}},
	}
}

// TestE2EFlagContextLinesOfAVectorHit runs the root command with the context
// flags on vector hits near the start, in the middle, and at the end of a
// file. Each shape is in a folder node and in a Git node's committed rows. It
// is also in the dirty overlay: an untracked file, and a modified file whose
// unit moved. The expected lines come from the fixture by hand. They are the
// match line and up to N lines on each side that the flag selects. The start
// and the end of the file cut them.
func TestE2EFlagContextLinesOfAVectorHit(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	type fixtureFile struct {
		path      string
		shape     e2eFlagContextFile
		noNewline bool
	}
	var files []fixtureFile
	for _, node := range []string{"folder", "committed", "untracked", "modified"} {
		for name, shape := range e2eFlagContextShapes(node) {
			path := filepath.Join(repo, node+"_"+name+".go")
			if node == "folder" {
				path = f.path("tree/loose/" + node + "_" + name + ".go")
			}
			files = append(files, fixtureFile{path: path, shape: shape, noNewline: name == "end"})
		}
	}
	for _, file := range files {
		switch base := filepath.Base(file.path); {
		case strings.HasPrefix(base, "modified_"):
			// The committed copy has its unit on line 1; the work tree
			// moves it.
			writeFixtureFile(t, file.path, "package p // SEMANTIC_TARGET committed copy of "+base+"\n")
		case strings.HasPrefix(base, "untracked_"):
		default:
			writeFixtureFile(t, file.path, file.shape.content(file.noNewline))
		}
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "context fixture")
	for _, file := range files {
		if base := filepath.Base(file.path); strings.HasPrefix(base, "modified_") || strings.HasPrefix(base, "untracked_") {
			writeFixtureFile(t, file.path, file.shape.content(file.noNewline))
		}
	}
	t.Chdir(t.TempDir())

	for _, test := range []struct {
		name          string
		flags         []string
		before, after int
	}{
		// The README gives up to 3 lines on each side by default.
		{"default", nil, 3, 3},
		{"-C 0", []string{"-C", "0"}, 0, 0},
		{"-C 1", []string{"-C", "1"}, 1, 1},
		{"-C 6", []string{"-C", "6"}, 6, 6},
		{"-C 50", []string{"-C", "50"}, 50, 50},
		{"--context=2", []string{"--context=2"}, 2, 2},
		{"-A 0", []string{"-A", "0"}, 0, 0},
		{"-A 2", []string{"-A", "2"}, 0, 2},
		{"-A 50", []string{"-A", "50"}, 0, 50},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append(slices.Clone(test.flags), e2eFlagQuery, f.path("tree"))
			output, err := e2eFlagColdWarm(t, func() (string, error) {
				return e2eFlagRunRoot(t, e2eFlagModel(e2eFlagNewScorer(t), nil), args...)
			})
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
			blocks := e2eFlagBlocks(output)
			if len(blocks) != len(files) {
				t.Fatalf("%v printed %d files, want %d:\n%s", args, len(blocks), len(files), output)
			}
			for _, file := range files {
				want := file.shape.span(file.shape.match-test.before, file.shape.match+test.after)
				if got := blocks[file.path]; got != want {
					t.Errorf("%v: %s prints\n%s\nwant\n%s", args, filepath.Base(file.path), got, want)
				}
			}
		})
	}
}

// TestE2EFlagNoBeforeContextFlag checks the documented flag set: the README
// Flags table has -A and -C, and no flag for the lines before a match only.
// The root command has no -B and no --before-context. Both forms stop with
// exit status 2 before any search, so no model starts and nothing prints.
func TestE2EFlagNoBeforeContextFlag(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "target.go", "package p\n// SEMANTIC_TARGET\n")
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	flags := newRootCmd(newLateOnSemanticModel).Flags()
	if flags.ShorthandLookup("B") != nil || flags.Lookup("before-context") != nil {
		t.Fatal("the root command has a before-context flag that the README does not list")
	}
	for _, args := range [][]string{
		{"--before-context", "2", e2eFlagQuery, folder},
		// -B is not a flag, so the splice makes it the query and 2 a
		// path operand that does not exist.
		{"-B", "2", e2eFlagQuery, folder},
	} {
		var calls atomic.Int32
		output, err := e2eFlagRunRoot(t, e2eFlagModel(e2eFlagNewScorer(t), &calls), args...)
		if exitCodeForError(err) != 2 || output != "" || calls.Load() != 0 {
			t.Fatalf("%v: exit status %d (err=%v), model starts %d, output:\n%s", args, exitCodeForError(err), err, calls.Load(), output)
		}
	}
}

// e2eFlagRankFixture writes the fixture of the -n tests under a tree with a
// folder node (tree/loose) and a Git node (tree/repo). Each file has one
// unit, a cosine to the query (zqnearNN), and a model score (zqmodelN). It
// makes the tree the current folder and returns the accepted files from best
// to worst, as paths below the tree.
//
// The order comes by hand from the rule of acceptFiles: the files that only
// vectors find share one lexical rank, so the model score alone orders them.
// Thus the order is committed.go (8), untracked.go (7), modified.go (6),
// fifth.go (5), and first.go (1). The order by cosine puts first.go first.
func e2eFlagRankFixture(t *testing.T) []string {
	t.Helper()
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	write := func(rel, body string) {
		writeFixtureFile(t, f.path("tree/"+rel), "package p\n// "+body+"\n")
	}
	write("loose/first.go", "zqnear98 zqmodel1 first")
	write("repo/committed.go", "zqnear96 zqmodel8 committed")
	// The committed copy of modified.go would rank first.
	write("repo/modified.go", "zqnear97 zqmodel10 committed copy")
	write("loose/fifth.go", "zqnear90 zqmodel5 fifth")
	// The model rejects the nearest file.
	write("repo/decoy.go", "zqnear99 zqmodel0 decoy")
	// The model would accept this file, but it is outside the radius.
	write("loose/far.go", "zqnear30 zqmodel9 far")
	write("repo/deleted.go", "zqnear95 zqmodel9 deleted")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "rank fixture")
	write("repo/modified.go", "zqnear92 zqmodel6 work tree copy")
	write("repo/untracked.go", "zqnear94 zqmodel7 untracked")
	if err := os.Remove(filepath.Join(repo, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.path("tree"))
	return []string{
		"repo/committed.go",
		"repo/untracked.go",
		"repo/modified.go",
		"loose/fifth.go",
		"loose/first.go",
	}
}

// TestE2EFlagLimitKeepsTheTopOfTheModelOrder runs the root command with -n on
// files that only vectors find. The files are in a folder node, and in a Git
// node's committed rows and dirty overlay. Each -n prints the first n files
// of the fused order worked out by hand (e2eFlagRankFixture), and a footer
// counts the other accepted files. The model scores the same files under
// every -n: -n limits only the display. A file that the model rejects, a
// file outside the radius, a deleted file, and the committed copy of a
// modified file never print.
func TestE2EFlagLimitKeepsTheTopOfTheModelOrder(t *testing.T) {
	requireTools(t)
	order := e2eFlagRankFixture(t)
	// The model scores every file within the radius: the accepted files and
	// the decoy. The work-tree copy of modified.go replaces its committed
	// copy.
	wantScored := []string{"committed.go", "decoy.go", "fifth.go", "first.go", "modified.go", "untracked.go"}
	for _, test := range []struct {
		flags []string
		shown int
		// footer is the last line of the output when files are hidden.
		footer string
	}{
		{nil, 5, ""},
		{[]string{"-n", "0"}, 5, ""},
		{[]string{"-n", "1"}, 1, "… 4 more files (showing 1 of 5)\n"},
		{[]string{"-n", "2"}, 2, "… 3 more files (showing 2 of 5)\n"},
		{[]string{"--limit=3"}, 3, "… 2 more files (showing 3 of 5)\n"},
		{[]string{"-n", "4"}, 4, "… 1 more file (showing 4 of 5)\n"},
		{[]string{"-n", "5"}, 5, ""},
		{[]string{"-n", "6"}, 5, ""},
	} {
		t.Run(strings.Join(append([]string{"flags"}, test.flags...), " "), func(t *testing.T) {
			var scorers []*e2eFlagScorer
			args := append(slices.Clone(test.flags), e2eFlagQuery, ".")
			output, err := e2eFlagColdWarm(t, func() (string, error) {
				scorer := e2eFlagNewScorer(t)
				scorers = append(scorers, scorer)
				return e2eFlagRunRoot(t, e2eFlagModel(scorer, nil), args...)
			})
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
			if got := e2eFlagHeaders(output); !slices.Equal(got, order[:test.shown]) {
				t.Fatalf("%v prints %v, want %v:\n%s", args, got, order[:test.shown], output)
			}
			if test.footer == "" && strings.Contains(output, "more file") || !strings.HasSuffix(output, test.footer) {
				t.Fatalf("%v: want the footer %q only when files are hidden:\n%s", args, test.footer, output)
			}
			if !strings.Contains(output, "work tree copy") && test.shown >= 3 {
				t.Fatalf("%v prints the committed copy of modified.go:\n%s", args, output)
			}
			for _, scorer := range scorers {
				if got := scorer.scoredNames(); !slices.Equal(got, wantScored) {
					t.Fatalf("%v: the model scored %v, want %v", args, got, wantScored)
				}
			}
		})
	}
}

// TestE2EFlagMaxMatchesKeepsEachVectorHit runs the root command with -m. The
// vector hits are in a folder node, and in a Git node's committed rows and
// dirty overlay. The test also needs one text hit: a file with three lines
// that hold both description words. A vector hit is one match. Thus every -m
// of 1 or more prints it with all its context lines. -m cuts only the text
// file to its first matches.
func TestE2EFlagMaxMatchesKeepsEachVectorHit(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	shapes := map[string]e2eFlagContextFile{}
	for _, node := range []string{"folder", "committed", "untracked"} {
		shape := e2eFlagContextShapes(node)["mid"]
		path := filepath.Join(repo, node+".go")
		if node == "folder" {
			path = f.path("tree/loose/folder.go")
		}
		shapes[path] = shape
		if node != "untracked" {
			writeFixtureFile(t, path, shape.content(false))
		}
	}
	var text strings.Builder
	text.WriteString("package p\n")
	for line := 2; line <= 30; line++ {
		if line%10 == 0 {
			fmt.Fprintf(&text, "// %s text match %d\n", e2eFlagQuery, line)
			continue
		}
		fmt.Fprintf(&text, "// filler %d\n", line)
	}
	textPath := filepath.Join(repo, "text.go")
	writeFixtureFile(t, textPath, text.String())
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "max matches fixture")
	untracked := filepath.Join(repo, "untracked.go")
	writeFixtureFile(t, untracked, shapes[untracked].content(false))
	t.Chdir(t.TempDir())

	for _, test := range []struct {
		flags       []string
		context     int
		textMatches int
		// footer is the last line of the text file when matches are hidden.
		footer string
	}{
		{nil, 3, 3, ""},
		{[]string{"-m", "0"}, 3, 3, ""},
		{[]string{"-m", "1"}, 3, 1, "… 2 more matches in this file\n"},
		{[]string{"--max-matches=2"}, 3, 2, "… 1 more match in this file\n"},
		{[]string{"-m", "1", "-C", "1"}, 1, 1, "… 2 more matches in this file\n"},
		{[]string{"-m", "1", "-C", "0"}, 0, 1, "… 2 more matches in this file\n"},
	} {
		t.Run(strings.Join(append([]string{"flags"}, test.flags...), " "), func(t *testing.T) {
			args := append(slices.Clone(test.flags), e2eFlagQuery, f.path("tree"))
			output, err := e2eFlagColdWarm(t, func() (string, error) {
				return e2eFlagRunRoot(t, e2eFlagModel(e2eFlagNewScorer(t), nil), args...)
			})
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
			blocks := e2eFlagBlocks(output)
			if len(blocks) != len(shapes)+1 {
				t.Fatalf("%v printed %d files, want %d:\n%s", args, len(blocks), len(shapes)+1, output)
			}
			for path, shape := range shapes {
				want := shape.span(shape.match-test.context, shape.match+test.context)
				if got := blocks[path]; got != want {
					t.Errorf("%v: the vector hit %s prints\n%s\nwant\n%s", args, filepath.Base(path), got, want)
				}
			}
			textBlock := blocks[textPath]
			if got := strings.Count(textBlock, e2eFlagQuery); got != test.textMatches {
				t.Errorf("%v: the text file prints %d matches, want %d:\n%s", args, got, test.textMatches, textBlock)
			}
			footers := 0
			if test.footer != "" {
				footers = 1
			}
			if !strings.HasSuffix(textBlock, test.footer) || strings.Count(output, "more match") != footers {
				t.Errorf("%v: want the line %q only under the text file:\n%s", args, test.footer, output)
			}
		})
	}
}

// TestE2EFlagRootGivesLimitAndMaxMatchesToTheSearch checks that the root
// command gives -n to the file limit and -m to the match limit of a one-word
// search with the bundled model. Each of three files holds the word three
// times, ten lines apart, so each file is a strict hit and no context line
// holds a match. The expected counts and footers come from the fixture by
// hand.
func TestE2EFlagRootGivesLimitAndMaxMatchesToTheSearch(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		var text strings.Builder
		fmt.Fprintf(&text, "package %s\n", name)
		for line := 2; line <= 30; line++ {
			if line%10 == 0 {
				fmt.Fprintf(&text, "// zqlimit %s %d\n", name, line)
				continue
			}
			fmt.Fprintf(&text, "// filler %d\n", line)
		}
		writeFileAt(t, folder, name+".go", text.String())
	}
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		flags          []string
		files, matches int
		// fileFooter ends the output, and matchFooter ends each file, when
		// the flag hides files or matches.
		fileFooter, matchFooter string
	}{
		{nil, 3, 3, "", ""},
		{[]string{"-n", "2"}, 2, 3, "… 1 more file (showing 2 of 3)\n", ""},
		{[]string{"-m", "1"}, 3, 1, "", "… 2 more matches in this file\n"},
		{[]string{"--limit=1", "--max-matches=2"}, 1, 2, "… 2 more files (showing 1 of 3)\n", "… 1 more match in this file\n"},
	} {
		args := append(slices.Clone(test.flags), "zqlimit", folder)
		output, err := e2eFlagRunRoot(t, nil, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output)
		}
		headers := len(e2eFlagHeaders(output))
		matches := strings.Count(output, "zqlimit")
		if headers != test.files || matches != test.files*test.matches {
			t.Errorf("%v prints %d files and %d matches, want %d files and %d matches:\n%s",
				args, headers, matches, test.files, test.files*test.matches, output)
		}
		if test.fileFooter == "" && strings.Contains(output, "more file") || !strings.HasSuffix(output, test.fileFooter) {
			t.Errorf("%v: want the footer %q only when files are hidden:\n%s", args, test.fileFooter, output)
		}
		matchFooters := 0
		if test.matchFooter != "" {
			matchFooters = test.files
		}
		if strings.Count(output, "more match") != matchFooters || matchFooters > 0 && strings.Count(output, test.matchFooter) != matchFooters {
			t.Errorf("%v: want the line %q under each file only when matches are hidden:\n%s", args, test.matchFooter, output)
		}
	}
}

// e2eFlagTargetTree writes a tree with vector targets. One is in a folder
// node (tree/loose/folder.go), and one in a Git node's committed rows
// (tree/repo/committed.go). Two are in its dirty overlay: an untracked file
// and a modified file. The target files hold no description word. text holds
// the content of tree/repo/text.go, or no file when it is empty. It returns
// the tree.
func e2eFlagTargetTree(t *testing.T, text string) string {
	t.Helper()
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	writeFixtureFile(t, f.path("tree/loose/folder.go"), "package p\n// SEMANTIC_TARGET folder\n")
	writeFixtureFile(t, filepath.Join(repo, "committed.go"), "package p\n// SEMANTIC_TARGET committed\n")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package p\n// plain committed text\n")
	if text != "" {
		writeFixtureFile(t, filepath.Join(repo, "text.go"), text)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "target tree")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package p\n// SEMANTIC_TARGET modified\n")
	writeFixtureFile(t, filepath.Join(repo, "untracked.go"), "package p\n// SEMANTIC_TARGET untracked\n")
	t.Chdir(f.path("tree"))
	return f.path("tree")
}

// e2eFlagSemanticCachePaths returns the cache paths of meaning-based data and
// of the bundled model's runtime below the HOME that setTestUserCache set.
func e2eFlagSemanticCachePaths(t *testing.T) []string {
	t.Helper()
	return cachePathsMatching(t, os.Getenv("HOME"), func(name string) bool {
		return name == joinedGenerationFile || name == semanticUnitsDir ||
			name == rerankerDir || strings.HasPrefix(name, semanticGenerationPrefix)
	})
}

// TestE2EFlagLexicalOnlyPrintsNoVectorResult runs the root command with
// --lexical-only, which gives RunE the lexical-only search policy. The vector
// targets are in a folder node, and in a Git node's committed rows and dirty
// overlay. The test also needs one text hit: a file with both description
// words. The default search prints the text file and the vector targets.
// --lexical-only prints only the text file, the one file that holds every
// word. On a cold cache, it writes no meaning-based data and does not install
// the bundled model. With vectors on disk, it adds no such cache path. When
// only vectors match, --lexical-only finds nothing.
func TestE2EFlagLexicalOnlyPrintsNoVectorResult(t *testing.T) {
	requireTools(t)
	e2eFlagTargetTree(t, "package p\n// "+e2eFlagQuery+" text\n")
	vectorFiles := []string{"loose/folder.go", "repo/committed.go", "repo/modified.go", "repo/text.go", "repo/untracked.go"}

	t.Run("cold", func(t *testing.T) {
		output, err := e2eFlagColdWarm(t, func() (string, error) {
			return e2eFlagRunRoot(t, nil, "--lexical-only", e2eFlagQuery, ".")
		})
		if err != nil || !slices.Equal(e2eFlagHeaders(output), []string{"repo/text.go"}) {
			t.Fatalf("err=%v, want only the text file:\n%s", err, output)
		}
		if paths := e2eFlagSemanticCachePaths(t); len(paths) != 0 {
			t.Fatalf("--lexical-only wrote meaning-based data or installed the model: %v", paths)
		}
	})

	t.Run("vectors on disk", func(t *testing.T) {
		setTestUserCache(t)
		described, err := e2eFlagRunRoot(t, e2eFlagModel(e2eFlagNewScorer(t), nil), e2eFlagQuery, ".")
		if got := e2eFlagHeaders(described); err != nil || !slices.Equal(e2eFlagSorted(got), vectorFiles) {
			t.Fatalf("the default search printed %v (err=%v), want %v", got, err, vectorFiles)
		}
		built := e2eFlagSemanticCachePaths(t)
		if len(built) == 0 {
			t.Fatal("the default search wrote no meaning-based data")
		}
		output, err := e2eFlagRunRoot(t, nil, "--lexical-only", e2eFlagQuery, ".")
		if err != nil || !slices.Equal(e2eFlagHeaders(output), []string{"repo/text.go"}) {
			t.Fatalf("err=%v, want only the text file:\n%s", err, output)
		}
		if paths := e2eFlagSemanticCachePaths(t); !slices.Equal(paths, built) {
			t.Fatalf("--lexical-only changed the meaning-based data or installed the model: %v, want %v", paths, built)
		}
		// Only vectors match a description that no file holds whole.
		const vectorOnly = "zqalpha zqgamma"
		described, err = e2eFlagRunRoot(t, e2eFlagModel(e2eFlagNewScorer(t), nil), vectorOnly, ".")
		if err != nil || len(e2eFlagHeaders(described)) != 4 {
			t.Fatalf("the default search of %q: err=%v\n%s", vectorOnly, err, described)
		}
		output, err = e2eFlagRunRoot(t, nil, "--lexical-only", "-n", "1", "-m", "1", "-C", "0", vectorOnly, ".")
		if !errors.Is(err, errNoMatch) || output != "" {
			t.Fatalf("--lexical-only %q: err=%v output:\n%s", vectorOnly, err, output)
		}
	})
}

// e2eFlagSorted returns a sorted copy of paths.
func e2eFlagSorted(paths []string) []string {
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	return sorted
}

// e2eFlagANSI matches one ANSI color code.
var e2eFlagANSI = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestE2EFlagColorOfAVectorHit prints vector hits of a folder node and of a
// Git node's committed rows and dirty overlay in color and as plain text.
// Color is presentation only: with the codes removed, the color output is
// the plain output. The header path is cyan and each line number is dim. A
// vector hit holds no query word, so no span has the match color. NO_COLOR
// wins over CLICOLOR_FORCE, and output through a pipe is plain.
func TestE2EFlagColorOfAVectorHit(t *testing.T) {
	requireTools(t)
	e2eFlagTargetTree(t, "")
	search := func() (string, error) {
		return e2eFlagRunRoot(t, e2eFlagModel(e2eFlagNewScorer(t), nil), e2eFlagQuery, ".")
	}
	unsetenv := func(name string) {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}

	unsetenv("NO_COLOR")
	unsetenv("CLICOLOR_FORCE")
	t.Setenv("TERM", "xterm-256color")
	plain, err := e2eFlagColdWarm(t, search)
	if err != nil || strings.Contains(plain, "\x1b") {
		t.Fatalf("err=%v, want plain output through a pipe:\n%q", err, plain)
	}
	want := []string{"loose/folder.go", "repo/committed.go", "repo/modified.go", "repo/untracked.go"}
	if got := e2eFlagSorted(e2eFlagHeaders(plain)); !slices.Equal(got, want) {
		t.Fatalf("plain output prints %v, want %v:\n%s", got, want, plain)
	}

	t.Setenv("CLICOLOR_FORCE", "1")
	color, err := e2eFlagColdWarm(t, search)
	if err != nil {
		t.Fatal(err)
	}
	if got := e2eFlagANSI.ReplaceAllString(color, ""); got != plain {
		t.Fatalf("the color output without its codes differs from the plain output:\n%q\nplain:\n%q", got, plain)
	}
	// The codes are cyan (36), dim (2), bold red (1;31), and reset (0).
	dimNumber := regexp.MustCompile("^\x1b\\[2m[0-9]+\x1b\\[0m ")
	for line := range strings.Lines(color) {
		switch {
		case line == "\n":
		case strings.HasPrefix(line, "## "):
			if !strings.HasPrefix(line, "## \x1b[36m") || !strings.Contains(line, "\x1b[0m (Go)") {
				t.Errorf("the header is not cyan: %q", line)
			}
		case !dimNumber.MatchString(line):
			t.Errorf("the line number is not dim: %q", line)
		}
	}
	if strings.Contains(color, "\x1b[1;31m") {
		t.Errorf("a vector hit has a match color:\n%q", color)
	}

	t.Setenv("NO_COLOR", "1")
	forced, err := search()
	if err != nil || forced != plain {
		t.Fatalf("NO_COLOR with CLICOLOR_FORCE: err=%v, want the plain output:\n%q", err, forced)
	}
}

// TestE2EFlagNoVectorResultExitsOneWithNoOutput runs the root command where
// vector search finds no file to print. The files are in a folder node, and
// in a Git node's committed rows and dirty overlay. In one case, every unit
// is outside the radius: the model would accept each file, but it scores
// none. In the other case, every unit is within the radius, and the model
// rejects each file. The search prints nothing, ends with exit status 1, and
// reports no error. This is also true with -n, -m and -C, on a cold and on a
// warm cache.
func TestE2EFlagNoVectorResultExitsOneWithNoOutput(t *testing.T) {
	requireTools(t)
	for _, test := range []struct {
		name, marks string
		scored      bool
	}{
		{"outside the radius", "zqnear30 zqmodel9", false},
		{"rejected by the model", "zqnear95 zqmodel0", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newGraphFixture(t)
			repo := f.GitRepo("tree/repo", "committed.go", "package p\n// "+test.marks+" committed\n")
			writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package p\n// plain committed text\n")
			gitRunIn(t, repo, "add", ".")
			gitRunIn(t, repo, "commit", "-q", "-m", "no result")
			writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package p\n// "+test.marks+" modified\n")
			writeFixtureFile(t, filepath.Join(repo, "untracked.go"), "package p\n// "+test.marks+" untracked\n")
			writeFixtureFile(t, f.path("tree/loose/folder.go"), "package p\n// "+test.marks+" folder\n")
			t.Chdir(f.path("tree"))
			for _, flags := range [][]string{nil, {"-n", "1", "-m", "1", "-C", "5"}} {
				var scorers []*e2eFlagScorer
				args := append(slices.Clone(flags), e2eFlagQuery, ".")
				output, err := e2eFlagColdWarm(t, func() (string, error) {
					scorer := e2eFlagNewScorer(t)
					scorers = append(scorers, scorer)
					return e2eFlagRunRoot(t, e2eFlagModel(scorer, nil), args...)
				})
				var stderr bytes.Buffer
				reportCLIError(&stderr, err, false)
				if output != "" || exitCodeForError(err) != 1 || stderr.Len() != 0 {
					t.Fatalf("%v: exit status %d (err=%v) stderr=%q output:\n%s", args, exitCodeForError(err), err, stderr.String(), output)
				}
				for _, scorer := range scorers {
					if scored := len(scorer.scoredNames()) == 4; scored != test.scored {
						t.Fatalf("%v: the model scored %v, want all four files: %t", args, scorer.scoredNames(), test.scored)
					}
				}
			}
		})
	}
}

// TestE2EFlagPathOperandsScopeVectorHits runs a description from inside a Git
// repository with path operands. The operands are a subdirectory of the
// repository (the path-operand scope, row T39 of querySupport), exact files,
// and an external folder outside the Git worktree. They are also an exact
// file there, Git and folder operands together, symlinked operands, and an
// operand given twice. Each target file holds the word zqopmark. A one-word
// text search with the same operands is the oracle. The vector results must
// be the oracle's files and the hand-made set. The model scores only those
// files, so it never scores a file that the scope drops. The Git node has
// committed, modified, untracked, deleted, and changed-away files.
//
// An exact file outside Git is a folder node of one file. The README says
// that every searched folder adds files by meaning. An exact file in Git
// gets its vector hit. Thus the cases "external exact file", "git file and
// folder file", and "symlinked file" expect the vector hit too.
func TestE2EFlagPathOperandsScopeVectorHits(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("work/repo", "README.md", "readme\n")
	target := func(name string) string {
		return "package p\n// zqopmark SEMANTIC_TARGET " + name + "\n"
	}
	committed := map[string]string{
		"in/in_committed.go":    target("in committed"),
		"in/sub/in_deep.go":     target("in deep"),
		"in/in_modified.go":     "package p\n// plain committed text\n",
		"in/in_changed_away.go": target("in changed away"),
		"in/in_deleted.go":      target("in deleted"),
		"in2/in2_sibling.go":    target("in2 sibling"),
		"out/out_committed.go":  target("out committed"),
		"top.go":                target("top"),
	}
	for rel, content := range committed {
		writeFixtureFile(t, filepath.Join(repo, rel), content)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "operand fixture")
	writeFixtureFile(t, filepath.Join(repo, "in/in_modified.go"), target("in modified"))
	writeFixtureFile(t, filepath.Join(repo, "in/in_changed_away.go"), "package p\n// plain work tree text\n")
	if err := os.Remove(filepath.Join(repo, "in/in_deleted.go")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "in/in_untracked.go"), target("in untracked"))
	writeFixtureFile(t, filepath.Join(repo, "out/out_untracked.go"), target("out untracked"))
	ext := f.path("ext")
	writeFixtureFile(t, filepath.Join(ext, "ext_notes.go"), target("ext notes"))
	writeFixtureFile(t, filepath.Join(ext, "sub/ext_deep.go"), target("ext deep"))
	writeFixtureFile(t, filepath.Join(ext, "ext_plain.go"), "package p\n// plain folder text\n")
	links := f.path("links")
	if err := os.MkdirAll(links, 0o755); err != nil {
		t.Fatal(err)
	}
	extLink := filepath.Join(links, "extlink")
	fileLink := filepath.Join(links, "notelink.go")
	if err := os.Symlink(ext, extLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(ext, "ext_notes.go"), fileLink); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	inScope := []string{"in_committed.go", "in_deep.go", "in_modified.go", "in_untracked.go"}
	extFiles := []string{"ext_deep.go", "ext_notes.go"}
	for _, test := range []struct {
		name     string
		operands []string
		// want holds the base name of each header, from the fixture.
		want []string
		// prefix is the start of every header, when the test sets it.
		prefix string
	}{
		{"git subdirectory", []string{"in"}, inScope, "in/"},
		{"git subdirectory with a slash", []string{"./in/"}, inScope, "in/"},
		{"git root", []string{"."}, append(slices.Clone(inScope), "in2_sibling.go", "out_committed.go", "out_untracked.go", "top.go"), ""},
		{"git exact committed file", []string{"in/in_committed.go"}, []string{"in_committed.go"}, "in/in_committed.go"},
		{"git exact modified file", []string{"in/in_modified.go"}, []string{"in_modified.go"}, "in/in_modified.go"},
		{"git exact untracked file", []string{"in/in_untracked.go"}, []string{"in_untracked.go"}, "in/in_untracked.go"},
		{"external folder", []string{ext}, extFiles, ext + "/"},
		{"external exact file", []string{filepath.Join(ext, "ext_notes.go")}, []string{"ext_notes.go"}, filepath.Join(ext, "ext_notes.go")},
		{"git and folder", []string{"in", ext}, append(slices.Clone(inScope), extFiles...), ""},
		{"git file and folder file", []string{"in/in_untracked.go", filepath.Join(ext, "sub/ext_deep.go")}, []string{"ext_deep.go", "in_untracked.go"}, ""},
		{"symlinked folder", []string{extLink}, extFiles, extLink + "/"},
		{"symlinked file", []string{fileLink}, []string{"notelink.go"}, fileLink},
		{"folder twice", []string{ext, ext}, extFiles, ext + "/"},
		{"git subdirectory twice", []string{"in", filepath.Join(repo, "in")}, inScope, ""},
		{"folder and its symlink", []string{ext, extLink}, extFiles, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var scorers []*e2eFlagScorer
			args := append([]string{e2eFlagQuery}, test.operands...)
			output, err := e2eFlagColdWarm(t, func() (string, error) {
				scorer := e2eFlagNewScorer(t)
				scorers = append(scorers, scorer)
				return e2eFlagRunRoot(t, e2eFlagModel(scorer, nil), args...)
			})
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
			oracle, err := e2eFlagRunRoot(t, nil, append([]string{"--lexical-only", "zqopmark"}, test.operands...)...)
			if err != nil {
				t.Fatalf("text oracle %v: %v", test.operands, err)
			}
			headers := e2eFlagSorted(e2eFlagHeaders(output))
			if want := e2eFlagSorted(e2eFlagHeaders(oracle)); !slices.Equal(headers, want) {
				t.Fatalf("%v: vector results %v, want the text oracle %v:\n%s", test.operands, headers, want, output)
			}
			names := make([]string, len(headers))
			for index, header := range headers {
				names[index] = filepath.Base(header)
				if !strings.HasPrefix(header, test.prefix) {
					t.Errorf("%v: the header %s does not start with %q", test.operands, header, test.prefix)
				}
			}
			want := e2eFlagSorted(test.want)
			if slices.Sort(names); !slices.Equal(names, want) {
				t.Fatalf("%v: vector results %v, want %v:\n%s", test.operands, names, want, output)
			}
			if test.name == "symlinked file" {
				// The header is the link as typed; the node holds the
				// target.
				want = []string{"ext_notes.go"}
			}
			for _, scorer := range scorers {
				if got := scorer.scoredNames(); !slices.Equal(got, want) {
					t.Fatalf("%v: the model scored %v, want only %v", test.operands, got, want)
				}
			}
		})
	}
}
