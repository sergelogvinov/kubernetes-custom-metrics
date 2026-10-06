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

// Package resource is the single table of the seven resource kinds the
// gateway serves (metric-gateway.md §1 decision 6, §3.3, §3.5): each kind's
// GroupVersionResource, whether it is namespaced, and its scope. These are
// built-in Kubernetes resources with stable versions, so the table is
// hardcoded rather than discovered through a RESTMapper.
package resource

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Scope is what a kind's metric value is measured over: its Pods, or the
// Node itself. A catalog base applies to a kind only when their scopes
// match (metric-gateway.md §2, §8).
type Scope string

// The two scopes.
const (
	ScopePod  Scope = "pod"
	ScopeNode Scope = "node"
)

// Kind is one of the seven supported resource kinds. Its value is the
// Kubernetes Kind string, as used in describedObject.
type Kind string

// The seven supported resource kinds.
const (
	Pod         Kind = "Pod"
	Node        Kind = "Node"
	Deployment  Kind = "Deployment"
	StatefulSet Kind = "StatefulSet"
	DaemonSet   Kind = "DaemonSet"
	Job         Kind = "Job"
	CronJob     Kind = "CronJob"
)

type spec struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}

var specs = map[Kind]spec{
	Pod:         {gvr: schema.GroupVersionResource{Version: "v1", Resource: "pods"}, namespaced: true},
	Node:        {gvr: schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, namespaced: false},
	Deployment:  {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, namespaced: true},
	StatefulSet: {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, namespaced: true},
	DaemonSet:   {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, namespaced: true},
	Job:         {gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, namespaced: true},
	CronJob:     {gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, namespaced: true},
}

// all fixes the order All returns, which discovery and kubectl-ctop's
// subcommands follow.
var all = []Kind{Pod, Node, Deployment, StatefulSet, DaemonSet, Job, CronJob}

// All returns every supported kind in a fixed order.
func All() []Kind {
	out := make([]Kind, len(all))
	copy(out, all)

	return out
}

// Lookup returns the kind served at gr, or false if gr is not supported.
func Lookup(gr schema.GroupResource) (Kind, bool) {
	for _, k := range all {
		if specs[k].gvr.GroupResource() == gr {
			return k, true
		}
	}

	return "", false
}

// Valid reports whether k is one of the supported kinds.
func (k Kind) Valid() bool {
	_, ok := specs[k]

	return ok
}

// GVR returns k's GroupVersionResource. It is the zero value for an
// unsupported kind.
func (k Kind) GVR() schema.GroupVersionResource {
	return specs[k].gvr
}

// GroupResource returns k's GroupResource, the form the custom-metrics API
// uses in requests and discovery.
func (k Kind) GroupResource() schema.GroupResource {
	return specs[k].gvr.GroupResource()
}

// GroupKind returns k's GroupKind, the form the custom-metrics client uses.
func (k Kind) GroupKind() schema.GroupKind {
	return schema.GroupKind{Group: specs[k].gvr.Group, Kind: string(k)}
}

// APIVersion returns k's apiVersion, as used in describedObject.
func (k Kind) APIVersion() string {
	return specs[k].gvr.GroupVersion().String()
}

// Namespaced reports whether k's objects live in a namespace. Only Node is
// cluster-scoped.
func (k Kind) Namespaced() bool {
	return specs[k].namespaced
}

// Scope returns what k's metric value is measured over: the Node itself for
// Node, its Pods for every other kind.
func (k Kind) Scope() Scope {
	if k == Node {
		return ScopeNode
	}

	return ScopePod
}
