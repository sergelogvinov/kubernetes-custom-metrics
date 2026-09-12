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
		SilenceUsage:  false,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.ResolveEnvironment(cmd.Flags().Changed, os.LookupEnv); err != nil {
				return err
			}

			return opts.Validate()
		},
	}

	opts.AddFlags(cmd.PersistentFlags())

	cmd.AddCommand(newVersionCommand())

	return cmd
}
