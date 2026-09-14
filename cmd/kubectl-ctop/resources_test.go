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
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func newTestCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	o.AddFlags(cmd.Flags())

	return cmd
}

func TestResourceDescriptors(t *testing.T) {
	want := map[string]struct {
		aliases    []string
		namespaced bool
		groupKind  string
		cpuBase    string
		memBase    string
	}{
		"pods":         {[]string{"po"}, true, "Pod", "cpu", "memory"},
		"nodes":        {nil, false, "Node", "node_cpu", "node_memory"},
		"deployments":  {[]string{"deploy"}, true, "apps/Deployment", "cpu", "memory"},
		"statefulsets": {[]string{"sts"}, true, "apps/StatefulSet", "cpu", "memory"},
		"daemonsets":   {[]string{"ds"}, true, "apps/DaemonSet", "cpu", "memory"},
		"jobs":         {[]string{"job"}, true, "batch/Job", "cpu", "memory"},
		"cronjobs":     {[]string{"cj"}, true, "batch/CronJob", "cpu", "memory"},
	}

	if len(resourceDescriptors) != len(want) {
		t.Fatalf("resourceDescriptors has %d entries, want %d", len(resourceDescriptors), len(want))
	}

	for _, d := range resourceDescriptors {
		exp, ok := want[d.Name]
		if !ok {
			t.Errorf("unexpected resource %q", d.Name)
			continue
		}

		if !equalStrings(d.Aliases, exp.aliases) {
			t.Errorf("%s: aliases = %v, want %v", d.Name, d.Aliases, exp.aliases)
		}
		if d.Namespaced != exp.namespaced {
			t.Errorf("%s: Namespaced = %v, want %v", d.Name, d.Namespaced, exp.namespaced)
		}

		gk := d.GroupKind.Kind
		if d.GroupKind.Group != "" {
			gk = d.GroupKind.Group + "/" + d.GroupKind.Kind
		}
		if gk != exp.groupKind {
			t.Errorf("%s: GroupKind = %s, want %s", d.Name, gk, exp.groupKind)
		}
		if d.CPUBase != exp.cpuBase || d.MemoryBase != exp.memBase {
			t.Errorf("%s: bases = %s/%s, want %s/%s", d.Name, d.CPUBase, d.MemoryBase, exp.cpuBase, exp.memBase)
		}
	}
}

func TestValidateArgs_NamespaceRejectedOnClusterScoped(t *testing.T) {
	nodes := resourceDescriptor{Name: "nodes", Namespaced: false}
	o := NewOptions()
	cmd := newTestCommand(o)

	if err := cmd.Flags().Set(flagNamespace, "prod"); err != nil {
		t.Fatalf("Set(--namespace) error = %v", err)
	}

	err := nodes.validateArgs(cmd, o, nil)
	if err == nil {
		t.Fatal("validateArgs() error = nil, want rejection of --namespace on a cluster-scoped resource")
	}
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("validateArgs() error = %v, want a *usageError", err)
	}
}

func TestValidateArgs_NoNamespaceFlagOnClusterScopedIsFine(t *testing.T) {
	nodes := resourceDescriptor{Name: "nodes", Namespaced: false}
	o := NewOptions()
	cmd := newTestCommand(o)

	if err := nodes.validateArgs(cmd, o, nil); err != nil {
		t.Errorf("validateArgs() error = %v, want nil when --namespace was never set", err)
	}
}

func TestValidateArgs_SelectorWithNamedObjectRejected(t *testing.T) {
	pods := resourceDescriptor{Name: "pods", Namespaced: true}
	o := NewOptions()
	o.Selector = "app=web"
	cmd := newTestCommand(o)

	err := pods.validateArgs(cmd, o, []string{"web-0"})
	if err == nil {
		t.Fatal("validateArgs() error = nil, want rejection of --selector with a named object")
	}
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("validateArgs() error = %v, want a *usageError", err)
	}
}

func TestValidateArgs_SelectorWithoutNameIsFine(t *testing.T) {
	pods := resourceDescriptor{Name: "pods", Namespaced: true}
	o := NewOptions()
	o.Selector = "app=web"
	cmd := newTestCommand(o)

	if err := pods.validateArgs(cmd, o, nil); err != nil {
		t.Errorf("validateArgs() error = %v, want nil for a wildcard request with a selector", err)
	}
}

func TestMaxOneArg(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}

	if err := maxOneArg(cmd, []string{"one"}); err != nil {
		t.Errorf("maxOneArg() error = %v, want nil for a single arg", err)
	}

	err := maxOneArg(cmd, []string{"one", "two"})
	if err == nil {
		t.Fatal("maxOneArg() error = nil, want rejection of a second positional arg")
	}
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("maxOneArg() error = %v, want a *usageError so it exits with code 2", err)
	}
}

func TestMetricName(t *testing.T) {
	if got, want := metricName("cpu", "avg", "5m"), "cpu_avg_5m"; got != want {
		t.Errorf("metricName() = %q, want %q", got, want)
	}
	if got, want := metricName("node_cpu", "p95", "1h"), "node_cpu_p95_1h"; got != want {
		t.Errorf("metricName() = %q, want %q", got, want)
	}
}

func TestParseSelector(t *testing.T) {
	sel, err := parseSelector("")
	if err != nil {
		t.Fatalf("parseSelector(\"\") error = %v", err)
	}
	if !sel.Empty() {
		t.Errorf("parseSelector(\"\") = %v, want the everything selector", sel)
	}

	sel, err = parseSelector("app=web")
	if err != nil {
		t.Fatalf("parseSelector() error = %v", err)
	}
	if sel.String() != "app=web" {
		t.Errorf("parseSelector() = %v, want app=web", sel)
	}

	if _, err := parseSelector("not a valid selector!!"); err == nil {
		t.Error("parseSelector() error = nil, want rejection of a malformed selector")
	}
}
