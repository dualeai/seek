package main

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestFlagTablesMatchCobraFlags guards a real footgun: splicePassthroughSeparator
// distinguishes a Zoekt query (`-file:test`) from a real flag using the
// knownFlagTokens / flagTakesValue tables. If a flag is added to cobra but not
// to those tables, the single-dash passthrough silently misparses (e.g. a new
// value-flag's value gets spliced as a positional). This keeps them in sync.
func TestFlagTablesMatchCobraFlags(t *testing.T) {
	cmd := newRootCmd()
	seen := map[string]bool{}
	check := func(f *pflag.Flag) {
		if seen[f.Name] {
			return
		}
		seen[f.Name] = true

		if long := "--" + f.Name; !isKnownFlagToken(long) {
			t.Errorf("flag %q is registered on cobra but missing from knownFlagTokens; "+
				"single-dash query passthrough will misparse", long)
		}
		if f.Shorthand != "" && !isKnownFlagToken("-"+f.Shorthand) {
			t.Errorf("shorthand -%s missing from knownFlagTokens", f.Shorthand)
		}

		// A value-consuming flag (non-bool) must be in flagTakesValue, or the
		// splicer treats its space-separated value as a splice candidate.
		if f.Value.Type() != "bool" {
			if long := "--" + f.Name; !flagTakesValue(long) {
				t.Errorf("value-flag %q missing from flagTakesValue", long)
			}
			if f.Shorthand != "" && !flagTakesValue("-"+f.Shorthand) {
				t.Errorf("value-flag shorthand -%s missing from flagTakesValue", f.Shorthand)
			}
		}
	}
	cmd.Flags().VisitAll(check)
	cmd.PersistentFlags().VisitAll(check)
}

// TestSplicePassthroughSeparator locks the contract that determines
// when an unknown leading `-token` is a Zoekt query (must pass through
// to pflag as positional via `--`) vs a typo in a known flag (must
// surface pflag's "unknown flag" error). Every case is named so the
// matrix doubles as documentation.
func TestSplicePassthroughSeparator(t *testing.T) {
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
			got := splicePassthroughSeparator(tc.in)
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

func TestRootCmd_RejectsEmptyArgs(t *testing.T) {
	root := newQuietRootCmd()
	root.SetArgs([]string{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing query")
	}
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

// TestSuggestFlagError_NilErrorReturnsNil — defensive guard: when
// pflag passes a nil error (no caller does today, but the contract
// allows it), suggestFlagError must pass it through unchanged.
func TestSuggestFlagError_NilErrorReturnsNil(t *testing.T) {
	root := newRootCmd()
	if got := suggestFlagError(root, nil); got != nil {
		t.Fatalf("suggestFlagError(_, nil) = %v, want nil", got)
	}
}

// TestRootCmd_NoArgsFriendlyMessage — naked `seek` must surface a
// hint to --help, not Cobra's generic "requires at least N arg(s)"
// boilerplate.
func TestRootCmd_NoArgsFriendlyMessage(t *testing.T) {
	root := newQuietRootCmd()
	root.SetArgs([]string{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing query")
	}
	if !strings.Contains(err.Error(), "missing query") {
		t.Fatalf("err=%v, want 'missing query' hint", err)
	}
	if !strings.Contains(err.Error(), "--help") {
		t.Fatalf("err=%v, want '--help' hint", err)
	}
}

// TestRootCmd_SubcommandTypoDetected — a near-miss subcommand name
// (e.g. `gcc` for `gc`, `garbage-colect` for `garbage-collect`) must
// be flagged with a did-you-mean instead of silently becoming a
// search query.
func TestRootCmd_SubcommandTypoDetected(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"gcc", `did you mean "gc"`},
		{"garbage-colect", `did you mean "garbage-collect"`},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			root := newQuietRootCmd()
			root.SetArgs([]string{tc.input})
			err := root.Execute()
			if err == nil {
				t.Fatalf("expected error for typo %q", tc.input)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q substring", err, tc.want)
			}
		})
	}
}

// TestRootArgsValidator_LegitimateQueryAccepted — call the validator
// directly so downstream errors (path lookup, query parse) cannot
// silently satisfy a negative-assertion. The validator must return nil
// for a query that's distant from every subcommand name.
func TestRootArgsValidator_LegitimateQueryAccepted(t *testing.T) {
	root := newRootCmd()
	if err := rootArgsValidator(root, []string{"a-very-different-token", "./cmd"}); err != nil {
		t.Fatalf("legitimate query rejected: %v", err)
	}
}

// TestClosestSubcommand — table-driven test of the closestSubcommand
// helper independent of the full Execute pipeline.
func TestClosestSubcommand(t *testing.T) {
	root := newRootCmd()
	cases := []struct {
		input string
		want  string
	}{
		{"gc", ""},                            // exact match → no suggestion
		{"garbage-collect", ""},               // exact alias match
		{"gcc", "gc"},                         // distance 1
		{"garbage-colect", "garbage-collect"}, // distance 1
		{"zzzzzzzz", ""},                      // distance ≥ 3 → no suggestion
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := closestSubcommand(root, tc.input)
			if got != tc.want {
				t.Errorf("closestSubcommand(%q)=%q, want %q", tc.input, got, tc.want)
			}
		})
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
		{[]string{"-verbose"}, true},
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
		{name: "version", args: []string{"--version"}},
		{name: "short bool version", args: []string{"--version=t"}},
		{name: "help path", args: []string{"needle", "help"}, want: true},
		{name: "help query after separator", args: []string{"--", "--help"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRunOpportunisticGC(tc.args); got != tc.want {
				t.Fatalf("shouldRunOpportunisticGC(%v)=%v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestLevenshtein dropped: pure algorithm detail. Threshold contract
// (≤2 → suggest, >2 → silent) is locked by TestSuggestFlagError_*
// and TestClosestSubcommand which exercise the threshold via the
// public callers. Adding a library swap (e.g. agext/levenshtein) is
// then a free change — no algorithm-specific test to update.
