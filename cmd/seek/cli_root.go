package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cliFlags holds the parsed values for the root command. newRootCmd binds
// them to the command's flags with pflag's BoolVar/BoolVarP/IntVarP helpers.
type cliFlags struct {
	verbose      bool
	lexicalOnly  bool
	limit        int
	maxMatches   int
	afterContext int
	context      int
	search       searchConfig
}

// splicePassthroughSeparator keeps Zoekt-style single-dash queries
// (`-file:test`, `-lang:go`) positional under pflag parsing. It scans args in
// order and stops at `--`, at `-`, or at the first argument that does not
// start with a dash. The first single-dash token that is not a known flag
// gets `--` before it, so pflag reads it and every later argument as
// positional.
//
// An unknown `--name` token gets no `--`: pflag reports "unknown flag", and
// suggestFlagError adds the did-you-mean hint (`--verbos` gives "did you mean
// --verbose?").
//
// `seek gc` takes only flags, so when args[0] is "gc" nothing is spliced.
func splicePassthroughSeparator(args []string, flags *pflag.FlagSet) []string {
	if len(args) == 0 || args[0] == "gc" {
		return args
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return args
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return args
		}
		// Strip optional `=value` to match the registered name.
		name, _, hasEq := strings.Cut(arg, "=")
		if flag := lookupExactFlag(flags, name); flag != nil {
			// Skip the value arg when the flag takes one and used
			// the space-separated form (`-n 5` not `-n=5`),
			// otherwise the next iteration would see `5` (or
			// `-5`) as a candidate splice point.
			if flag.NoOptDefVal == "" && !hasEq && i+1 < len(args) {
				i++
			}
			continue
		}
		// An unknown long form (`--foo`) goes to pflag, which reports
		// "unknown flag". An unknown single-dash token (`-file:test`)
		// gets `--` before it, so pflag reads the rest as positional.
		if strings.HasPrefix(arg, "--") {
			return args
		}
		out := make([]string, 0, len(args)+1)
		out = append(out, args[:i]...)
		out = append(out, "--")
		out = append(out, args[i:]...)
		return out
	}
	return args
}

func lookupExactFlag(flags *pflag.FlagSet, token string) *pflag.Flag {
	if name, ok := strings.CutPrefix(token, "--"); ok && name != "" {
		return flags.Lookup(name)
	}
	if len(token) == 2 && token[0] == '-' {
		return flags.ShorthandLookup(token[1:])
	}
	return nil
}

// newRootCmd builds the seek root command with shared flags, examples,
// and the search RunE callback. A search that gives the model text loads its
// model with newModel. Subcommands attach via AddCommand.
func newRootCmd(newModel semanticModelFactory) *cobra.Command {
	flags := &cliFlags{search: defaultSearchConfig()}
	cmd := &cobra.Command{
		Use:   "seek [flags] <query> [path...]",
		Short: "Ranked local code search with exact and descriptive queries",
		Long: `Seek searches the current folder, the same as ".", or the selected files
and folders, with each Git repository below them in Git's own view.
A query also uses the bundled local model to find and rank code by meaning.

Results are ranked, not exhaustive. With -n 0, Seek displays every returned
file. Use an exhaustive search for counts, renames, and absence checks.

A result that the model adds must pass its score threshold; files that contain
every query word stay. With no result, Seek prints nothing and exits with code
1. This does not prove absence.

The first search of a large folder can take tens of seconds and use several GiB
while Seek builds text and meaning-based indexes. Use --lexical-only to skip
the meaning-based index and all model work.`,
		Example: `  seek 'sym:Foo'              find definitions whose name contains Foo
  seek 'lang:go func main'    rank Go files for these words
  seek 'file:cmd -file:test'  paths matching cmd, excluding tests
  seek 'TODO' ./src           search a specific subtree
  seek 'request auth flow'    search a description with the local model`,
		Args: rootArgsValidator,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			slog.SetDefault(newCLILogger(os.Stderr, flags.verbose))
			// Report a bad provider pin here, at the default log level. Left to
			// the encoder, it would surface as one more reason to keep lexical
			// search, which the fallback path hides behind --verbose.
			//
			// Only for commands that can use the model: `gc`, `completion` and
			// a --lexical-only search do not read the setting, so a typo in it
			// must not stop them.
			if flags.lexicalOnly || !commandUsesSemanticModel(cmd) {
				return nil
			}
			_, err := lateOnForcedProvider()
			return err
		},
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if flags.limit < 0 {
				return fmt.Errorf("--limit must be ≥ 0, got %d", flags.limit)
			}
			if flags.maxMatches < 0 {
				return fmt.Errorf("--max-matches must be ≥ 0, got %d", flags.maxMatches)
			}
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("query is empty")
			}
			return selectSearchConfig(cmd, flags)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			policy := defaultSearchPolicy()
			if flags.lexicalOnly {
				policy = lexicalOnlySearchPolicy()
			}
			return runSearchCommand(
				cmd.Context(),
				args[0],
				args[1:],
				flags.limit,
				flags.maxMatches,
				flags.search,
				newSearchRunConfig(policy, newModel),
			)
		},
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	cmd.PersistentFlags().BoolVarP(&flags.verbose, "verbose", "v", false, "show debug logs and detailed errors")
	cmd.Flags().BoolVar(&flags.lexicalOnly, "lexical-only", false, "text search only; skip the meaning-based index and model")
	cmd.Flags().IntVarP(&flags.limit, "limit", "n", 0, "maximum files to display (0 = all returned files)")
	cmd.Flags().IntVarP(&flags.maxMatches, "max-matches", "m", 0, "maximum matches per file (0 = all returned matches)")
	cmd.Flags().IntVarP(&flags.afterContext, "after-context", "A", 0, "lines to display after each match")
	cmd.Flags().IntVarP(&flags.context, "context", "C", 0, "lines to display before and after each match")

	// The template adds the "seek " prefix, so the version drops it: the
	// output is `seek <ver>`, not `seek seek <ver>`.
	cmd.Version = strings.TrimPrefix(versionString(), "seek ")
	cmd.SetVersionTemplate("seek {{.Version}}\n")

	// Make Cobra add the --help and --version flags now, so the Usage
	// changes below apply. Cobra otherwise adds them during Execute, after
	// newRootCmd has returned.
	cmd.InitDefaultHelpFlag()
	cmd.InitDefaultVersionFlag()
	if f := cmd.Flags().Lookup("version"); f != nil {
		f.Usage = "print version and exit"
	}
	if f := cmd.Flags().Lookup("help"); f != nil {
		f.Usage = "print this help and exit"
	}

	cmd.SetFlagErrorFunc(suggestFlagError)
	gc := newGCCmd()
	gc.SetFlagErrorFunc(suggestFlagError)
	cmd.AddCommand(gc)
	return cmd
}

// newSearchRunConfig returns the run config of policy. Only a policy with
// semantic search gets the model factory newModel.
func newSearchRunConfig(policy searchPolicy, newModel semanticModelFactory) searchRunConfig {
	config := searchRunConfig{policy: policy}
	if policy.semanticEnabled() {
		config.newModel = newModel
	}
	return config
}

// selectSearchConfig sets flags.search from --after-context or --context, or
// to the default. It rejects both flags together and a negative line count;
// the count has no upper bound.
func selectSearchConfig(cmd *cobra.Command, flags *cliFlags) error {
	afterChanged := cmd.Flags().Changed("after-context")
	contextChanged := cmd.Flags().Changed("context")
	if afterChanged && contextChanged {
		return fmt.Errorf("--after-context and --context cannot be used together")
	}
	if afterChanged {
		if flags.afterContext < 0 {
			return fmt.Errorf("--after-context must be 0 or more, got %d", flags.afterContext)
		}
		flags.search = explicitSearchConfig(flags.afterContext, true)
		return nil
	}
	if contextChanged {
		if flags.context < 0 {
			return fmt.Errorf("--context must be 0 or more, got %d", flags.context)
		}
		flags.search = explicitSearchConfig(flags.context, false)
		return nil
	}
	flags.search = defaultSearchConfig()
	return nil
}

// rootArgsValidator replaces cobra.MinimumNArgs(1) so the no-args path emits a
// useful hint. A non-empty first positional argument remains a possible search
// query. Cobra dispatches exact subcommand names before this validator runs.
func rootArgsValidator(_ *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing query (try 'seek --help' for usage)")
	}
	return nil
}

// suggestFlagError adds a did-you-mean hint, by Levenshtein distance, to the
// error of an unknown flag. Cobra suggests only command names and pflag
// suggests nothing, so this hook puts the hint in the error message that the
// CLI presenter shows.
func suggestFlagError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	notExistErr, ok := errors.AsType[*pflag.NotExistError](err)
	if !ok {
		return err
	}
	bad := notExistErr.GetSpecifiedName()
	if bad == "" {
		return err
	}
	candidates := collectFlagNames(cmd)
	suggestion := closestFlag(bad, candidates)
	if suggestion == "" {
		return err
	}
	return fmt.Errorf("%w (did you mean --%s?)", err, suggestion)
}

// collectFlagNames returns every long-form flag name registered on the
// command and its ancestors. Includes inherited persistent flags so
// suggestions like "did you mean --verbose?" work on subcommands.
func collectFlagNames(cmd *cobra.Command) []string {
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		names = append(names, f.Name)
	})
	cmd.InheritedFlags().VisitAll(func(f *pflag.Flag) {
		names = append(names, f.Name)
	})
	return names
}

// closestFlag returns the candidate whose Levenshtein distance to `want`
// is minimal and at most 2 (Cobra's default SuggestionsMinimumDistance).
// Returns "" when no candidate is close enough.
func closestFlag(want string, candidates []string) string {
	best := ""
	bestDist := 3
	for _, c := range candidates {
		d := levenshtein(want, c)
		if d < bestDist {
			best = c
			bestDist = d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	curr := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// commandUsesSemanticModel reports whether this command can load the model.
// Only the root search command does; every subcommand is management or output.
func commandUsesSemanticModel(cmd *cobra.Command) bool {
	return cmd != nil && !cmd.HasParent()
}
