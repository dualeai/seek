package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestSplicePassthroughSeparator checks when splicePassthroughSeparator
// reads an unknown leading `-token` as a Zoekt query (it goes to pflag as a
// positional argument after a new `--`) and when as a typo of a known flag
// (a `--token`, for which pflag gives its "unknown flag" error). Each case
// has a name, so the table also documents the rule.
func TestSplicePassthroughSeparator(t *testing.T) {
	flags := newRootCmd(newLateOnSemanticModel).Flags()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, nil},
		{"bare query", []string{"needle"}, []string{"needle"}},
		{"query with paths", []string{"needle", "./cmd"}, []string{"needle", "./cmd"}},
		{
			"single-dash zoekt query passthrough",
			[]string{"-file:test", "./cmd"},
			[]string{"--", "-file:test", "./cmd"},
		},
		{
			"double-dash unknown NOT spliced (pflag will error + suggest)",
			[]string{"--unknown", "needle"},
			[]string{"--unknown", "needle"},
		},
		{
			"negative-int value to known flag not spliced",
			[]string{"-n", "-5", "needle"},
			[]string{"-n", "-5", "needle"},
		},
		{
			"known short flag preserved",
			[]string{"-v", "needle"},
			[]string{"-v", "needle"},
		},
		{
			"known long flag preserved",
			[]string{"--verbose", "needle"},
			[]string{"--verbose", "needle"},
		},
		{
			"known flag with space-separated value skips value",
			[]string{"-n", "5", "needle"},
			[]string{"-n", "5", "needle"},
		},
		{
			"known flag with = form",
			[]string{"--limit=5", "needle"},
			[]string{"--limit=5", "needle"},
		},
		{
			"negative numeric query starting with n",
			[]string{"-n123"},
			[]string{"--", "-n123"},
		},
		{
			"negative numeric query starting with m",
			[]string{"-m123"},
			[]string{"--", "-m123"},
		},
		{
			"negative numeric query starting with A",
			[]string{"-A360"},
			[]string{"--", "-A360"},
		},
		{
			"negative numeric query starting with C",
			[]string{"-C12"},
			[]string{"--", "-C12"},
		},
		{
			"negative query starting with n",
			[]string{"-needle"},
			[]string{"--", "-needle"},
		},
		{
			"negative query starting with m",
			[]string{"-main"},
			[]string{"--", "-main"},
		},
		{
			"negative query starting with A",
			[]string{"-API"},
			[]string{"--", "-API"},
		},
		{
			"negative query starting with C",
			[]string{"-Cplusplus"},
			[]string{"--", "-Cplusplus"},
		},
		{
			"explicit -- terminator already present",
			[]string{"--", "-file:test"},
			[]string{"--", "-file:test"},
		},
		{
			"gc subcommand: never splice",
			[]string{"gc", "--force"},
			[]string{"gc", "--force"},
		},
		{
			"gc subcommand dry-run",
			[]string{"gc", "--dry-run", "--all"},
			[]string{"gc", "--dry-run", "--all"},
		},
		{
			"flags then single-dash query",
			[]string{"-v", "-file:test"},
			[]string{"-v", "--", "-file:test"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splicePassthroughSeparator(tc.in, flags)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// newQuietRootCmd builds the root command with its output and error output
// discarded, so a test does not print Cobra's usage text.
func newQuietRootCmd() *cobra.Command {
	root := newRootCmd(newLateOnSemanticModel)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root
}

func TestRootCmd_RejectsWhitespaceQuery(t *testing.T) {
	root := newQuietRootCmd()
	root.SetArgs([]string{"   "})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for whitespace-only query")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err=%v, want 'empty' substring", err)
	}
}

func TestRootCmd_RejectsNegativeLimit(t *testing.T) {
	root := newQuietRootCmd()
	root.SetArgs([]string{"-n", "-5", "needle"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for negative --limit")
	}
	if !strings.Contains(err.Error(), "must be") {
		t.Fatalf("err=%v, want 'must be' substring", err)
	}
}

// TestSearchRunConfigBindsTheModelFactory checks which model factory the run
// config binds: the given factory with the default policy, and no factory with
// the lexical-only policy, so that a --lexical-only search starts no model.
func TestSearchRunConfigBindsTheModelFactory(t *testing.T) {
	if config := newSearchRunConfig(defaultSearchPolicy(), newLateOnSemanticModel); config.newModel == nil {
		t.Fatalf("default run config has no model: %+v", config)
	}
	if lexical := newSearchRunConfig(lexicalOnlySearchPolicy(), newLateOnSemanticModel); lexical.newModel != nil {
		t.Fatalf("lexical-only run config started a model: %+v", lexical)
	}
}

// contextMarkerLine is the line of the marker in the file of
// newContextLinesFolder. The file has contextMarkerLine-1 lines on each side
// of the marker.
const contextMarkerLine = 1051

// newContextLinesFolder writes a folder with one text file of
// 2*contextMarkerLine-1 lines. The marker is on contextMarkerLine, and each
// other line holds its own word.
func newContextLinesFolder(t *testing.T) string {
	t.Helper()
	requireTools(t)
	setTestUserCache(t)
	var content strings.Builder
	for line := 1; line < 2*contextMarkerLine; line++ {
		if line == contextMarkerLine {
			content.WriteString("zqcontextmarker\n")
			continue
		}
		fmt.Fprintf(&content, "zqline%04d\n", line)
	}
	folder := t.TempDir()
	writeFileAt(t, folder, "lines.txt", content.String())
	return folder
}

// printedLineNumbers runs the root command with flags and a text search for
// the marker in folder. It returns the line numbers that the output prints.
func printedLineNumbers(t *testing.T, folder string, flags ...string) ([]int, error) {
	t.Helper()
	root := newQuietRootCmd()
	args := append([]string{"--lexical-only"}, flags...)
	root.SetArgs(append(args, "zqcontextmarker", folder))
	out, err := captureStdout(t, root.Execute)
	if err != nil {
		return nil, err
	}
	var numbers []int
	for line := range strings.Lines(out) {
		if strings.HasPrefix(line, "## ") {
			continue
		}
		field, _, _ := strings.Cut(line, " ")
		number, convErr := strconv.Atoi(field)
		if convErr != nil {
			t.Fatalf("output line %q has no line number:\n%s", line, out)
		}
		numbers = append(numbers, number)
	}
	return numbers, nil
}

// assertLineSpan fails the test unless numbers holds each line from first to
// last once, in order.
func assertLineSpan(t *testing.T, args []string, numbers []int, first, last int) {
	t.Helper()
	want := make([]int, 0, last-first+1)
	for number := first; number <= last; number++ {
		want = append(want, number)
	}
	if slices.Equal(numbers, want) {
		return
	}
	span := "no lines"
	if len(numbers) > 0 {
		span = fmt.Sprintf("%d lines from %d to %d", len(numbers), numbers[0], numbers[len(numbers)-1])
	}
	t.Fatalf("%v: printed %s, want lines %d to %d", args, span, first, last)
}

// TestContextLinesHaveNoUpperBound runs `seek -C 1000` and `seek -A 1000`:
// the output prints 1000 lines on each side that the flag selects.
func TestContextLinesHaveNoUpperBound(t *testing.T) {
	folder := newContextLinesFolder(t)
	for _, tc := range []struct {
		args        []string
		first, last int
	}{
		{args: []string{"-C", "1000"}, first: contextMarkerLine - 1000, last: contextMarkerLine + 1000},
		{args: []string{"-A", "1000"}, first: contextMarkerLine, last: contextMarkerLine + 1000},
	} {
		got, err := printedLineNumbers(t, folder, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		assertLineSpan(t, tc.args, got, tc.first, tc.last)
	}
}

// TestSelectSearchConfig runs the root command with the context flags. The
// output prints the lines that each flag selects, and a bad flag value stops
// the command before the search.
func TestSelectSearchConfig(t *testing.T) {
	folder := newContextLinesFolder(t)
	cases := []struct {
		name          string
		args          []string
		before, after int
		wantErr       string
	}{
		// The README gives up to 3 lines on each side by default.
		{name: "default", before: 3, after: 3},
		{name: "after", args: []string{"-A", "360"}, after: 360},
		{name: "context", args: []string{"--context=12"}, before: 12, after: 12},
		{name: "zero context", args: []string{"-C", "0"}},
		{name: "large context", args: []string{"-C", "513"}, before: 513, after: 513},
		{name: "negative after", args: []string{"-A=-1"}, wantErr: "must be 0 or more"},
		{name: "negative context", args: []string{"--context=-1"}, wantErr: "must be 0 or more"},
		{name: "mixed modes", args: []string{"-A", "2", "-C", "3"}, wantErr: "cannot be used together"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := printedLineNumbers(t, folder, tc.args...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			assertLineSpan(t, tc.args, got, contextMarkerLine-tc.before, contextMarkerLine+tc.after)
		})
	}
}

// TestRootCmd_VersionOutputHasNoDoublePrefix runs `seek --version` and
// reads stdout: the printed line starts with exactly one "seek " prefix. It
// checks the printed output, not the layout of the Version field, so a
// rename of the field or a change to SetVersionTemplate does not break it.
func TestRootCmd_VersionOutputHasNoDoublePrefix(t *testing.T) {
	root := newRootCmd(newLateOnSemanticModel)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "seek ") {
		t.Fatalf("output %q must start with 'seek '", got)
	}
	if strings.HasPrefix(got, "seek seek ") {
		t.Fatalf("output %q has duplicated 'seek ' prefix", got)
	}
}

func TestSuggestFlagError_AppendsDidYouMean(t *testing.T) {
	root := newQuietRootCmd()
	root.SetArgs([]string{"--verbos", "foo"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected unknown-flag error")
	}
	if !strings.Contains(err.Error(), "did you mean --verbose?") {
		t.Fatalf("err=%v, want did-you-mean for --verbose", err)
	}
	notExistErr, ok := errors.AsType[*pflag.NotExistError](err)
	if !ok {
		t.Fatalf("error=%v, want wrapped pflag.NotExistError", err)
	}
	if got := notExistErr.GetSpecifiedName(); got != "verbos" {
		t.Fatalf("specified flag=%q, want verbos", got)
	}
}

func TestSuggestFlagError_NoSuggestionWhenTooFar(t *testing.T) {
	root := newQuietRootCmd()
	root.SetArgs([]string{"--zzzzz-totally-unrelated", "foo"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected unknown-flag error")
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("err=%v, must NOT include suggestion (Levenshtein > 2)", err)
	}
}

// TestSuggestFlagError_SubcommandInheritedFlag checks that a typo of an
// inherited persistent flag on the gc subcommand still gets a suggestion. It
// covers the InheritedFlags branch of collectFlagNames (the local Flags of
// `gc` do not include --verbose).
func TestSuggestFlagError_SubcommandInheritedFlag(t *testing.T) {
	root := newQuietRootCmd()
	root.SetArgs([]string{"gc", "--verbos"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected unknown-flag error")
	}
	if !strings.Contains(err.Error(), "did you mean --verbose?") {
		t.Fatalf("err=%v, want did-you-mean for inherited --verbose", err)
	}
}

func TestHasVerboseArg(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"foo"}, false},
		{[]string{"-v"}, true},
		{[]string{"--verbose"}, true},
		{[]string{"-v=true"}, true},
		{[]string{"--verbose=true"}, true},
		{[]string{"-v=false"}, false},
		{[]string{"--verbose=false"}, false},
		{[]string{"-v", "--verbose=false"}, false},
		{[]string{"--verbose=false", "-v"}, true},
		{[]string{"-verbose"}, false},
		{[]string{"foo", "-v"}, true},
		{[]string{"--", "-v"}, false}, // -- terminates flag scope
		{[]string{"-v", "--"}, true},
	}
	for _, tc := range cases {
		if got := hasVerboseArg(tc.args); got != tc.want {
			t.Errorf("hasVerboseArg(%v)=%v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestShouldRunOpportunisticGC(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{name: "search", args: []string{"needle"}, want: true},
		{name: "root help", args: []string{"--help"}},
		{name: "short help", args: []string{"-h"}},
		{name: "numeric bool help", args: []string{"--help=1"}},
		{name: "subcommand help", args: []string{"gc", "--help"}},
		{name: "help command", args: []string{"help"}},
		{name: "verbose help command", args: []string{"-v", "help", "gc"}},
		{name: "numeric bool verbose help command", args: []string{"--verbose=0", "help"}},
		{name: "gc command", args: []string{"gc"}},
		{name: "gc dry run", args: []string{"gc", "--dry-run"}},
		{name: "verbose gc alias", args: []string{"--verbose", "garbage-collect"}},
		{name: "completion command", args: []string{"completion", "zsh"}},
		{name: "version", args: []string{"--version"}},
		{name: "short bool version", args: []string{"--version=t"}},
		{name: "help path", args: []string{"needle", "help"}, want: true},
		{name: "help query after separator", args: []string{"--", "--help"}, want: true},
		{name: "gc query after separator", args: []string{"--", "gc"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRunOpportunisticGC(tc.args); got != tc.want {
				t.Fatalf("shouldRunOpportunisticGC(%v)=%v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestBadProviderPinOnlyStopsCommandsThatUseTheModel(t *testing.T) {
	// A typo in SEEK_PROVIDER stops a search that would use the model, and
	// does not stop commands that never read the setting.
	t.Setenv(lateOnProviderEnv, "gpu")
	// Each command also has a bad argument that fails after the provider
	// check, so the error tells which check stopped it first.
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "search", args: []string{"-n", "-5", "needle"}, want: lateOnProviderEnv + ` is "gpu"`},
		{name: "lexical only", args: []string{"--lexical-only", "-n", "-5", "needle"}, want: "--limit must be"},
		{name: "subcommand", args: []string{"gc", "--sort=bogus"}, want: "--sort must be"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newQuietRootCmd()
			root.SetArgs(test.args)
			if err := root.Execute(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
