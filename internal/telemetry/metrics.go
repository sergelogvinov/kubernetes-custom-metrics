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

// Package telemetry defines the gateway's own Prometheus metrics
// (metric-gateway.md §4 "Cache Metrics"). It registers them in the
// generic-apiserver /metrics registry, so the server has only one /metrics
// endpoint.
package telemetry

import (
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Metrics holds every gateway self-metric. Labels always come from a small,
// known set — a resolver.Kind name or a catalog-validated metric name —
// never a namespace, object name, selector, or caller identity.
type Metrics struct {
	CacheHits             *metrics.CounterVec
	CacheMisses           *metrics.CounterVec
	SingleflightCollapsed *metrics.Counter
	PromQueryDuration     *metrics.HistogramVec
	QueryErrors           *metrics.CounterVec
	CronJobFallback       *metrics.Counter
	CronJobNotFound       *metrics.Counter
}

// New builds an unregistered Metrics set.
func New() *Metrics {
	return &Metrics{
		CacheHits: metrics.NewCounterVec(&metrics.CounterOpts{
			Name:           "gateway_cache_hits_total",
			Help:           "Number of response-cache hits, by resource kind and metric name.",
			StabilityLevel: metrics.ALPHA,
		}, []string{"resource", "metric"}),
		CacheMisses: metrics.NewCounterVec(&metrics.CounterOpts{
			Name:           "gateway_cache_misses_total",
			Help:           "Number of response-cache misses, by resource kind and metric name.",
			StabilityLevel: metrics.ALPHA,
		}, []string{"resource", "metric"}),
		SingleflightCollapsed: metrics.NewCounter(&metrics.CounterOpts{
			Name:           "gateway_singleflight_collapsed_total",
			Help:           "Number of requests that joined an already in-flight computation instead of starting a new one.",
			StabilityLevel: metrics.ALPHA,
		}),
		PromQueryDuration: metrics.NewHistogramVec(&metrics.HistogramOpts{
			Name:           "gateway_prom_query_duration_seconds",
			Help:           "Duration of one Prometheus.Client.Query call.",
			Buckets:        metrics.DefBuckets,
			StabilityLevel: metrics.ALPHA,
		}, []string{"resource", "metric"}),
		QueryErrors: metrics.NewCounterVec(&metrics.CounterOpts{
			Name:           "gateway_query_errors_total",
			Help:           "Number of failed metric requests, by reason.",
			StabilityLevel: metrics.ALPHA,
		}, []string{"reason"}),
		CronJobFallback: metrics.NewCounter(&metrics.CounterOpts{
			Name:           "gateway_cronjob_fallback_total",
			Help:           "Number of CronJob resolutions that used the recent-Jobs fallback instead of an active Job.",
			StabilityLevel: metrics.ALPHA,
		}),
		CronJobNotFound: metrics.NewCounter(&metrics.CounterOpts{
			Name:           "gateway_cronjob_notfound_total",
			Help:           "Number of CronJob resolutions with neither an active nor a recent Job.",
			StabilityLevel: metrics.ALPHA,
		}),
	}
}

// Register registers every metric into generic-apiserver's own /metrics
// registry (legacyregistry), the one AdapterBase's server exposes.
func (m *Metrics) Register() {
	legacyregistry.MustRegister(
		m.CacheHits,
		m.CacheMisses,
		m.SingleflightCollapsed,
		m.PromQueryDuration,
		m.QueryErrors,
		m.CronJobFallback,
		m.CronJobNotFound,
	)
}
