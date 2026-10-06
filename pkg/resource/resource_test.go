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

package resource_test

import (
	"testing"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind       resource.Kind
		gr         schema.GroupResource
		apiVersion string
		namespaced bool
		scope      resource.Scope
	}{
		{resource.Pod, schema.GroupResource{Resource: "pods"}, "v1", true, resource.ScopePod},
		{resource.Node, schema.GroupResource{Resource: "nodes"}, "v1", false, resource.ScopeNode},
		{resource.Deployment, schema.GroupResource{Group: "apps", Resource: "deployments"}, "apps/v1", true, resource.ScopePod},
		{resource.StatefulSet, schema.GroupResource{Group: "apps", Resource: "statefulsets"}, "apps/v1", true, resource.ScopePod},
		{resource.DaemonSet, schema.GroupResource{Group: "apps", Resource: "daemonsets"}, "apps/v1", true, resource.ScopePod},
		{resource.Job, schema.GroupResource{Group: "batch", Resource: "jobs"}, "batch/v1", true, resource.ScopePod},
		{resource.CronJob, schema.GroupResource{Group: "batch", Resource: "cronjobs"}, "batch/v1", true, resource.ScopePod},
	}

	all := resource.All()
	if len(all) != len(tests) {
		t.Fatalf("All() = %d kinds, want %d", len(all), len(tests))
	}

	for i, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			t.Parallel()

			if all[i] != tt.kind {
				t.Errorf("All()[%d] = %q, want %q", i, all[i], tt.kind)
			}
			if !tt.kind.Valid() {
				t.Errorf("Valid() = false")
			}
			if got, ok := resource.Lookup(tt.gr); !ok || got != tt.kind {
				t.Errorf("Lookup(%v) = %q, %v; want %q, true", tt.gr, got, ok, tt.kind)
			}
			if got := tt.kind.GroupResource(); got != tt.gr {
				t.Errorf("GroupResource() = %v, want %v", got, tt.gr)
			}
			if got := tt.kind.GroupKind(); got != (schema.GroupKind{Group: tt.gr.Group, Kind: string(tt.kind)}) {
				t.Errorf("GroupKind() = %v", got)
			}
			if got := tt.kind.APIVersion(); got != tt.apiVersion {
				t.Errorf("APIVersion() = %q, want %q", got, tt.apiVersion)
			}
			if got := tt.kind.Namespaced(); got != tt.namespaced {
				t.Errorf("Namespaced() = %v, want %v", got, tt.namespaced)
			}
			if got := tt.kind.Scope(); got != tt.scope {
				t.Errorf("Scope() = %q, want %q", got, tt.scope)
			}
		})
	}
}

func TestLookupUnsupported(t *testing.T) {
	t.Parallel()

	for _, gr := range []schema.GroupResource{
		{Resource: "services"},
		{Group: "apps", Resource: "replicasets"},
		{Group: "extensions", Resource: "deployments"},
	} {
		if k, ok := resource.Lookup(gr); ok {
			t.Errorf("Lookup(%v) = %q, true; want false", gr, k)
		}
	}

	if resource.Kind("ReplicaSet").Valid() {
		t.Error(`Kind("ReplicaSet").Valid() = true`)
	}
}

func TestAllReturnsCopy(t *testing.T) {
	t.Parallel()

	a := resource.All()
	a[0] = "Mutated"

	if resource.All()[0] != resource.Pod {
		t.Error("All() exposes its backing slice")
	}
}
