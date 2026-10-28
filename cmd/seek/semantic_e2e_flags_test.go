package main

import (
	"bytes"
	"errors"
	"fmt"
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
// name holds. So every result of it comes from vector search, unless a test
// writes a text file that holds both words. The flag tests run it with the
// test model (hybridTestScorer), whose mode, not the text, accepts or rejects
// every file. Thus they prove which files and lines the flags print, not which
// files the model finds by meaning.
const e2eFlagQuery = "zqalpha zqbeta"

// e2eFlagScoredNames returns the base name of each document that scorer
// scored, over all its calls, sorted. A document that the search scores twice
// shows twice (documentPaths).
func e2eFlagScoredNames(scorer *observedModel) []string {
	paths := documentPaths(scorer.documents())
	names := make([]string, len(paths))
	for index, path := range paths {
		names[index] = filepath.Base(path)
	}
	slices.Sort(names)
	return names
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

// e2eFlagContextFile is one fixture file of the context tests: its lines and
// the first line of each of its units. A vector hit prints the first line of
// one unit as its match line.
type e2eFlagContextFile struct {
	lines []string
	units []int
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
// Each shape holds tag, so files with different tags differ in content, and
// the output prints none of them as a copy of another. The package unit starts
// on line 1, and each func starts a unit. These are line 2 of 6 in "start",
// lines 7 and 14 of 16 in "mid", and line 7 of 7 in "end".
func e2eFlagContextShapes(tag string) map[string]e2eFlagContextFile {
	return map[string]e2eFlagContextFile{
		"start": {units: []int{1, 2}, lines: []string{
			"package p // " + tag,
			"func zqStart() {",
			"\t// start " + tag,
			"\t_ = 4",
			"}",
			"// tail 6",
		}},
		"mid": {units: []int{1, 7, 14}, lines: []string{
			"package p",
			"// " + tag + " line 2",
			"// line 3",
			"// line 4",
			"// line 5",
			"// line 6",
			"func zqMid() {",
			"\t// mid " + tag,
			"\t_ = 9",
			"\t_ = 10",
			"\t_ = 11",
			"}",
			"",
			"func zqTail() {",
			"\t_ = 15",
			"}",
		}},
		"end": {units: []int{1, 7}, lines: []string{
			"package p",
			"// " + tag + " line 2",
			"// line 3",
			"// line 4",
			"// line 5",
			"// line 6",
			"func zqEnd() { _ = \"end " + tag + "\" }",
		}},
	}
}

// TestE2EFlagContextLinesOfAVectorHit runs the root command with the context
// flags on vector hits near the start, in the middle, and at the end of a
// file. Each shape is in a folder node and in a Git node's committed rows. It
// is also in the dirty overlay: an untracked file, and a modified file whose
// committed copy is one line. The test model accepts every file, and the
// vector scan, not the test, picks the unit that each file prints. A search
// with -C 0 gives that hit line: it must start a unit of the work-tree copy.
// Each flag then prints the hit line and up to N lines on each side that the
// flag selects. The start and the end of the file cut them.
func TestE2EFlagContextLinesOfAVectorHit(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	// The first commit is a fixture file, not a README, because the test
	// model prints every file.
	first := e2eFlagContextShapes("committed")["start"]
	repo := f.GitRepo("tree/repo", "committed_start.go", first.content(false))
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
			// The committed copy has one unit on line 1; the work tree
			// moves the units.
			writeFixtureFile(t, file.path, "package p // committed copy of "+base+"\n")
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
	search := func(args []string) (string, error) {
		return e2eFlagColdWarm(t, func() (string, error) {
			return seekWith(t, modelFactory(&hybridTestScorer{mode: payloadAccept}), args...)
		})
	}

	probe := []string{"-C", "0", e2eFlagQuery, f.path("tree")}
	output, err := search(probe)
	if err != nil {
		t.Fatalf("%v: %v\n%s", probe, err, output)
	}
	blocks := bodyTexts(output)
	if len(blocks) != len(files) {
		t.Fatalf("%v printed %d files, want %d:\n%s", probe, len(blocks), len(files), output)
	}
	hits := make(map[string]int, len(files))
	for _, file := range files {
		block := blocks[file.path]
		number, _, _ := strings.Cut(block, " ")
		hit, err := strconv.Atoi(number)
		if err != nil || !slices.Contains(file.shape.units, hit) || block != file.shape.span(hit, hit) {
			t.Fatalf("%v: %s prints\n%s\nwant the first line of one of its units %v", probe, filepath.Base(file.path), block, file.shape.units)
		}
		hits[file.path] = hit
	}

	for _, test := range []struct {
		name          string
		flags         []string
		before, after int
	}{
		// The README gives up to 3 lines on each side by default.
		{"default", nil, 3, 3},
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
			output, err := search(args)
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
			blocks := bodyTexts(output)
			if len(blocks) != len(files) {
				t.Fatalf("%v printed %d files, want %d:\n%s", args, len(blocks), len(files), output)
			}
			for _, file := range files {
				hit := hits[file.path]
				want := file.shape.span(hit-test.before, hit+test.after)
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
	writeFileAt(t, folder, "target.go", "package p\n// target\n")
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
		var starts atomic.Int32
		output, err := seekWith(t, failingModelFactory(&starts), args...)
		if exitCodeForError(err) != 2 || output != "" || starts.Load() != 0 {
			t.Fatalf("%v: exit status %d (err=%v), model starts %d, output:\n%s", args, exitCodeForError(err), err, starts.Load(), output)
		}
	}
}

// TestE2EFlagMaxMatchesKeepsEachVectorHit runs the root command with -m. The
// vector hits are in a folder node, and in a Git node's committed rows and
// dirty overlay. The test also needs one text hit: a file with three lines
// that hold both description words. The test model accepts every file. A
// vector hit is one match. With -C 0 it prints one line. Each -m of 1 or more
// prints its block as -m 0 does with the same context. -m cuts only the text
// file to its first matches.
func TestE2EFlagMaxMatchesKeepsEachVectorHit(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	var text strings.Builder
	text.WriteString("package p\n")
	for line := 2; line <= 30; line++ {
		if line%10 == 0 {
			fmt.Fprintf(&text, "// %s text match %d\n", e2eFlagQuery, line)
			continue
		}
		fmt.Fprintf(&text, "// filler %d\n", line)
	}
	// The first commit is the text file, not a README, because the test
	// model prints every file.
	repo := f.GitRepo("tree/repo", "text.go", text.String())
	textPath := filepath.Join(repo, "text.go")
	vectorPaths := []string{f.path("tree/loose/folder.go"), filepath.Join(repo, "committed.go"), filepath.Join(repo, "untracked.go")}
	for _, path := range vectorPaths[:2] {
		writeFixtureFile(t, path, e2eFlagContextShapes(filepath.Base(path))["mid"].content(false))
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "max matches fixture")
	writeFixtureFile(t, vectorPaths[2], e2eFlagContextShapes("untracked.go")["mid"].content(false))
	t.Chdir(t.TempDir())

	// unlimited maps each -C value ("" for the default) to the vector blocks
	// of the first row with that value, which has -m 0.
	unlimited := make(map[string]map[string]string)
	for _, test := range []struct {
		flags []string
		// context is the -C value of flags, or "" for the default.
		context     string
		textMatches int
		// footer is the last line of the text file when matches are hidden.
		footer string
	}{
		{[]string{"-m", "0"}, "", 3, ""},
		{[]string{"-m", "0", "-C", "1"}, "1", 3, ""},
		{[]string{"-m", "0", "-C", "0"}, "0", 3, ""},
		{nil, "", 3, ""},
		{[]string{"-m", "1"}, "", 1, "… 2 more matches in this file\n"},
		{[]string{"--max-matches=2"}, "", 2, "… 1 more match in this file\n"},
		{[]string{"-m", "1", "-C", "1"}, "1", 1, "… 2 more matches in this file\n"},
		{[]string{"-m", "1", "-C", "0"}, "0", 1, "… 2 more matches in this file\n"},
	} {
		t.Run(strings.Join(append([]string{"flags"}, test.flags...), " "), func(t *testing.T) {
			args := append(slices.Clone(test.flags), e2eFlagQuery, f.path("tree"))
			output, err := e2eFlagColdWarm(t, func() (string, error) {
				return seekWith(t, modelFactory(&hybridTestScorer{mode: payloadAccept}), args...)
			})
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
			blocks := bodyTexts(output)
			if len(blocks) != len(vectorPaths)+1 {
				t.Fatalf("%v printed %d files, want %d:\n%s", args, len(blocks), len(vectorPaths)+1, output)
			}
			want, ok := unlimited[test.context]
			if !ok {
				want = blocks
				unlimited[test.context] = blocks
			}
			for _, path := range vectorPaths {
				got := blocks[path]
				if got != want[path] {
					t.Errorf("%v: the vector hit %s prints\n%s\nwant its block under -m 0\n%s", args, filepath.Base(path), got, want[path])
				}
				if test.context == "0" && strings.Count(got, "\n") != 1 {
					t.Errorf("%v: the vector hit %s prints more than one line:\n%s", args, filepath.Base(path), got)
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
// search. Each of three files holds the word three times, ten lines apart. So
// each file is a strict hit, and no context line holds a match. The test model
// in its far mode adds no file by meaning, so only the strict hits print. The
// expected counts and footers come from the fixture by hand.
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
		output, _, err := stubSearch(t, payloadFar, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output)
		}
		headers := len(headerPaths(output))
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

// e2eFlagTargetTree writes a tree with one file in a folder node
// (tree/loose/folder.go) and one in a Git node's committed rows
// (tree/repo/committed.go). Two are in its dirty overlay: an untracked file
// and a modified file. These files hold no description word. text holds the
// content of tree/repo/text.go, or no file when it is empty. The tree has no
// README, because the test model prints every file. It makes the tree the
// working directory.
func e2eFlagTargetTree(t *testing.T, text string) {
	t.Helper()
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "committed.go", "package p\n// committed\n")
	writeFixtureFile(t, f.path("tree/loose/folder.go"), "package p\n// folder\n")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package p\n// committed copy\n")
	if text != "" {
		writeFixtureFile(t, filepath.Join(repo, "text.go"), text)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "target tree")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package p\n// work tree copy\n")
	writeFixtureFile(t, filepath.Join(repo, "untracked.go"), "package p\n// untracked\n")
	t.Chdir(f.path("tree"))
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
// --lexical-only, which gives RunE the lexical-only search policy. The files
// are in a folder node, and in a Git node's committed rows and dirty overlay.
// The test also needs one text hit: a file with both description words. The
// default search, with the test model that accepts every file, prints every
// file. --lexical-only prints only the text file, the one file that holds
// every word. On a cold cache, it starts no model, writes no meaning-based
// data, and does not install the bundled model. With vectors on disk, it adds
// no such cache path. When only vectors match, --lexical-only finds nothing.
func TestE2EFlagLexicalOnlyPrintsNoVectorResult(t *testing.T) {
	requireTools(t)
	e2eFlagTargetTree(t, "package p\n// "+e2eFlagQuery+" text\n")
	files := []string{"loose/folder.go", "repo/committed.go", "repo/modified.go", "repo/text.go", "repo/untracked.go"}

	t.Run("cold", func(t *testing.T) {
		var starts atomic.Int32
		output, err := e2eFlagColdWarm(t, func() (string, error) {
			return seekWith(t, failingModelFactory(&starts), "--lexical-only", e2eFlagQuery, ".")
		})
		if err != nil || !slices.Equal(headerPaths(output), []string{"repo/text.go"}) || starts.Load() != 0 {
			t.Fatalf("err=%v, model starts %d, want only the text file and no model start:\n%s", err, starts.Load(), output)
		}
		if paths := e2eFlagSemanticCachePaths(t); len(paths) != 0 {
			t.Fatalf("--lexical-only wrote meaning-based data or installed the model: %v", paths)
		}
	})

	t.Run("vectors on disk", func(t *testing.T) {
		setTestUserCache(t)
		accept := modelFactory(&hybridTestScorer{mode: payloadAccept})
		described, err := seekWith(t, accept, e2eFlagQuery, ".")
		if got := headerPaths(described); err != nil || !slices.Equal(got, files) {
			t.Fatalf("the default search printed %v (err=%v), want %v", got, err, files)
		}
		built := e2eFlagSemanticCachePaths(t)
		if len(built) == 0 {
			t.Fatal("the default search wrote no meaning-based data")
		}
		output, err := seek(t, "--lexical-only", e2eFlagQuery, ".")
		if err != nil || !slices.Equal(headerPaths(output), []string{"repo/text.go"}) {
			t.Fatalf("err=%v, want only the text file:\n%s", err, output)
		}
		if paths := e2eFlagSemanticCachePaths(t); !slices.Equal(paths, built) {
			t.Fatalf("--lexical-only changed the meaning-based data or installed the model: %v, want %v", paths, built)
		}
		// Only vectors match a description that no file holds whole.
		const vectorOnly = "zqalpha zqgamma"
		described, err = seekWith(t, accept, vectorOnly, ".")
		if got := headerPaths(described); err != nil || !slices.Equal(got, files) {
			t.Fatalf("the default search of %q printed %v (err=%v), want %v", vectorOnly, got, err, files)
		}
		output, err = seek(t, "--lexical-only", "-n", "1", "-m", "1", "-C", "0", vectorOnly, ".")
		if !errors.Is(err, errNoMatch) || output != "" {
			t.Fatalf("--lexical-only %q: err=%v output:\n%s", vectorOnly, err, output)
		}
	})
}

// e2eFlagANSI matches one ANSI color code.
var e2eFlagANSI = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestE2EFlagColorOfAVectorHit prints vector hits of a folder node and of a
// Git node's committed rows and dirty overlay in color and as plain text. The
// test model accepts every file. Color is presentation only: with the codes
// removed, the color output is the plain output. The header path is cyan and
// each line number is dim. A vector hit holds no query word, so no span has
// the match color. NO_COLOR wins over CLICOLOR_FORCE, and output through a
// pipe is plain.
func TestE2EFlagColorOfAVectorHit(t *testing.T) {
	requireTools(t)
	e2eFlagTargetTree(t, "")
	search := func() (string, error) {
		return seekWith(t, modelFactory(&hybridTestScorer{mode: payloadAccept}), e2eFlagQuery, ".")
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
	if got := headerPaths(plain); !slices.Equal(got, want) {
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
// in a Git node's committed rows and dirty overlay. The test model makes
// every unit a candidate, scores every live file, and rejects each one. So
// the model's verdict, not an empty candidate set, empties the output.
// The search prints nothing, ends with exit status 1, and reports no error.
// This is also true with -n, -m and -C, on a cold and on a warm cache. The
// test model cannot put a unit just outside the retrieval radius, so this
// test does not cover that case.
func TestE2EFlagNoVectorResultExitsOneWithNoOutput(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "committed.go", "package p\n// committed\n")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package p\n// committed copy\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "no result")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package p\n// work tree copy\n")
	writeFixtureFile(t, filepath.Join(repo, "untracked.go"), "package p\n// untracked\n")
	writeFixtureFile(t, f.path("tree/loose/folder.go"), "package p\n// folder\n")
	t.Chdir(f.path("tree"))
	live := []string{"committed.go", "folder.go", "modified.go", "untracked.go"}
	for _, flags := range [][]string{nil, {"-n", "1", "-m", "1", "-C", "5"}} {
		var scorers []*observedModel
		args := append(slices.Clone(flags), e2eFlagQuery, ".")
		output, err := e2eFlagColdWarm(t, func() (string, error) {
			scorer := observeModel(&hybridTestScorer{mode: payloadReject})
			scorers = append(scorers, scorer)
			return seekWith(t, scorer.factory(), args...)
		})
		var stderr bytes.Buffer
		reportCLIError(&stderr, err, false)
		if output != "" || exitCodeForError(err) != 1 || stderr.Len() != 0 {
			t.Fatalf("%v: exit status %d (err=%v) stderr=%q output:\n%s", args, exitCodeForError(err), err, stderr.String(), output)
		}
		for _, scorer := range scorers {
			if got := e2eFlagScoredNames(scorer); !slices.Equal(got, live) {
				t.Fatalf("%v: the model scored %v, want every live file %v", args, got, live)
			}
		}
	}
}

// TestE2EFlagPathOperandsScopeVectorHits runs a description from inside a Git
// repository with path operands. The operands are a subdirectory of the
// repository (the querySupport row "path-operand scope"), exact files, and an
// external folder outside the Git worktree. They are also an exact file there,
// Git and folder operands together, symlinked operands, and an operand given
// twice. Every file holds the word zqopmark, and the test model accepts every
// file. A one-word text search with the same operands is the oracle. The
// vector results must have its header lines, with their [uncommitted] tags,
// and the hand-made set of files. The model scores only those files, so it
// never scores a file that the scope drops. The Git node has committed,
// modified, untracked, deleted, and changed-away files. The warm search embeds
// no unit.
//
// An exact file outside Git is a folder node of one file. The README says
// that every searched folder adds files by meaning. An exact file in Git
// gets its vector hit. Thus the cases "external exact file", "git file and
// folder file", and "symlinked file" expect the vector hit too.
func TestE2EFlagPathOperandsScopeVectorHits(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	target := func(name string) string {
		return "package p\n// zqopmark " + name + "\n"
	}
	// The first commit is a fixture file, not a README, because the test
	// model prints every file.
	repo := f.GitRepo("work/repo", "top.go", target("top"))
	committed := map[string]string{
		"in/in_committed.go":    target("in committed"),
		"in/sub/in_deep.go":     target("in deep"),
		"in/in_modified.go":     "package p\n// plain committed text\n",
		"in/in_changed_away.go": target("in changed away, committed copy"),
		"in/in_deleted.go":      target("in deleted"),
		"in2/in2_sibling.go":    target("in2 sibling"),
		"out/out_committed.go":  target("out committed"),
	}
	for rel, content := range committed {
		writeFixtureFile(t, filepath.Join(repo, rel), content)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "operand fixture")
	writeFixtureFile(t, filepath.Join(repo, "in/in_modified.go"), target("in modified"))
	writeFixtureFile(t, filepath.Join(repo, "in/in_changed_away.go"), target("in changed away, work tree copy"))
	if err := os.Remove(filepath.Join(repo, "in/in_deleted.go")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(repo, "in/in_untracked.go"), target("in untracked"))
	writeFixtureFile(t, filepath.Join(repo, "out/out_untracked.go"), target("out untracked"))
	ext := f.path("ext")
	writeFixtureFile(t, filepath.Join(ext, "ext_notes.go"), target("ext notes"))
	writeFixtureFile(t, filepath.Join(ext, "sub/ext_deep.go"), target("ext deep"))
	writeFixtureFile(t, filepath.Join(ext, "ext_plain.go"), target("ext plain"))
	links := f.path("links")
	if err := os.MkdirAll(links, 0o755); err != nil {
		t.Fatal(err)
	}
	extLink := filepath.Join(links, "extlink")
	fileLink := filepath.Join(links, "notelink.go")
	if err := os.Symlink(ext, extLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ext, "ext_notes.go"), fileLink); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	inScope := []string{"in_changed_away.go", "in_committed.go", "in_deep.go", "in_modified.go", "in_untracked.go"}
	extFiles := []string{"ext_deep.go", "ext_notes.go", "ext_plain.go"}
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
			var scorers []*observedModel
			args := append([]string{e2eFlagQuery}, test.operands...)
			output, err := e2eFlagColdWarm(t, func() (string, error) {
				scorer := observeModel(&hybridTestScorer{mode: payloadAccept})
				scorers = append(scorers, scorer)
				return seekWith(t, scorer.factory(), args...)
			})
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
			oracle, err := seek(t, append([]string{"--lexical-only", "zqopmark"}, test.operands...)...)
			if err != nil {
				t.Fatalf("text oracle %v: %v", test.operands, err)
			}
			if got, want := headerTexts(output), headerTexts(oracle); !slices.Equal(got, want) {
				t.Fatalf("%v: vector results %q, want the text oracle %q:\n%s", test.operands, got, want, output)
			}
			headers := headerPaths(output)
			names := make([]string, len(headers))
			for index, header := range headers {
				names[index] = filepath.Base(header)
				if !strings.HasPrefix(header, test.prefix) {
					t.Errorf("%v: the header %s does not start with %q", test.operands, header, test.prefix)
				}
			}
			want := slices.Sorted(slices.Values(test.want))
			if slices.Sort(names); !slices.Equal(names, want) {
				t.Fatalf("%v: vector results %v, want %v:\n%s", test.operands, names, want, output)
			}
			if test.name == "symlinked file" {
				// The header is the link as typed; the node holds the
				// target.
				want = []string{"ext_notes.go"}
			}
			for _, scorer := range scorers {
				if got := e2eFlagScoredNames(scorer); !slices.Equal(got, want) {
					t.Fatalf("%v: the model scored %v, want only %v", test.operands, got, want)
				}
			}
			if embeds := scorers[1].embeds.Load(); embeds != 0 {
				t.Fatalf("%v: the warm search embedded units in %d calls, want 0", test.operands, embeds)
			}
		})
	}
}
