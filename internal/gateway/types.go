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

// Package gateway runs the steps of one metric request — parse, cache
// lookup, singleflight, resolve, evaluate, convert, cache store, telemetry —
// and implements
// sigs.k8s.io/custom-metrics-apiserver/pkg/provider.CustomMetricsProvider
// on top of them. It does not know about HTTP routing. Authentication and
// authorization have already run before any exported method here is
// called.
package gateway

import (
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Request is one provider call in the gateway's own shape, independent of
// the provider interface.
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
// *custom_metrics.MetricValue.
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

// approxItemOverheadBytes is a rough, deliberately high size per item for
// the response cache's byte limit. Most of an item's memory is object
// names and map/slice overhead, not the value itself, so a fixed number
// that counts more bytes than the real response is the safe choice.
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
