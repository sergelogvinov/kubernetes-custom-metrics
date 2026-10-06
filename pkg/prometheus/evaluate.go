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

package prometheus

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"golang.org/x/sync/errgroup"
)

// Computation is one shared computation: a catalog base's temporal
// statistic over one or more resolved targets (metric-gateway.md §3.2).
type Computation struct {
	Base   catalog.Base
	Stat   catalog.Stat
	Window time.Duration
	// Namespace scopes pod-scoped bases; empty for node scope.
	Namespace string
	Targets   []Target
}

// Target is one resolved object to evaluate. Which field identifies it in
// the backend depends on the base's scope, so callers fill both and never
// choose.
type Target struct {
	// Name is the object's own name: the identity a node-scoped base
	// matches on.
	Name string
	// Pods are the object's retained, deduplicated Pod names: the identity
	// set a pod-scoped base aggregates over.
	Pods []string
}

// Sample is one target's outcome.
type Sample struct {
	Value float64
	// Present is false when the target's metric is genuinely absent: it
	// has no identities to match, no selected identity was active in the
	// window (normalized input), or no sample exists (raw input). Absence
	// is not an error (metric-gateway.md §3.6, §3.7).
	Present bool
}

// Evaluation is a whole Computation's outcome.
type Evaluation struct {
	// Time is the aligned evaluation instant every sample shares
	// (metric-gateway.md §3.2).
	Time time.Time
	// Samples[i] is Computation.Targets[i]'s outcome.
	Samples []Sample
}

// targetLabel is the synthetic label Evaluate attaches to each target's
// expression so one backend query answers every target at once.
const targetLabel = "gateway_target"

// freshnessGateSeconds is the maximum age of an active member's source
// sample (metric-gateway.md §3.7).
const freshnessGateSeconds = 30

// Evaluate computes comp's statistic for every target. It captures one
// aligned evaluation time and issues at most four backend queries for the
// whole computation however many targets it has (metric-gateway.md §6.3):
// usage, any-active, coverage and freshness for normalized input, or a
// single usage query for catalog.AggregationRaw.
//
// It returns an error — one of ErrQueryTooLarge, ErrIncompleteCoverage,
// ErrStaleData, ErrBackend, or a context error wrapped in ErrBackend — if
// any target fails validation; a computation never returns partial
// results.
func (c *Client) Evaluate(ctx context.Context, comp Computation) (Evaluation, error) {
	eval := Evaluation{
		Time:    alignToGrid(c.clock.Now()),
		Samples: make([]Sample, len(comp.Targets)),
	}

	reqs := make([]request, 0, len(comp.Targets))
	index := make([]int, 0, len(comp.Targets))

	for i, target := range comp.Targets {
		names := target.Pods
		if comp.Base.Scope == catalog.ScopeNode {
			names = []string{target.Name}
		}
		if len(names) == 0 || names[0] == "" {
			// No eligible retained members: absent, and never rendered
			// as an unrestricted query (metric-gateway.md §3.3).
			continue
		}

		reqs = append(reqs, request{
			Cluster:     c.cluster,
			Series:      comp.Base.Series,
			Scope:       comp.Base.Scope,
			Quantity:    quantityFor(comp.Base.Unit),
			Aggregation: comp.Base.Aggregation,
			Stat:        comp.Stat,
			Window:      comp.Window,
			Namespace:   comp.Namespace,
			Names:       names,
		})
		index = append(index, i)
	}

	if len(reqs) == 0 {
		return eval, nil
	}

	var (
		samples []Sample
		err     error
	)
	if comp.Base.Aggregation == catalog.AggregationRaw {
		samples, err = c.evaluateRaw(ctx, reqs, eval.Time)
	} else {
		samples, err = c.evaluateNormalized(ctx, reqs, eval.Time)
	}
	if err != nil {
		return Evaluation{}, err
	}

	for j, i := range index {
		eval.Samples[i] = samples[j]
	}

	return eval, nil
}

// evaluateNormalized issues the four validation-bearing queries
// concurrently and classifies each target: absent when never active,
// ErrIncompleteCoverage or ErrStaleData when its signals fail, otherwise
// its usage value.
func (c *Client) evaluateNormalized(ctx context.Context, reqs []request, ts time.Time) ([]Sample, error) {
	renderers := []func(request) (string, error){renderUsage, renderAnyActive, renderCoverage, renderFreshness}

	queries := make([]string, len(renderers))
	for i, render := range renderers {
		q, err := renderBatch(reqs, render)
		if err != nil {
			return nil, err
		}
		queries[i] = q
	}

	results := make([]targetValues, len(queries))

	group, groupCtx := errgroup.WithContext(ctx)
	for i, q := range queries {
		group.Go(func() error {
			values, err := c.runByTarget(groupCtx, q, ts, len(reqs))
			results[i] = values

			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	usage, anyActive, coverage, freshness := results[0], results[1], results[2], results[3]

	samples := make([]Sample, len(reqs))
	for j := range reqs {
		if v, ok := anyActive.get(j); !ok || v <= 0 {
			// No selected identity was ever active in the window: a
			// genuinely absent metric (metric-gateway.md §3.7).
			continue
		}
		if v, ok := coverage.get(j); ok && v > 0 {
			return nil, ErrIncompleteCoverage
		}
		if v, ok := freshness.get(j); ok && v > freshnessGateSeconds {
			return nil, ErrStaleData
		}

		v, ok := usage.get(j)
		if !ok {
			// An active member exists but the usage series itself has no
			// sample: incomplete coverage, not silently "no data".
			return nil, ErrIncompleteCoverage
		}
		if err := requireNonNegative(v); err != nil {
			return nil, err
		}

		samples[j] = Sample{Value: v, Present: true}
	}

	return samples, nil
}

// evaluateRaw issues the single raw usage query. With no active signal,
// "no sample" and "genuinely absent" are indistinguishable: both are
// absent (renderRawUsage).
func (c *Client) evaluateRaw(ctx context.Context, reqs []request, ts time.Time) ([]Sample, error) {
	query, err := renderBatch(reqs, renderRawUsage)
	if err != nil {
		return nil, err
	}

	usage, err := c.runByTarget(ctx, query, ts, len(reqs))
	if err != nil {
		return nil, err
	}

	samples := make([]Sample, len(reqs))
	for j := range reqs {
		v, ok := usage.get(j)
		if !ok {
			continue
		}
		if err := requireNonNegative(v); err != nil {
			return nil, err
		}

		samples[j] = Sample{Value: v, Present: true}
	}

	return samples, nil
}

// renderBatch renders one expression per request, labels each with its
// position under targetLabel, and unions them into a single query. Every
// per-target expression aggregates without a by() clause, so each
// contributes at most one series and the labels never collide.
func renderBatch(reqs []request, render func(request) (string, error)) (string, error) {
	parts := make([]string, len(reqs))
	for j, req := range reqs {
		expr, err := render(req)
		if err != nil {
			return "", err
		}
		parts[j] = fmt.Sprintf(`label_replace(%s, %q, %q, "", "")`, expr, targetLabel, strconv.Itoa(j))
	}

	query := strings.Join(parts, " or ")
	if err := checkQuerySize(query); err != nil {
		return "", err
	}

	return query, nil
}
