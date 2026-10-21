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
		Short:   "Delete unused search indexes and compiled model files",
		Long: fmt.Sprintf(`Delete the least recently used search indexes while the index cache is over
its budget, and compiled model files no search has used for %s. Seek rebuilds
either when it needs them; a removed model file also makes Seek check its
run-time provider again. The budget is 10%% of the cache volume, or less to keep
5%% of the volume free; set %s (for example 20G) to change it.

Use --all to delete every index and compiled model file that is not in use. Use
--dry-run to show what would be deleted, with sizes. Use --force to run cleanup
even if it ran recently. Use --sort=size with --dry-run to find the largest
indexes.`, humanDuration(providerArtifactMaxAge), envCacheBudget),
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
	cmd.Flags().BoolVar(&opts.force, "force", false, "run cleanup even if it ran recently")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "show what would be deleted")
	cmd.Flags().BoolVar(&opts.all, "all", false, "delete every search index that is not in use")
	cmd.Flags().StringVar(&opts.sort, "sort", "name", "sort by name, age, or size")
	// Registration can only fail if the flag above is missing — programmer
	// error, not a runtime condition.
	_ = cmd.RegisterFlagCompletionFunc("sort",
		cobra.FixedCompletions(gcSortValues, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}
