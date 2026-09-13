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

// Package gateway implements the gateway's use-case flow — parse, cache
// lookup, singleflight, resolve, query, validate, convert, cache store,
// telemetry (design.md §6) — and
// sigs.k8s.io/custom-metrics-apiserver/pkg/provider.CustomMetricsProvider
// directly on top of it. It is unaware of the provider interface's own
// callers (AdapterBase's REST storage) and of HTTP routing; delegated
// authentication/authorization has already run by the time any exported
// method here is invoked (design.md §6, "Request execution order").
package gateway

import (
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Request is one provider call translated into the gateway's own,
// provider-independent shape (design.md §6).
type Request struct {
	// Verb is "get" for a named lookup or "list" for a wildcard one —
	// part of the cache key, and otherwise unused.
	Verb          string
	Namespace     string
	GroupResource schema.GroupResource
	// Name is empty for a wildcard request.
	Name           string
	Metric         string
	ObjectSelector labels.Selector
	MetricSelector labels.Selector
}

// Item is one resolved object's computed value, carrying enough identity
// and quantity information for the provider boundary to build a
// *custom_metrics.MetricValue (design.md §6 step 9).
type Item struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        types.UID
	MetricName string
	Value      float64
	Unit       catalog.Unit
	Timestamp  time.Time
	Window     time.Duration
}

// Result is a whole request's outcome: a named request's Result has
// exactly one Item; a wildcard request's Result has zero or more, sorted
// by namespace/name/UID (metric-gateway.md §3.6). Result is cached as a
// single immutable unit and must never be mutated by a caller after
// receiving it from Service.Get or a *cache.Cache[Result].
type Result struct {
	Items []Item
}

// approxItemOverheadBytes is a conservative, deliberately rough per-item
// accounted size for the response cache's byte budget — Value/Timestamp
// fields dominate a real MetricValue's encoded size far less than object
// identity strings and per-entry map/slice overhead, so a fixed
// per-item constant errs on the side of counting more bytes than the
// actual encoded response, matching design.md §6's "account ... size
// conservatively for byte eviction, not only the encoded response size."
const approxItemOverheadBytes = 256

// sizeOfResult estimates result's accounted cache size.
func sizeOfResult(result Result) int64 {
	return int64(len(result.Items)) * approxItemOverheadBytes
}

// NewCache builds the Tier-1 response cache (metric-gateway.md §4), sized
// with sizeOfResult. cmd/custom-metrics uses this rather than calling
// cache.New directly so the byte-accounting sizer stays an internal detail
// of this package.
func NewCache(maxEntries int, maxBytes int64) *cache.Cache[Result] {
	return cache.New(maxEntries, maxBytes, sizeOfResult)
}
