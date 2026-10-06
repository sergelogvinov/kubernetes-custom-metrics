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

// Package catalog owns the immutable, validated metric catalog: decoding
// and validating the catalog ConfigMap document, expanding it into
// custom.metrics.k8s.io discovery entries, and parsing/building metric
// names against the grammar in metric-gateway.md §2.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/provider"
	"sigs.k8s.io/yaml"
)

// MaxDiscoveryMetrics is the discovery-entry ceiling the full, revised
// default catalog produces: (6 namespaced resources × 2 pod-scoped bases +
// 1 cluster-scoped resource × 2 node-scoped bases) × 8 stats × 7 windows
// (metric-gateway.md §2).
const MaxDiscoveryMetrics = 784

// DiscoveryMode selects how much of the metric grammar ListAllMetrics
// advertises. Discovery is informational only: every name the grammar
// parses is served regardless of whether it is advertised.
type DiscoveryMode string

const (
	// DiscoveryFull advertises every base × resource × stat × canonical
	// window combination (784 entries for the full default catalog).
	DiscoveryFull DiscoveryMode = "full"
	// DiscoveryMinimal advertises one entry per base × resource, using
	// MinimalStat and MinimalWindow, as an example of the naming scheme.
	DiscoveryMinimal DiscoveryMode = "minimal"
	// DiscoveryNone advertises no metrics.
	DiscoveryNone DiscoveryMode = "none"
)

// MinimalStat and MinimalWindow form the single entry DiscoveryMinimal
// advertises per base and resource.
const (
	MinimalStat   = StatAvg
	MinimalWindow = Window5m
)

// discoveryModes lists every valid DiscoveryMode, in documented order.
var discoveryModes = []DiscoveryMode{DiscoveryFull, DiscoveryMinimal, DiscoveryNone}

// DiscoveryModes returns every valid DiscoveryMode.
func DiscoveryModes() []DiscoveryMode {
	return slices.Clone(discoveryModes)
}

// Valid reports whether m is one of DiscoveryModes.
func (m DiscoveryMode) Valid() bool {
	return slices.Contains(discoveryModes, m)
}

// ParseDiscoveryMode parses s case-insensitively, ignoring surrounding
// whitespace. The error for an unknown value lists the valid modes.
func ParseDiscoveryMode(s string) (DiscoveryMode, error) {
	m := DiscoveryMode(strings.ToLower(strings.TrimSpace(s)))
	if !m.Valid() {
		return "", fmt.Errorf("unknown discovery mode %q, must be one of %v", s, discoveryModes)
	}

	return m, nil
}

// Base is one validated catalog base metric definition.
type Base struct {
	Name        string
	Series      string
	Unit        Unit
	Scope       Scope
	Aggregation Aggregation
}

// Catalog is the immutable, validated set of base metric definitions this
// gateway serves, plus its precomputed custom.metrics.k8s.io discovery
// entries.
type Catalog struct {
	revision string
	bases    map[string]Base
	entries  []provider.CustomMetricInfo
}

type rawCatalog struct {
	Bases map[string]rawBase `json:"bases"`
}

type rawBase struct {
	Series      string `json:"series"`
	Unit        string `json:"unit"`
	Scope       string `json:"scope"`
	Aggregation string `json:"aggregation"`
}

// fixedBaseSpec pins the unit and scope every valid base name must declare:
// "the four base names have fixed units/scopes; series names may be changed
// only to equivalent normalized inputs" (metric-gateway.md §8).
type fixedBaseSpec struct {
	Unit  Unit
	Scope Scope
}

var fixedBaseSpecs = map[string]fixedBaseSpec{
	"cpu":         {Unit: UnitCores, Scope: ScopePod},
	"memory":      {Unit: UnitBytes, Scope: ScopePod},
	"node_cpu":    {Unit: UnitCores, Scope: ScopeNode},
	"node_memory": {Unit: UnitBytes, Scope: ScopeNode},
}

// seriesNamePattern matches a bare Prometheus metric identifier. The
// catalog's series field must name a normalized input series, never an
// arbitrary PromQL expression (metric-gateway.md §8).
var seriesNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// Load decodes, validates, and expands a catalog ConfigMap document,
// advertising discovery entries according to mode. maxDiscoveryMetrics
// rejects catalogs whose advertised discovery entries would exceed the
// configured --discovery-max-metrics ceiling.
func Load(data []byte, mode DiscoveryMode, maxDiscoveryMetrics int) (*Catalog, error) {
	var raw rawCatalog
	if err := yaml.UnmarshalStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("catalog: decoding: %w", err)
	}

	if len(raw.Bases) == 0 {
		return nil, fmt.Errorf("catalog: at least one base is required")
	}

	bases := make(map[string]Base, len(raw.Bases))
	for name, rb := range raw.Bases {
		base, err := validateBase(name, rb)
		if err != nil {
			return nil, err
		}
		bases[name] = base
	}

	var stats []Stat
	var windows []Window
	switch mode {
	case DiscoveryFull:
		stats, windows = Stats(), Windows()
	case DiscoveryMinimal:
		stats, windows = []Stat{MinimalStat}, []Window{MinimalWindow}
	case DiscoveryNone:
	default:
		return nil, fmt.Errorf("catalog: unknown discovery mode %q", mode)
	}

	entries := discoveryEntries(bases, stats, windows)
	if len(entries) > maxDiscoveryMetrics {
		return nil, fmt.Errorf("catalog: %d discovery entries exceeds the configured maximum of %d", len(entries), maxDiscoveryMetrics)
	}

	sum := sha256.Sum256(data)

	return &Catalog{
		revision: hex.EncodeToString(sum[:]),
		bases:    bases,
		entries:  entries,
	}, nil
}

func validateBase(name string, rb rawBase) (Base, error) {
	spec, ok := fixedBaseSpecs[name]
	if !ok {
		return Base{}, fmt.Errorf("catalog: unknown base %q", name)
	}

	if !seriesNamePattern.MatchString(rb.Series) {
		return Base{}, fmt.Errorf("catalog: base %q: series %q is not a bare metric identifier", name, rb.Series)
	}

	unit := Unit(rb.Unit)
	if unit != spec.Unit {
		return Base{}, fmt.Errorf("catalog: base %q: unit must be %q, got %q", name, spec.Unit, rb.Unit)
	}

	scope := Scope(rb.Scope)
	if scope != spec.Scope {
		return Base{}, fmt.Errorf("catalog: base %q: scope must be %q, got %q", name, spec.Scope, rb.Scope)
	}

	aggregation := Aggregation(rb.Aggregation)
	switch aggregation {
	case AggregationSumThenStat, AggregationStatThenSum, AggregationRaw:
	default:
		return Base{}, fmt.Errorf("catalog: base %q: unknown aggregation %q", name, rb.Aggregation)
	}

	return Base{Name: name, Series: rb.Series, Unit: unit, Scope: scope, Aggregation: aggregation}, nil
}

// resourceScope is one Kubernetes resource a base's scope applies to, per
// the API paths in metric-gateway.md §3.5.
type resourceScope struct {
	groupResource schema.GroupResource
	namespaced    bool
}

var podScopedResources = []resourceScope{
	{groupResource: schema.GroupResource{Resource: "pods"}, namespaced: true},
	{groupResource: schema.GroupResource{Group: "apps", Resource: "deployments"}, namespaced: true},
	{groupResource: schema.GroupResource{Group: "apps", Resource: "statefulsets"}, namespaced: true},
	{groupResource: schema.GroupResource{Group: "apps", Resource: "daemonsets"}, namespaced: true},
	{groupResource: schema.GroupResource{Group: "batch", Resource: "jobs"}, namespaced: true},
	{groupResource: schema.GroupResource{Group: "batch", Resource: "cronjobs"}, namespaced: true},
}

var nodeScopedResources = []resourceScope{
	{groupResource: schema.GroupResource{Resource: "nodes"}, namespaced: false},
}

func resourcesFor(scope Scope) []resourceScope {
	if scope == ScopeNode {
		return nodeScopedResources
	}

	return podScopedResources
}

// discoveryEntries expands bases × stats × windows into the
// provider.CustomMetricInfo entries ListAllMetrics returns, in a
// deterministic order.
func discoveryEntries(bases map[string]Base, stats []Stat, windows []Window) []provider.CustomMetricInfo {
	names := make([]string, 0, len(bases))
	for name := range bases {
		names = append(names, name)
	}
	sort.Strings(names)

	var entries []provider.CustomMetricInfo
	for _, name := range names {
		base := bases[name]
		for _, res := range resourcesFor(base.Scope) {
			for _, stat := range stats {
				for _, window := range windows {
					entries = append(entries, provider.CustomMetricInfo{
						GroupResource: res.groupResource,
						Namespaced:    res.namespaced,
						Metric:        BuildMetricName(name, stat, window),
					})
				}
			}
		}
	}

	return entries
}

// Revision is a content hash of the loaded catalog document, used as part
// of the response-cache key (metric-gateway.md §4) so a catalog rollout
// invalidates previously cached results.
func (c *Catalog) Revision() string {
	return c.revision
}

// Entries returns the catalog's discovery entries, for
// provider.CustomMetricsProvider.ListAllMetrics.
func (c *Catalog) Entries() []provider.CustomMetricInfo {
	out := make([]provider.CustomMetricInfo, len(c.entries))
	copy(out, c.entries)

	return out
}

// Base looks up a validated base by name.
func (c *Catalog) Base(name string) (Base, bool) {
	b, ok := c.bases[name]

	return b, ok
}

// Parse decodes metricName against the catalog's configured bases.
func (c *Catalog) Parse(metricName string) (ParsedMetric, bool) {
	return parseMetricName(metricName, c.bases)
}
