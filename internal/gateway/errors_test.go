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
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/resolver"
	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/telemetry"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/prometheus"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
)

// TestService_Get_ClassifiesEveryFailureOnce checks, through Service.Get,
// that each failure source yields both the specified Status code
// (metric-gateway.md §3.6) and its own gateway_query_errors_total reason,
// rather than collapsing to "internal".
func TestService_Get_ClassifiesEveryFailureOnce(t *testing.T) {
	ok := []resolver.Resolution{podResolution("web-0", "uid-web-0")}

	tests := []struct {
		name       string
		req        func() Request
		resolver   *fakeResolver
		evaluator  *fakeEvaluator
		inflight   int
		wantCode   int32
		wantReason string
	}{
		{
			name: "metric selector",
			req: func() Request {
				req := podRequest("web-0")
				req.MetricSelector = labels.SelectorFromSet(labels.Set{"app": "web"})

				return req
			},
			wantCode:   400,
			wantReason: "bad-request",
		},
		{
			name: "unknown metric",
			req: func() Request {
				req := podRequest("web-0")
				req.Metric = "does_not_exist_avg_5m"

				return req
			},
			wantCode:   404,
			wantReason: "metric-not-supported",
		},
		{
			name:       "object not found",
			resolver:   &fakeResolver{err: &resolver.NotFoundError{Kind: resolver.KindPod, Namespace: "prod", Name: "web-0", Message: "gone"}},
			wantCode:   404,
			wantReason: "not-found",
		},
		{
			name:       "no eligible members",
			evaluator:  &fakeEvaluator{},
			wantCode:   404,
			wantReason: "not-eligible",
		},
		{
			name:       "service account forbidden",
			resolver:   &fakeResolver{err: &resolver.ForbiddenError{Kind: resolver.KindPod, Err: errors.New("RBAC denied")}},
			wantCode:   503,
			wantReason: "service-account-forbidden",
		},
		{
			name:       "admission rejected",
			inflight:   -1,
			wantCode:   429,
			wantReason: "admission-rejected",
		},
		{
			name:       "query too large",
			evaluator:  &fakeEvaluator{err: prometheus.ErrQueryTooLarge},
			wantCode:   413,
			wantReason: "query-too-large",
		},
		{
			name:       "incomplete coverage",
			evaluator:  &fakeEvaluator{err: prometheus.ErrIncompleteCoverage},
			wantCode:   503,
			wantReason: "incomplete-coverage",
		},
		{
			name:       "stale data",
			evaluator:  &fakeEvaluator{err: prometheus.ErrStaleData},
			wantCode:   503,
			wantReason: "stale-data",
		},
		{
			name:       "backend timeout",
			evaluator:  &fakeEvaluator{err: fmt.Errorf("%w: %w", prometheus.ErrBackend, context.DeadlineExceeded)},
			wantCode:   504,
			wantReason: "deadline-exceeded",
		},
		{
			name:       "backend failure",
			evaluator:  &fakeEvaluator{err: fmt.Errorf("%w: connection refused", prometheus.ErrBackend)},
			wantCode:   503,
			wantReason: "backend",
		},
		{
			name:       "unrecognized failure",
			evaluator:  &fakeEvaluator{err: errors.New("boom")},
			wantCode:   503,
			wantReason: "internal",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.resolver
			if res == nil {
				res = &fakeResolver{resolutions: ok}
			}
			ev := tc.evaluator
			if ev == nil {
				ev = &fakeEvaluator{sample: present(1)}
			}
			inflight := 128
			if tc.inflight < 0 {
				inflight = 0
			}
			req := podRequest("web-0")
			if tc.req != nil {
				req = tc.req()
			}

			m := telemetry.New()
			metrics.NewKubeRegistry().MustRegister(m.QueryErrors)

			svc := NewService(Deps{
				Catalog:       testCatalog(t),
				Resolver:      res,
				Evaluator:     ev,
				Cache:         cache.New(100, 1<<20, sizeOfResult),
				Flights:       cache.NewGroup[Result](context.Background(), time.Minute),
				Inflight:      cache.NewLimiter("inflight-requests", inflight),
				Computations:  cache.NewLimiter("shared-computations", 32),
				CacheTTLShort: 15 * time.Second,
				CacheTTLLong:  10 * time.Minute,
				Metrics:       m,
			})

			_, err := svc.Get(context.Background(), req)
			if got := statusCode(t, err); got != tc.wantCode {
				t.Errorf("code = %d, want %d", got, tc.wantCode)
			}

			count, err := testutil.GetCounterMetricValue(m.QueryErrors.WithLabelValues(tc.wantReason))
			if err != nil {
				t.Fatalf("reading counter: %v", err)
			}
			if count != 1 {
				t.Errorf("gateway_query_errors_total{reason=%q} = %v, want 1", tc.wantReason, count)
			}
		})
	}
}
