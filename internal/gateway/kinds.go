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

package gateway

import (
	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/resolver"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kindSpec pairs a resolver.Kind with the fixed apiVersion its
// describedObject uses (metric-gateway.md §3.5).
type kindSpec struct {
	kind       resolver.Kind
	apiVersion string
}

// groupResourceKinds is the fixed set of resource/metric combinations this
// gateway ever serves (metric-gateway.md §3.5). It does not depend on a
// RESTMapper: these are built-in Kubernetes resources with stable
// versions, mirroring pkg/catalog's own resourcesFor table.
var groupResourceKinds = map[schema.GroupResource]kindSpec{
	{Resource: "pods"}:                        {kind: resolver.KindPod, apiVersion: "v1"},
	{Resource: "nodes"}:                       {kind: resolver.KindNode, apiVersion: "v1"},
	{Group: "apps", Resource: "deployments"}:  {kind: resolver.KindDeployment, apiVersion: "apps/v1"},
	{Group: "apps", Resource: "statefulsets"}: {kind: resolver.KindStatefulSet, apiVersion: "apps/v1"},
	{Group: "apps", Resource: "daemonsets"}:   {kind: resolver.KindDaemonSet, apiVersion: "apps/v1"},
	{Group: "batch", Resource: "jobs"}:        {kind: resolver.KindJob, apiVersion: "batch/v1"},
	{Group: "batch", Resource: "cronjobs"}:    {kind: resolver.KindCronJob, apiVersion: "batch/v1"},
}

// kindForGroupResource resolves gr to its resolver.Kind and apiVersion, and
// reports whether base — the catalog base the request's metric name parsed
// to — actually applies to that kind's scope (pod-scoped bases apply to
// Pods and the five workload kinds; node-scoped bases apply only to Nodes).
// A false result means "unsupported base/resource combination", which
// metric-gateway.md §2 says is simply not advertised and returns 404.
func kindForGroupResource(gr schema.GroupResource, base catalog.Base) (kindSpec, bool) {
	spec, ok := groupResourceKinds[gr]
	if !ok {
		return kindSpec{}, false
	}

	wantsNode := base.Scope == catalog.ScopeNode
	isNode := spec.kind == resolver.KindNode
	if wantsNode != isNode {
		return kindSpec{}, false
	}

	return spec, true
}
