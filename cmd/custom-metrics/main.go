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
	"syscall"

	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/telemetry"
	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	basecmd "sigs.k8s.io/custom-metrics-apiserver/pkg/cmd"
)

// gatewayAdapter embeds AdapterBase per design.md §2.1/§10: the framework
// owns generic-apiserver wiring (secure serving, delegated authn/authz,
// discovery, health endpoints); this repository supplies the
// provider.CustomMetricsProvider built in internal/gateway and its own
// gateway-specific Options registered alongside AdapterBase's own flags.
type gatewayAdapter struct {
	basecmd.AdapterBase
}

// usageError marks a flag-parsing, Args, or Options.Validate failure so run
// can map it to exit code 2 instead of the general-failure exit code 1.
type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func main() {
	quietClientDisconnects()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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

// NewRootCommand builds the custom-metrics command tree: a single command
// wrapping basecmd.AdapterBase (design.md §5.1, §10) plus this
// repository's gateway-specific Options on the same *pflag.FlagSet.
func NewRootCommand() *cobra.Command {
	adapter := newGatewayAdapter()
	opts := NewOptions()

	cmd := &cobra.Command{
		Use:           "custom-metrics",
		Short:         "Aggregated custom.metrics.k8s.io API server backed by Prometheus",
		Version:       versionString(),
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.ResolveEnvironment(cmd.Flags().Changed, os.LookupEnv); err != nil {
				return err
			}
			if err := opts.Validate(); err != nil {
				return err
			}

			dynamicClient, err := adapter.DynamicClient()
			if err != nil {
				return fmt.Errorf("building the lister dynamic client: %w", err)
			}

			return runAdapter(cmd.Context(), adapter, opts, dynamicClient)
		},
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err: err}
	})

	adapter.FlagSet = cmd.Flags()
	adapter.InstallFlags()
	opts.AddFlags(cmd.Flags())
	installKlogFlags(cmd.Flags())

	return cmd
}

func newGatewayAdapter() *gatewayAdapter {
	adapter := &gatewayAdapter{}
	adapter.Name = "custom-metrics"

	return adapter
}

// runAdapter loads and validates the catalog, wires the real
// internal/gateway provider into adapter, injects readiness checks, and
// serves until ctx is canceled. dynamicClient backs the resolver's
// Kubernetes access — adapter.DynamicClient() in production, a fake
// dynamic.Interface in tests — so this function, and everything it calls,
// is exercised identically in both. It is factored out of NewRootCommand's
// RunE for exactly that reason.
func runAdapter(ctx context.Context, adapter *gatewayAdapter, opts *Options, dynamicClient dynamic.Interface) error {
	cat, err := loadCatalog(opts.CatalogPath, opts.DiscoveryMode, opts.DiscoveryMaxMetrics)
	if err != nil {
		return err
	}

	metrics := telemetry.New()
	metrics.Register()

	gatewayProvider, promClient, err := buildGateway(ctx, opts, cat, dynamicClient, metrics)
	if err != nil {
		return err
	}

	adapter.WithCustomMetrics(gatewayProvider)

	config, err := adapter.Config()
	if err != nil {
		return fmt.Errorf("configuring adapter: %w", err)
	}
	addReadyzChecks(config, cat, dynamicClient, promClient)

	return adapter.Run(ctx)
}
