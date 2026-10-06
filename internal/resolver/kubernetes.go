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

package resolver

import (
	"context"
	"fmt"
	"slices"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

// resourceInterfaceFor returns the dynamic client scoped to kind's resource,
// namespaced when namespace is non-empty.
func (r *Resolver) resourceInterfaceFor(kind resource.Kind, namespace string) dynamic.ResourceInterface {
	if kind.Namespaced() && namespace != "" {
		return r.client.Resource(kind.GVR()).Namespace(namespace)
	}

	return r.client.Resource(kind.GVR())
}

// getObject fetches one named object, classifying NotFound/Forbidden errors
// so callers get a resolver-typed error rather than a raw client-go one.
func (r *Resolver) getObject(ctx context.Context, kind resource.Kind, namespace, name string) (*unstructured.Unstructured, error) {
	obj, err := r.resourceInterfaceFor(kind, namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, classifyError(kind, namespace, name, err)
	}

	return obj, nil
}

// listObjects lists every object of kind matching selector, following
// continue tokens until exhausted (metric-gateway.md §6.3: bounded,
// paginated lists).
func (r *Resolver) listObjects(ctx context.Context, kind resource.Kind, namespace string, selector labels.Selector) ([]unstructured.Unstructured, error) {
	ri := r.resourceInterfaceFor(kind, namespace)

	opts := metav1.ListOptions{Limit: r.pageSize}
	if selector != nil && !selector.Empty() {
		opts.LabelSelector = selector.String()
	}

	var items []unstructured.Unstructured
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		list, err := ri.List(ctx, opts)
		if err != nil {
			return nil, classifyError(kind, namespace, "", err)
		}
		items = append(items, list.Items...)

		cont := list.GetContinue()
		if cont == "" {
			break
		}
		opts.Continue = cont
	}

	return items, nil
}

// classifyError turns a raw client-go error into a resolver error: a missing
// object becomes *NotFoundError (404 for the caller); missing RBAC for the
// gateway ServiceAccount becomes *ForbiddenError (503 for the caller, never
// 403 — metric-gateway.md §3.3).
func classifyError(kind resource.Kind, namespace, name string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return &NotFoundError{Kind: kind, Namespace: namespace, Name: name, Message: err.Error()}
	case apierrors.IsForbidden(err):
		return &ForbiddenError{Kind: kind, Namespace: namespace, Name: name, Err: err}
	default:
		return fmt.Errorf("resolver: %s %s: %w", kind, namespacedName(namespace, name), err)
	}
}

// objectRefFrom extracts identity fields common to every Kubernetes object.
func objectRefFrom(obj *unstructured.Unstructured) ObjectRef {
	return ObjectRef{
		Namespace:         obj.GetNamespace(),
		Name:              obj.GetName(),
		UID:               obj.GetUID(),
		CreationTimestamp: obj.GetCreationTimestamp().Time,
	}
}

// extractSelector decodes obj's spec.selector into a labels.Selector,
// honoring matchExpressions as well as matchLabels
// (metav1.LabelSelectorAsSelector).
func extractSelector(obj *unstructured.Unstructured) (labels.Selector, error) {
	raw, found, err := unstructured.NestedMap(obj.Object, "spec", "selector")
	if err != nil {
		return nil, fmt.Errorf("reading spec.selector: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("object has no spec.selector")
	}

	var selector metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &selector); err != nil {
		return nil, fmt.Errorf("decoding spec.selector: %w", err)
	}

	sel, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return nil, fmt.Errorf("invalid spec.selector: %w", err)
	}

	return sel, nil
}

// unionPodNames lists Pods in namespace matching each selector and returns
// the deduplicated union of their names, sorted for determinism. Selectors
// evaluated by Kubernetes when listing Pods, never inserted as arbitrary
// labels into backend queries (metric-gateway.md §3.3). Pod names are
// unique within a namespace at any point in time, so deduplicating by name
// here matches how the rendered PromQL later matches on the "pod" label.
func (r *Resolver) unionPodNames(ctx context.Context, namespace string, selectors []labels.Selector) ([]string, error) {
	seen := make(map[string]struct{})
	var names []string

	for _, selector := range selectors {
		pods, err := r.listObjects(ctx, resource.Pod, namespace, selector)
		if err != nil {
			return nil, err
		}

		for i := range pods {
			name := pods[i].GetName()
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}

	slices.Sort(names)

	return names, nil
}
