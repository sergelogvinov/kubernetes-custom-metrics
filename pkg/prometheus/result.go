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

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// decodeSingle validates and decodes a Prometheus query response that is
// expected to collapse to at most one unlabeled series (every query this
// package renders wraps its outermost aggregation without a `by()` clause,
// so a well-formed backend never returns more than one item).
//
// It returns (0, false, nil) for a genuinely empty result (the query found
// no matching data — an absent signal, not an error), (value, true, nil)
// for exactly one finite sample, and a non-nil error — wrapping ErrBackend
// — for protocol errors, warnings/partial data, unexpected result types,
// duplicate identities (more than one series), or non-finite values
// (design.md §8).
func decodeSingle(value model.Value, warnings promv1.Warnings, err error) (float64, bool, error) {
	if err != nil {
		return 0, false, fmt.Errorf("%w: %v", ErrBackend, err)
	}
	if len(warnings) > 0 {
		return 0, false, fmt.Errorf("%w: warnings: %v", ErrBackend, []string(warnings))
	}

	vector, ok := value.(model.Vector)
	if !ok {
		return 0, false, fmt.Errorf("%w: expected a vector result, got %T", ErrBackend, value)
	}

	switch len(vector) {
	case 0:
		return 0, false, nil
	case 1:
		v := float64(vector[0].Value)
		if math.IsNaN(v) {
			// Prometheus renders a filtered-out comparison as an absent
			// series, not NaN, so a literal NaN here is a genuine
			// anomaly, not "no data".
			return 0, false, fmt.Errorf("%w: NaN value", ErrBackend)
		}
		if math.IsInf(v, 0) {
			return 0, false, fmt.Errorf("%w: non-finite (Inf) value", ErrBackend)
		}

		return v, true, nil
	default:
		return 0, false, fmt.Errorf("%w: expected exactly one series, got %d (duplicate identities?)", ErrBackend, len(vector))
	}
}

// resultCount reports how many series/samples value carries, for logging
// (metric-gateway.md §8: a well-formed query never returns more than one,
// so a larger count is itself a useful diagnostic — see decodeSingle).
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
