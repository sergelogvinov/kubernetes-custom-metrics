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

package catalog

import (
	"fmt"
	"strings"
	"time"
)

// Stat is one of the eight temporal statistics the metric-name grammar
// supports (metric-gateway.md §2).
type Stat string

// Supported statistics, in the grammar's documented order.
const (
	StatAvg    Stat = "avg"
	StatMax    Stat = "max"
	StatMin    Stat = "min"
	StatP50    Stat = "p50"
	StatP90    Stat = "p90"
	StatP95    Stat = "p95"
	StatP99    Stat = "p99"
	StatStddev Stat = "stddev"
)

// Stats returns the grammar's supported statistics in a fixed order
// (metric-gateway.md §2).
func Stats() []Stat {
	return []Stat{StatAvg, StatMax, StatMin, StatP50, StatP90, StatP95, StatP99, StatStddev}
}

// Window is a metric name's temporal-window segment (metric-gateway.md §2).
// Any Go-duration-syntax string of at least MinWindow is a valid window
// (e.g. "26m", "2h"); the seven Windows() values are just the canonical
// set DiscoveryFull advertises, not an exhaustive list of what the grammar
// accepts.
type Window string

// MinWindow is the shortest window the grammar accepts. Sub-minute
// windows are rejected: the normalized recording rules/subqueries evaluate
// on a fixed grid (gridStep in pkg/prometheus) and CPU input is
// already a five-minute-smoothed rate, so a window shorter than a minute
// would carry too few grid points to mean anything (metric-gateway.md §2).
const MinWindow = time.Minute

// Supported windows, in the grammar's documented order. This is the
// canonical set DiscoveryFull advertises (catalog.go's discoveryEntries;
// DiscoveryMinimal advertises only MinimalWindow) — not the
// full set of windows parseMetricName accepts, which is any positive
// duration (Window.Duration).
const (
	Window1m  Window = "1m"
	Window5m  Window = "5m"
	Window15m Window = "15m"
	Window1h  Window = "1h"
	Window6h  Window = "6h"
	Window12h Window = "12h"
	Window24h Window = "24h"
)

// Windows returns the grammar's canonical windows in a fixed order
// (metric-gateway.md §2), the set DiscoveryFull advertises. Arbitrary
// windows outside this set are still valid metric names (Window.Duration);
// they are simply never advertised in discovery.
func Windows() []Window {
	return []Window{Window1m, Window5m, Window15m, Window1h, Window6h, Window12h, Window24h}
}

// Duration parses w as a Go duration string, returning zero if w is
// malformed or shorter than MinWindow.
func (w Window) Duration() time.Duration {
	d, err := time.ParseDuration(string(w))
	if err != nil || d < MinWindow {
		return 0
	}

	return d
}

// Unit is a base's fixed physical unit (metric-gateway.md §8).
type Unit string

// The two fixed units the catalog's four base names use.
const (
	UnitCores Unit = "cores"
	UnitBytes Unit = "bytes"
)

// Scope is a base's fixed applicability: pod-scoped bases apply to Pods and
// the five namespaced workload kinds; node-scoped bases apply only to Nodes
// (metric-gateway.md §2, §8).
type Scope string

// The two fixed scopes the catalog's four base names use.
const (
	ScopePod  Scope = "pod"
	ScopeNode Scope = "node"
)

// Aggregation selects how a pod-scoped base combines multiple pods' temporal
// statistics into one workload value (metric-gateway.md §3.2), or opts a
// base out of the normalized input contract entirely.
type Aggregation string

// The three supported aggregation strategies. AggregationRaw computes
// directly from raw cAdvisor/node-exporter series with no
// active/completeness/freshness validation, for clusters without the
// normalized pod_active/pod_cpu_complete/pod_memory_complete or
// node_active/node_complete recording rules (metric-gateway.md §8).
const (
	AggregationSumThenStat Aggregation = "sum-then-stat"
	AggregationStatThenSum Aggregation = "stat-then-sum"
	AggregationRaw         Aggregation = "raw"
)

var validStats = statSet()

func statSet() map[Stat]bool {
	m := make(map[Stat]bool, len(Stats()))
	for _, s := range Stats() {
		m[s] = true
	}

	return m
}

// ParsedMetric is a metric name decomposed into its catalog base and the
// grammar's stat/window segments.
type ParsedMetric struct {
	Base   Base
	Stat   Stat
	Window Window
}

// BuildMetricName renders the grammar's <base>_<stat>_<window> form
// (metric-gateway.md §2).
func BuildMetricName(base string, stat Stat, window Window) string {
	return fmt.Sprintf("%s_%s_%s", base, stat, window)
}

// parseMetricName decodes name by matching a known catalog base prefix
// first, then validating the remaining stat/window segments — never by
// splitting the name into three underscore-separated fields, since base
// names contain underscores themselves, e.g. "node_cpu".
func parseMetricName(name string, bases map[string]Base) (ParsedMetric, bool) {
	for baseName, base := range bases {
		prefix := baseName + "_"
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		statStr, windowStr, ok := strings.Cut(strings.TrimPrefix(name, prefix), "_")
		if !ok {
			continue
		}

		stat, window := Stat(statStr), Window(windowStr)
		if !validStats[stat] || window.Duration() <= 0 {
			continue
		}

		return ParsedMetric{Base: base, Stat: stat, Window: window}, true
	}

	return ParsedMetric{}, false
}
