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

// Package prometheus renders structured metric requests to PromQL and
// safely executes/decodes them against a Prometheus-compatible backend. It
// has no knowledge of catalogs, caching, or Kubernetes objects: callers
// supply already-resolved, escaped identity names — pod names for pod
// scope, a single node name for node scope (design.md §8; metric-gateway.md
// §3.2, §3.7).
package prometheus

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Scope is whether a request targets pod-scoped or node-scoped identities.
// Defined locally (not imported from pkg/catalog) so this package
// stays independent of the catalog's configuration model.
type Scope string

// The two supported scopes.
const (
	ScopePod  Scope = "pod"
	ScopeNode Scope = "node"
)

// Quantity is the physical quantity a request measures. Combined with
// Scope it selects the fixed completeness-series name (metric-gateway.md
// §8: "Completeness/lifecycle series names and label schema are fixed in
// v1"), independent of whichever usage series name the catalog configures.
type Quantity string

// The two supported quantities.
const (
	QuantityCPU    Quantity = "cpu"
	QuantityMemory Quantity = "memory"
)

// Aggregation selects how multiple identities combine into one value
// (metric-gateway.md §3.2), or opts a base out of the normalized input
// contract entirely.
type Aggregation string

// The three supported aggregation strategies. AggregationSumThenStat and
// AggregationStatThenSum operate on normalized recording-rule input
// (Series) with full coverage/freshness validation against
// pod_active/pod_cpu_complete/pod_memory_complete or
// node_active/node_complete (§3.7). AggregationRaw instead computes
// directly from raw cAdvisor/node-exporter series
// (container_cpu_usage_seconds_total, container_memory_working_set_bytes,
// node_cpu_seconds_total, node_memory_MemTotal_bytes/MemAvailable_bytes)
// with no such validation, for clusters that do not run the normalized
// active/completeness recording rules; Client.Query skips the
// any-active/coverage/freshness queries entirely for this aggregation
// (client.go), so a scrape gap or restart can silently under/over-count.
const (
	AggregationSumThenStat Aggregation = "sum-then-stat"
	AggregationStatThenSum Aggregation = "stat-then-sum"
	AggregationRaw         Aggregation = "raw"
)

// Stat is one of the eight supported temporal statistics.
type Stat string

// The eight supported statistics.
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

// gridStep is the fixed evaluation grid normalized recording rules and
// subqueries use (metric-gateway.md §3.2).
const gridStep = "60s"

// Request describes one target's computation: the exact identity set to
// aggregate, rendered against one normalized input series.
type Request struct {
	// Cluster is the exact backend cluster label. Empty omits the cluster
	// matcher entirely rather than matching an empty label value, so a
	// single-cluster backend with no "cluster" label still works.
	Cluster string
	// Series is the normalized input series name (catalog Base.Series —
	// possibly renamed from its default, but always one of the four fixed
	// quantities' equivalent normalized inputs).
	Series string
	// Scope and Quantity together select the fixed lifecycle/completeness
	// series names, independent of Series' configured name.
	Scope    Scope
	Quantity Quantity
	// Aggregation selects sum-then-stat or stat-then-sum. Immaterial (but
	// still required to be one of the two valid values) when len(UIDs)==1.
	Aggregation Aggregation
	Stat        Stat
	// Window is the requested statistical window.
	Window time.Duration
	// Namespace scopes pod-scoped queries; empty for node scope.
	Namespace string
	// Names is the exact, deduplicated, escaped identity set to aggregate:
	// pod names for pod scope, a single node name for node scope. Must
	// never be empty — an empty selection must never become an
	// unrestricted query (metric-gateway.md §3.3).
	Names []string
	// QueryTime is the single evaluation instant for this computation,
	// captured once and aligned to the last fully completed grid step
	// (metric-gateway.md §3.2). Use AlignToGrid.
	QueryTime time.Time
}

// Sentinel/typed errors Query can return, distinguishing the caller-facing
// status code they map to.
var (
	// ErrEmptySelection means Request.Names was empty.
	ErrEmptySelection = errors.New("prometheus: empty UID selection")
	// ErrQueryTooLarge means a rendered query exceeded the configured
	// size limit (metric-gateway.md §6.3) — maps to 413.
	ErrQueryTooLarge = errors.New("prometheus: rendered query exceeds size limit")
	// ErrIncompleteCoverage means an active member lacked complete
	// coverage somewhere in the window — maps to 503.
	ErrIncompleteCoverage = errors.New("prometheus: incomplete coverage for an active member")
	// ErrStaleData means an active member's underlying sample was older
	// than the 30-second freshness gate somewhere in the window — maps to
	// 503.
	ErrStaleData = errors.New("prometheus: active member's data exceeds the freshness gate")
	// ErrBackend wraps a generic backend/protocol failure — maps to 503.
	ErrBackend = errors.New("prometheus: backend query failed")
)

// AlignToGrid rounds t down to the last fully completed 15-second grid
// step (metric-gateway.md §3.2). Callers capture this once per computation
// and reuse it for every Request in that computation.
func AlignToGrid(t time.Time) time.Time {
	return t.Truncate(15 * time.Second)
}

// activeSeriesName returns the fixed lifecycle series name for scope.
func activeSeriesName(scope Scope) string {
	if scope == ScopeNode {
		return "node_active"
	}

	return "pod_active"
}

// completeSeriesName returns the fixed completeness series name for
// (scope, quantity). Node scope has a single node_complete series shared by
// both quantities: both usage series come from the same node-exporter
// scrape, so node_cpu_complete and node_memory_complete would always be
// identical (monitoring/rules.yaml).
func completeSeriesName(scope Scope, quantity Quantity) string {
	if scope == ScopeNode {
		return "node_complete"
	}
	if quantity == QuantityCPU {
		return "pod_cpu_complete"
	}

	return "pod_memory_complete"
}

var nameMetaRegexp = regexp.MustCompile(`[.+*?()|[\]{}^$\\]`)

// identityLabel returns the label name a scope's identities are matched
// on: node names identify node-scoped series, pod names identify
// pod-scoped ones.
func identityLabel(scope Scope) string {
	if scope == ScopeNode {
		return "node"
	}

	return "pod"
}

// selector renders a normalized-series instant-vector selector, escaping
// the cluster/namespace label values and each identity name (defense in
// depth: names are exact identities, never inserted as arbitrary regex —
// metric-gateway.md §3.3). The cluster matcher is omitted when cluster is
// empty (no --cluster configured); namespace is omitted when empty (node
// scope).
func selector(series, cluster, namespace, idLabel string, names []string) string {
	parts := make([]string, 0, 3)
	if cluster != "" {
		parts = append(parts, fmt.Sprintf("cluster=%s", quoteLabelValue(cluster)))
	}
	if namespace != "" {
		parts = append(parts, fmt.Sprintf("namespace=%s", quoteLabelValue(namespace)))
	}
	parts = append(parts, fmt.Sprintf("%s=~%s", idLabel, quoteLabelValue(namePattern(names))))

	return fmt.Sprintf("%s{%s}", series, strings.Join(parts, ","))
}

func quoteLabelValue(v string) string {
	return strconv.Quote(v)
}

// namePattern renders an alternation regex exactly matching one of names,
// with any regex metacharacter in a name escaped so it is always treated
// literally.
func namePattern(names []string) string {
	escaped := make([]string, len(names))
	for i, n := range names {
		escaped[i] = nameMetaRegexp.ReplaceAllString(n, `\$0`)
	}

	return strings.Join(escaped, "|")
}

// formatWindow renders d in terse Prometheus duration syntax ("1h", not
// Go's "1h0m0s"). Callers only ever pass one of the seven supported grammar
// windows or a short duration used by tests.
func formatWindow(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
}

// statFunc returns the *_over_time function name for avg/max/min/stddev.
// Quantiles are rendered separately via quantileArg.
func statFunc(stat Stat) (string, bool) {
	switch stat {
	case StatAvg:
		return "avg_over_time", true
	case StatMax:
		return "max_over_time", true
	case StatMin:
		return "min_over_time", true
	case StatStddev:
		return "stddev_over_time", true
	default:
		return "", false
	}
}

func quantileArg(stat Stat) (string, bool) {
	switch stat {
	case StatP50:
		return "0.5", true
	case StatP90:
		return "0.9", true
	case StatP95:
		return "0.95", true
	case StatP99:
		return "0.99", true
	default:
		return "", false
	}
}

// wrapStat wraps rangeVector with the PromQL function for stat.
func wrapStat(stat Stat, rangeVector string) (string, error) {
	if fn, ok := statFunc(stat); ok {
		return fmt.Sprintf("%s(%s)", fn, rangeVector), nil
	}
	if q, ok := quantileArg(stat); ok {
		return fmt.Sprintf("quantile_over_time(%s, %s)", q, rangeVector), nil
	}

	return "", fmt.Errorf("prometheus: unknown stat %q", stat)
}

// renderUsage renders the sum-then-stat or stat-then-sum expression for
// req — the value query (metric-gateway.md §3.2).
//
// sum-then-stat sums all selected identities into one series at each grid
// step, then applies the temporal statistic; a confirmed-inactive member
// naturally contributes nothing to sum(), matching the union-active-grid
// contract with no extra zero-fill needed.
//
// stat-then-sum applies the statistic to each identity separately, then
// sums the results; explicit zero-fill for confirmed-inactive points is
// required here so every member's statistic covers the same union-active
// grid width, not just the instants it happened to report a sample.
func renderUsage(req Request) (string, error) {
	if len(req.Names) == 0 {
		return "", ErrEmptySelection
	}

	idLabel := identityLabel(req.Scope)
	sel := selector(req.Series, req.Cluster, req.Namespace, idLabel, req.Names)
	window := formatWindow(req.Window)

	switch req.Aggregation {
	case AggregationSumThenStat:
		rangeVector := fmt.Sprintf("(sum(%s))[%s:%s]", sel, window, gridStep)

		return wrapStat(req.Stat, rangeVector)

	case AggregationStatThenSum:
		activeSel := selector(activeSeriesName(req.Scope), req.Cluster, req.Namespace, idLabel, req.Names)
		zeroFilled := fmt.Sprintf("(%s or (%s == 0))", sel, activeSel)
		rangeVector := fmt.Sprintf("%s[%s:%s]", zeroFilled, window, gridStep)

		stat, err := wrapStat(req.Stat, rangeVector)
		if err != nil {
			return "", err
		}

		return fmt.Sprintf("sum(%s)", stat), nil

	default:
		return "", fmt.Errorf("prometheus: unknown aggregation %q", req.Aggregation)
	}
}

// Raw series AggregationRaw computes from directly, bypassing Request.Series
// and the normalized recording-rule input contract entirely (§8).
const (
	rawContainerCPUSeries     = "container_cpu_usage_seconds_total"
	rawContainerMemorySeries  = "container_memory_working_set_bytes"
	rawNodeCPUSeries          = "node_cpu_seconds_total"
	rawNodeMemTotalSeries     = "node_memory_MemTotal_bytes"
	rawNodeMemAvailableSeries = "node_memory_MemAvailable_bytes"
)

// rawNodeCPUModes are the node_cpu_seconds_total modes that represent real
// usage, matching monitoring/rules.yaml's node_cpu_usage_cores rule: idle,
// iowait and guest/guest_nice are excluded to avoid idle accounting and
// guest double counting.
const rawNodeCPUModes = "user|nice|system|irq|softirq|steal"

// renderRawUsage renders AggregationRaw's usage expression, computing
// directly from raw cAdvisor/node-exporter series the same way
// monitoring/rules.yaml's recording rules do, but inline at query time
// instead of via a recording rule, and with no
// active/completeness/freshness validation — Client.Query never issues the
// any-active/coverage/freshness queries for this aggregation (client.go).
func renderRawUsage(req Request) (string, error) {
	if len(req.Names) == 0 {
		return "", ErrEmptySelection
	}

	switch req.Scope {
	case ScopePod:
		return renderRawPodUsage(req)
	case ScopeNode:
		return renderRawNodeUsage(req)
	default:
		return "", fmt.Errorf("prometheus: aggregation %q: unknown scope %q", AggregationRaw, req.Scope)
	}
}

// renderRawPodUsage sums raw per-container cAdvisor series into a
// per-identity value (five-minute rate before sum for CPU, deduplicating a
// double-scraped container with max by container), mirroring
// pod_cpu_usage_cores/pod_memory_working_set_bytes.
func renderRawPodUsage(req Request) (string, error) {
	var series string
	switch req.Quantity {
	case QuantityCPU:
		series = rawContainerCPUSeries
	case QuantityMemory:
		series = rawContainerMemorySeries
	default:
		return "", fmt.Errorf("prometheus: aggregation %q: unknown quantity %q", AggregationRaw, req.Quantity)
	}

	idLabel := identityLabel(req.Scope)
	matchers := containerMatchers(req.Cluster, req.Namespace, idLabel, req.Names)

	perContainer := fmt.Sprintf("%s{%s}", series, matchers)
	if req.Quantity == QuantityCPU {
		perContainer = fmt.Sprintf("rate(%s[5m])", perContainer)
	}

	perIdentity := fmt.Sprintf("sum by (%s) (max by (%s, container) (%s))", idLabel, idLabel, perContainer)
	rangeVector := fmt.Sprintf("(%s)[%s:%s]", perIdentity, formatWindow(req.Window), gridStep)

	return wrapStat(req.Stat, rangeVector)
}

// renderRawNodeUsage computes directly from raw node-exporter series,
// mirroring node_cpu_usage_cores/node_memory_used_bytes. Identity is
// matched on "kubernetes_node_name", not "node" or "instance": the raw
// series carry no "node" label at all (only the normalized recording rules
// attach one, via label_replace from "instance"), and "instance" itself is
// typically the scrape target address/pod IP, not the Kubernetes Node
// name — "kubernetes_node_name" is the label node-exporter's scrape
// relabeling is expected to carry instead.
func renderRawNodeUsage(req Request) (string, error) {
	matchers := nodeExporterMatchers(req.Cluster, req.Names)

	var perIdentity string
	switch req.Quantity {
	case QuantityCPU:
		perIdentity = fmt.Sprintf(
			`sum(max by (mode) (rate(%s{mode=~"%s",%s}[5m])))`,
			rawNodeCPUSeries, rawNodeCPUModes, matchers,
		)
	case QuantityMemory:
		perIdentity = fmt.Sprintf(
			"(max(%s{%s}) - max(%s{%s}))",
			rawNodeMemTotalSeries, matchers, rawNodeMemAvailableSeries, matchers,
		)
	default:
		return "", fmt.Errorf("prometheus: aggregation %q: unknown quantity %q", AggregationRaw, req.Quantity)
	}

	rangeVector := fmt.Sprintf("(%s)[%s:%s]", perIdentity, formatWindow(req.Window), gridStep)

	return wrapStat(req.Stat, rangeVector)
}

// containerMatchers renders the label-matcher body (no series name, no
// enclosing braces) for a raw per-container cAdvisor selector: the fixed
// exclusions monitoring/rules.yaml also applies (root cgroups, the pause
// container, empty container identities), plus cluster/namespace/identity
// matchers.
func containerMatchers(cluster, namespace, idLabel string, names []string) string {
	parts := []string{`container!=""`, `container!="POD"`, `image!=""`}
	if cluster != "" {
		parts = append(parts, fmt.Sprintf("cluster=%s", quoteLabelValue(cluster)))
	}
	if namespace != "" {
		parts = append(parts, fmt.Sprintf("namespace=%s", quoteLabelValue(namespace)))
	}
	parts = append(parts, fmt.Sprintf("%s=~%s", idLabel, quoteLabelValue(namePattern(names))))

	return strings.Join(parts, ",")
}

// nodeExporterMatchers renders the label-matcher body (no series name, no
// enclosing braces) for a raw node-exporter selector: cluster plus a
// "kubernetes_node_name" identity matcher (see renderRawNodeUsage).
func nodeExporterMatchers(cluster string, names []string) string {
	parts := make([]string, 0, 2)
	if cluster != "" {
		parts = append(parts, fmt.Sprintf("cluster=%s", quoteLabelValue(cluster)))
	}
	parts = append(parts, fmt.Sprintf("kubernetes_node_name=~%s", quoteLabelValue(namePattern(names))))

	return strings.Join(parts, ",")
}

// renderAnyActive renders a query that is present (and > 0) if at least
// one selected identity was active anywhere in the window, and absent
// otherwise — distinguishing a genuinely absent metric (404 for named
// requests) from a coverage/freshness failure (metric-gateway.md §3.7).
func renderAnyActive(req Request) (string, error) {
	if len(req.Names) == 0 {
		return "", ErrEmptySelection
	}

	activeSel := selector(activeSeriesName(req.Scope), req.Cluster, req.Namespace, identityLabel(req.Scope), req.Names)

	return fmt.Sprintf("max_over_time(count(%s == 1)[%s:%s])", activeSel, formatWindow(req.Window), gridStep), nil
}

// renderCoverage renders a query that is present (and > 0) if, anywhere in
// the window, a selected identity was active without either a confirmed
// complete==1 signal or a usage sample at that same grid point — the
// combined completeness-signal and raw-series-gap check
// (metric-gateway.md §3.7).
func renderCoverage(req Request) (string, error) {
	if len(req.Names) == 0 {
		return "", ErrEmptySelection
	}

	idLabel := identityLabel(req.Scope)
	activeSel := selector(activeSeriesName(req.Scope), req.Cluster, req.Namespace, idLabel, req.Names)
	completeSel := selector(completeSeriesName(req.Scope, req.Quantity), req.Cluster, req.Namespace, idLabel, req.Names)
	usageSel := selector(req.Series, req.Cluster, req.Namespace, idLabel, req.Names)

	badMembers := fmt.Sprintf(
		"((%s == 1) unless on (%s) (%s == 1)) or ((%s == 1) unless on (%s) (%s))",
		activeSel, idLabel, completeSel, activeSel, idLabel, usageSel,
	)

	return fmt.Sprintf("max_over_time(count(%s)[%s:%s])", badMembers, formatWindow(req.Window), gridStep), nil
}

// renderFreshness renders a query that is present (with the maximum
// observed staleness in seconds) if any selected identity's active signal
// carried a sample; the caller rejects results over the 30-second
// freshness gate (metric-gateway.md §3.7).
func renderFreshness(req Request) (string, error) {
	if len(req.Names) == 0 {
		return "", ErrEmptySelection
	}

	activeSel := selector(activeSeriesName(req.Scope), req.Cluster, req.Namespace, identityLabel(req.Scope), req.Names)

	return fmt.Sprintf("max(max_over_time((time() - timestamp(%s == 1))[%s:%s]))", activeSel, formatWindow(req.Window), gridStep), nil
}

// maxRenderedQueryBytes is the fixed v1 safety limit on one rendered query
// (metric-gateway.md §6.3).
const maxRenderedQueryBytes = 1 << 20

func checkQuerySize(query string) error {
	if len(query) > maxRenderedQueryBytes {
		return fmt.Errorf("%w: %d bytes", ErrQueryTooLarge, len(query))
	}

	return nil
}
