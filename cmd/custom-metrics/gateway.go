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

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/gateway"
	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/resolver"
	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/telemetry"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/prometheus"
	"k8s.io/client-go/dynamic"
)

// loadCatalog reads and validates the catalog ConfigMap document at path,
// loaded and validated before the serving socket opens (design.md §9, §10
// step 2).
func loadCatalog(path string, mode catalog.DiscoveryMode, maxDiscoveryMetrics int) (*catalog.Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", path, err)
	}

	cat, err := catalog.Load(data, mode, maxDiscoveryMetrics)
	if err != nil {
		return nil, fmt.Errorf("loading catalog %s: %w", path, err)
	}

	return cat, nil
}

// buildGateway constructs the real internal/gateway provider from opts and
// cat, using dynamicClient for the resolver's Kubernetes access
// (adapter.DynamicClient() in production; a fake dynamic.Interface in
// tests, per design.md §10 step 3). ctx bounds every shared computation's
// lifetime (canceled on server shutdown, per cache.Group's contract) — it
// must be the same context passed to adapter.Run(ctx), not a per-request
// one. It returns the provider (for adapter.WithCustomMetrics) and the
// Prometheus client (for the readiness check in health.go).
func buildGateway(ctx context.Context, opts *Options, cat *catalog.Catalog, dynamicClient dynamic.Interface, metrics *telemetry.Metrics) (*gateway.Provider, *prometheus.Client, error) {
	res := resolver.New(dynamicClient, resolver.WithCronJobFallbackWindow(opts.CronJobFallbackWindow))

	promClient, err := prometheus.NewClient(prometheus.ClientConfig{
		URL:                  opts.PrometheusURL,
		Timeout:              opts.PrometheusTimeout,
		MaxConns:             opts.PrometheusMaxConns,
		CAFile:               opts.PrometheusCAFile,
		TokenFile:            opts.PrometheusTokenFile,
		MaxConcurrentQueries: opts.MaxConcurrentQueries,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("configuring prometheus client: %w", err)
	}

	svc := gateway.NewService(gateway.Deps{
		Catalog:  cat,
		Cluster:  opts.Cluster,
		Resolver: res,
		Querier:  promClient,

		Cache:        gateway.NewCache(opts.CacheSize, opts.CacheMaxBytes),
		Flights:      cache.NewGroup[gateway.Result](ctx, opts.RequestTimeout),
		Inflight:     cache.NewLimiter("inflight-requests", opts.MaxInflightRequests),
		Computations: cache.NewLimiter("shared-computations", opts.MaxSharedComputations),

		CacheTTLShort: opts.CacheTTLShort,
		CacheTTLLong:  opts.CacheTTLLong,

		Metrics: metrics,
	})

	return gateway.NewProvider(cat, svc), promClient, nil
}
