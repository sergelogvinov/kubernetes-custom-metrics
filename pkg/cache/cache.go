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

// Package cache provides the gateway's in-memory response cache, joining
// of duplicate requests, and admission limits (metric-gateway.md §4, §6.3).
// It does not know what it stores: the cache is generic over the value
// type, so tests can use simple stand-in values.
package cache

import (
	"container/list"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// MaxEntryBytes bounds one cacheable value; larger results are computed
// and returned normally but never stored (metric-gateway.md §4: "Entries
// larger than 1 MiB are returned but not cached").
const MaxEntryBytes = 1 << 20

// Defaults matching metric-gateway.md §6.1's --cache-size/--cache-max-bytes
// and --cache-ttl-short/--cache-ttl-long flags, and the window threshold
// between them (§4 Tier 1 table).
const (
	DefaultMaxEntries  = 10000
	DefaultMaxBytes    = 64 << 20
	ShortWindowTTL     = 15 * time.Second
	LongWindowTTL      = 10 * time.Minute
	WindowTTLThreshold = time.Hour
)

// TTLFor returns shortTTL for window<=1h, longTTL otherwise
// (metric-gateway.md §4 Tier 1 table). shortTTL/longTTL are passed in
// rather than hardcoded so callers can use their configured
// --cache-ttl-short/--cache-ttl-long values.
func TTLFor(window time.Duration, shortTTL, longTTL time.Duration) time.Duration {
	if window <= WindowTTLThreshold {
		return shortTTL
	}

	return longTTL
}

// Cache is a size- and byte-bounded, TTL-expiring LRU keyed by Key.
//
// It caches successful, complete results only; callers must never Set an
// error result (metric-gateway.md §4: "Cache successful complete results
// only ... Do not cache errors"). A Get hit returns the stored value
// unmodified — hits never rewrite timestamps or other content. Cache
// treats V as immutable once Set: if V contains pointers or slices,
// callers must not change a value obtained from Get or passed to Set.
// This package does not copy values; it relies on callers never changing
// them.
type Cache[V any] struct {
	clock      clock.Clock
	maxEntries int
	maxBytes   int64
	sizer      func(V) int64

	mu    sync.Mutex
	order *list.List
	items map[Key]*list.Element
	bytes int64
}

type entry[V any] struct {
	key       Key
	value     V
	size      int64
	expiresAt time.Time
}

// Option configures a Cache.
type Option[V any] func(*Cache[V])

// WithClock overrides the time source used for TTL expiry. Tests inject a
// fake clock and advance it explicitly; production uses the default
// clock.RealClock.
func WithClock[V any](c clock.Clock) Option[V] {
	return func(cache *Cache[V]) { cache.clock = c }
}

// New builds a Cache bounded by maxEntries and accounted maxBytes, sizing
// each value with sizer.
func New[V any](maxEntries int, maxBytes int64, sizer func(V) int64, opts ...Option[V]) *Cache[V] {
	c := &Cache[V]{
		clock:      clock.RealClock{},
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		sizer:      sizer,
		order:      list.New(),
		items:      make(map[Key]*list.Element),
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// Get returns the cached value for key if present and unexpired. A miss
// (absent or lazily-expired) returns the zero value and false.
func (c *Cache[V]) Get(key Key) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		var zero V

		return zero, false
	}

	e, _ := elem.Value.(*entry[V])
	if !e.expiresAt.After(c.clock.Now()) {
		c.removeElement(elem)

		var zero V

		return zero, false
	}

	c.order.MoveToFront(elem)

	return e.value, true
}

// Set stores value under key with the given ttl, evicting the
// least-recently-used entries as needed to respect the entry and byte
// caps. A value larger than MaxEntryBytes is silently not cached — the
// caller still uses its own already-computed value; only the cache entry
// is skipped.
func (c *Cache[V]) Set(key Key, value V, ttl time.Duration) {
	size := c.sizer(value)
	if size > MaxEntryBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.removeElement(elem)
	}

	e := &entry[V]{key: key, value: value, size: size, expiresAt: c.clock.Now().Add(ttl)}
	elem := c.order.PushFront(e)
	c.items[key] = elem
	c.bytes += size

	c.evict()
}

func (c *Cache[V]) evict() {
	for c.order.Len() > c.maxEntries || c.bytes > c.maxBytes {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.removeElement(back)
	}
}

func (c *Cache[V]) removeElement(elem *list.Element) {
	e, _ := elem.Value.(*entry[V])
	delete(c.items, e.key)
	c.order.Remove(elem)
	c.bytes -= e.size
}

// Len returns the current number of entries, including ones that are
// lazily expired but not yet evicted. Test/metrics use only.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.order.Len()
}

// Bytes returns the current accounted byte total. Test/metrics use only.
func (c *Cache[V]) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.bytes
}
