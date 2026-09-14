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
		Short:   "Delete old search indexes from the Seek cache",
		Long: fmt.Sprintf(`Delete search indexes that have not been used for %s.
Use --all to delete every index that is not in use. Use --dry-run to show what
would be deleted. Use --force to run cleanup even if it ran recently. Use
--sort=size with --dry-run to find the largest indexes.`, humanDuration(defaultGCMaxAge)),
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
