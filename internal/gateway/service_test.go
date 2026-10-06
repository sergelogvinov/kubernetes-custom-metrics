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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/resolver"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const testCatalogYAML = `
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: pod
    aggregation: sum-then-stat
  memory:
    series: pod_memory_working_set_bytes
    unit: bytes
    scope: pod
    aggregation: stat-then-sum
  node_cpu:
    series: node_cpu_usage_cores
    unit: cores
    scope: node
    aggregation: sum-then-stat
  node_memory:
    series: node_memory_used_bytes
    unit: bytes
    scope: node
    aggregation: stat-then-sum
`

func testCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()

	return testCatalogWithDiscovery(t, catalog.DiscoveryFull)
}

func testCatalogWithDiscovery(t *testing.T, mode catalog.DiscoveryMode) *catalog.Catalog {
	t.Helper()

	cat, err := catalog.Load([]byte(testCatalogYAML), mode, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}

	return cat
}

// fakeResolver is a lightweight stand-in for internal/resolver.Resolver.
type fakeResolver struct {
	mu          sync.Mutex
	resolutions []resolver.Resolution
	err         error
	calls       atomic.Int32
	lastTarget  resolver.Target
}

func (f *fakeResolver) Resolve(_ context.Context, target resolver.Target) ([]resolver.Resolution, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastTarget = target
	f.mu.Unlock()

	return f.resolutions, f.err
}

// evalTime is the evaluation time fakeEvaluator reports.
var evalTime = time.Unix(1_700_000_000, 0)

// fakeEvaluator is a lightweight stand-in for *prometheus.Client. Each
// target gets sample, or fn(target) when fn is set.
type fakeEvaluator struct {
	mu       sync.Mutex
	sample   prometheus.Sample
	fn       func(prometheus.Target) prometheus.Sample
	err      error
	calls    atomic.Int32
	lastComp prometheus.Computation
}

func (f *fakeEvaluator) Evaluate(_ context.Context, comp prometheus.Computation) (prometheus.Evaluation, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastComp = comp
	f.mu.Unlock()

	if f.err != nil {
		return prometheus.Evaluation{}, f.err
	}

	samples := make([]prometheus.Sample, len(comp.Targets))
	for i, target := range comp.Targets {
		if f.fn != nil {
			samples[i] = f.fn(target)
		} else {
			samples[i] = f.sample
		}
	}

	return prometheus.Evaluation{Time: evalTime, Samples: samples}, nil
}

func present(v float64) prometheus.Sample {
	return prometheus.Sample{Value: v, Present: true}
}

func newTestService(t *testing.T, cat *catalog.Catalog, res Resolver, ev Evaluator) *Service {
	t.Helper()

	return NewService(Deps{
		Catalog:       cat,
		Resolver:      res,
		Evaluator:     ev,
		Cache:         cache.New(100, 1<<20, sizeOfResult),
		Flights:       cache.NewGroup[Result](context.Background(), time.Minute),
		Inflight:      cache.NewLimiter("inflight-requests", 128),
		Computations:  cache.NewLimiter("shared-computations", 32),
		CacheTTLShort: 15 * time.Second,
		CacheTTLLong:  10 * time.Minute,
	})
}

func podRequest(name string) Request {
	return Request{
		Verb:           "get",
		Namespace:      "prod",
		GroupResource:  schema.GroupResource{Resource: "pods"},
		Name:           name,
		Metric:         "cpu_avg_5m",
		ObjectSelector: labels.Everything(),
		MetricSelector: labels.Everything(),
	}
}

func podResolution(name, uid string) resolver.Resolution {
	return resolver.Resolution{
		Object:   resolver.ObjectRef{Namespace: "prod", Name: name, UID: types.UID(uid)},
		PodNames: []string{name},
	}
}

func statusCode(t *testing.T, err error) int32 {
	t.Helper()

	var statusErr *apierrors.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v (%T), want *apierrors.StatusError", err, err)
	}

	return statusErr.ErrStatus.Code
}

// --- happy path -------------------------------------------------------

func TestService_Get_NamedHit(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
	q := &fakeEvaluator{sample: present(1.5)}
	svc := newTestService(t, testCatalog(t), res, q)

	result, err := svc.Get(context.Background(), podRequest("web-0"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(result.Items))
	}
	item := result.Items[0]
	if item.Value != 1.5 || item.UID != "uid-web-0" || item.Namespace != "prod" || item.Name != "web-0" {
		t.Errorf("item = %+v", item)
	}
	if item.APIVersion != "v1" || item.Kind != "Pod" {
		t.Errorf("item apiVersion/kind = %s/%s, want v1/Pod", item.APIVersion, item.Kind)
	}
	if item.MetricName != "cpu_avg_5m" {
		t.Errorf("item.MetricName = %q", item.MetricName)
	}

	if !item.Timestamp.Equal(evalTime) {
		t.Errorf("item.Timestamp = %s, want the evaluation time %s", item.Timestamp, evalTime)
	}

	// The computation must carry the catalog base, the parsed stat/window
	// and the resolved pod names.
	comp := q.lastComp
	if comp.Base.Series != "pod_cpu_usage_cores" || comp.Base.Aggregation != catalog.AggregationSumThenStat {
		t.Errorf("Base = %+v", comp.Base)
	}
	if comp.Stat != catalog.StatAvg || comp.Window != 5*time.Minute || comp.Namespace != "prod" {
		t.Errorf("Stat/Window/Namespace = %s/%s/%s", comp.Stat, comp.Window, comp.Namespace)
	}
	if len(comp.Targets) != 1 || comp.Targets[0].Name != "web-0" || len(comp.Targets[0].Pods) != 1 || comp.Targets[0].Pods[0] != "web-0" {
		t.Errorf("Targets = %+v", comp.Targets)
	}
}

// TestService_Get_ServesNamesDiscoveryDoesNotAdvertise pins the contract
// that --discovery-mode only shapes ListAllMetrics: canonical and
// non-canonical names are served even when nothing is advertised.
func TestService_Get_ServesNamesDiscoveryDoesNotAdvertise(t *testing.T) {
	for _, mode := range []catalog.DiscoveryMode{catalog.DiscoveryMinimal, catalog.DiscoveryNone} {
		for _, metric := range []string{"cpu_p95_1h", "memory_max_26m"} {
			t.Run(string(mode)+"/"+metric, func(t *testing.T) {
				res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
				q := &fakeEvaluator{sample: present(1.5)}
				svc := newTestService(t, testCatalogWithDiscovery(t, mode), res, q)

				req := podRequest("web-0")
				req.Metric = metric
				result, err := svc.Get(context.Background(), req)
				if err != nil {
					t.Fatalf("Get(%s): %v", metric, err)
				}
				if len(result.Items) != 1 || result.Items[0].MetricName != metric {
					t.Errorf("Items = %+v, want one item for %s", result.Items, metric)
				}
			})
		}
	}
}

func TestService_Get_NodeTargetCarriesNodeName(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{
		{Object: resolver.ObjectRef{Name: "worker-1", UID: "uid-worker-1"}},
	}}
	q := &fakeEvaluator{sample: present(2.0)}
	svc := newTestService(t, testCatalog(t), res, q)

	req := Request{
		Verb:           "get",
		GroupResource:  schema.GroupResource{Resource: "nodes"},
		Name:           "worker-1",
		Metric:         "node_cpu_avg_5m",
		ObjectSelector: labels.Everything(),
		MetricSelector: labels.Everything(),
	}

	result, err := svc.Get(context.Background(), req)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Kind != "Node" {
		t.Fatalf("result = %+v", result)
	}
	if len(q.lastComp.Targets) != 1 || q.lastComp.Targets[0].Name != "worker-1" {
		t.Errorf("Targets = %+v, want one target named worker-1", q.lastComp.Targets)
	}
	if q.lastComp.Base.Scope != catalog.ScopeNode {
		t.Errorf("Scope = %q, want node", q.lastComp.Base.Scope)
	}
}

func TestService_Get_StatThenSumAggregationWired(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
	q := &fakeEvaluator{sample: present(1)}
	svc := newTestService(t, testCatalog(t), res, q)

	req := podRequest("web-0")
	req.Metric = "memory_avg_5m"

	if _, err := svc.Get(context.Background(), req); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if q.lastComp.Base.Aggregation != catalog.AggregationStatThenSum {
		t.Errorf("Aggregation = %q, want stat-then-sum (memory base)", q.lastComp.Base.Aggregation)
	}
	if q.lastComp.Base.Unit != catalog.UnitBytes {
		t.Errorf("Unit = %q, want bytes", q.lastComp.Base.Unit)
	}
}

func TestService_Get_WildcardReturnsSortedItemsAndCachesEmptyList(t *testing.T) {
	res := &fakeResolver{resolutions: nil}
	q := &fakeEvaluator{}
	svc := newTestService(t, testCatalog(t), res, q)

	req := podRequest("")
	req.Verb = "list"

	result, err := svc.Get(context.Background(), req)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("len(Items) = %d, want 0", len(result.Items))
	}

	// A second identical call must hit the cache, not call the resolver
	// again.
	if _, err := svc.Get(context.Background(), req); err != nil {
		t.Fatalf("Get (2nd): %v", err)
	}
	if got := res.calls.Load(); got != 1 {
		t.Errorf("resolver called %d times, want 1 (2nd call should hit cache)", got)
	}
}

func TestService_Get_WildcardOmitsAbsentItems(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{
		podResolution("web-0", "uid-web-0"),
		podResolution("web-1", "uid-web-1"),
	}}

	q := &fakeEvaluator{fn: func(target prometheus.Target) prometheus.Sample {
		if target.Name == "web-0" {
			return prometheus.Sample{} // absent
		}

		return present(5)
	}}
	svc := newTestService(t, testCatalog(t), res, q)

	req := podRequest("")
	req.Verb = "list"

	result, err := svc.Get(context.Background(), req)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Name != "web-1" {
		t.Fatalf("result.Items = %+v, want only web-1", result.Items)
	}
	if got := q.calls.Load(); got != 1 {
		t.Errorf("evaluator called %d times, want 1 (one computation for every target)", got)
	}
}

// --- error mapping (metric-gateway.md §3.6) --------------------------

func TestService_Get_MetricSelectorNonempty400(t *testing.T) {
	svc := newTestService(t, testCatalog(t), &fakeResolver{}, &fakeEvaluator{})

	sel, err := labels.Parse("app=web")
	if err != nil {
		t.Fatal(err)
	}
	req := podRequest("web-0")
	req.MetricSelector = sel

	_, err = svc.Get(context.Background(), req)
	if got := statusCode(t, err); got != 400 {
		t.Errorf("code = %d, want 400", got)
	}
}

func TestService_Get_UnknownMetric404(t *testing.T) {
	svc := newTestService(t, testCatalog(t), &fakeResolver{}, &fakeEvaluator{})

	req := podRequest("web-0")
	req.Metric = "does_not_exist_avg_5m"

	_, err := svc.Get(context.Background(), req)
	if got := statusCode(t, err); got != 404 {
		t.Errorf("code = %d, want 404", got)
	}
}

func TestService_Get_ScopeMismatch404(t *testing.T) {
	svc := newTestService(t, testCatalog(t), &fakeResolver{}, &fakeEvaluator{})

	// node_cpu is node-scoped; requesting it against a Pod is unsupported.
	req := podRequest("web-0")
	req.Metric = "node_cpu_avg_5m"

	_, err := svc.Get(context.Background(), req)
	if got := statusCode(t, err); got != 404 {
		t.Errorf("code = %d, want 404", got)
	}
}

func TestService_Get_NamedObjectNotFound404(t *testing.T) {
	res := &fakeResolver{err: &resolver.NotFoundError{Kind: resolver.KindPod, Namespace: "prod", Name: "web-0", Message: `pods "web-0" not found`}}
	svc := newTestService(t, testCatalog(t), res, &fakeEvaluator{})

	_, err := svc.Get(context.Background(), podRequest("web-0"))
	if got := statusCode(t, err); got != 404 {
		t.Errorf("code = %d, want 404", got)
	}
}

func TestService_Get_CronJobNotFoundPreservesExactMessage(t *testing.T) {
	message := "no active or recent (24h) Jobs for CronJob prod/nightly-batch"
	res := &fakeResolver{err: &resolver.NotFoundError{Kind: resolver.KindCronJob, Namespace: "prod", Name: "nightly-batch", Message: message}}
	svc := newTestService(t, testCatalog(t), res, &fakeEvaluator{})

	req := podRequest("nightly-batch")
	req.GroupResource = schema.GroupResource{Group: "batch", Resource: "cronjobs"}

	_, err := svc.Get(context.Background(), req)
	if got := statusCode(t, err); got != 404 {
		t.Errorf("code = %d, want 404", got)
	}
	var statusErr *apierrors.StatusError
	errors.As(err, &statusErr) //nolint:errcheck // statusCode above already asserted this succeeds
	if statusErr.ErrStatus.Message != message {
		t.Errorf("message = %q, want %q", statusErr.ErrStatus.Message, message)
	}
}

func TestService_Get_NoEligibleMembersNamed404(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
	q := &fakeEvaluator{} // absent
	svc := newTestService(t, testCatalog(t), res, q)

	_, err := svc.Get(context.Background(), podRequest("web-0"))
	if got := statusCode(t, err); got != 404 {
		t.Errorf("code = %d, want 404", got)
	}
}

func TestService_Get_ZeroPodNamesNamed404(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{
		{Object: resolver.ObjectRef{Namespace: "prod", Name: "web", UID: "uid-deploy"}, PodNames: nil},
	}}
	svc := newTestService(t, testCatalog(t), res, &fakeEvaluator{})

	req := podRequest("web")
	req.GroupResource = schema.GroupResource{Group: "apps", Resource: "deployments"}

	_, err := svc.Get(context.Background(), req)
	if got := statusCode(t, err); got != 404 {
		t.Errorf("code = %d, want 404", got)
	}
}

func TestService_Get_ServiceAccountForbidden503(t *testing.T) {
	res := &fakeResolver{err: &resolver.ForbiddenError{Kind: resolver.KindPod, Namespace: "prod", Name: "web-0", Err: errors.New("RBAC denied")}}
	svc := newTestService(t, testCatalog(t), res, &fakeEvaluator{})

	_, err := svc.Get(context.Background(), podRequest("web-0"))
	if got := statusCode(t, err); got != 503 {
		t.Errorf("code = %d, want 503", got)
	}
}

func TestService_Get_IncompleteCoverage503(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
	q := &fakeEvaluator{err: prometheus.ErrIncompleteCoverage}
	svc := newTestService(t, testCatalog(t), res, q)

	_, err := svc.Get(context.Background(), podRequest("web-0"))
	if got := statusCode(t, err); got != 503 {
		t.Errorf("code = %d, want 503", got)
	}
}

func TestService_Get_QueryTooLarge413(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
	q := &fakeEvaluator{err: prometheus.ErrQueryTooLarge}
	svc := newTestService(t, testCatalog(t), res, q)

	_, err := svc.Get(context.Background(), podRequest("web-0"))
	if got := statusCode(t, err); got != 413 {
		t.Errorf("code = %d, want 413", got)
	}
}

func TestService_Get_DeadlineExceeded504(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
	q := &fakeEvaluator{err: context.DeadlineExceeded}
	svc := newTestService(t, testCatalog(t), res, q)

	_, err := svc.Get(context.Background(), podRequest("web-0"))
	if got := statusCode(t, err); got != 504 {
		t.Errorf("code = %d, want 504", got)
	}
}

func TestService_Get_ResolverForbidden_DoesNotQuery(t *testing.T) {
	res := &fakeResolver{err: &resolver.ForbiddenError{Kind: resolver.KindPod, Err: errors.New("x")}}
	q := &fakeEvaluator{}
	svc := newTestService(t, testCatalog(t), res, q)

	if _, err := svc.Get(context.Background(), podRequest("web-0")); err == nil {
		t.Fatal("expected an error")
	}
	if q.calls.Load() != 0 {
		t.Error("evaluator was called despite a resolver failure")
	}
}

// --- admission -----------------------------------------------------------

func TestService_Get_InflightAdmissionRejected429(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
	block := make(chan struct{})
	q := &fakeEvaluator{fn: func(prometheus.Target) prometheus.Sample {
		<-block

		return present(1)
	}}

	svc := NewService(Deps{
		Catalog:       testCatalog(t),
		Resolver:      res,
		Evaluator:     q,
		Cache:         cache.New(100, 1<<20, sizeOfResult),
		Flights:       cache.NewGroup[Result](context.Background(), time.Minute),
		Inflight:      cache.NewLimiter("inflight-requests", 1),
		Computations:  cache.NewLimiter("shared-computations", 32),
		CacheTTLShort: 15 * time.Second,
		CacheTTLLong:  10 * time.Minute,
	})

	// Two DIFFERENT named objects so the second doesn't just join the
	// first's singleflight (which would need no new inflight slot either,
	// but this test specifically wants to exhaust the inflight limiter).
	done := make(chan error, 1)
	go func() {
		_, err := svc.Get(context.Background(), podRequest("web-0"))
		done <- err
	}()

	time.Sleep(20 * time.Millisecond) // let the first call occupy the only slot

	_, err := svc.Get(context.Background(), podRequest("web-1"))
	if got := statusCode(t, err); got != 429 {
		t.Errorf("code = %d, want 429", got)
	}

	close(block)
	if err := <-done; err != nil {
		t.Errorf("first call err = %v, want nil", err)
	}
}

// --- singleflight collapse -------------------------------------------

func TestService_Get_ConcurrentIdenticalRequestsCollapse(t *testing.T) {
	res := &fakeResolver{resolutions: []resolver.Resolution{podResolution("web-0", "uid-web-0")}}
	start := make(chan struct{})
	q := &fakeEvaluator{fn: func(prometheus.Target) prometheus.Sample {
		<-start

		return present(3)
	}}
	svc := newTestService(t, testCatalog(t), res, q)

	const n = 10
	var wg sync.WaitGroup
	results := make([]Result, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.Get(context.Background(), podRequest("web-0"))
		}(i)
	}

	time.Sleep(30 * time.Millisecond)
	close(start)
	wg.Wait()

	if got := res.calls.Load(); got != 1 {
		t.Errorf("resolver called %d times, want 1", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
		if len(results[i].Items) != 1 || results[i].Items[0].Value != 3 {
			t.Errorf("caller %d result = %+v", i, results[i])
		}
	}
}

// --- ListAllMetrics ----------------------------------------------------

func TestProvider_ListAllMetrics_DelegatesToCatalog(t *testing.T) {
	cat := testCatalog(t)
	svc := newTestService(t, cat, &fakeResolver{}, &fakeEvaluator{})
	p := NewProvider(cat, svc)

	got := p.ListAllMetrics()
	want := cat.Entries()
	if len(got) != len(want) {
		t.Errorf("len(ListAllMetrics()) = %d, want %d", len(got), len(want))
	}
}
