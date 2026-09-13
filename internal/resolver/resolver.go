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

// Package resolver translates a requested target (Pod, Node, Deployment,
// StatefulSet, DaemonSet, Job, or CronJob — named or wildcard) into resolved
// object identities plus the deduplicated, retained Pod UIDs that back its
// metric value, using the gateway ServiceAccount's own view of the cluster
// (design.md §7; metric-gateway.md §3.3, §3.4).
package resolver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/clock"
)

// Kind is one of the seven resource kinds the gateway resolves
// (metric-gateway.md §1 decision 6, §3.3).
type Kind string

// The seven supported resource kinds.
const (
	KindPod         Kind = "Pod"
	KindNode        Kind = "Node"
	KindDeployment  Kind = "Deployment"
	KindStatefulSet Kind = "StatefulSet"
	KindDaemonSet   Kind = "DaemonSet"
	KindJob         Kind = "Job"
	KindCronJob     Kind = "CronJob"
)

// kindSpec is the fixed GroupVersionResource and scope for a Kind. These are
// built-in Kubernetes resources with stable versions, so the resolver
// hardcodes them rather than performing RESTMapper discovery.
type kindSpec struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}

var kindSpecs = map[Kind]kindSpec{
	KindPod:         {gvr: schema.GroupVersionResource{Version: "v1", Resource: "pods"}, namespaced: true},
	KindNode:        {gvr: schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, namespaced: false},
	KindDeployment:  {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, namespaced: true},
	KindStatefulSet: {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, namespaced: true},
	KindDaemonSet:   {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, namespaced: true},
	KindJob:         {gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, namespaced: true},
	KindCronJob:     {gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, namespaced: true},
}

// ObjectRef identifies one resolved Kubernetes object.
type ObjectRef struct {
	Namespace         string
	Name              string
	UID               types.UID
	CreationTimestamp time.Time
}

// Resolution is one resolved target: the object's own identity, plus the
// deduplicated, retained Pod names that back a pod-scoped metric value. For
// a Pod target this is the pod's own name; for a workload it is the pods
// selected by its spec.selector (design.md §7); for a Node target it is
// empty — node-scoped metrics key off Object.Name directly.
type Resolution struct {
	Object   ObjectRef
	PodNames []string
	// CronJobFallback is true when this resolution came from a CronJob's
	// recent-Jobs fallback (metric-gateway.md §3.4 step 2) rather than an
	// active Job. Always false for every other Kind. Telemetry use only
	// (gateway_cronjob_fallback_total).
	CronJobFallback bool
}

// Target is a request for one Kubernetes object (Name set) or every object
// matching ObjectSelector in scope (Name empty — the wildcard path,
// metric-gateway.md §3.5, §3.6).
type Target struct {
	Kind Kind
	// Namespace is ignored for Node, the only cluster-scoped kind.
	Namespace string
	// Name selects a single named object. Empty means the wildcard path.
	Name string
	// ObjectSelector filters candidate objects on the wildcard path. Nil or
	// labels.Everything() matches every object in scope. Ignored for named
	// targets.
	ObjectSelector labels.Selector
}

// NotFoundError indicates the requested named object does not exist, or —
// for a CronJob — that no active or recent Job exists to derive metrics
// from. It maps to the caller's 404 (metric-gateway.md §3.4, §3.6).
type NotFoundError struct {
	Kind      Kind
	Namespace string
	Name      string
	Message   string
}

func (e *NotFoundError) Error() string {
	if e.Message != "" {
		return e.Message
	}

	return fmt.Sprintf("resolver: %s %q not found", e.Kind, namespacedName(e.Namespace, e.Name))
}

// ForbiddenError indicates the gateway's own ServiceAccount lacks the
// Kubernetes RBAC to read a resource it needs to resolve a target — a
// service configuration failure distinct from a caller's delegated-
// authorization 403, and one that must map to the caller's 503 instead
// (design.md §7; metric-gateway.md §3.3).
type ForbiddenError struct {
	Kind      Kind
	Namespace string
	Name      string
	Err       error
}

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("resolver: service account forbidden reading %s %q: %v", e.Kind, namespacedName(e.Namespace, e.Name), e.Err)
}

func (e *ForbiddenError) Unwrap() error {
	return e.Err
}

func namespacedName(namespace, name string) string {
	if namespace == "" {
		return name
	}

	return namespace + "/" + name
}

// DefaultCronJobFallbackWindow is the default recent-Job lookback
// (metric-gateway.md §1 decision 7, §3.4).
const DefaultCronJobFallbackWindow = 24 * time.Hour

// defaultPageSize bounds each List call; results are paginated via the
// returned continue token (metric-gateway.md §6.3).
const defaultPageSize = int64(500)

// Resolver resolves targets against the gateway ServiceAccount's own view
// of the cluster, obtained from a plain dynamic.Interface (design.md §3
// rule 4: this package does not import AdapterBase or any cmd/custom-metrics
// type).
type Resolver struct {
	client dynamic.Interface
	clock  clock.PassiveClock

	cronJobFallbackWindow time.Duration
	pageSize              int64
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithClock overrides the clock used for CronJob fallback-window
// evaluation. Tests inject a fake clock; never sleep in real time
// (plan.md T3 Done criteria).
func WithClock(c clock.PassiveClock) Option {
	return func(r *Resolver) { r.clock = c }
}

// WithCronJobFallbackWindow overrides the recent-Job lookback (default
// DefaultCronJobFallbackWindow).
func WithCronJobFallbackWindow(d time.Duration) Option {
	return func(r *Resolver) { r.cronJobFallbackWindow = d }
}

// WithPageSize overrides the List page size (default defaultPageSize).
// Tests use a small size to exercise continue-token pagination.
func WithPageSize(n int64) Option {
	return func(r *Resolver) { r.pageSize = n }
}

// New builds a Resolver backed by client.
func New(client dynamic.Interface, opts ...Option) *Resolver {
	r := &Resolver{
		client:                client,
		clock:                 clock.RealClock{},
		cronJobFallbackWindow: DefaultCronJobFallbackWindow,
		pageSize:              defaultPageSize,
	}
	for _, opt := range opts {
		opt(r)
	}

	return r
}

// Resolve resolves target. A named target (Target.Name set) returns exactly
// one Resolution or a *NotFoundError. A wildcard target returns every
// matching object's Resolution sorted by namespace/name/UID, or an empty
// slice if none match — wildcards never 404 (metric-gateway.md §3.6).
func (r *Resolver) Resolve(ctx context.Context, target Target) ([]Resolution, error) {
	spec, ok := kindSpecs[target.Kind]
	if !ok {
		return nil, fmt.Errorf("resolver: unsupported kind %q", target.Kind)
	}

	namespace := target.Namespace
	if !spec.namespaced {
		namespace = ""
	}

	if target.Name != "" {
		resolution, err := r.resolveNamed(ctx, target.Kind, spec, namespace, target.Name)
		if err != nil {
			return nil, err
		}

		return []Resolution{resolution}, nil
	}

	return r.resolveWildcard(ctx, target.Kind, spec, namespace, target.ObjectSelector)
}

func (r *Resolver) resolveNamed(ctx context.Context, kind Kind, spec kindSpec, namespace, name string) (Resolution, error) {
	switch kind {
	case KindPod, KindNode:
		obj, err := r.getObject(ctx, spec, kind, namespace, name)
		if err != nil {
			return Resolution{}, err
		}
		ref := objectRefFrom(obj)

		podNames := []string(nil)
		if kind == KindPod {
			podNames = []string{ref.Name}
		}

		return Resolution{Object: ref, PodNames: podNames}, nil

	case KindCronJob:
		return r.resolveNamedCronJob(ctx, namespace, name)

	default: // Deployment, StatefulSet, DaemonSet, Job
		obj, err := r.getObject(ctx, spec, kind, namespace, name)
		if err != nil {
			return Resolution{}, err
		}
		ref := objectRefFrom(obj)

		selector, err := extractSelector(obj)
		if err != nil {
			return Resolution{}, fmt.Errorf("resolver: %s %s: %w", kind, namespacedName(namespace, name), err)
		}

		podNames, err := r.unionPodNames(ctx, namespace, []labels.Selector{selector})
		if err != nil {
			return Resolution{}, err
		}

		return Resolution{Object: ref, PodNames: podNames}, nil
	}
}

func (r *Resolver) resolveWildcard(ctx context.Context, kind Kind, spec kindSpec, namespace string, objectSelector labels.Selector) ([]Resolution, error) {
	objs, err := r.listObjects(ctx, spec, kind, namespace, objectSelector)
	if err != nil {
		return nil, err
	}

	resolutions := make([]Resolution, 0, len(objs))

	for i := range objs {
		obj := &objs[i]
		ref := objectRefFrom(obj)

		switch kind {
		case KindPod:
			resolutions = append(resolutions, Resolution{Object: ref, PodNames: []string{ref.Name}})

		case KindNode:
			resolutions = append(resolutions, Resolution{Object: ref})

		case KindCronJob:
			podNames, usedFallback, err := r.resolveCronJobPodNames(ctx, obj)
			if err != nil {
				if _, ok := errors.AsType[*NotFoundError](err); ok { //nolint:errcheck
					// Wildcards omit objects with no eligible retained
					// members rather than failing the whole list
					// (metric-gateway.md §3.6).
					continue
				}

				return nil, err
			}
			resolutions = append(resolutions, Resolution{Object: ref, PodNames: podNames, CronJobFallback: usedFallback})

		default: // Deployment, StatefulSet, DaemonSet, Job
			selector, err := extractSelector(obj)
			if err != nil {
				return nil, fmt.Errorf("resolver: %s %s: %w", kind, namespacedName(ref.Namespace, ref.Name), err)
			}
			podNames, err := r.unionPodNames(ctx, ref.Namespace, []labels.Selector{selector})
			if err != nil {
				return nil, err
			}
			resolutions = append(resolutions, Resolution{Object: ref, PodNames: podNames})
		}
	}

	sort.Slice(resolutions, func(i, j int) bool {
		a, b := resolutions[i].Object, resolutions[j].Object
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}

		return a.UID < b.UID
	})

	return resolutions, nil
}
