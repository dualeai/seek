package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cliFlags holds the parsed values for the root command. Bound to
// rootCmd flags via pflag's Var/BoolVarP/IntVarP helpers.
type cliFlags struct {
	verbose      bool
	limit        int
	maxMatches   int
	afterContext int
	context      int
	search       searchConfig
}

// splicePassthroughSeparator preserves Zoekt-style single-dash queries
// (`-file:test`, `-lang:go`) under pflag parsing. Scan
// args left-to-right; the first arg starting with a SINGLE dash that
// isn't a known flag (or `-` / `--`) triggers an injection of `--`
// before it so pflag treats it plus everything after as positional.
//
// `--xxxx` unknown tokens are NOT spliced — pflag will surface the
// "unknown flag" error and Cobra's did-you-mean suggestion. That keeps
// typo detection working for the long-form (`--verbos` → "did you
// mean --verbose?").
//
// `seek gc` is special: arguments after the subcommand are all flags
// or none. If args[0] == "gc", splice nothing.
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
		// Long-form unknown (`--foo`) → let pflag surface the
		// "unknown flag" error + did-you-mean suggestion. Single-dash
		// unknown (`-file:test`) → splice `--` so pflag treats the
		// rest as positional.
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
// and the search RunE callback. Subcommands attach via AddCommand.
func newRootCmd() *cobra.Command {
	flags := &cliFlags{search: defaultSearchConfig()}
	cmd := &cobra.Command{
		Use:   "seek [flags] <query> [path...]",
		Short: "BM25-ranked code search with persistent caching",
		Long: `seek searches the current Git worktree by default, or the files and
folders you pass. Files and directories inside a Git worktree are searched
through the Git index, scoped to your selection; paths excluded by .gitignore
are searched as plain files or folders instead, as is anything outside a Git
worktree. Visible nested Git worktrees under selected directories are searched
once.`,
		Example: `  seek 'sym:Foo'              find definitions named Foo (ctags)
  seek 'lang:go func main'    Go files containing both tokens
  seek 'file:cmd -file:test'  paths matching cmd, excluding tests
  seek 'TODO' ./src           search a specific subtree`,
		Args: rootArgsValidator,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			configureCLILogging(os.Stderr, flags.verbose)
			return nil
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
			if err := selectSearchConfig(cmd, flags); err != nil {
				return err
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithSearchConfig(
				cmd.Context(),
				args[0],
				args[1:],
				flags.limit,
				flags.maxMatches,
				flags.search,
			)
		},
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	cmd.PersistentFlags().BoolVarP(&flags.verbose, "verbose", "v", false, "show debug logs and detailed errors")
	cmd.Flags().IntVarP(&flags.limit, "limit", "n", 0, "maximum number of files to display (≥ 0, 0 = unlimited)")
	cmd.Flags().IntVarP(&flags.maxMatches, "max-matches", "m", 0, "maximum matches per file (≥ 0, 0 = unlimited)")
	cmd.Flags().IntVarP(&flags.afterContext, "after-context", "A", 0, "lines to display after each match (0–512)")
	cmd.Flags().IntVarP(&flags.context, "context", "C", 0, "lines to display before and after each match (0–512)")

	// Version output keeps current format: `seek <ver>` with no doubling.
	cmd.Version = strings.TrimPrefix(versionString(), "seek ")
	cmd.SetVersionTemplate("seek {{.Version}}\n")

	// Force Cobra to materialize the --help / --version flags now so
	// the description overrides below stick. Cobra otherwise adds them
	// lazily during Execute, by which point newRootCmd has returned.
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

func configureCLILogging(w io.Writer, verbose bool) {
	slog.SetDefault(newCLILogger(w, verbose))
}

func selectSearchConfig(cmd *cobra.Command, flags *cliFlags) error {
	afterChanged := cmd.Flags().Changed("after-context")
	contextChanged := cmd.Flags().Changed("context")
	if afterChanged && contextChanged {
		return fmt.Errorf("--after-context and --context cannot be used together")
	}
	if afterChanged {
		if flags.afterContext < 0 || flags.afterContext > maxSearchContextLines {
			return fmt.Errorf("--after-context must be between 0 and %d, got %d", maxSearchContextLines, flags.afterContext)
		}
		flags.search = explicitSearchConfig(flags.afterContext, true)
		return nil
	}
	if contextChanged {
		if flags.context < 0 || flags.context > maxSearchContextLines {
			return fmt.Errorf("--context must be between 0 and %d, got %d", maxSearchContextLines, flags.context)
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

// suggestFlagError appends a Levenshtein-based did-you-mean hint when a
// user mistypes a flag. SilenceErrors=true is in effect on the parent
// command, so Cobra's built-in suggestion line is suppressed; this hook
// folds the suggestion into the error message that the CLI presenter surfaces.
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
// is minimal AND ≤ 2 (matches Cobra's default SuggestionsMinimumDistance).
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
