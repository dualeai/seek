package main

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestSplicePassthroughSeparator locks the contract that determines
// when an unknown leading `-token` is a Zoekt query (must pass through
// to pflag as positional via `--`) vs a typo in a known flag (must
// surface pflag's "unknown flag" error). Every case is named so the
// matrix doubles as documentation.
func TestSplicePassthroughSeparator(t *testing.T) {
	flags := newRootCmd().Flags()
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

// newQuietRootCmd builds the root command with stdout/stderr silenced
// so test runs don't leak Cobra's auto-generated usage prints. Shared
// boilerplate across the TestRootCmd_* + TestSuggestFlagError_* cases.
func newQuietRootCmd() *cobra.Command {
	root := newRootCmd()
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

// TestDefaultSearchRunConfigBindsAcceptancePolicies checks the default run
// config: the model factory with semantic search, none without, and the one
// per-file acceptance value.
func TestDefaultSearchRunConfigBindsAcceptancePolicies(t *testing.T) {
	if config := defaultSearchRunConfig(defaultSearchPolicy()); config.newModel == nil {
		t.Fatalf("default run config has no model: %+v", config)
	}
	if lexical := defaultSearchRunConfig(lexicalOnlySearchPolicy()); lexical.newModel != nil {
		t.Fatalf("lexical-only run config started a model: %+v", lexical)
	}
	if pipelineTauFile != 0.6 {
		t.Fatalf("tau_file=%g, want 0.6", pipelineTauFile)
	}
}

// TestContextLinesHaveNoUpperBound checks that -C and -A accept 1000 lines.
func TestContextLinesHaveNoUpperBound(t *testing.T) {
	for _, args := range [][]string{{"-C", "1000"}, {"-A", "1000"}} {
		flags := &cliFlags{search: defaultSearchConfig()}
		cmd := &cobra.Command{}
		cmd.Flags().IntVarP(&flags.afterContext, "after-context", "A", 0, "")
		cmd.Flags().IntVarP(&flags.context, "context", "C", 0, "")
		if err := cmd.Flags().Parse(args); err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		if err := selectSearchConfig(cmd, flags); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if flags.search.opts.NumContextLines != 1000 {
			t.Fatalf("%v: context lines=%d, want 1000", args, flags.search.opts.NumContextLines)
		}
	}
}

func TestSelectSearchConfig(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantLines int
		wantAfter bool
		wantErr   string
	}{
		{name: "default", wantLines: searchContextLines},
		{name: "after", args: []string{"-A", "360"}, wantLines: 360, wantAfter: true},
		{name: "context", args: []string{"--context=12"}, wantLines: 12},
		{name: "zero context", args: []string{"-C", "0"}, wantLines: 0},
		{name: "large context", args: []string{"-C", "513"}, wantLines: 513},
		{name: "negative after", args: []string{"-A=-1"}, wantErr: "must be 0 or more"},
		{name: "negative context", args: []string{"--context=-1"}, wantErr: "must be 0 or more"},
		{name: "mixed modes", args: []string{"-A", "2", "-C", "3"}, wantErr: "cannot be used together"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := &cliFlags{limit: 20, maxMatches: 1, search: defaultSearchConfig()}
			cmd := &cobra.Command{}
			cmd.Flags().IntVarP(&flags.afterContext, "after-context", "A", 0, "")
			cmd.Flags().IntVarP(&flags.context, "context", "C", 0, "")
			if err := cmd.Flags().Parse(tc.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}

			err := selectSearchConfig(cmd, flags)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectSearchConfig: %v", err)
			}
			if flags.search.opts.NumContextLines != tc.wantLines {
				t.Fatalf("context lines=%d, want %d", flags.search.opts.NumContextLines, tc.wantLines)
			}
			if flags.search.afterOnly != tc.wantAfter {
				t.Fatalf("afterOnly=%v, want %v", flags.search.afterOnly, tc.wantAfter)
			}
		})
	}
}

// TestRootCmd_VersionOutputHasNoDoublePrefix — drive `seek --version`
// end-to-end and capture stdout. Asserts the user-visible contract
// (the printed line starts with exactly one "seek " prefix), not the
// internal Version field layout — so renaming the field or switching
// to SetVersionTemplate would not break the test as long as the
// printed output stays sane.
func TestRootCmd_VersionOutputHasNoDoublePrefix(t *testing.T) {
	root := newRootCmd()
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

// TestSuggestFlagError_SubcommandInheritedFlag — a typo of an
// inherited PersistentFlag on the gc subcommand should still surface a
// suggestion. Covers the collectFlagNames → InheritedFlags branch
// (the local Flags of `gc` don't include --verbose).
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
	// A typo in SEEK_PROVIDER must be loud for a search that would use the
	// model, and must not stop commands that never read the setting.
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
