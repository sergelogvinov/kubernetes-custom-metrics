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
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
)

func TestGroup_CollapsesConcurrentCalls(t *testing.T) {
	g := cache.NewGroup[int](context.Background(), time.Minute)

	var calls atomic.Int32
	start := make(chan struct{})

	fn := func(ctx context.Context) (int, error) {
		calls.Add(1)
		<-start

		return 42, nil
	}

	const waiters = 10
	results := make([]int, waiters)

	var wg sync.WaitGroup
	wg.Add(waiters)
	for i := range waiters {
		go func(i int) {
			defer wg.Done()
			v, err := groupDo(t, g, "key", fn)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			results[i] = v
		}(i)
	}

	// Give every goroutine a chance to join the same flight before letting
	// fn complete.
	time.Sleep(50 * time.Millisecond)
	close(start)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("fn called %d times, want exactly 1 (all callers should collapse)", got)
	}
	for i, v := range results {
		if v != 42 {
			t.Errorf("caller %d result = %d, want 42", i, v)
		}
	}
}

// groupDo is a small helper so TestGroup_CollapsesConcurrentCalls's inner
// goroutines can call Do without repeating its 3-return-value signature at
// every call site.
func groupDo(t *testing.T, g *cache.Group[int], key string, fn func(context.Context) (int, error)) (int, error) {
	t.Helper()
	v, _, err := g.Do(context.Background(), key, fn)

	return v, err
}

func TestGroup_DifferentKeysDoNotCollapse(t *testing.T) {
	g := cache.NewGroup[int](context.Background(), time.Minute)

	var calls atomic.Int32
	fn := func(ctx context.Context) (int, error) {
		return int(calls.Add(1)), nil
	}

	v1, _, err1 := g.Do(context.Background(), "key-1", fn)
	v2, _, err2 := g.Do(context.Background(), "key-2", fn)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v, %v", err1, err2)
	}
	if v1 == v2 {
		t.Errorf("distinct keys shared a result: %d == %d", v1, v2)
	}
}

func TestGroup_DisconnectedCallerDoesNotCancelOthers(t *testing.T) {
	g := cache.NewGroup[int](context.Background(), time.Minute)

	release := make(chan struct{})
	fnCtxDone := make(chan struct{}, 1)

	fn := func(ctx context.Context) (int, error) {
		<-release
		select {
		case <-ctx.Done():
			fnCtxDone <- struct{}{}
		default:
		}

		return 7, nil
	}

	// Caller A joins first with a context it will cancel early.
	callerACtx, cancelA := context.WithCancel(context.Background())
	aDone := make(chan struct{})
	var aErr error
	go func() {
		defer close(aDone)
		_, _, aErr = g.Do(callerACtx, "key", fn)
	}()

	// Give A time to register as the first (triggering) caller.
	time.Sleep(20 * time.Millisecond)
	cancelA()
	<-aDone
	if !errors.Is(aErr, context.Canceled) {
		t.Errorf("caller A err = %v, want context.Canceled", aErr)
	}

	// Caller B joins the same still-in-flight key after A gave up.
	bDone := make(chan struct{})
	var bVal int
	var bErr error
	go func() {
		defer close(bDone)
		bVal, _, bErr = g.Do(context.Background(), "key", fn)
	}()

	time.Sleep(20 * time.Millisecond)
	close(release) // let fn finish
	<-bDone

	if bErr != nil {
		t.Fatalf("caller B err = %v, want nil (shared computation must survive A's disconnect)", bErr)
	}
	if bVal != 7 {
		t.Errorf("caller B value = %d, want 7", bVal)
	}
	select {
	case <-fnCtxDone:
		t.Error("fn observed its context as Done — caller A's cancellation leaked into the shared computation")
	default:
	}
}

func TestGroup_SharedContextBoundedByRequestTimeout(t *testing.T) {
	g := cache.NewGroup[int](context.Background(), 20*time.Millisecond)

	started := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		close(started)
		<-ctx.Done()

		return 0, ctx.Err()
	}

	_, _, err := g.Do(context.Background(), "key", fn)
	<-started

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestGroup_SharedContextBoundedByBaseCancellation(t *testing.T) {
	base, cancelBase := context.WithCancel(context.Background())
	g := cache.NewGroup[int](base, time.Minute)

	started := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		close(started)
		<-ctx.Done()

		return 0, ctx.Err()
	}

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, _, err = g.Do(context.Background(), "key", fn)
	}()

	<-started
	cancelBase()
	<-done

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled (server shutdown must bound shared work)", err)
	}
}

func TestGroup_CompletesEvenIfAllCallersDisconnect(t *testing.T) {
	g := cache.NewGroup[int](context.Background(), time.Minute)

	completed := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		time.Sleep(50 * time.Millisecond)
		close(completed)

		return 9, nil
	}

	callerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Do(callerCtx, "key", fn) //nolint:errcheck // result unused: this test only cares that fn itself runs to completion
	}()

	time.Sleep(5 * time.Millisecond)
	cancel() // the only caller disconnects almost immediately
	<-done

	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Error("shared computation did not run to completion after its only caller disconnected")
	}
}
