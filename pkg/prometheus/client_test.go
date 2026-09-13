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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockResult describes how the fake Prometheus server should answer one
// specific rendered query.
type mockResult struct {
	values     []string // vector sample values; nil/empty means an empty vector
	warnings   []string
	errorType  string
	errorMsg   string
	httpStatus int
	sleep      time.Duration
}

func newMockPrometheus(t *testing.T, byQuery map[string]mockResult) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		query := r.FormValue("query")

		res, ok := byQuery[query]
		if !ok {
			t.Errorf("mock server: unexpected query: %s", query)
			http.Error(w, "unexpected query", http.StatusInternalServerError)

			return
		}
		if res.sleep > 0 {
			time.Sleep(res.sleep)
		}

		w.Header().Set("Content-Type", "application/json")
		if res.httpStatus != 0 && res.httpStatus != http.StatusOK {
			w.WriteHeader(res.httpStatus)
		}

		body := map[string]any{}
		if res.errorType != "" {
			body["status"] = "error"
			body["errorType"] = res.errorType
			body["error"] = res.errorMsg
		} else {
			result := make([]map[string]any, 0, len(res.values))
			for _, v := range res.values {
				result = append(result, map[string]any{
					"metric": map[string]string{},
					"value":  []any{1_700_000_000.0, v},
				})
			}
			body["status"] = "success"
			if len(res.warnings) > 0 {
				body["warnings"] = res.warnings
			}
			body["data"] = map[string]any{
				"resultType": "vector",
				"result":     result,
			}
		}

		_ = json.NewEncoder(w).Encode(body)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()

	client, err := NewClient(ClientConfig{URL: url, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	return client
}

// queriesFor renders the four sub-queries Client.Query issues for req, for
// use as map keys when building a mock server's expected responses.
func queriesFor(t *testing.T, req Request) (usage, anyActive, coverage, freshness string) {
	t.Helper()

	var err error
	if usage, err = renderUsage(req); err != nil {
		t.Fatalf("renderUsage: %v", err)
	}
	if anyActive, err = renderAnyActive(req); err != nil {
		t.Fatalf("renderAnyActive: %v", err)
	}
	if coverage, err = renderCoverage(req); err != nil {
		t.Fatalf("renderCoverage: %v", err)
	}
	if freshness, err = renderFreshness(req); err != nil {
		t.Fatalf("renderFreshness: %v", err)
	}

	return usage, anyActive, coverage, freshness
}

func TestClient_Query_Success(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"1.5"}},
		anyActive: {values: []string{"1"}},
		coverage:  {}, // empty: no bad members
		freshness: {values: []string{"2"}},
	})

	result, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !ok {
		t.Fatal("Query ok = false, want true")
	}
	if result.Value != 1.5 {
		t.Errorf("Value = %v, want 1.5", result.Value)
	}
	if !result.Timestamp.Equal(req.QueryTime) {
		t.Errorf("Timestamp = %v, want %v", result.Timestamp, req.QueryTime)
	}
}

func TestClient_Query_AbsentWhenNeverActive(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"0"}},
		anyActive: {}, // empty: never active
		coverage:  {},
		freshness: {},
	})

	result, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if ok {
		t.Errorf("Query ok = true, want false (absent); result = %+v", result)
	}
}

// TestClient_QueryRaw_IssuesOnlyOneQuery proves AggregationRaw skips the
// any-active/coverage/freshness queries entirely: newMockPrometheus fails
// the test on any query it wasn't told to expect, so a single registered
// query is sufficient to prove none of the other three were issued.
func TestClient_QueryRaw_IssuesOnlyOneQuery(t *testing.T) {
	req := baseRequest()
	req.Aggregation = AggregationRaw

	usage, err := renderRawUsage(req)
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	server := newMockPrometheus(t, map[string]mockResult{
		usage: {values: []string{"1.5"}},
	})

	result, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !ok {
		t.Fatal("Query ok = false, want true")
	}
	if result.Value != 1.5 {
		t.Errorf("Value = %v, want 1.5", result.Value)
	}
}

func TestClient_QueryRaw_AbsentWhenNoSample(t *testing.T) {
	req := baseRequest()
	req.Aggregation = AggregationRaw

	usage, err := renderRawUsage(req)
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	server := newMockPrometheus(t, map[string]mockResult{
		usage: {}, // empty: no sample
	})

	result, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if ok {
		t.Errorf("Query ok = true, want false (absent); result = %+v", result)
	}
}

func TestClient_QueryRaw_NegativeUsageRejected(t *testing.T) {
	req := baseRequest()
	req.Aggregation = AggregationRaw

	usage, err := renderRawUsage(req)
	if err != nil {
		t.Fatalf("renderRawUsage: %v", err)
	}

	server := newMockPrometheus(t, map[string]mockResult{
		usage: {values: []string{"-1"}},
	})

	if _, _, err := newTestClient(t, server.URL).Query(context.Background(), req); !errors.Is(err, ErrBackend) {
		t.Errorf("err = %v, want ErrBackend", err)
	}
}

func TestClient_Query_IncompleteCoverageRejected(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"1.5"}},
		anyActive: {values: []string{"1"}},
		coverage:  {values: []string{"1"}}, // one bad member found
		freshness: {values: []string{"2"}},
	})

	_, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if ok {
		t.Fatal("Query ok = true, want false")
	}
	if !errors.Is(err, ErrIncompleteCoverage) {
		t.Errorf("err = %v, want ErrIncompleteCoverage", err)
	}
}

func TestClient_Query_StaleDataRejected(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"1.5"}},
		anyActive: {values: []string{"1"}},
		coverage:  {},
		freshness: {values: []string{"31"}}, // over the 30s gate
	})

	_, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if ok {
		t.Fatal("Query ok = true, want false")
	}
	if !errors.Is(err, ErrStaleData) {
		t.Errorf("err = %v, want ErrStaleData", err)
	}
}

func TestClient_Query_NegativeUsageRejected(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"-1"}},
		anyActive: {values: []string{"1"}},
		coverage:  {},
		freshness: {values: []string{"2"}},
	})

	_, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if ok {
		t.Fatal("Query ok = true, want false")
	}
	if !errors.Is(err, ErrBackend) {
		t.Errorf("err = %v, want ErrBackend", err)
	}
}

func TestClient_Query_DuplicateIdentitiesRejected(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"1", "2"}}, // two series: malformed
		anyActive: {values: []string{"1"}},
		coverage:  {},
		freshness: {values: []string{"2"}},
	})

	_, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if ok {
		t.Fatal("Query ok = true, want false")
	}
	if !errors.Is(err, ErrBackend) {
		t.Errorf("err = %v, want ErrBackend", err)
	}
}

func TestClient_Query_WarningsRejected(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"1.5"}, warnings: []string{"query processing would load too many samples"}},
		anyActive: {values: []string{"1"}},
		coverage:  {},
		freshness: {values: []string{"2"}},
	})

	_, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if ok {
		t.Fatal("Query ok = true, want false")
	}
	if !errors.Is(err, ErrBackend) {
		t.Errorf("err = %v, want ErrBackend", err)
	}
}

func TestClient_Query_ProtocolErrorRejected(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {errorType: "bad_data", errorMsg: "invalid query", httpStatus: http.StatusUnprocessableEntity},
		anyActive: {values: []string{"1"}},
		coverage:  {},
		freshness: {values: []string{"2"}},
	})

	_, ok, err := newTestClient(t, server.URL).Query(context.Background(), req)
	if ok {
		t.Fatal("Query ok = true, want false")
	}
	if !errors.Is(err, ErrBackend) {
		t.Errorf("err = %v, want ErrBackend", err)
	}
}

func TestClient_Query_ContextTimeoutSurfaces(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"1.5"}, sleep: 200 * time.Millisecond},
		anyActive: {values: []string{"1"}},
		coverage:  {},
		freshness: {values: []string{"2"}},
	})

	client, err := NewClient(ClientConfig{URL: server.URL, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, ok, err := client.Query(context.Background(), req)
	if ok {
		t.Fatal("Query ok = true, want false")
	}
	if err == nil {
		t.Fatal("Query err = nil, want a timeout error")
	}
}

func TestClient_Ping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "scalar", "result": []any{1_700_000_000.0, "1"}},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	if err := client.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestClient_Ping_UnreachableBackend(t *testing.T) {
	client := newTestClient(t, "http://127.0.0.1:1")
	if err := client.Ping(context.Background()); err == nil {
		t.Error("Ping succeeded against an unreachable backend, want an error")
	}
}

func TestClient_MaxConcurrentQueriesBoundsInFlightRequests(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := maxObserved.Load()
			if n <= old || maxObserved.CompareAndSwap(old, n) {
				break
			}
		}
		<-release

		query := r.FormValue("query")
		var values []string
		switch query {
		case usage:
			values = []string{"1.5"}
		case anyActive:
			values = []string{"1"}
		case coverage:
			values = nil
		case freshness:
			values = []string{"2"}
		}

		result := make([]map[string]any, 0, len(values))
		for _, v := range values {
			result = append(result, map[string]any{"metric": map[string]string{}, "value": []any{1_700_000_000.0, v}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": result},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{URL: server.URL, Timeout: 5 * time.Second, MaxConcurrentQueries: 2})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Client.Query issues 4 sub-queries per call; run two calls concurrently
	// (8 sub-queries total) against a limit of 2.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			_, _, _ = client.Query(context.Background(), req)
		})
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := maxObserved.Load(); got > 2 {
		t.Errorf("observed %d concurrent backend queries, want <= 2 (MaxConcurrentQueries)", got)
	}
}

func TestClient_Query_TimeoutIsDistinguishableFromOtherBackendErrors(t *testing.T) {
	req := baseRequest()
	usage, anyActive, coverage, freshness := queriesFor(t, req)

	server := newMockPrometheus(t, map[string]mockResult{
		usage:     {values: []string{"1.5"}, sleep: 200 * time.Millisecond},
		anyActive: {values: []string{"1"}},
		coverage:  {},
		freshness: {values: []string{"2"}},
	})

	client, err := NewClient(ClientConfig{URL: server.URL, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, _, err = client.Query(context.Background(), req)
	if !errors.Is(err, ErrBackend) {
		t.Errorf("err = %v, want it to also match ErrBackend", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to also match context.DeadlineExceeded (so callers can map it to 504)", err)
	}
}

func TestClient_RejectsEmptySelection(t *testing.T) {
	client := newTestClient(t, "http://127.0.0.1:0")

	req := baseRequest()
	req.Names = nil

	_, _, err := client.Query(context.Background(), req)
	if !errors.Is(err, ErrEmptySelection) {
		t.Errorf("err = %v, want ErrEmptySelection", err)
	}
}

func TestNewClient_RejectsUserinfoURL(t *testing.T) {
	_, err := NewClient(ClientConfig{URL: "https://user:pass@prometheus.example.com"})
	if err == nil {
		t.Fatal("NewClient succeeded for a URL containing userinfo, want error")
	}
}

func TestNewClient_RejectsUnsupportedScheme(t *testing.T) {
	_, err := NewClient(ClientConfig{URL: "ftp://prometheus.example.com"})
	if err == nil {
		t.Fatal("NewClient succeeded for an unsupported scheme, want error")
	}
}

func TestRejectCredentialBearingRedirect(t *testing.T) {
	target, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://user:pass@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectCredentialBearingRedirect(target, nil); err == nil {
		t.Error("expected an error for a redirect target containing userinfo")
	}

	plain, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, 0, 10)
	for range 10 {
		via = append(via, plain)
	}
	if err := rejectCredentialBearingRedirect(plain, via); err == nil {
		t.Error("expected an error after 10 redirects")
	}
}

func TestTokenFileRoundTripper_RereadsOnEveryRequest(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("token-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotAuth []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	rt := &tokenFileRoundTripper{next: http.DefaultTransport, path: tokenPath}
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if _, err := client.Do(req); err != nil {
		t.Fatalf("request 1: %v", err)
	}

	if err := os.WriteFile(tokenPath, []byte("token-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if _, err := client.Do(req2); err != nil {
		t.Fatalf("request 2: %v", err)
	}

	want := []string{"Bearer token-one", "Bearer token-two"}
	for i, w := range want {
		if gotAuth[i] != w {
			t.Errorf("request %d Authorization = %q, want %q", i+1, gotAuth[i], w)
		}
	}
}

func TestLimitedReadCloser_EnforcesByteLimit(t *testing.T) {
	body := strings.Repeat("x", 100)
	limited := &limitedReadCloser{ReadCloser: io.NopCloser(strings.NewReader(body)), remaining: 10}

	buf := make([]byte, 100)
	total := 0
	for {
		n, err := limited.Read(buf)
		total += n
		if err != nil {
			break
		}
	}

	if total > 10 {
		t.Errorf("read %d bytes, want at most 10 before an error", total)
	}
}
