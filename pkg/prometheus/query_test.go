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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
)

func baseRequest() request {
	return request{
		Cluster:     "example",
		Series:      "pod_cpu_usage_cores",
		Scope:       catalog.ScopePod,
		Quantity:    quantityCPU,
		Aggregation: catalog.AggregationSumThenStat,
		Stat:        catalog.StatAvg,
		Window:      time.Hour,
		Namespace:   "prod",
		Names:       []string{"web-0", "web-1"},
	}
}

// TestRenderUsage_MatchesIllustrativeExample proves renderUsage produces
// exactly the illustrative query from metric-gateway.md §3.2.
func TestRenderUsage_MatchesIllustrativeExample(t *testing.T) {
	got, err := renderUsage(baseRequest())
	if err != nil {
		t.Fatalf("renderUsage: %v", err)
	}

	want := `avg_over_time((sum(pod_cpu_usage_cores{cluster="example",namespace="prod",pod=~"web-0|web-1"}))[1h:60s])`
	if got != want {
		t.Errorf("renderUsage() = %q, want %q", got, want)
	}
}

func TestRenderUsage_StatThenSumZeroFillsConfirmedInactivePoints(t *testing.T) {
	req := baseRequest()
	req.Aggregation = catalog.AggregationStatThenSum

	got, err := renderUsage(req)
	if err != nil {
		t.Fatalf("renderUsage: %v", err)
	}

	want := `sum(avg_over_time((pod_cpu_usage_cores{cluster="example",namespace="prod",pod=~"web-0|web-1"} or (pod_active{cluster="example",namespace="prod",pod=~"web-0|web-1"} == 0))[1h:60s]))`
	if got != want {
		t.Errorf("renderUsage() = %q, want %q", got, want)
	}
}

func TestRenderUsage_NodeScopeOmitsNamespaceAndUsesNodeActive(t *testing.T) {
	req := baseRequest()
	req.Scope = catalog.ScopeNode
	req.Quantity = quantityCPU
	req.Series = "node_cpu_usage_cores"
	req.Namespace = ""
	req.Aggregation = catalog.AggregationStatThenSum
	req.Names = []string{"worker-1"}

	got, err := renderUsage(req)
	if err != nil {
		t.Fatalf("renderUsage: %v", err)
	}

	if strings.Contains(got, "namespace=") {
		t.Errorf("renderUsage() = %q, must not include a namespace clause for node scope", got)
	}
	if !strings.Contains(got, "node_active{") {
		t.Errorf("renderUsage() = %q, want node_active in the zero-fill clause", got)
	}
	if !strings.Contains(got, `node_cpu_usage_cores{cluster="example",node=~"worker-1"}`) {
		t.Errorf("renderUsage() = %q, want a bare cluster+node selector", got)
	}
}

// TestRenderUsage_EmptyClusterOmitsClusterMatcher proves an unset
// request.Cluster (no --cluster configured) renders without a cluster
// label matcher at all, rather than matching an empty cluster label value.
func TestRenderUsage_EmptyClusterOmitsClusterMatcher(t *testing.T) {
	req := baseRequest()
	req.Cluster = ""

	got, err := renderUsage(req)
	if err != nil {
		t.Fatalf("renderUsage: %v", err)
	}

	want := `avg_over_time((sum(pod_cpu_usage_cores{namespace="prod",pod=~"web-0|web-1"}))[1h:60s])`
	if got != want {
		t.Errorf("renderUsage() = %q, want %q", got, want)
	}
}

// TestRenderUsage_EveryStatCombination is the design.md §8 golden-query
// matrix: normalized CPU vs memory, pod vs node scope, both aggregation
// orders, and every statistic.
func TestRenderUsage_EveryStatCombination(t *testing.T) {
	stats := []catalog.Stat{catalog.StatAvg, catalog.StatMax, catalog.StatMin, catalog.StatP50, catalog.StatP90, catalog.StatP95, catalog.StatP99, catalog.StatStddev}
	scopes := []struct {
		scope     catalog.Scope
		namespace string
		series    string
	}{
		{catalog.ScopePod, "prod", "pod_cpu_usage_cores"},
		{catalog.ScopeNode, "", "node_cpu_usage_cores"},
	}
	quantities := []quantity{quantityCPU, quantityMemory}
	aggregations := []catalog.Aggregation{catalog.AggregationSumThenStat, catalog.AggregationStatThenSum}

	for _, sc := range scopes {
		for _, quantity := range quantities {
			for _, agg := range aggregations {
				for _, stat := range stats {
					t.Run(string(sc.scope)+"/"+string(quantity)+"/"+string(agg)+"/"+string(stat), func(t *testing.T) {
						req := baseRequest()
						req.Scope = sc.scope
						req.Namespace = sc.namespace
						req.Series = sc.series
						req.Quantity = quantity
						req.Aggregation = agg
						req.Stat = stat

						got, err := renderUsage(req)
						if err != nil {
							t.Fatalf("renderUsage: %v", err)
						}

						wantFunc, isPlainStat := statFunc(stat)
						switch {
						case isPlainStat && !strings.Contains(got, wantFunc):
							t.Errorf("query %q missing %q", got, wantFunc)
						case !isPlainStat && !strings.Contains(got, "quantile_over_time("):
							t.Errorf("query %q missing quantile_over_time", got)
						}

						if agg == catalog.AggregationStatThenSum && !strings.HasPrefix(got, "sum(") {
							t.Errorf("stat-then-sum query %q must be wrapped in an outer sum()", got)
						}
						if agg == catalog.AggregationSumThenStat && strings.Contains(got, " or (") {
							t.Errorf("sum-then-stat query %q must not zero-fill", got)
						}
					})
				}
			}
		}
	}
}

func TestRenderUsage_RejectsEmptySelection(t *testing.T) {
	req := baseRequest()
	req.Names = nil

	if _, err := renderUsage(req); err != ErrEmptySelection {
		t.Errorf("renderUsage err = %v, want ErrEmptySelection", err)
	}
	if _, err := renderAnyActive(req); err != ErrEmptySelection {
		t.Errorf("renderAnyActive err = %v, want ErrEmptySelection", err)
	}
	if _, err := renderCoverage(req); err != ErrEmptySelection {
		t.Errorf("renderCoverage err = %v, want ErrEmptySelection", err)
	}
	if _, err := renderFreshness(req); err != ErrEmptySelection {
		t.Errorf("renderFreshness err = %v, want ErrEmptySelection", err)
	}
}

func TestRenderRawUsage_CPUUsesRate(t *testing.T) {
	req := baseRequest()
	req.Aggregation = catalog.AggregationRaw

	got, err := renderRawUsage(req)
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	want := fmt.Sprintf(
		`avg_over_time((sum(max by (pod, container) (rate(container_cpu_usage_seconds_total`+
			`{container!="",container!="POD",image!="",cluster="example",namespace="prod",pod=~"web-0|web-1"}[5m]))))[1h:%s])`,
		gridStep,
	)
	if got != want {
		t.Errorf("renderRawUsage() = %q, want %q", got, want)
	}
}

func TestRenderRawUsage_MemoryHasNoRate(t *testing.T) {
	req := baseRequest()
	req.Aggregation = catalog.AggregationRaw
	req.Quantity = quantityMemory

	got, err := renderRawUsage(req)
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	if strings.Contains(got, "rate(") {
		t.Errorf("renderRawUsage() = %q, must not wrap memory in rate()", got)
	}
	if !strings.Contains(got, "container_memory_working_set_bytes{") {
		t.Errorf("renderRawUsage() = %q, want container_memory_working_set_bytes", got)
	}
}

func TestRenderRawUsage_EmptyClusterOmitsClusterMatcher(t *testing.T) {
	req := baseRequest()
	req.Aggregation = catalog.AggregationRaw
	req.Cluster = ""

	got, err := renderRawUsage(req)
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	if strings.Contains(got, "cluster=") {
		t.Errorf("renderRawUsage() = %q, must not include a cluster clause", got)
	}
}

func nodeRawRequest() request {
	req := baseRequest()
	req.Aggregation = catalog.AggregationRaw
	req.Scope = catalog.ScopeNode
	req.Namespace = ""
	req.Names = []string{"worker-1"}

	return req
}

func TestRenderRawUsage_NodeCPUMatchesOnKubernetesNodeName(t *testing.T) {
	got, err := renderRawUsage(nodeRawRequest())
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	want := fmt.Sprintf(
		`avg_over_time((sum(max by (mode) (rate(node_cpu_seconds_total{mode=~"user|nice|system|irq|softirq|steal",cluster="example",kubernetes_node_name=~"worker-1"}[5m]))))[1h:%s])`,
		gridStep,
	)
	if got != want {
		t.Errorf("renderRawUsage() = %q, want %q", got, want)
	}
	if strings.Contains(got, "node=~") || strings.Contains(got, "instance=~") {
		t.Errorf("renderRawUsage() = %q, must match on kubernetes_node_name, not node/instance", got)
	}
}

func TestRenderRawUsage_NodeMemoryIsTotalMinusAvailable(t *testing.T) {
	req := nodeRawRequest()
	req.Quantity = quantityMemory

	got, err := renderRawUsage(req)
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	want := fmt.Sprintf(
		`avg_over_time(((max(node_memory_MemTotal_bytes{cluster="example",kubernetes_node_name=~"worker-1"}) - `+
			`max(node_memory_MemAvailable_bytes{cluster="example",kubernetes_node_name=~"worker-1"})))[1h:%s])`,
		gridStep,
	)
	if got != want {
		t.Errorf("renderRawUsage() = %q, want %q", got, want)
	}
}

func TestRenderRawUsage_NodeEmptyClusterOmitsClusterMatcher(t *testing.T) {
	req := nodeRawRequest()
	req.Cluster = ""

	got, err := renderRawUsage(req)
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	if strings.Contains(got, "cluster=") {
		t.Errorf("renderRawUsage() = %q, must not include a cluster clause", got)
	}
}

func TestRenderRawUsage_RejectsEmptySelection(t *testing.T) {
	req := baseRequest()
	req.Aggregation = catalog.AggregationRaw
	req.Names = nil

	if _, err := renderRawUsage(req); err != ErrEmptySelection {
		t.Errorf("renderRawUsage err = %v, want ErrEmptySelection", err)
	}
}

func TestCompleteSeriesName_FixedMapping(t *testing.T) {
	cases := []struct {
		scope    catalog.Scope
		quantity quantity
		want     string
	}{
		{catalog.ScopePod, quantityCPU, "pod_cpu_complete"},
		{catalog.ScopePod, quantityMemory, "pod_memory_complete"},
		{catalog.ScopeNode, quantityCPU, "node_complete"},
		{catalog.ScopeNode, quantityMemory, "node_complete"},
	}
	for _, tc := range cases {
		if got := completeSeriesName(tc.scope, tc.quantity); got != tc.want {
			t.Errorf("completeSeriesName(%s,%s) = %q, want %q", tc.scope, tc.quantity, got, tc.want)
		}
	}
}

func TestRenderCoverage_ChecksCompletenessAndRawGap(t *testing.T) {
	got, err := renderCoverage(baseRequest())
	if err != nil {
		t.Fatalf("renderCoverage: %v", err)
	}

	for _, want := range []string{"pod_active{", "pod_cpu_complete{", "pod_cpu_usage_cores{", "unless on (pod)"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderCoverage() = %q, want substring %q", got, want)
		}
	}
}

func TestRenderFreshness_UsesTimestampFunction(t *testing.T) {
	got, err := renderFreshness(baseRequest())
	if err != nil {
		t.Fatalf("renderFreshness: %v", err)
	}
	if !strings.Contains(got, "timestamp(pod_active{") {
		t.Errorf("renderFreshness() = %q, want a timestamp(pod_active{...}) clause", got)
	}
}

func TestNamePattern_EscapesRegexMetacharacters(t *testing.T) {
	req := baseRequest()
	req.Names = []string{"web.with|special(chars)"}

	got, err := renderUsage(req)
	if err != nil {
		t.Fatalf("renderUsage: %v", err)
	}
	if !strings.Contains(got, `pod=~"web\\.with\\|special\\(chars\\)"`) {
		t.Errorf("renderUsage() = %q, want the name's regex metacharacters backslash-escaped", got)
	}
}

func TestFormatWindow(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{15 * time.Minute, "15m"},
		{time.Hour, "1h"},
		{6 * time.Hour, "6h"},
		{12 * time.Hour, "12h"},
		{24 * time.Hour, "24h"},
		{10 * time.Second, "10s"},
	}
	for _, tc := range cases {
		if got := formatWindow(tc.d); got != tc.want {
			t.Errorf("formatWindow(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestCheckQuerySize(t *testing.T) {
	if err := checkQuerySize(strings.Repeat("a", maxRenderedQueryBytes)); err != nil {
		t.Errorf("checkQuerySize(exactly at limit) = %v, want nil", err)
	}
	if err := checkQuerySize(strings.Repeat("a", maxRenderedQueryBytes+1)); !errors.Is(err, ErrQueryTooLarge) {
		t.Errorf("checkQuerySize(over limit) = %v, want ErrQueryTooLarge", err)
	}
}
