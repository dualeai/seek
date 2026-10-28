package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newGCCmd builds the `seek gc` subcommand, which runs runGCCommandCmd with
// its flags.
func newGCCmd() *cobra.Command {
	opts := &gcCmdOptions{}
	cmd := &cobra.Command{
		Use:     "gc",
		Aliases: []string{"garbage-collect"},
		Short:   "Delete unused search indexes, compiled model files, and other versions' runtimes",
		Long: fmt.Sprintf(`Delete the least recently used search indexes while the index cache is over
its budget, and compiled model files no search has used for %s. Seek rebuilds
either when it needs them; a removed model file also makes Seek check its
run-time provider again. It also deletes the runtimes of other Seek versions
whose directories are older than %[1]s. The next use of such a version extracts
them again. The budget is 10%% of the cache volume, or less to keep 5%% of the
volume free; set %[2]s (for example 20G) to change it.

Use --all to delete every index that is not in use, every compiled model file,
and every runtime of another version. Use --dry-run to show what would be
deleted, with sizes. Use --force to run cleanup even if it ran recently. Use
--sort=size with --dry-run to find the largest indexes.`, humanDuration(providerArtifactMaxAge), envCacheBudget),
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("seek gc takes no positional arguments (got %q)", args[0])
			}
			return nil
		},
		// gc takes no positional arguments. Without this, shells complete
		// file names after `seek gc <TAB>`.
		ValidArgsFunction: cobra.NoFileCompletions,
		SilenceErrors:     true,
		SilenceUsage:      true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGCCommandCmd(cmd.Context(), *opts)
		},
	}
	cmd.Flags().BoolVar(&opts.force, "force", false, "run cleanup even if it ran recently")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "show what would be deleted")
	cmd.Flags().BoolVar(&opts.all, "all", false, "delete every index not in use, every compiled model, and other versions' runtimes")
	cmd.Flags().StringVar(&opts.sort, "sort", "name", "sort by name, age, or size")
	// Registration fails only when the flag is missing or already has a
	// completion function. Both are programming errors, not run-time
	// conditions.
	_ = cmd.RegisterFlagCompletionFunc("sort",
		cobra.FixedCompletions(gcSortValues, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}
