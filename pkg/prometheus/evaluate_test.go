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

package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	clocktesting "k8s.io/utils/clock/testing"
)

// queryKind is which of Evaluate's batch queries a backend request is.
type queryKind string

const (
	kindUsage     queryKind = "usage"
	kindAnyActive queryKind = "any-active"
	kindCoverage  queryKind = "coverage"
	kindFreshness queryKind = "freshness"
)

// kindOf classifies a rendered batch query by the shape only its kind has,
// so the fake backend answers by meaning rather than by exact PromQL text.
func kindOf(query string) queryKind {
	switch {
	case strings.Contains(query, "timestamp("):
		return kindFreshness
	case strings.Contains(query, " unless on "):
		return kindCoverage
	case strings.HasPrefix(query, "label_replace(max_over_time(count("):
		return kindAnyActive
	default:
		return kindUsage
	}
}

// mockResult describes how the fake backend answers one query kind.
type mockResult struct {
	// values[i] is target i's sample value; "" means no series.
	values     []string
	duplicate  bool // also emit a second series for target 0
	warnings   []string
	errorType  string
	errorMsg   string
	httpStatus int
	sleep      time.Duration
}

// fakeBackend is a Prometheus HTTP API stand-in that answers each query
// kind from a per-target table and records every query it receives.
type fakeBackend struct {
	server *httptest.Server

	mu      sync.Mutex
	queries []string
}

func (f *fakeBackend) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.queries...)
}

func newFakeBackend(t *testing.T, byKind map[queryKind]mockResult) *fakeBackend {
	t.Helper()

	f := &fakeBackend{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		query := r.FormValue("query")

		f.mu.Lock()
		f.queries = append(f.queries, query)
		f.mu.Unlock()

		res, ok := byKind[kindOf(query)]
		if !ok {
			t.Errorf("fake backend: unexpected %s query: %s", kindOf(query), query)
			http.Error(w, "unexpected query", http.StatusInternalServerError)

			return
		}
		if res.sleep > 0 {
			time.Sleep(res.sleep)
		}

		w.Header().Set("Content-Type", "application/json")
		if res.httpStatus != 0 && res.httpStatus != http.StatusOK {
			w.WriteHeader(res.httpStatus)
		}

		body := map[string]any{}
		if res.errorType != "" {
			body["status"] = "error"
			body["errorType"] = res.errorType
			body["error"] = res.errorMsg
		} else {
			result := make([]map[string]any, 0, len(res.values))
			emit := func(target int, v string) {
				result = append(result, map[string]any{
					"metric": map[string]string{targetLabel: strconv.Itoa(target)},
					"value":  []any{1_700_000_000.0, v},
				})
			}
			for i, v := range res.values {
				if v != "" {
					emit(i, v)
				}
			}
			if res.duplicate {
				emit(0, "1")
			}
			body["status"] = "success"
			if len(res.warnings) > 0 {
				body["warnings"] = res.warnings
			}
			body["data"] = map[string]any{"resultType": "vector", "result": result}
		}

		_ = json.NewEncoder(w).Encode(body)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	return f
}

// healthy answers every validation query for n targets as active, fully
// covered and fresh, with the given usage values.
func healthy(usage ...string) map[queryKind]mockResult {
	ones := make([]string, len(usage))
	fresh := make([]string, len(usage))
	for i := range usage {
		ones[i], fresh[i] = "1", "2"
	}

	return map[queryKind]mockResult{
		kindUsage:     {values: usage},
		kindAnyActive: {values: ones},
		kindCoverage:  {},
		kindFreshness: {values: fresh},
	}
}

var testNow = time.Date(2026, 1, 1, 12, 0, 47, 0, time.UTC)

func newEvalClient(t *testing.T, url string, cfg ClientConfig) *Client {
	t.Helper()

	cfg.URL = url
	cfg.Cluster = "example"
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = clocktesting.NewFakePassiveClock(testNow)
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	return client
}

var (
	podCPU = catalog.Base{
		Name: "cpu", Series: "pod_cpu_usage_cores", Unit: catalog.UnitCores,
		Scope: catalog.ScopePod, Aggregation: catalog.AggregationSumThenStat,
	}
	nodeCPU = catalog.Base{
		Name: "node_cpu", Series: "node_cpu_usage_cores", Unit: catalog.UnitCores,
		Scope: catalog.ScopeNode, Aggregation: catalog.AggregationSumThenStat,
	}
)

func podComputation(targets ...Target) Computation {
	return Computation{
		Base:      podCPU,
		Stat:      catalog.StatAvg,
		Window:    time.Hour,
		Namespace: "prod",
		Targets:   targets,
	}
}

func rawComputation(targets ...Target) Computation {
	comp := podComputation(targets...)
	comp.Base.Aggregation = catalog.AggregationRaw

	return comp
}

var web = Target{Name: "web", Pods: []string{"web-0", "web-1"}}

func TestEvaluate_Success(t *testing.T) {
	backend := newFakeBackend(t, healthy("1.5"))

	eval, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), podComputation(web))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(eval.Samples) != 1 || eval.Samples[0] != (Sample{Value: 1.5, Present: true}) {
		t.Errorf("Samples = %+v, want [{1.5 true}]", eval.Samples)
	}
}

// TestEvaluate_TimeAlignsToSixtySecondGrid pins metric-gateway.md §3.2:
// the evaluation time is the last fully completed 60s grid step.
func TestEvaluate_TimeAlignsToSixtySecondGrid(t *testing.T) {
	backend := newFakeBackend(t, healthy("1.5"))

	eval, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), podComputation(web))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	want := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if !eval.Time.Equal(want) {
		t.Errorf("Time = %s, want %s", eval.Time, want)
	}
}

func TestEvaluate_AbsentWhenNeverActive(t *testing.T) {
	backend := newFakeBackend(t, map[queryKind]mockResult{
		kindUsage:     {values: []string{"0"}},
		kindAnyActive: {},
		kindCoverage:  {},
		kindFreshness: {},
	})

	eval, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), podComputation(web))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if eval.Samples[0].Present {
		t.Errorf("Samples[0] = %+v, want absent", eval.Samples[0])
	}
}

// TestEvaluate_ManyTargetsShareFourQueries pins the §6.3 budget: a
// wildcard computation issues four backend queries in total, not four per
// target, and each target is classified independently.
func TestEvaluate_ManyTargetsShareFourQueries(t *testing.T) {
	backend := newFakeBackend(t, map[queryKind]mockResult{
		kindUsage:     {values: []string{"", "2", "3"}},
		kindAnyActive: {values: []string{"", "1", "1"}}, // target 0 never active
		kindCoverage:  {},
		kindFreshness: {values: []string{"", "2", "2"}},
	})

	comp := podComputation(
		Target{Name: "a", Pods: []string{"a-0"}},
		Target{Name: "b", Pods: []string{"b-0"}},
		Target{Name: "c", Pods: []string{"c-0", "c-1"}},
	)

	eval, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), comp)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	want := []Sample{{}, {Value: 2, Present: true}, {Value: 3, Present: true}}
	for i, w := range want {
		if eval.Samples[i] != w {
			t.Errorf("Samples[%d] = %+v, want %+v", i, eval.Samples[i], w)
		}
	}
	if got := len(backend.received()); got != 4 {
		t.Errorf("backend received %d queries, want 4", got)
	}
}

func TestEvaluate_TargetsWithoutIdentitiesAreAbsentAndNeverQueried(t *testing.T) {
	// Only the target with identities reaches the batch, at position 0.
	backend := newFakeBackend(t, healthy("5"))

	comp := podComputation(Target{Name: "idle"}, Target{Name: "busy", Pods: []string{"busy-0"}})

	eval, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), comp)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if eval.Samples[0].Present || eval.Samples[1] != (Sample{Value: 5, Present: true}) {
		t.Errorf("Samples = %+v, want [absent, {5 true}]", eval.Samples)
	}
	for _, q := range backend.received() {
		if strings.Contains(q, "idle") {
			t.Errorf("query mentions the identity-less target: %s", q)
		}
	}

	// With no identities at all, nothing reaches the backend.
	idle, err := newEvalClient(t, "http://127.0.0.1:1", ClientConfig{}).Evaluate(context.Background(), podComputation(Target{Name: "idle"}))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if idle.Samples[0].Present {
		t.Errorf("Samples[0] = %+v, want absent", idle.Samples[0])
	}
}

func TestEvaluate_NodeScopeMatchesNodeName(t *testing.T) {
	backend := newFakeBackend(t, healthy("4"))

	comp := Computation{Base: nodeCPU, Stat: catalog.StatMax, Window: time.Hour, Targets: []Target{{Name: "worker-1"}}}

	eval, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), comp)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if eval.Samples[0] != (Sample{Value: 4, Present: true}) {
		t.Errorf("Samples[0] = %+v", eval.Samples[0])
	}
	for _, q := range backend.received() {
		if !strings.Contains(q, `node=~"worker-1"`) || strings.Contains(q, "namespace=") {
			t.Errorf("node query = %s, want a node=~\"worker-1\" matcher and no namespace", q)
		}
	}
}

func TestEvaluate_RawIssuesOnlyOneQuery(t *testing.T) {
	backend := newFakeBackend(t, map[queryKind]mockResult{kindUsage: {values: []string{"1.5", ""}}})

	comp := rawComputation(web, Target{Name: "api", Pods: []string{"api-0"}})

	eval, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), comp)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if eval.Samples[0] != (Sample{Value: 1.5, Present: true}) || eval.Samples[1].Present {
		t.Errorf("Samples = %+v, want [{1.5 true}, absent]", eval.Samples)
	}
	if got := len(backend.received()); got != 1 {
		t.Errorf("backend received %d queries, want 1", got)
	}
}

func TestEvaluate_RawNegativeUsageRejected(t *testing.T) {
	backend := newFakeBackend(t, map[queryKind]mockResult{kindUsage: {values: []string{"-1"}}})

	_, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), rawComputation(web))
	if !errors.Is(err, ErrBackend) {
		t.Errorf("err = %v, want ErrBackend", err)
	}
}

func TestEvaluate_ValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		backend map[queryKind]mockResult
		want    error
	}{
		{
			name: "incomplete coverage",
			backend: func() map[queryKind]mockResult {
				m := healthy("1.5")
				m[kindCoverage] = mockResult{values: []string{"1"}}

				return m
			}(),
			want: ErrIncompleteCoverage,
		},
		{
			name: "active without usage sample",
			backend: func() map[queryKind]mockResult {
				m := healthy("1.5")
				m[kindUsage] = mockResult{}

				return m
			}(),
			want: ErrIncompleteCoverage,
		},
		{
			name: "stale data",
			backend: func() map[queryKind]mockResult {
				m := healthy("1.5")
				m[kindFreshness] = mockResult{values: []string{"31"}}

				return m
			}(),
			want: ErrStaleData,
		},
		{
			name:    "negative usage",
			backend: healthy("-1"),
			want:    ErrBackend,
		},
		{
			name: "duplicate series for one target",
			backend: func() map[queryKind]mockResult {
				m := healthy("1.5")
				m[kindUsage] = mockResult{values: []string{"1.5"}, duplicate: true}

				return m
			}(),
			want: ErrBackend,
		},
		{
			name: "warnings",
			backend: func() map[queryKind]mockResult {
				m := healthy("1.5")
				m[kindUsage] = mockResult{values: []string{"1.5"}, warnings: []string{"query processing would load too many samples"}}

				return m
			}(),
			want: ErrBackend,
		},
		{
			name: "protocol error",
			backend: func() map[queryKind]mockResult {
				m := healthy("1.5")
				m[kindUsage] = mockResult{errorType: "bad_data", errorMsg: "invalid query", httpStatus: http.StatusUnprocessableEntity}

				return m
			}(),
			want: ErrBackend,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := newFakeBackend(t, tc.backend)

			_, err := newEvalClient(t, backend.server.URL, ClientConfig{}).Evaluate(context.Background(), podComputation(web))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEvaluate_QueryTooLarge(t *testing.T) {
	pods := make([]string, 0, 60_000)
	for i := range 60_000 {
		pods = append(pods, "pod-with-a-long-generated-name-"+strconv.Itoa(i))
	}

	_, err := newEvalClient(t, "http://127.0.0.1:1", ClientConfig{}).Evaluate(context.Background(), podComputation(Target{Name: "big", Pods: pods}))
	if !errors.Is(err, ErrQueryTooLarge) {
		t.Errorf("err = %v, want ErrQueryTooLarge", err)
	}
}

func TestEvaluate_TimeoutIsDistinguishableFromOtherBackendErrors(t *testing.T) {
	m := healthy("1.5")
	m[kindUsage] = mockResult{values: []string{"1.5"}, sleep: 200 * time.Millisecond}
	backend := newFakeBackend(t, m)

	client := newEvalClient(t, backend.server.URL, ClientConfig{Timeout: 20 * time.Millisecond})

	_, err := client.Evaluate(context.Background(), podComputation(web))
	if !errors.Is(err, ErrBackend) {
		t.Errorf("err = %v, want it to also match ErrBackend", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to also match context.DeadlineExceeded (so callers can map it to 504)", err)
	}
}

// TestEvaluate_TimeoutExcludesQueueWait checks that the per-query timeout
// starts once a query holds a concurrency slot: with one slot, the fourth
// query waits ~3 × 60ms before it is sent, well past the 100ms per-query
// timeout, yet each query itself finishes within it.
func TestEvaluate_TimeoutExcludesQueueWait(t *testing.T) {
	const sleep = 60 * time.Millisecond

	m := healthy("1.5")
	for kind, res := range m {
		res.sleep = sleep
		m[kind] = res
	}
	backend := newFakeBackend(t, m)

	client := newEvalClient(t, backend.server.URL, ClientConfig{Timeout: 100 * time.Millisecond, MaxConcurrentQueries: 1})

	eval, err := client.Evaluate(context.Background(), podComputation(web))
	if err != nil || !eval.Samples[0].Present {
		t.Fatalf("Evaluate = (%+v, %v), want a value: queue wait must not count against the per-query timeout", eval, err)
	}
}

func TestEvaluate_MaxConcurrentQueriesBoundsInFlightRequests(t *testing.T) {
	var inFlight, maxObserved atomic.Int32
	release := make(chan struct{})

	m := healthy("1.5")
	inner := newFakeBackend(t, m)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := maxObserved.Load()
			if n <= old || maxObserved.CompareAndSwap(old, n) {
				break
			}
		}
		<-release

		inner.server.Config.Handler.ServeHTTP(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := newEvalClient(t, server.URL, ClientConfig{Timeout: 5 * time.Second, MaxConcurrentQueries: 2})

	// Each Evaluate issues 4 queries; run two concurrently (8 total)
	// against a limit of 2.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			_, _ = client.Evaluate(context.Background(), podComputation(web))
		})
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := maxObserved.Load(); got > 2 {
		t.Errorf("observed %d concurrent backend queries, want <= 2 (MaxConcurrentQueries)", got)
	}
}
