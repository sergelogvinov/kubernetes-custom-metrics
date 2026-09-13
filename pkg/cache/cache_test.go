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

package cache_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	clocktesting "k8s.io/utils/clock/testing"
)

// testValue is the synthetic Result stand-in this package's tests use
// (plan.md T5: "this task can develop and test against synthetic Result
// stand-ins").
type testValue struct {
	id   string
	size int64
}

func sizeOf(v testValue) int64 { return v.size }

func keyN(n int) cache.Key {
	return cache.Key{ObjectName: fmt.Sprintf("obj-%d", n)}
}

func TestCache_SetThenGet(t *testing.T) {
	c := cache.New(10, 1<<20, sizeOf)

	c.Set(keyN(1), testValue{id: "a", size: 10}, time.Minute)

	got, ok := c.Get(keyN(1))
	if !ok {
		t.Fatal("Get() ok = false, want true")
	}
	if got.id != "a" {
		t.Errorf("Get() = %+v, want id=a", got)
	}
}

func TestCache_MissForAbsentKey(t *testing.T) {
	c := cache.New(10, 1<<20, sizeOf)

	if _, ok := c.Get(keyN(1)); ok {
		t.Error("Get() ok = true for a key that was never Set")
	}
}

func TestCache_TTLExpiryUsesInjectedClock(t *testing.T) {
	fakeClock := clocktesting.NewFakeClock(time.Unix(1_700_000_000, 0))
	c := cache.New(10, 1<<20, sizeOf, cache.WithClock[testValue](fakeClock))

	c.Set(keyN(1), testValue{id: "a", size: 1}, 15*time.Second)

	if _, ok := c.Get(keyN(1)); !ok {
		t.Fatal("Get() immediately after Set() ok = false, want true")
	}

	fakeClock.Step(14 * time.Second)
	if _, ok := c.Get(keyN(1)); !ok {
		t.Error("Get() at 14s (before TTL) ok = false, want true")
	}

	fakeClock.Step(2 * time.Second) // now at 16s, past the 15s TTL
	if _, ok := c.Get(keyN(1)); ok {
		t.Error("Get() at 16s (after TTL) ok = true, want false")
	}

	// The expired entry is lazily evicted by the failed Get.
	if got := c.Len(); got != 0 {
		t.Errorf("Len() after expiry = %d, want 0", got)
	}
}

func TestCache_DoesNotRewriteValueOnHit(t *testing.T) {
	fakeClock := clocktesting.NewFakeClock(time.Unix(1_700_000_000, 0))
	c := cache.New(10, 1<<20, sizeOf, cache.WithClock[testValue](fakeClock))

	original := testValue{id: "original", size: 1}
	c.Set(keyN(1), original, time.Minute)

	fakeClock.Step(30 * time.Second)

	got, ok := c.Get(keyN(1))
	if !ok || got.id != "original" {
		t.Errorf("Get() = %+v, ok=%v, want unchanged original value", got, ok)
	}
}

func TestCache_EntryCapEvictsLeastRecentlyUsed(t *testing.T) {
	c := cache.New(3, 1<<30, sizeOf)

	c.Set(keyN(1), testValue{id: "1", size: 1}, time.Minute)
	c.Set(keyN(2), testValue{id: "2", size: 1}, time.Minute)
	c.Set(keyN(3), testValue{id: "3", size: 1}, time.Minute)

	// Touch key 1 so key 2 becomes the least recently used.
	if _, ok := c.Get(keyN(1)); !ok {
		t.Fatal("expected key 1 present before eviction")
	}

	c.Set(keyN(4), testValue{id: "4", size: 1}, time.Minute)

	if c.Len() != 3 {
		t.Errorf("Len() = %d, want 3 (bounded by entry cap)", c.Len())
	}
	if _, ok := c.Get(keyN(2)); ok {
		t.Error("key 2 (least recently used) survived eviction")
	}
	for _, k := range []int{1, 3, 4} {
		if _, ok := c.Get(keyN(k)); !ok {
			t.Errorf("key %d evicted unexpectedly", k)
		}
	}
}

func TestCache_ByteCapEvicts(t *testing.T) {
	c := cache.New(100, 25, sizeOf)

	c.Set(keyN(1), testValue{id: "1", size: 10}, time.Minute)
	c.Set(keyN(2), testValue{id: "2", size: 10}, time.Minute)
	c.Set(keyN(3), testValue{id: "3", size: 10}, time.Minute) // pushes total to 30 > 25

	if got := c.Bytes(); got > 25 {
		t.Errorf("Bytes() = %d, want <= 25", got)
	}
	if _, ok := c.Get(keyN(1)); ok {
		t.Error("key 1 (oldest) survived byte-cap eviction")
	}
	if _, ok := c.Get(keyN(3)); !ok {
		t.Error("key 3 (newest) was evicted, want it retained")
	}
}

func TestCache_OversizedEntryNotCached(t *testing.T) {
	c := cache.New(100, 1<<30, sizeOf)

	c.Set(keyN(1), testValue{id: "big", size: cache.MaxEntryBytes + 1}, time.Minute)

	if _, ok := c.Get(keyN(1)); ok {
		t.Error("an entry over MaxEntryBytes was cached, want it skipped")
	}
	if got := c.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestCache_BoundsHoldUnderConcurrentLoad(t *testing.T) {
	const maxEntries = 20
	const maxBytes = 200
	c := cache.New(maxEntries, maxBytes, sizeOf)

	var wg sync.WaitGroup
	for i := range 500 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Set(keyN(i%50), testValue{id: fmt.Sprintf("v%d", i), size: 5}, time.Minute)
		}(i)
	}
	wg.Wait()

	if got := c.Len(); got > maxEntries {
		t.Errorf("Len() = %d, want <= %d after concurrent load", got, maxEntries)
	}
	if got := c.Bytes(); got > maxBytes {
		t.Errorf("Bytes() = %d, want <= %d after concurrent load", got, maxBytes)
	}
}

func TestCache_ConcurrentGetSet(t *testing.T) {
	c := cache.New(50, 1<<20, sizeOf)

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c.Set(keyN(i%20), testValue{id: fmt.Sprintf("v%d", i), size: 1}, time.Minute)
		}(i)
		go func(i int) {
			defer wg.Done()
			c.Get(keyN(i % 20))
		}(i)
	}
	wg.Wait()
}

func TestTTLFor(t *testing.T) {
	short, long := 15*time.Second, 10*time.Minute

	cases := []struct {
		window time.Duration
		want   time.Duration
	}{
		{time.Minute, short},
		{time.Hour, short},
		{time.Hour + time.Second, long},
		{24 * time.Hour, long},
	}
	for _, tc := range cases {
		if got := cache.TTLFor(tc.window, short, long); got != tc.want {
			t.Errorf("TTLFor(%s) = %s, want %s", tc.window, got, tc.want)
		}
	}
}
