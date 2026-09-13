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
	"errors"
	"sync"
	"testing"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
)

func TestLimiter_AdmitsUpToMax(t *testing.T) {
	l := cache.NewLimiter("inflight-requests", 2)

	release1, err := l.TryAcquire()
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	release2, err := l.TryAcquire()
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	if got := l.InUse(); got != 2 {
		t.Errorf("InUse() = %d, want 2", got)
	}

	_, err = l.TryAcquire()
	var rejected *cache.AdmissionRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("acquire 3 err = %v, want *AdmissionRejectedError", err)
	}
	if rejected.Limit != "inflight-requests" || rejected.Max != 2 {
		t.Errorf("rejected = %+v, want Limit=inflight-requests Max=2", rejected)
	}

	release1()
	if got := l.InUse(); got != 1 {
		t.Errorf("InUse() after one release = %d, want 1", got)
	}

	if _, err := l.TryAcquire(); err != nil {
		t.Errorf("acquire after release: %v, want nil (a freed slot must become available)", err)
	}

	release2()
}

func TestLimiter_ReleaseIsIdempotent(t *testing.T) {
	l := cache.NewLimiter("shared-computations", 1)

	release, err := l.TryAcquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	release()
	release() // must not double-decrement

	if got := l.InUse(); got != 0 {
		t.Errorf("InUse() after double release = %d, want 0", got)
	}
}

func TestLimiter_ConcurrentAcquireRelease(t *testing.T) {
	const maxSlots = 8
	l := cache.NewLimiter("inflight-requests", maxSlots)

	var wg sync.WaitGroup

	for range 200 {
		wg.Go(func() {
			release, err := l.TryAcquire()
			if err != nil {
				return
			}
			if got := l.InUse(); got > maxSlots {
				t.Errorf("InUse() = %d, want <= %d", got, maxSlots)
			}
			release()
		})
	}
	wg.Wait()

	if got := l.InUse(); got != 0 {
		t.Errorf("InUse() after all goroutines finished = %d, want 0", got)
	}
}
