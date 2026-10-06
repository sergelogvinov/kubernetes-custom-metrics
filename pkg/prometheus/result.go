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
	"fmt"
	"math"
	"strconv"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// targetValues holds one decoded batch query's value per target position;
// a missing entry means that target's expression returned no series.
type targetValues map[int]float64

func (v targetValues) get(j int) (float64, bool) {
	value, ok := v[j]

	return value, ok
}

// decodeByTarget validates and decodes a batch query response (renderBatch)
// into one value per target position in [0, n).
//
// A target with no series is simply missing (an absent signal, not an
// error). It returns a non-nil error — wrapping ErrBackend — for protocol
// errors, warnings/partial data, unexpected result types, a series without
// a valid target label, duplicate series for one target, or non-finite
// values (design.md §8).
func decodeByTarget(value model.Value, warnings promv1.Warnings, err error, n int) (targetValues, error) {
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackend, err)
	}
	if len(warnings) > 0 {
		return nil, fmt.Errorf("%w: warnings: %v", ErrBackend, []string(warnings))
	}

	vector, ok := value.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("%w: expected a vector result, got %T", ErrBackend, value)
	}

	values := make(targetValues, len(vector))
	for _, sample := range vector {
		label := string(sample.Metric[targetLabel])

		j, err := strconv.Atoi(label)
		if err != nil || j < 0 || j >= n {
			return nil, fmt.Errorf("%w: series with unexpected %s=%q", ErrBackend, targetLabel, label)
		}
		if _, dup := values[j]; dup {
			return nil, fmt.Errorf("%w: more than one series for target %d (duplicate identities?)", ErrBackend, j)
		}

		v := float64(sample.Value)
		if math.IsNaN(v) {
			// Prometheus renders a filtered-out comparison as an absent
			// series, not NaN, so a literal NaN here is a genuine
			// anomaly, not "no data".
			return nil, fmt.Errorf("%w: NaN value", ErrBackend)
		}
		if math.IsInf(v, 0) {
			return nil, fmt.Errorf("%w: non-finite (Inf) value", ErrBackend)
		}

		values[j] = v
	}

	return values, nil
}

// resultCount reports how many series/samples value carries, for logging
// (a well-formed batch query returns at most one per target — see
// decodeByTarget).
func resultCount(value model.Value) int {
	switch v := value.(type) {
	case model.Vector:
		return len(v)
	case model.Matrix:
		return len(v)
	case *model.Scalar:
		return 1
	case *model.String:
		return 1
	default:
		return 0
	}
}

// requireNonNegative rejects a negative usage value rather than clamping
// it (metric-gateway.md §3.2: "Reject negative, nonfinite, and overflowing
// results rather than clamping them").
func requireNonNegative(v float64) error {
	if v < 0 {
		return fmt.Errorf("%w: negative value %v", ErrBackend, v)
	}

	return nil
}
