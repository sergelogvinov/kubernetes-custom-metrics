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
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/resource"
)

// Resolver is the small interface Service needs — exactly
// (*resolver.Resolver).Resolve's signature, so the real type fits it and
// service_test.go can use a simple fake instead.
type Resolver interface {
	Resolve(ctx context.Context, target resolver.Target) ([]resolver.Resolution, error)
}

// Evaluator is the narrow surface Service depends on — exactly
// (*prometheus.Client).Evaluate's signature.
type Evaluator interface {
	Evaluate(ctx context.Context, comp prometheus.Computation) (prometheus.Evaluation, error)
}

// Deps are Service's collaborators, constructed and wired by cmd/custom-metrics.
type Deps struct {
	Catalog   *catalog.Catalog
	Resolver  Resolver
	Evaluator Evaluator

	Cache        *cache.Cache[Result]
	Flights      *cache.Group[Result]
	Inflight     *cache.Limiter
	Computations *cache.Limiter

	CacheTTLShort time.Duration
	CacheTTLLong  time.Duration

	// Metrics records telemetry (metric-gateway.md §4). A nil Metrics
	// disables recording.
	Metrics *telemetry.Metrics
}

// Service runs the steps of one metric request, independent of the
// provider interface.
type Service struct {
	catalog   *catalog.Catalog
	resolver  Resolver
	evaluator Evaluator

	cache        *cache.Cache[Result]
	flights      *cache.Group[Result]
	inflight     *cache.Limiter
	computations *cache.Limiter

	cacheTTLShort time.Duration
	cacheTTLLong  time.Duration

	metrics *telemetry.Metrics
}

// NewService builds a Service from deps, applying defaults for optional
// fields.
func NewService(deps Deps) *Service {
	return &Service{
		catalog:       deps.Catalog,
		resolver:      deps.Resolver,
		evaluator:     deps.Evaluator,
		cache:         deps.Cache,
		flights:       deps.Flights,
		inflight:      deps.Inflight,
		computations:  deps.Computations,
		cacheTTLShort: deps.CacheTTLShort,
		cacheTTLLong:  deps.CacheTTLLong,
		metrics:       deps.Metrics,
	}
}

// Get runs the full request path: parse → cache
// lookup → admission/singleflight → resolve → evaluate → build result →
// cache store → (telemetry throughout). Step 1 (delegated
// authentication/authorization) has already run in AdapterBase's filter
// chain before Get is ever called. Every failure is a Kubernetes Status
// error (metric-gateway.md §3.6), classified once here.
func (s *Service) Get(ctx context.Context, req Request) (Result, error) {
	result, err := s.get(ctx, req)
	if err != nil {
		reason, status := classify(err)
		s.recordQueryError(reason)

		return Result{}, status
	}

	return result, nil
}

// get is Get with unclassified, domain-typed errors.
func (s *Service) get(ctx context.Context, req Request) (Result, error) {
	// Step 1: parse metric syntax and validate catalog membership/resource
	// applicability; metricLabelSelector is unsupported in v1 regardless of
	// named/wildcard (metric-gateway.md §3.6). A nonempty object selector on
	// a named request has no representation in the provider interface
	// itself (GetMetricByName takes no selector), so the framework already
	// prevents that case before this code runs.
	if req.MetricSelector != nil && !req.MetricSelector.Empty() {
		return Result{}, metricSelectorError{}
	}

	parsed, ok := s.catalog.Parse(req.Metric)
	if !ok {
		return Result{}, &metricNotSupportedError{req: req}
	}

	// An unsupported resource, or a base whose scope does not fit it (for
	// example node_cpu on a Pod), is simply not advertised: 404
	// (metric-gateway.md §2).
	kind, ok := resource.Lookup(req.GroupResource)
	if !ok || kind.Scope() != parsed.Base.Scope {
		return Result{}, &metricNotSupportedError{req: req}
	}

	resourceLabel := string(kind)

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
		return Result{}, err
	}
	defer releaseInflight()

	// Step 4: bounded singleflight, keyed identically to the cache.
	result, shared, err := s.flights.Do(ctx, keyString(key), func(flightCtx context.Context) (Result, error) {
		// Check the cache again: another flight may have finished and
		// stored a result between our miss above and this point.
		if cached, ok := s.cache.Get(key); ok {
			return cached, nil
		}

		releaseComputation, err := s.computations.TryAcquire()
		if err != nil {
			return Result{}, err
		}
		defer releaseComputation()

		return s.compute(flightCtx, req, kind, parsed, key)
	})
	if shared {
		s.recordCollapsed()
	}

	return result, err
}

// compute implements steps 5-8: resolve, evaluate every resolved object
// in one computation, build and cache the result.
func (s *Service) compute(ctx context.Context, req Request, kind resource.Kind, parsed catalog.ParsedMetric, key cache.Key) (Result, error) {
	target := resolver.Target{
		Kind:           kind,
		Namespace:      req.Namespace,
		Name:           req.Name,
		ObjectSelector: req.ObjectSelector,
	}

	resolutions, err := s.resolver.Resolve(ctx, target)
	if err != nil {
		s.recordCronJobOutcome(kind, err)

		return Result{}, err
	}

	targets := make([]prometheus.Target, len(resolutions))
	for i, res := range resolutions {
		s.recordCronJobFallback(kind, res)

		targets[i] = prometheus.Target{Members: res.Members}
	}

	eval, err := s.evaluator.Evaluate(ctx, prometheus.Computation{
		Base:      parsed.Base,
		Stat:      parsed.Stat,
		Window:    parsed.Window.Duration(),
		Namespace: req.Namespace,
		Targets:   targets,
	})
	if err != nil {
		return Result{}, err
	}

	items := make([]Item, 0, len(resolutions))

	for i, sample := range eval.Samples {
		if !sample.Present {
			// No eligible retained members, or verified inactivity
			// throughout the window (metric-gateway.md §3.6).
			if req.Name != "" {
				return Result{}, &notEligibleError{req: req}
			}

			continue
		}

		res := resolutions[i]
		items = append(items, Item{
			APIVersion: kind.APIVersion(),
			Kind:       string(kind),
			Namespace:  res.Object.Namespace,
			Name:       res.Object.Name,
			UID:        res.Object.UID,
			MetricName: req.Metric,
			Value:      sample.Value,
			Unit:       parsed.Base.Unit,
			Timestamp:  eval.Time,
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

func (s *Service) recordQueryError(reason string) {
	if s.metrics != nil {
		s.metrics.QueryErrors.WithLabelValues(reason).Inc()
	}
}

func (s *Service) recordCronJobFallback(kind resource.Kind, res resolver.Resolution) {
	if s.metrics != nil && kind == resource.CronJob && res.CronJobFallback {
		s.metrics.CronJobFallback.Inc()
	}
}

func (s *Service) recordCronJobOutcome(kind resource.Kind, err error) {
	if s.metrics == nil || kind != resource.CronJob {
		return
	}
	if _, ok := errors.AsType[*resolver.NotFoundError](err); ok { //nolint:errcheck
		s.metrics.CronJobNotFound.Inc()
	}
}
