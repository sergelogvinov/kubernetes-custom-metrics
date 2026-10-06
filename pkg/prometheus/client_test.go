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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()

	client, err := NewClient(ClientConfig{URL: url, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	return client
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
