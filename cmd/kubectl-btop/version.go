package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// version and commit are set via -ldflags "-X main.version=... -X main.commit=..."
// (see the Makefile's GO_LDFLAGS).
var (
	version = "dev"
	commit  = "none"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "version",
		Short:         "Print the btop version",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "btop version %s (commit %s, %s, %s/%s)\n",
				version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)

			return err
		},
	}
}
