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

package catalog_test

import (
	_ "embed"
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

//go:embed testdata/catalog.yaml
var testdata []byte

func TestLoad_FullCatalog(t *testing.T) {
	data := testdata

	cat, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cat.Revision() == "" {
		t.Error("Revision() is empty")
	}

	entries := cat.Entries()
	if got, want := len(entries), catalog.MaxDiscoveryMetrics; got != want {
		t.Errorf("len(Entries()) = %d, want %d", got, want)
	}

	base, ok := cat.Base("node_cpu")
	if !ok {
		t.Fatal(`Base("node_cpu") not found`)
	}
	if base.Series != "node_cpu_usage_cores" || base.Unit != catalog.UnitCores || base.Scope != catalog.ScopeNode {
		t.Errorf("node_cpu base = %+v, want series=node_cpu_usage_cores unit=cores scope=node", base)
	}
}

func TestLoad_TwoLoadsOfSameBytesAgree(t *testing.T) {
	data := testdata

	a, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if a.Revision() != b.Revision() {
		t.Errorf("Revision() differs across identical loads: %q vs %q", a.Revision(), b.Revision())
	}
}

func TestLoad_DifferentBytesDifferentRevision(t *testing.T) {
	full := testdata

	a, err := catalog.Load(full, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load(full): %v", err)
	}

	cpuOnly := []byte(`
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: pod
    aggregation: sum-then-stat
`)
	b, err := catalog.Load(cpuOnly, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load(cpuOnly): %v", err)
	}

	if a.Revision() == b.Revision() {
		t.Error("Revision() matched for different catalog contents")
	}

	// A single pod-scoped base applies to 6 namespaced resources; 6 × 8 stats
	// × 7 windows = 336 entries.
	if got, want := len(b.Entries()), 336; got != want {
		t.Errorf("len(Entries()) for cpu-only catalog = %d, want %d", got, want)
	}
}

func TestLoad_RejectsEmptyCatalog(t *testing.T) {
	if _, err := catalog.Load([]byte(`bases: {}`), catalog.MaxDiscoveryMetrics); err == nil {
		t.Error("Load(empty bases) succeeded, want error")
	}
}

func TestLoad_RejectsUnknownTopLevelField(t *testing.T) {
	data := []byte(`
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: pod
    aggregation: sum-then-stat
extra: not-allowed
`)
	if _, err := catalog.Load(data, catalog.MaxDiscoveryMetrics); err == nil {
		t.Error("Load(unknown top-level field) succeeded, want error")
	}
}

func TestLoad_RejectsUnknownBaseField(t *testing.T) {
	data := []byte(`
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: pod
    aggregation: sum-then-stat
    extra: not-allowed
`)
	if _, err := catalog.Load(data, catalog.MaxDiscoveryMetrics); err == nil {
		t.Error("Load(unknown base field) succeeded, want error")
	}
}

func TestLoad_RejectsUnknownBaseName(t *testing.T) {
	data := []byte(`
bases:
  http_rps:
    series: http_requests_per_second
    unit: cores
    scope: pod
    aggregation: sum-then-stat
`)
	if _, err := catalog.Load(data, catalog.MaxDiscoveryMetrics); err == nil {
		t.Error("Load(unknown base name) succeeded, want error")
	}
}

func TestLoad_RejectsArbitraryPromQLInSeries(t *testing.T) {
	data := []byte(`
bases:
  cpu:
    series: "sum(pod_cpu_usage_cores)"
    unit: cores
    scope: pod
    aggregation: sum-then-stat
`)
	if _, err := catalog.Load(data, catalog.MaxDiscoveryMetrics); err == nil {
		t.Error("Load(PromQL expression as series) succeeded, want error")
	}
}

func TestLoad_RejectsWrongUnit(t *testing.T) {
	data := []byte(`
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: bytes
    scope: pod
    aggregation: sum-then-stat
`)
	if _, err := catalog.Load(data, catalog.MaxDiscoveryMetrics); err == nil {
		t.Error("Load(wrong unit) succeeded, want error")
	}
}

func TestLoad_RejectsWrongScope(t *testing.T) {
	data := []byte(`
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: node
    aggregation: sum-then-stat
`)
	if _, err := catalog.Load(data, catalog.MaxDiscoveryMetrics); err == nil {
		t.Error("Load(wrong scope) succeeded, want error")
	}
}

func TestLoad_RejectsUnknownAggregation(t *testing.T) {
	data := []byte(`
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: pod
    aggregation: average-then-sum
`)
	if _, err := catalog.Load(data, catalog.MaxDiscoveryMetrics); err == nil {
		t.Error("Load(unknown aggregation) succeeded, want error")
	}
}

func TestLoad_AcceptsRawAggregationForPodScope(t *testing.T) {
	data := []byte(`
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: pod
    aggregation: raw
`)
	cat, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	base, ok := cat.Base("cpu")
	if !ok {
		t.Fatal(`Base("cpu") not found`)
	}
	if base.Aggregation != catalog.AggregationRaw {
		t.Errorf("Aggregation = %q, want %q", base.Aggregation, catalog.AggregationRaw)
	}
}

func TestLoad_AcceptsRawAggregationForNodeScope(t *testing.T) {
	data := []byte(`
bases:
  node_cpu:
    series: node_cpu_usage_cores
    unit: cores
    scope: node
    aggregation: raw
`)
	cat, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	base, ok := cat.Base("node_cpu")
	if !ok {
		t.Fatal(`Base("node_cpu") not found`)
	}
	if base.Aggregation != catalog.AggregationRaw {
		t.Errorf("Aggregation = %q, want %q", base.Aggregation, catalog.AggregationRaw)
	}
}

func TestLoad_RejectsExceedingDiscoveryMax(t *testing.T) {
	data := testdata

	if _, err := catalog.Load(data, 10); err == nil {
		t.Error("Load(maxDiscoveryMetrics=10) succeeded for the 784-entry catalog, want error")
	}
}

func TestDiscoveryEntries_ResourceShape(t *testing.T) {
	data := testdata

	cat, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	byResource := map[schema.GroupResource]bool{}
	for _, e := range cat.Entries() {
		byResource[e.GroupResource] = e.Namespaced
	}

	cases := []struct {
		gr         schema.GroupResource
		namespaced bool
	}{
		{schema.GroupResource{Resource: "pods"}, true},
		{schema.GroupResource{Resource: "nodes"}, false},
		{schema.GroupResource{Group: "apps", Resource: "deployments"}, true},
		{schema.GroupResource{Group: "apps", Resource: "statefulsets"}, true},
		{schema.GroupResource{Group: "apps", Resource: "daemonsets"}, true},
		{schema.GroupResource{Group: "batch", Resource: "jobs"}, true},
		{schema.GroupResource{Group: "batch", Resource: "cronjobs"}, true},
	}
	for _, tc := range cases {
		namespaced, ok := byResource[tc.gr]
		if !ok {
			t.Errorf("no discovery entry for resource %+v", tc.gr)

			continue
		}
		if namespaced != tc.namespaced {
			t.Errorf("resource %+v: namespaced = %v, want %v", tc.gr, namespaced, tc.namespaced)
		}
	}
}

func TestParseMetricName_RoundTripsAllGrammarCombinations(t *testing.T) {
	data := testdata

	cat, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	baseNames := []string{"cpu", "memory", "node_cpu", "node_memory"}
	count := 0
	for _, baseName := range baseNames {
		for _, stat := range catalog.Stats() {
			for _, window := range catalog.Windows() {
				name := catalog.BuildMetricName(baseName, stat, window)

				parsed, ok := cat.Parse(name)
				if !ok {
					t.Errorf("Parse(%q) failed to parse", name)

					continue
				}
				if parsed.Base.Name != baseName || parsed.Stat != stat || parsed.Window != window {
					t.Errorf("Parse(%q) = %+v, want base=%s stat=%s window=%s", name, parsed, baseName, stat, window)
				}
				count++
			}
		}
	}

	if want := len(baseNames) * len(catalog.Stats()) * len(catalog.Windows()); count != want {
		t.Fatalf("checked %d combinations, want %d", count, want)
	}
	if want := 224; count != want {
		t.Fatalf("checked %d combinations, want the documented 224", count)
	}
}

func TestParseMetricName_RejectsMalformed(t *testing.T) {
	data := testdata

	cat, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []string{
		"",
		"cpu",
		"cpu_avg",
		"cpu_avg_",
		"cpu_avg_5x",
		"cpu_p999_5m",
		"http_rps_avg_5m",
		"cpu_avg_5m_extra",
		"memory_avg_5m_extra_extra",
		"_avg_5m",
		"cpu__5m",
	}
	for _, name := range cases {
		if parsed, ok := cat.Parse(name); ok {
			t.Errorf("Parse(%q) = %+v, want rejection", name, parsed)
		}
	}
}

func TestParseMetricName_AcceptsArbitraryWindows(t *testing.T) {
	data := testdata

	cat, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		name       string
		wantWindow catalog.Window
	}{
		{"cpu_avg_26m", "26m"},
		{"cpu_avg_34m", "34m"},
		{"cpu_avg_2h", "2h"},
		{"cpu_avg_90s", "90s"},
	}
	for _, tt := range cases {
		parsed, ok := cat.Parse(tt.name)
		if !ok {
			t.Errorf("Parse(%q) failed to parse, want acceptance of the non-canonical window", tt.name)
			continue
		}
		if parsed.Window != tt.wantWindow {
			t.Errorf("Parse(%q).Window = %q, want %q", tt.name, parsed.Window, tt.wantWindow)
		}
	}
}

func TestParseMetricName_RejectsWindowsBelowMinimum(t *testing.T) {
	data := testdata

	cat, err := catalog.Load(data, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []string{"cpu_avg_0m", "cpu_avg_-5m", "cpu_avg_0s", "cpu_avg_30s", "cpu_avg_59s"}
	for _, name := range cases {
		if parsed, ok := cat.Parse(name); ok {
			t.Errorf("Parse(%q) = %+v, want rejection of a window below the 1m minimum", name, parsed)
		}
	}
}

func TestWindowDuration(t *testing.T) {
	cases := []struct {
		window catalog.Window
		want   time.Duration
	}{
		{"5m", 5 * time.Minute},
		{"26m", 26 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"1m", time.Minute},
		{"60s", time.Minute},
		{"59s", 0},
		{"0m", 0},
		{"-5m", 0},
		{"not-a-duration", 0},
	}
	for _, tt := range cases {
		if got := tt.window.Duration(); got != tt.want {
			t.Errorf("Window(%q).Duration() = %v, want %v", tt.window, got, tt.want)
		}
	}
}

func TestParseMetricName_UnconfiguredBaseIsUnknown(t *testing.T) {
	cpuOnly := []byte(`
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: pod
    aggregation: sum-then-stat
`)
	cat, err := catalog.Load(cpuOnly, catalog.MaxDiscoveryMetrics)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if parsed, ok := cat.Parse("memory_avg_5m"); ok {
		t.Errorf("Parse(memory_avg_5m) = %+v, want rejection (memory not configured)", parsed)
	}
}
