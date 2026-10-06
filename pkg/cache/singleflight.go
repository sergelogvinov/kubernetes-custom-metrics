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
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Group joins identical computations that run at the same time, by key
// (metric-gateway.md §4 Tier 2). It is kept separate from Cache on purpose:
// the two have different lifetimes and tests.
//
// Unlike a bare singleflight.Group, the function executing a shared
// computation always runs with a context Group itself owns — derived from
// Group's base context (bounded by server shutdown) and a fixed timeout
// (--request-timeout) — never from whichever caller happened to trigger
// it. Each caller's own context only governs how long that caller waits;
// it never cancels the shared computation or other waiters (metric-gateway.md
// §4: "Each waiter can cancel independently ... one disconnected caller
// must not cancel other waiters"). The slot for a given key is held until
// its computation ends, even if every waiter's own context is canceled
// first (metric-gateway.md §6.3).
type Group[V any] struct {
	base    context.Context //nolint:containedctx // deliberately held: the shared-computation parent, not a per-call context
	timeout time.Duration

	mu      sync.Mutex
	flights map[string]*flight

	sf singleflight.Group
}

type flight struct {
	ctx    context.Context //nolint:containedctx // one flight's shared, gateway-owned computation context
	cancel context.CancelFunc
}

// NewGroup builds a Group whose shared computations are derived from base
// (canceled on server shutdown) with requestTimeout bounding each
// individual flight's total duration.
func NewGroup[V any](base context.Context, requestTimeout time.Duration) *Group[V] {
	return &Group[V]{
		base:    base,
		timeout: requestTimeout,
		flights: make(map[string]*flight),
	}
}

// Do executes fn at most once per concurrently-requested key, or joins an
// in-flight call for the same key. callerCtx governs only this specific
// waiter: if callerCtx is canceled first, Do returns callerCtx.Err() for
// this caller without affecting the shared computation or any other
// waiter. shared reports whether the result was (or would have been)
// shared with at least one other caller.
func (g *Group[V]) Do(callerCtx context.Context, key string, fn func(ctx context.Context) (V, error)) (value V, shared bool, err error) {
	g.mu.Lock()
	fl, exists := g.flights[key]
	if !exists {
		ctx, cancel := context.WithTimeout(g.base, g.timeout)
		fl = &flight{ctx: ctx, cancel: cancel}
		g.flights[key] = fl
	}
	g.mu.Unlock()

	resultCh := g.sf.DoChan(key, func() (any, error) {
		defer func() {
			g.mu.Lock()
			if current, ok := g.flights[key]; ok && current == fl {
				delete(g.flights, key)
			}
			g.mu.Unlock()
			fl.cancel()
		}()

		return fn(fl.ctx)
	})

	select {
	case res := <-resultCh:
		if res.Err != nil {
			var zero V

			return zero, res.Shared, res.Err
		}
		v, _ := res.Val.(V)

		return v, res.Shared, nil

	case <-callerCtx.Done():
		var zero V

		return zero, false, callerCtx.Err()
	}
}
