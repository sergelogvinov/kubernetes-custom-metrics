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

package cache

import (
	"fmt"
	"sync"
	"time"
)

// RetryAfter is the fixed backoff hint metric-gateway.md §6.3 specifies
// for both admission limits ("reject ... with 429 and Retry-After: 1").
const RetryAfter = time.Second

// AdmissionRejectedError means a Limiter was already at capacity. The
// gateway maps this to HTTP 429 with a Retry-After: 1 header
// (metric-gateway.md §6.3).
type AdmissionRejectedError struct {
	// Limit names which budget was exhausted, e.g. "inflight-requests" or
	// "shared-computations".
	Limit string
	// Max is the configured capacity that was exhausted.
	Max int
}

func (e *AdmissionRejectedError) Error() string {
	return fmt.Sprintf("cache: %s admission limit (%d) exceeded", e.Limit, e.Max)
}

// Limiter is a simple counting admission gate: at most Max concurrent
// holders of a slot at any time, with an explicit Release — no queuing.
// Two independent instances back --max-inflight-requests (every admitted
// metric request, including waiters joining an existing flight) and
// --max-shared-computations (only newly created flights; joining an
// existing one needs no new slot) per metric-gateway.md §6.3.
type Limiter struct {
	name string
	max  int

	mu      sync.Mutex
	current int
}

// NewLimiter builds a Limiter admitting at most maxSlots concurrent
// holders. name is used only in AdmissionRejectedError.Limit.
func NewLimiter(name string, maxSlots int) *Limiter {
	return &Limiter{name: name, max: maxSlots}
}

// TryAcquire attempts to reserve one slot. It returns nil and holds the
// slot (release it exactly once via the returned func) on success, or a
// non-nil *AdmissionRejectedError if the limiter is already at capacity.
func (l *Limiter) TryAcquire() (release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.current >= l.max {
		return nil, &AdmissionRejectedError{Limit: l.name, Max: l.max}
	}

	l.current++

	var once sync.Once

	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.current--
			l.mu.Unlock()
		})
	}, nil
}

// InUse returns the current number of held slots. Test/metrics use only.
func (l *Limiter) InUse() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.current
}
