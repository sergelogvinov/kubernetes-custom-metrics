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
	"fmt"
	"io"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/resource"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// resourceDescriptor drives one resource subcommand: its canonical name,
// aliases, whether it is namespaced, the GroupKind the custom-metrics API
// expects, and the cpu/memory metric base names for that resource.
// node_cpu/node_memory apply only to Nodes (metric-gateway.md §2).
type resourceDescriptor struct {
	Name       string
	Aliases    []string
	Namespaced bool
	GroupKind  schema.GroupKind
	CPUBase    string
	MemoryBase string
}

// resourceAliases are the kubectl-style short names each subcommand
// accepts. Name, GroupKind and namespacing come from pkg/resource; the
// cpu/memory base names follow the kind's scope.
var resourceAliases = map[resource.Kind][]string{
	resource.Pod:         {"po"},
	resource.Deployment:  {"deploy"},
	resource.StatefulSet: {"sts"},
	resource.DaemonSet:   {"ds"},
	resource.Job:         {"job"},
	resource.CronJob:     {"cj"},
}

var resourceDescriptors = newResourceDescriptors()

func newResourceDescriptors() []resourceDescriptor {
	kinds := resource.All()
	descs := make([]resourceDescriptor, 0, len(kinds))

	for _, kind := range kinds {
		cpuBase, memoryBase := "cpu", "memory"
		if kind.Scope() == resource.ScopeNode {
			cpuBase, memoryBase = "node_cpu", "node_memory"
		}

		descs = append(descs, resourceDescriptor{
			Name:       kind.GVR().Resource,
			Aliases:    resourceAliases[kind],
			Namespaced: kind.Namespaced(),
			GroupKind:  kind.GroupKind(),
			CPUBase:    cpuBase,
			MemoryBase: memoryBase,
		})
	}

	return descs
}

// newResourceCommands builds the seven resource subcommands from
// resourceDescriptors. All of them share o.
func newResourceCommands(o *Options) []*cobra.Command {
	cmds := make([]*cobra.Command, 0, len(resourceDescriptors))
	for _, desc := range resourceDescriptors {
		cmds = append(cmds, newResourceCommand(desc, o))
	}

	return cmds
}

func newResourceCommand(desc resourceDescriptor, o *Options) *cobra.Command {
	return &cobra.Command{
		Use:           desc.Name,
		Aliases:       desc.Aliases,
		Short:         fmt.Sprintf("Show historical CPU and memory usage for %s", desc.Name),
		Args:          maxOneArg,
		SilenceUsage:  true,
		SilenceErrors: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return desc.validateArgs(cmd, o, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResource(cmd.Context(), desc, o, args, cmd.OutOrStdout())
		},
	}
}

// maxOneArg wraps cobra.MaximumNArgs(1) so that too many arguments exit with
// code 2, like every other usage error.
func maxOneArg(cmd *cobra.Command, args []string) error {
	if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
		return &usageError{err: err}
	}

	return nil
}

// validateArgs rejects --namespace on a cluster-scoped resource and
// --selector together with a named object.
func (d resourceDescriptor) validateArgs(cmd *cobra.Command, o *Options, args []string) error {
	if !d.Namespaced && cmd.Flags().Changed(flagNamespace) {
		return &usageError{err: fmt.Errorf("--%s is not valid for %s", flagNamespace, d.Name)}
	}
	if len(args) == 1 && o.Selector != "" {
		return &usageError{err: fmt.Errorf("--%s cannot be combined with a named object", flagSelector)}
	}

	return nil
}

// runResource fetches, joins, and prints one resource's rows. It runs once;
// there is no watch or refresh mode.
func runResource(ctx context.Context, desc resourceDescriptor, o *Options, args []string, stdout io.Writer) error {
	client, err := NewClient(o)
	if err != nil {
		return err
	}

	namespace := ""
	if desc.Namespaced {
		namespace = o.Namespace
		if namespace == "" {
			namespace = client.Namespace()
		}
	}

	cpuMetric := metricName(desc.CPUBase, o.Stat, o.Window)
	memMetric := metricName(desc.MemoryBase, o.Stat, o.Window)
	collector := NewCollector(client)

	var rows []joinedRow

	if len(args) == 1 {
		row, err := collector.CollectNamed(ctx, desc, namespace, args[0], cpuMetric, memMetric)
		if err != nil {
			return err
		}

		rows = []joinedRow{row}
	} else {
		selector, err := parseSelector(o.Selector)
		if err != nil {
			return &usageError{err: err}
		}

		rows, err = collector.CollectList(ctx, desc, namespace, selector, cpuMetric, memMetric)
		if err != nil {
			return err
		}
	}

	return renderRows(stdout, o, desc, rows)
}

func metricName(base, stat, window string) string {
	return fmt.Sprintf("%s_%s_%s", base, stat, window)
}

func parseSelector(s string) (labels.Selector, error) {
	if s == "" {
		return labels.Everything(), nil
	}

	return labels.Parse(s)
}
