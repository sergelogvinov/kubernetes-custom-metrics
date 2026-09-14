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

package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// blockingRoundTripper simulates a stalled backend: it never returns until
// the request's context is done, and signals started once it is in flight.
type blockingRoundTripper struct {
	started chan struct{}
}

func (b *blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	close(b.started)
	<-req.Context().Done()

	return nil, req.Context().Err()
}

func TestCancelTransport_CancelsInFlightRoundTrip(t *testing.T) {
	base := &blockingRoundTripper{started: make(chan struct{})}
	transport := &cancelTransport{base: base}

	ctx, cancel := context.WithCancel(context.Background())
	unbind := transport.bind(ctx)
	defer unbind()

	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid", nil)
		if err != nil {
			done <- err

			return
		}

		_, err = transport.RoundTrip(req)
		done <- err
	}()

	select {
	case <-base.started:
	case <-time.After(time.Second):
		t.Fatal("round trip never started")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RoundTrip() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RoundTrip did not return after the bound context was canceled")
	}
}

type recordingRoundTripper struct {
	gotCtx context.Context
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.gotCtx = req.Context()

	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func TestCancelTransport_UnbindStopsUsingTheOldContext(t *testing.T) {
	base := &recordingRoundTripper{}
	transport := &cancelTransport{base: base}

	boundCtx, cancel := context.WithCancel(context.Background())
	unbind := transport.bind(boundCtx)
	cancel()
	unbind()

	reqCtx := context.Background()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://example.invalid", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}

	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}

	if base.gotCtx != reqCtx { //nolint:staticcheck // identity comparison is the point of the test
		t.Error("RoundTrip() used the previously bound (now canceled) context after unbind; want the request's own context")
	}
}
