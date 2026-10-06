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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

// maxDecompressedResponseBytes bounds one backend response body, after
// transport-level decompression (metric-gateway.md §6.3).
const maxDecompressedResponseBytes = 16 << 20

// ClientConfig configures the bounded backend HTTP transport
// (metric-gateway.md §6.1).
type ClientConfig struct {
	// Cluster is the exact backend "cluster" label every query matches.
	// Empty omits the matcher, for a single-cluster backend with no
	// "cluster" label.
	Cluster string
	// URL is the Prometheus base address, e.g.
	// "https://prometheus.monitoring.svc:9090". Required. Must not contain
	// userinfo.
	URL string
	// Timeout bounds each individual backend query once it holds a
	// query slot; time spent queued for a slot is bounded only by the
	// caller's context (default DefaultTimeout).
	Timeout time.Duration
	// MaxConns bounds the connection pool (default DefaultMaxConns).
	MaxConns int
	// CAFile optionally overrides the system root CA pool for verifying
	// the backend's TLS certificate. TLS verification is never disabled.
	CAFile string
	// TokenFile optionally names a file containing a bearer token, re-read
	// on every request so rotation takes effect without a restart. Its
	// contents are never logged.
	TokenFile string
	// MaxConcurrentQueries bounds how many backend HTTP queries this
	// Client will have in flight at once, process-wide
	// (default DefaultMaxConcurrentQueries —
	// metric-gateway.md §6.3: "at most 16 backend queries run
	// concurrently per process"). Queries beyond the limit block
	// (queue) rather than being rejected; this is a resource-protection
	// bound on the backend, not a caller-facing admission decision.
	MaxConcurrentQueries int
	// Clock captures each computation's evaluation time
	// (default clock.RealClock{}).
	Clock clock.PassiveClock
}

// Defaults applied by NewClient to zero-valued ClientConfig fields. The
// gateway's --prometheus-timeout, --prometheus-max-conns and
// --max-concurrent-queries flags default to these same values.
const (
	DefaultTimeout              = 5 * time.Second
	DefaultMaxConns             = 100
	DefaultMaxConcurrentQueries = 16
)

// Client is a bounded Prometheus HTTP API client. TLS verification is
// never disabled; credential-bearing redirects and URLs are rejected
// (metric-gateway.md §6.1).
type Client struct {
	api     promv1.API
	cluster string
	clock   clock.PassiveClock
	timeout time.Duration
	queries chan struct{} // counting semaphore bounding concurrent backend queries
}

// NewClient builds a Client from cfg.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("prometheus: URL is required")
	}

	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("prometheus: invalid URL: %w", err)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("prometheus: URL must not contain userinfo")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("prometheus: unsupported URL scheme %q", parsed.Scheme)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxConns := cfg.MaxConns
	if maxConns <= 0 {
		maxConns = DefaultMaxConns
	}
	maxConcurrentQueries := cfg.MaxConcurrentQueries
	if maxConcurrentQueries <= 0 {
		maxConcurrentQueries = DefaultMaxConcurrentQueries
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.RealClock{}
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pool, err := loadCAFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.RootCAs = pool
	}

	transport := &http.Transport{
		TLSClientConfig:     tlsConfig,
		MaxConnsPerHost:     maxConns,
		MaxIdleConnsPerHost: maxConns,
		IdleConnTimeout:     90 * time.Second,
	}

	var roundTripper http.RoundTripper = &limitedBodyRoundTripper{next: transport, limit: maxDecompressedResponseBytes}
	if cfg.TokenFile != "" {
		roundTripper = &tokenFileRoundTripper{next: roundTripper, path: cfg.TokenFile}
	}

	httpClient := &http.Client{
		Transport:     roundTripper,
		CheckRedirect: rejectCredentialBearingRedirect,
	}

	apiClient, err := promapi.NewClient(promapi.Config{Address: cfg.URL, Client: httpClient})
	if err != nil {
		return nil, fmt.Errorf("prometheus: building client: %w", err)
	}

	return &Client{
		api:     promv1.NewAPI(apiClient),
		cluster: cfg.Cluster,
		clock:   clk,
		timeout: timeout,
		queries: make(chan struct{}, maxConcurrentQueries),
	}, nil
}

// Ping checks, with a timeout, that the backend answers. The readiness
// probe uses it. It does not check that any real query works.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	_, _, err := c.api.Query(ctx, "1", time.Now())

	return err
}

// runByTarget executes one batch query (renderBatch) at ts and decodes one
// value per target position in [0, n).
func (c *Client) runByTarget(ctx context.Context, query string, ts time.Time, n int) (targetValues, error) {
	// Queueing for a slot is bounded by the caller's request deadline
	// only; the per-query timeout starts once the query can actually be
	// sent, so a burst of queued queries does not time out before
	// reaching the backend.
	select {
	case c.queries <- struct{}{}:
		defer func() { <-c.queries }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrBackend, ctx.Err())
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	klog.V(4).InfoS("querying prometheus", "query", query, "queryTime", ts)

	value, warnings, err := c.api.Query(ctx, query, ts)
	duration := time.Since(start)

	if err != nil {
		// Always visible (not gated behind -v), since a backend failure or
		// timeout here is the usual explanation for a request that later
		// shows up as a canceled/stream-closed error further up the stack.
		klog.ErrorS(err, "prometheus query failed", "query", query, "duration", duration)
	} else {
		klog.V(4).InfoS("prometheus query completed", "query", query, "duration", duration, "results", resultCount(value), "warnings", warnings)
	}

	if err != nil && ctx.Err() != nil {
		// Preserve the context error's identity (errors.Is against
		// context.DeadlineExceeded) alongside the ErrBackend
		// classification, so callers can distinguish a timeout (504) from
		// other backend failures (503) — metric-gateway.md §3.6.
		return nil, fmt.Errorf("%w: %w", ErrBackend, ctx.Err())
	}

	return decodeByTarget(value, warnings, err, n)
}

// rejectCredentialBearingRedirect enforces metric-gateway.md §6.1: "reject
// URLs containing userinfo and do not forward credentials across
// redirects." net/http already strips Authorization/Cookie headers on a
// cross-host redirect; this adds the userinfo check and a bound on
// redirect count.
func rejectCredentialBearingRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("prometheus: stopped after 10 redirects")
	}
	if req.URL.User != nil {
		return fmt.Errorf("prometheus: refusing to follow a redirect to a URL containing userinfo")
	}

	return nil
}

func loadCAFile(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("prometheus: reading CA file: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("prometheus: %s contains no usable PEM certificates", path)
	}

	return pool, nil
}

// tokenFileRoundTripper attaches a bearer token read fresh from a file on
// every request, so rotation takes effect without a process restart. The
// token is never logged.
type tokenFileRoundTripper struct {
	next http.RoundTripper
	path string
}

func (t *tokenFileRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	data, err := os.ReadFile(t.path)
	if err != nil {
		return nil, fmt.Errorf("prometheus: reading token file: %w", err)
	}

	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(data)))

	return t.next.RoundTrip(req)
}

// limitedBodyRoundTripper caps the decompressed response body size a
// caller can read, independent of any server-advertised Content-Length
// (metric-gateway.md §6.3).
type limitedBodyRoundTripper struct {
	next  http.RoundTripper
	limit int64
}

func (l *limitedBodyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	resp.Body = &limitedReadCloser{ReadCloser: resp.Body, remaining: l.limit}

	return resp, nil
}

type limitedReadCloser struct {
	io.ReadCloser

	remaining int64
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, fmt.Errorf("prometheus: response body exceeds %d byte limit", maxDecompressedResponseBytes)
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}

	n, err := l.ReadCloser.Read(p)
	l.remaining -= int64(n)

	return n, err
}
