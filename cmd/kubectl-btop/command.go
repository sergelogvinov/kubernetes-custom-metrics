package main

import (
	"os"

	"github.com/spf13/cobra"
)

// NewRootCommand builds the btop command tree. The seven resource
// subcommands land with the client implementation (resources.go); this is
// scaffolding plus the shared persistent flags every subcommand inherits.
func NewRootCommand() *cobra.Command {
	opts := NewOptions()

	cmd := &cobra.Command{
		Use:           "btop",
		Short:         "Show historical CPU and memory usage for Kubernetes workloads",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.ResolveEnvironment(cmd.Flags().Changed, os.LookupEnv); err != nil {
				return err
			}

			return opts.Validate()
		},
	}

	// Flag-parsing errors bypass PersistentPreRunE entirely, so they need
	// their own wrap to map to exit code 2 like every other usage error
	// (design.md §4). Child commands inherit this when they don't set their
	// own.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err: err}
	})

	opts.AddFlags(cmd.PersistentFlags())

	cmd.AddCommand(newVersionCommand())
	cmd.AddCommand(newResourceCommands(opts)...)

	return cmd
}
