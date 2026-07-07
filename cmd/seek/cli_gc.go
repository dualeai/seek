package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newGCCmd() *cobra.Command {
	opts := &gcCmdOptions{}
	cmd := &cobra.Command{
		Use:     "gc",
		Aliases: []string{"garbage-collect"},
		Short:   "Garbage-collect the seek cache",
		Long: fmt.Sprintf(`Evict per-corpus caches older than the TTL (default %s) or all
non-locked corpora with --all. Honors a per-process throttle gate
unless --force is passed. --dry-run prints the plan without mutating
anything. --sort orders the table by name, age, or size — use
--dry-run --sort=size to see what takes space.`, humanDuration(defaultGCMaxAge)),
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("seek gc takes no positional arguments (got %q)", args[0])
			}
			return nil
		},
		// gc takes no positional args. Without this, shells default to
		// file completion after `seek gc <TAB>`, which is meaningless
		// and confusing — gc only takes --flags.
		ValidArgsFunction: cobra.NoFileCompletions,
		SilenceErrors:     true,
		SilenceUsage:      true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGCCommandCmd(cmd.Context(), *opts)
		},
	}
	cmd.Flags().BoolVar(&opts.force, "force", false, "bypass throttle gate (.last-gc)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "print plan, evict nothing")
	cmd.Flags().BoolVar(&opts.all, "all", false, "evict every corpus not actively locked (TTL=0)")
	cmd.Flags().StringVar(&opts.sort, "sort", "name", "row order: name|age|size (age = oldest first, size = largest first)")
	// Registration can only fail if the flag above is missing — programmer
	// error, not a runtime condition.
	_ = cmd.RegisterFlagCompletionFunc("sort",
		cobra.FixedCompletions(gcSortValues, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}
