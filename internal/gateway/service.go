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
	"strings"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/resolver"
	"github.com/sergelogvinov/kubernetes-custom-metrics/internal/telemetry"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
)

// Resolver is the narrow surface Service depends on — exactly
// (*resolver.Resolver).Resolve's signature, so the concrete type satisfies
// it structurally and service_test.go can use a lightweight fake instead
// (design.md §3 rule 3).
type Resolver interface {
	Resolve(ctx context.Context, target resolver.Target) ([]resolver.Resolution, error)
}

// Querier is the narrow surface Service depends on — exactly
// (*prometheus.Client).Query's signature.
type Querier interface {
	Query(ctx context.Context, req prometheus.Request) (prometheus.Result, bool, error)
}

// Deps are Service's collaborators, constructed and wired by cmd/custom-metrics.
type Deps struct {
	Catalog  *catalog.Catalog
	Cluster  string
	Resolver Resolver
	Querier  Querier

	Cache        *cache.Cache[Result]
	Flights      *cache.Group[Result]
	Inflight     *cache.Limiter
	Computations *cache.Limiter

	CacheTTLShort time.Duration
	CacheTTLLong  time.Duration

	// Clock captures the aligned evaluation time (metric-gateway.md §3.2).
	// Defaults to clock.RealClock{}.
	Clock clock.Clock

	// Metrics records telemetry (metric-gateway.md §4). A nil Metrics
	// disables recording.
	Metrics *telemetry.Metrics
}

// Service implements the gateway's use-case flow independent of the
// provider interface (design.md §6).
type Service struct {
	catalog  *catalog.Catalog
	cluster  string
	resolver Resolver
	querier  Querier

	cache        *cache.Cache[Result]
	flights      *cache.Group[Result]
	inflight     *cache.Limiter
	computations *cache.Limiter

	cacheTTLShort time.Duration
	cacheTTLLong  time.Duration

	clock   clock.Clock
	metrics *telemetry.Metrics
}

// NewService builds a Service from deps, applying defaults for optional
// fields.
func NewService(deps Deps) *Service {
	c := deps.Clock
	if c == nil {
		c = clock.RealClock{}
	}

	return &Service{
		catalog:       deps.Catalog,
		cluster:       deps.Cluster,
		resolver:      deps.Resolver,
		querier:       deps.Querier,
		cache:         deps.Cache,
		flights:       deps.Flights,
		inflight:      deps.Inflight,
		computations:  deps.Computations,
		cacheTTLShort: deps.CacheTTLShort,
		cacheTTLLong:  deps.CacheTTLLong,
		clock:         c,
		metrics:       deps.Metrics,
	}
}

// Get executes the full request path from design.md §6: parse → cache
// lookup → admission/singleflight → resolve → capture eval time → query →
// validate coverage → build result → cache store → (telemetry throughout).
// Step 1 (delegated authentication/authorization) has already run in
// AdapterBase's filter chain before Get is ever called.
func (s *Service) Get(ctx context.Context, req Request) (Result, error) {
	// Step 1: parse metric syntax and validate catalog membership/resource
	// applicability; metricLabelSelector is unsupported in v1 regardless of
	// named/wildcard (metric-gateway.md §3.6). A nonempty object selector on
	// a named request has no representation in the provider interface
	// itself (GetMetricByName takes no selector), so the framework already
	// prevents that case before this code runs.
	if req.MetricSelector != nil && !req.MetricSelector.Empty() {
		return Result{}, apierrors.NewBadRequest("metricLabelSelector is not supported in v1")
	}

	parsed, ok := s.catalog.Parse(req.Metric)
	if !ok {
		return Result{}, metricNotSupportedError(req)
	}

	spec, ok := kindForGroupResource(req.GroupResource, parsed.Base)
	if !ok {
		return Result{}, metricNotSupportedError(req)
	}

	resourceLabel := string(spec.kind)

	// Step 2: canonicalize both selectors and form the cache key.
	key := cache.Key{
		CatalogRevision:    s.catalog.Revision(),
		Verb:               req.Verb,
		Namespace:          req.Namespace,
		GroupResource:      req.GroupResource,
		ObjectName:         req.Name,
		MetricName:         req.Metric,
		ObjectSelectorHash: cache.HashSelector(req.ObjectSelector),
		MetricSelectorHash: cache.HashSelector(req.MetricSelector),
	}

	// Step 3: an unexpired cache hit returns immediately, preserving
	// object UIDs and evaluation timestamps untouched.
	if result, ok := s.cache.Get(key); ok {
		s.recordCacheHit(resourceLabel, req.Metric)

		return result, nil
	}
	s.recordCacheMiss(resourceLabel, req.Metric)

	releaseInflight, err := s.inflight.TryAcquire()
	if err != nil {
		return Result{}, mapAdmissionError(err)
	}
	defer releaseInflight()

	// Step 4: bounded singleflight, keyed identically to the cache.
	result, shared, err := s.flights.Do(ctx, keyString(key), func(flightCtx context.Context) (Result, error) {
		// Recheck the cache: another flight may have completed and stored
		// a result between our miss above and entering this closure
		// (design.md §6 step 4: "recheck the cache").
		if cached, ok := s.cache.Get(key); ok {
			return cached, nil
		}

		releaseComputation, err := s.computations.TryAcquire()
		if err != nil {
			return Result{}, mapAdmissionError(err)
		}
		defer releaseComputation()

		return s.compute(flightCtx, req, spec, parsed, key)
	})
	if shared {
		s.recordCollapsed()
	}
	if err != nil {
		s.recordQueryError(err)

		return Result{}, err
	}

	return result, nil
}

// compute implements steps 5-8: resolve, capture one aligned evaluation
// time, query every resolved object, build and cache the result.
func (s *Service) compute(ctx context.Context, req Request, spec kindSpec, parsed catalog.ParsedMetric, key cache.Key) (Result, error) {
	target := resolver.Target{
		Kind:           spec.kind,
		Namespace:      req.Namespace,
		Name:           req.Name,
		ObjectSelector: req.ObjectSelector,
	}

	resolutions, err := s.resolver.Resolve(ctx, target)
	if err != nil {
		s.recordCronJobOutcome(spec.kind, err)

		return Result{}, mapResolverError(err)
	}

	// Step 6: query time captured once per computation, aligned to the
	// last fully completed grid step (metric-gateway.md §3.2). Every
	// resolution in this request uses this same instant.
	queryTime := prometheus.AlignToGrid(s.clock.Now())

	items := make([]Item, 0, len(resolutions))

	for _, res := range resolutions {
		s.recordCronJobFallback(spec.kind, res)

		names := identityNames(parsed.Base, res)
		if len(names) == 0 {
			// A workload currently selecting zero pods: no eligible
			// retained members (metric-gateway.md §3.6).
			if req.Name != "" {
				return Result{}, namedObjectNotEligibleError(req)
			}

			continue
		}

		pReq := prometheus.Request{
			Cluster:     s.cluster,
			Series:      parsed.Base.Series,
			Scope:       prometheus.Scope(parsed.Base.Scope),
			Quantity:    quantityFor(parsed.Base.Unit),
			Aggregation: prometheus.Aggregation(parsed.Base.Aggregation),
			Stat:        prometheus.Stat(parsed.Stat),
			Window:      parsed.Window.Duration(),
			Namespace:   req.Namespace,
			Names:       names,
			QueryTime:   queryTime,
		}

		queried, ok, err := s.querier.Query(ctx, pReq)
		if err != nil {
			return Result{}, mapQuerierError(err)
		}
		if !ok {
			// No eligible retained members, or verified inactivity
			// throughout the window (metric-gateway.md §3.6).
			if req.Name != "" {
				return Result{}, namedObjectNotEligibleError(req)
			}

			continue
		}

		items = append(items, Item{
			APIVersion: spec.apiVersion,
			Kind:       string(spec.kind),
			Namespace:  res.Object.Namespace,
			Name:       res.Object.Name,
			UID:        res.Object.UID,
			MetricName: req.Metric,
			Value:      queried.Value,
			Unit:       parsed.Base.Unit,
			Timestamp:  queried.Timestamp,
			Window:     parsed.Window.Duration(),
		})
	}

	result := Result{Items: items}

	// Step 8: cache successful, complete results only — including a valid
	// empty wildcard list — never errors.
	ttl := cache.TTLFor(parsed.Window.Duration(), s.cacheTTLShort, s.cacheTTLLong)
	s.cache.Set(key, result, ttl)

	return result, nil
}

// identityNames returns the exact escaped identity name set a base's
// normalized series query aggregates over: a node-scoped base keys off the
// resolved object's own name directly (there is no "member" concept for a
// Node); a pod-scoped base uses the resolver's retained Pod names.
func identityNames(base catalog.Base, res resolver.Resolution) []string {
	if base.Scope == catalog.ScopeNode {
		return []string{res.Object.Name}
	}

	return res.PodNames
}

func quantityFor(unit catalog.Unit) prometheus.Quantity {
	if unit == catalog.UnitCores {
		return prometheus.QuantityCPU
	}

	return prometheus.QuantityMemory
}

// keyString renders key as a stable string for the singleflight group,
// which — unlike Cache — keys on plain strings.
func keyString(key cache.Key) string {
	return strings.Join([]string{
		key.CatalogRevision,
		key.Verb,
		key.Namespace,
		key.GroupResource.String(),
		key.ObjectName,
		key.MetricName,
		key.ObjectSelectorHash,
		key.MetricSelectorHash,
	}, "\x00")
}

func (s *Service) recordCacheHit(resource, metric string) {
	if s.metrics != nil {
		s.metrics.CacheHits.WithLabelValues(resource, metric).Inc()
	}
}

func (s *Service) recordCacheMiss(resource, metric string) {
	if s.metrics != nil {
		s.metrics.CacheMisses.WithLabelValues(resource, metric).Inc()
	}
}

func (s *Service) recordCollapsed() {
	if s.metrics != nil {
		s.metrics.SingleflightCollapsed.Inc()
	}
}

func (s *Service) recordQueryError(err error) {
	if s.metrics != nil {
		s.metrics.QueryErrors.WithLabelValues(errorReason(err)).Inc()
	}
}

func (s *Service) recordCronJobFallback(kind resolver.Kind, res resolver.Resolution) {
	if s.metrics != nil && kind == resolver.KindCronJob && res.CronJobFallback {
		s.metrics.CronJobFallback.Inc()
	}
}

func (s *Service) recordCronJobOutcome(kind resolver.Kind, err error) {
	if s.metrics == nil || kind != resolver.KindCronJob {
		return
	}
	if _, ok := errors.AsType[*resolver.NotFoundError](err); ok { //nolint:errcheck
		s.metrics.CronJobNotFound.Inc()
	}
}
