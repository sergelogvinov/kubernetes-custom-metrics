package main

import (
	"context"
	"fmt"
	"io"

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

var resourceDescriptors = []resourceDescriptor{
	{Name: "pods", Aliases: []string{"po"}, Namespaced: true, GroupKind: schema.GroupKind{Kind: "Pod"}, CPUBase: "cpu", MemoryBase: "memory"},
	{Name: "nodes", Namespaced: false, GroupKind: schema.GroupKind{Kind: "Node"}, CPUBase: "node_cpu", MemoryBase: "node_memory"},
	{Name: "deployments", Aliases: []string{"deploy"}, Namespaced: true, GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, CPUBase: "cpu", MemoryBase: "memory"},
	{Name: "statefulsets", Aliases: []string{"sts"}, Namespaced: true, GroupKind: schema.GroupKind{Group: "apps", Kind: "StatefulSet"}, CPUBase: "cpu", MemoryBase: "memory"},
	{Name: "daemonsets", Aliases: []string{"ds"}, Namespaced: true, GroupKind: schema.GroupKind{Group: "apps", Kind: "DaemonSet"}, CPUBase: "cpu", MemoryBase: "memory"},
	{Name: "jobs", Aliases: []string{"job"}, Namespaced: true, GroupKind: schema.GroupKind{Group: "batch", Kind: "Job"}, CPUBase: "cpu", MemoryBase: "memory"},
	{Name: "cronjobs", Aliases: []string{"cj"}, Namespaced: true, GroupKind: schema.GroupKind{Group: "batch", Kind: "CronJob"}, CPUBase: "cpu", MemoryBase: "memory"},
}

// newResourceCommands builds the seven resource subcommands from
// resourceDescriptors, all sharing o (design.md §5.3).
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

// maxOneArg wraps cobra.MaximumNArgs(1) so Args rejection maps to exit code
// 2 like every other usage error (design.md §4).
func maxOneArg(cmd *cobra.Command, args []string) error {
	if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
		return &usageError{err: err}
	}

	return nil
}

// validateArgs rejects --namespace on a cluster-scoped resource and
// --selector combined with a named object (design.md §5.3 point 5).
func (d resourceDescriptor) validateArgs(cmd *cobra.Command, o *Options, args []string) error {
	if !d.Namespaced && cmd.Flags().Changed(flagNamespace) {
		return &usageError{err: fmt.Errorf("--%s is not valid for %s", flagNamespace, d.Name)}
	}
	if len(args) == 1 && o.Selector != "" {
		return &usageError{err: fmt.Errorf("--%s cannot be combined with a named object", flagSelector)}
	}

	return nil
}

// runResource fetches, joins, and renders one resource's rows: the fetch →
// join → output path (design.md §12), with no watch/refresh logic (T10).
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
