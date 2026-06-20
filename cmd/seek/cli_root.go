package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cliFlags holds the parsed values for the root command. Bound to
// rootCmd flags via pflag's Var/BoolVarP/IntVarP helpers.
type cliFlags struct {
	verbose    bool
	limit      int
	maxMatches int
}

// knownFlagTokens enumerates the root command's flag names so
// splicePassthroughSeparator can distinguish "actual flag" from
// "unknown single-dash token that's really a Zoekt query". Drift
// against the cobra flag definitions below breaks the single-dash
// query passthrough contract.
//
// gc subcommand flags are NOT listed: the splicer early-returns on
// args[0]=="gc" so it never inspects subsequent tokens.
var knownFlagTokens = []string{
	"-h", "--help",
	"-v", "--verbose",
	"--version",
	"-n", "--limit",
	"-m", "--max-matches",
}

// splicePassthroughSeparator preserves Zoekt-style single-dash queries
// (`-file:test`, `-lang:go`) under pflag's POSIX-strict parser. Scan
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
func splicePassthroughSeparator(args []string) []string {
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
		if isKnownFlagToken(name) {
			// Skip the value arg when the flag takes one and used
			// the space-separated form (`-n 5` not `-n=5`),
			// otherwise the next iteration would see `5` (or
			// `-5`) as a candidate splice point.
			if flagTakesValue(name) && !hasEq && i+1 < len(args) {
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

func isKnownFlagToken(name string) bool {
	for _, k := range knownFlagTokens {
		if k == name {
			return true
		}
	}
	return false
}

func flagTakesValue(name string) bool {
	switch name {
	case "-n", "--limit", "-m", "--max-matches":
		return true
	}
	return false
}

// newRootCmd builds the seek root command with shared flags, examples,
// and the search RunE callback. Subcommands attach via AddCommand.
func newRootCmd() *cobra.Command {
	flags := &cliFlags{}
	cmd := &cobra.Command{
		Use:   "seek [flags] <query> [path...]",
		Short: "BM25-ranked code search with persistent caching",
		Long: `seek searches the current Git worktree by default, or the files and
folders you pass. Git worktree directories use Git rules; exact files search
only that file; normal folders use filesystem rules. Nested Git worktrees are
searched once with Git behavior.`,
		Example: `  seek 'sym:Foo'              find definitions named Foo (ctags)
  seek 'lang:go func main'    Go files containing both tokens
  seek 'file:cmd -file:test'  paths matching cmd, excluding tests
  seek 'TODO' ./src           search a specific subtree`,
		Args: rootArgsValidator,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			logLevel := slog.LevelWarn
			if flags.verbose {
				logLevel = slog.LevelDebug
			}
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
			slog.SetDefault(logger)
			if flags.verbose {
				log.SetOutput(newSlogWriter(logger))
				log.SetFlags(0)
			} else {
				log.SetOutput(io.Discard)
			}
			return nil
		},
		PreRunE: func(_ *cobra.Command, args []string) error {
			if flags.limit < 0 {
				return fmt.Errorf("--limit must be ≥ 0, got %d", flags.limit)
			}
			if flags.maxMatches < 0 {
				return fmt.Errorf("--max-matches must be ≥ 0, got %d", flags.maxMatches)
			}
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("query is empty")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), args[0], args[1:], flags.limit, flags.maxMatches)
		},
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	cmd.PersistentFlags().BoolVarP(&flags.verbose, "verbose", "v", false, "enable debug logging")
	cmd.Flags().IntVarP(&flags.limit, "limit", "n", 0, "maximum number of files to display (≥ 0, 0 = unlimited)")
	cmd.Flags().IntVarP(&flags.maxMatches, "max-matches", "m", 0, "maximum matches per file (≥ 0, 0 = unlimited)")

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

// rootArgsValidator replaces cobra.MinimumNArgs(1) so the no-args path
// emits a friendly hint AND so subcommand typos (e.g. `seek gcc` for
// `seek gc`, `seek garbage-colect` for `seek garbage-collect`) are
// caught instead of silently being treated as a search query.
func rootArgsValidator(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing query (try 'seek --help' for usage)")
	}
	if suggestion := closestSubcommand(cmd, args[0]); suggestion != "" {
		return fmt.Errorf("unknown subcommand %q (did you mean %q?)", args[0], suggestion)
	}
	return nil
}

// closestSubcommand returns the subcommand name (or alias) within
// Levenshtein distance 2 of `want`, or "" when no candidate is close
// enough. Only consults registered child commands, so it cannot flag
// a legitimate query as a typo unless the user actually mistyped a
// subcommand-looking token.
func closestSubcommand(cmd *cobra.Command, want string) string {
	best := ""
	bestDist := 3
	for _, c := range cmd.Commands() {
		if c.Hidden {
			continue
		}
		for _, name := range append([]string{c.Name()}, c.Aliases...) {
			if name == want {
				return ""
			}
			d := levenshtein(want, name)
			if d > 0 && d < bestDist {
				best = name
				bestDist = d
			}
		}
	}
	return best
}

// suggestFlagError appends a Levenshtein-based did-you-mean hint when a
// user mistypes a flag. SilenceErrors=true is in effect on the parent
// command, so Cobra's built-in suggestion line is suppressed; this hook
// folds the suggestion into the error message that main()'s plain
// `seek: <err>` printer surfaces.
func suggestFlagError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	bad := extractUnknownFlagName(err.Error())
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

// extractUnknownFlagName parses pflag's "unknown flag: --foo" error
// text. pflag does not export a sentinel error or a typed accessor for
// the offending flag, so string extraction is the only option.
func extractUnknownFlagName(msg string) string {
	const prefix = "unknown flag: --"
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		// Short-flag form: "unknown shorthand flag: 'x' in -x"
		return ""
	}
	return strings.TrimSpace(msg[idx+len(prefix):])
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

// executeCLI is the entry point main() calls. Returns the error from
// Cobra so main() can map it to an exit code via exitCodeForError.
func executeCLI(ctx context.Context) error {
	root := newRootCmd()
	root.SetArgs(splicePassthroughSeparator(os.Args[1:]))
	return root.ExecuteContext(ctx)
}
