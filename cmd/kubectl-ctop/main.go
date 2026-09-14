/*
Copyright 2026 Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
)

type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := NewRootCommand() //nolint:contextcheck
	cmd.SetIn(stdin)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs(args)

	if err := cmd.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		if _, ok := errors.AsType[*usageError](err); ok { //nolint:errcheck
			return 2
		}

		return 1
	}

	return 0
}

func NewRootCommand() *cobra.Command {
	opts := NewOptions()

	cmd := &cobra.Command{
		Use:           "ctop",
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

	opts.AddFlags(cmd.PersistentFlags())

	cmd.AddCommand(newVersionCommand())
	cmd.AddCommand(newResourceCommands(opts)...)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err: err}
	})

	return cmd
}
