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
	"fmt"
	"slices"
	"time"

	"github.com/spf13/pflag"
)

const (
	flagWindow         = "window"
	flagStat           = "stat"
	flagSelector       = "selector"
	flagNamespace      = "namespace"
	flagNoHeaders      = "no-headers"
	flagSortBy         = "sort-by"
	flagOutput         = "output"
	flagRequestTimeout = "request-timeout"
	flagKubeconfig     = "kubeconfig"
	flagContext        = "context"

	envWindow         = "WINDOW"
	envNamespace      = "NAMESPACE"
	envOutput         = "OUTPUT"
	envRequestTimeout = "REQUEST_TIMEOUT"
	envContext        = "CONTEXT"
)

const (
	defaultWindow         = "5m"
	defaultStat           = "avg"
	defaultSortBy         = "name"
	defaultOutput         = "table"
	defaultRequestTimeout = 35 * time.Second
)

var (
	validStats   = []string{"avg", "max", "min", "p50", "p90", "p95", "p99", "stddev"}
	validSortBy  = []string{"cpu", "memory", "name"}
	validOutputs = []string{"table", "json", "yaml"}
)

// Options holds the effective value of every persistent btop flag, after
// flag > environment variable > default resolution (design.md §5.2).
type Options struct {
	Window         string
	Stat           string
	Selector       string
	Namespace      string
	NoHeaders      bool
	SortBy         string
	Output         string
	RequestTimeout time.Duration
	Kubeconfig     string
	Context        string
}

// NewOptions returns Options bound to their documented hardcoded defaults.
// AddFlags registers pflag bindings against these same fields, and
// ResolveEnvironment later overrides only the fields the user did not pass
// explicitly on the command line.
func NewOptions() *Options {
	return &Options{
		Window:         defaultWindow,
		Stat:           defaultStat,
		SortBy:         defaultSortBy,
		Output:         defaultOutput,
		RequestTimeout: defaultRequestTimeout,
	}
}

// AddFlags registers every persistent btop flag on fs, bound directly to o's
// fields. Every resource subcommand inherits these through cobra's
// PersistentFlags() merge; no subcommand registers flags of its own
// (design.md §5.3).
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.Window, flagWindow, o.Window, "Statistical window, e.g. 5m, 26m, 2h (any Go duration, minimum 1m)")
	fs.StringVar(&o.Stat, flagStat, o.Stat, "Statistic (avg, max, min, p50, p90, p95, p99, stddev)")
	fs.StringVarP(&o.Selector, flagSelector, "l", o.Selector, "Label selector")
	fs.StringVarP(&o.Namespace, flagNamespace, "n", o.Namespace, "Namespace (rejected for nodes; defaults to the kubeconfig context namespace)")
	fs.BoolVar(&o.NoHeaders, flagNoHeaders, o.NoHeaders, "Omit table headers")
	fs.StringVar(&o.SortBy, flagSortBy, o.SortBy, "Sort rows by cpu, memory, or name")
	fs.StringVarP(&o.Output, flagOutput, "o", o.Output, "Output format: table, json, yaml")
	fs.DurationVar(&o.RequestTimeout, flagRequestTimeout, o.RequestTimeout, "Timeout bounding discovery and metric HTTP requests")
	fs.StringVar(&o.Kubeconfig, flagKubeconfig, o.Kubeconfig, "Path to the kubeconfig file")
	fs.StringVar(&o.Context, flagContext, o.Context, "Kubeconfig context to use")
}

// ResolveEnvironment overrides every field whose flag was not explicitly set
// with its documented environment variable (metric-gateway.md §6.2). changed
// reports whether a given flag was explicitly passed; lookupEnv is injected
// so tests never mutate the process environment. Stat, Selector, SortBy,
// and NoHeaders have no environment variable and are flag-only. KUBECONFIG is
// deliberately excluded here too: client-go itself applies that environment
// variable's path-list merging, and copying it into a single Kubeconfig
// field would collapse that behavior to one file (design.md §5.2).
func (o *Options) ResolveEnvironment(changed func(name string) bool, lookupEnv func(string) (string, bool)) error {
	if !changed(flagNamespace) {
		if v, ok := lookupEnv(envNamespace); ok {
			o.Namespace = v
		}
	}
	if !changed(flagContext) {
		if v, ok := lookupEnv(envContext); ok {
			o.Context = v
		}
	}
	if !changed(flagWindow) {
		if v, ok := lookupEnv(envWindow); ok {
			o.Window = v
		}
	}
	if !changed(flagRequestTimeout) {
		if v, ok := lookupEnv(envRequestTimeout); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return envError(flagRequestTimeout, envRequestTimeout, v)
			}
			o.RequestTimeout = d
		}
	}
	if !changed(flagOutput) {
		if v, ok := lookupEnv(envOutput); ok {
			o.Output = v
		}
	}

	return nil
}

// Validate rejects malformed effective values regardless of whether they
// came from a flag or an environment variable (metric-gateway.md §6.2).
// Cross-flag/resource rules (namespace on nodes, selector with a named
// object) are descriptor-driven and live in each subcommand's PreRunE.
func (o *Options) Validate() error {
	if !validWindow(o.Window) {
		return valueError(flagWindow, o.Window)
	}
	if !slices.Contains(validStats, o.Stat) {
		return valueError(flagStat, o.Stat)
	}
	if !slices.Contains(validSortBy, o.SortBy) {
		return valueError(flagSortBy, o.SortBy)
	}
	if !slices.Contains(validOutputs, o.Output) {
		return valueError(flagOutput, o.Output)
	}
	if o.RequestTimeout <= 0 {
		return &usageError{err: fmt.Errorf("--%s must be positive, got %s", flagRequestTimeout, o.RequestTimeout)}
	}

	return nil
}

// minWindow is the shortest window the metric-name grammar accepts
// (pkg/catalog.MinWindow, kept here as a plain constant so this CLI
// binary does not need to import the gateway's internal packages).
const minWindow = time.Minute

// validWindow reports whether s is a Go-duration-syntax string of at
// least minWindow (e.g. "5m", "26m", "2h"), matching the metric-name
// grammar's window segment (metric-gateway.md §2), which accepts any such
// duration, not just the seven canonical presets discovery advertises.
func validWindow(s string) bool {
	d, err := time.ParseDuration(s)

	return err == nil && d >= minWindow
}

func envError(flag, env, value string) error {
	return &usageError{err: fmt.Errorf("invalid value %q for environment variable %s (--%s)", value, env, flag)}
}

func valueError(flag, value string) error {
	return &usageError{err: fmt.Errorf("invalid value %q for --%s", value, flag)}
}
