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
	"context"
	"errors"
	"fmt"

	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/resolver"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/provider"
)

// This file is the gateway's one error seam: Service passes every internal
// failure through untouched, and classify maps it once — at the end of
// Service.Get — to both its bounded telemetry reason and the exact
// Kubernetes Status/HTTP code from metric-gateway.md §3.6. 401/403 are
// handled upstream by AdapterBase's delegated authentication/authorization
// (T1) and never reach this code.

// metricSelectorError rejects a nonempty metricLabelSelector, unsupported
// in v1 (metric-gateway.md §3.6).
type metricSelectorError struct{}

func (metricSelectorError) Error() string {
	return "metricLabelSelector is not supported in v1"
}

// metricNotSupportedError reports an unknown metric name or an unsupported
// metric/resource combination (metric-gateway.md §2, §3.6).
type metricNotSupportedError struct {
	req Request
}

func (e *metricNotSupportedError) Error() string {
	return fmt.Sprintf("metric %s is not supported for %s", e.req.Metric, e.req.GroupResource)
}

// notEligibleError reports a named object with no eligible retained
// members, or verified inactivity throughout the window
// (metric-gateway.md §3.6).
type notEligibleError struct {
	req Request
}

func (e *notEligibleError) Error() string {
	return fmt.Sprintf("no eligible members for %s %s/%s", e.req.GroupResource, e.req.Namespace, e.req.Name)
}

// classify maps err to its gateway_query_errors_total{reason} label (a
// fixed word, never text from the caller) and to the Status error returned
// to the caller.
//
// Order matters: a backend timeout wraps both prometheus.ErrBackend and
// context.DeadlineExceeded, and must surface as 504, not 503.
func classify(err error) (string, error) {
	if notFound, ok := errors.AsType[*resolver.NotFoundError](err); ok {
		return "not-found", resolverNotFoundStatus(notFound)
	}
	if e, ok := errors.AsType[*metricNotSupportedError](err); ok {
		return "metric-not-supported", provider.NewMetricNotFoundError(e.req.GroupResource, e.req.Metric)
	}
	if e, ok := errors.AsType[*notEligibleError](err); ok {
		return "not-eligible", provider.NewMetricNotFoundForError(e.req.GroupResource, e.req.Metric, e.req.Name)
	}

	switch {
	case isType[metricSelectorError](err):
		return "bad-request", apierrors.NewBadRequest(err.Error())
	case isType[*resolver.ForbiddenError](err):
		// The gateway's own ServiceAccount has no permission: this is 503,
		// different from a 403 for a caller who may not read the metric.
		return "service-account-forbidden", apierrors.NewServiceUnavailable(err.Error())
	case isType[*cache.AdmissionRejectedError](err):
		return "admission-rejected", apierrors.NewTooManyRequests(err.Error(), int(cache.RetryAfter.Seconds()))
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline-exceeded", apierrors.NewTimeoutError(err.Error(), 1)
	case errors.Is(err, context.Canceled):
		return "canceled", apierrors.NewServiceUnavailable(err.Error())
	case errors.Is(err, prometheus.ErrQueryTooLarge):
		return "query-too-large", apierrors.NewRequestEntityTooLargeError(err.Error())
	case errors.Is(err, prometheus.ErrIncompleteCoverage):
		return "incomplete-coverage", apierrors.NewServiceUnavailable(err.Error())
	case errors.Is(err, prometheus.ErrStaleData):
		return "stale-data", apierrors.NewServiceUnavailable(err.Error())
	case errors.Is(err, prometheus.ErrBackend):
		return "backend", apierrors.NewServiceUnavailable(err.Error())
	default:
		// Any other resolver or backend failure is still a service
		// configuration/availability problem, never the caller's fault.
		return "internal", apierrors.NewServiceUnavailable(err.Error())
	}
}

func isType[E error](err error) bool {
	_, ok := errors.AsType[E](err)

	return ok
}

// resolverNotFoundStatus renders a resolver not-found using the exact
// message resolver already computed (e.g. a real Kubernetes NotFound
// message), which is more specific than any fixed provider template —
// still shaped exactly like provider.NewMetricNotFoundForError.
// metric-gateway.md §3.4 mandates the CronJob message verbatim, which the
// provider package's constructors cannot produce, so this is the one place
// this package hand-builds a Status.
func resolverNotFoundStatus(notFound *resolver.NotFoundError) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    404,
		Reason:  metav1.StatusReasonNotFound,
		Message: notFound.Error(),
	}}
}
