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

	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/resolver"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/provider"
)

// This file maps every internal failure this package owns to the exact
// Kubernetes Status/HTTP code from metric-gateway.md §3.6. 401/403 are
// handled upstream by AdapterBase's delegated authentication/authorization
// (T1) and never reach this code.

// errorReason classifies err for the bounded-cardinality
// gateway_query_errors_total{reason} metric (design.md §6 step 10: never
// label with unbounded caller-supplied content).
func errorReason(err error) string {
	switch {
	case isType[*resolver.NotFoundError](err):
		return "not-found"
	case isType[*resolver.ForbiddenError](err):
		return "service-account-forbidden"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline-exceeded"
	case errors.Is(err, prometheus.ErrQueryTooLarge):
		return "query-too-large"
	case errors.Is(err, prometheus.ErrIncompleteCoverage):
		return "incomplete-coverage"
	case errors.Is(err, prometheus.ErrStaleData):
		return "stale-data"
	case errors.Is(err, prometheus.ErrBackend):
		return "backend"
	case isType[*cache.AdmissionRejectedError](err):
		return "admission-rejected"
	default:
		return "internal"
	}
}

func isType[E error](err error) bool {
	_, ok := errors.AsType[E](err)

	return ok
}

// mapResolverError maps an internal/resolver error to the caller's status
// code (design.md §7: ForbiddenError — the gateway's own ServiceAccount
// lacking permission — is a 503, a distinct failure class from a caller's
// delegated-authorization 403).
func mapResolverError(err error) error {
	if notFound, ok := errors.AsType[*resolver.NotFoundError](err); ok {
		if notFound.Kind == resolver.KindCronJob {
			// metric-gateway.md §3.4 mandates this exact message; the
			// provider package's constructors cannot produce it, so this
			// is the one place this package hand-builds a Status.
			return &apierrors.StatusError{ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    404,
				Reason:  metav1.StatusReasonNotFound,
				Message: notFound.Error(),
			}}
		}

		return notFoundError(notFound)
	}

	if forbidden, ok := errors.AsType[*resolver.ForbiddenError](err); ok {
		return apierrors.NewServiceUnavailable(forbidden.Error())
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return apierrors.NewTimeoutError(err.Error(), 1)
	}

	return apierrors.NewServiceUnavailable(err.Error())
}

// notFoundError renders a generic (non-CronJob) resolver not-found using
// the exact message resolver already computed (e.g. a real Kubernetes
// NotFound message), which is more specific than any fixed provider
// template — still shaped exactly like provider.NewMetricNotFoundForError.
func notFoundError(notFound *resolver.NotFoundError) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    404,
		Reason:  metav1.StatusReasonNotFound,
		Message: notFound.Error(),
	}}
}

// mapQuerierError maps a pkg/prometheus error to the caller's status
// code (metric-gateway.md §3.6).
func mapQuerierError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return apierrors.NewTimeoutError(err.Error(), 1)
	case errors.Is(err, prometheus.ErrQueryTooLarge):
		return apierrors.NewRequestEntityTooLargeError(err.Error())
	default:
		// ErrIncompleteCoverage, ErrStaleData, ErrBackend, ErrEmptySelection,
		// and anything else this package does not specifically recognize
		// are all backend/protocol failures — 503.
		return apierrors.NewServiceUnavailable(err.Error())
	}
}

// mapAdmissionError maps a *cache.AdmissionRejectedError to 429 with the
// spec's fixed Retry-After (metric-gateway.md §6.3).
func mapAdmissionError(err error) error {
	if _, ok := errors.AsType[*cache.AdmissionRejectedError](err); ok { //nolint:errcheck
		return apierrors.NewTooManyRequests(err.Error(), int(cache.RetryAfter.Seconds()))
	}

	return apierrors.NewServiceUnavailable(err.Error())
}

// metricNotSupportedError reports an unknown or unsupported metric/resource
// combination (metric-gateway.md §2, §3.6) using the recommended provider
// constructor.
func metricNotSupportedError(req Request) error {
	return provider.NewMetricNotFoundError(req.GroupResource, req.Metric)
}

// namedObjectNotEligibleError reports a named object with no eligible
// retained members or verified inactivity throughout the window, or a
// named object that itself does not exist and was not already turned into
// a *resolver.NotFoundError (metric-gateway.md §3.6).
func namedObjectNotEligibleError(req Request) error {
	return provider.NewMetricNotFoundForError(req.GroupResource, req.Metric, req.Name)
}
