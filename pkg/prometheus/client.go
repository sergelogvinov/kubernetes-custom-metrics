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
	"golang.org/x/sync/errgroup"
	"k8s.io/klog/v2"
)

// maxDecompressedResponseBytes bounds one backend response body, after
// transport-level decompression (metric-gateway.md §6.3).
const maxDecompressedResponseBytes = 16 << 20

// Result is one successfully computed, validated metric value.
type Result struct {
	Value     float64
	Timestamp time.Time
}

// ClientConfig configures the bounded backend HTTP transport
// (metric-gateway.md §6.1).
type ClientConfig struct {
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
	// concurrently per process"). Query calls beyond the limit block
	// (queue) rather than being rejected; this is a resource-protection
	// bound on the backend, not a caller-facing admission decision.
	MaxConcurrentQueries int
}

// Defaults applied by NewClient to zero-valued ClientConfig fields. The
// gateway's --prometheus-timeout, --prometheus-max-conns and
// --max-concurrent-queries flags default to these same values.
const (
	DefaultTimeout              = 5 * time.Second
	DefaultMaxConns             = 100
	DefaultMaxConcurrentQueries = 16
)

// Querier is the narrow surface internal/gateway depends on.
type Querier interface {
	// Query computes req's value. It returns (result, true, nil) when at
	// least one selected identity was active with complete, fresh
	// coverage; (Result{}, false, nil) when no selected identity was ever
	// active in the window (a genuinely absent metric, not an error); or a
	// non-nil error — one of ErrEmptySelection, ErrQueryTooLarge,
	// ErrIncompleteCoverage, ErrStaleData, ErrBackend, or a context
	// error — otherwise.
	Query(ctx context.Context, req Request) (Result, bool, error)
}

// Client is a bounded Prometheus HTTP API client. TLS verification is
// never disabled; credential-bearing redirects and URLs are rejected
// (metric-gateway.md §6.1).
type Client struct {
	api     promv1.API
	timeout time.Duration
	queries chan struct{} // counting semaphore bounding concurrent backend queries
}

var _ Querier = (*Client)(nil)

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

	return &Client{api: promv1.NewAPI(apiClient), timeout: timeout, queries: make(chan struct{}, maxConcurrentQueries)}, nil
}

// Query implements Querier. For req.Aggregation == AggregationRaw it
// delegates to queryRaw (a single unvalidated usage query). Otherwise it
// issues up to four concurrent backend queries — matching the "at most
// four [backend queries] per shared computation" v1 budget
// (metric-gateway.md §6.3) — and combines their results: whether any
// identity was ever active in the window, whether coverage/completeness
// held throughout, whether data stayed within the freshness gate, and the
// aggregated usage value itself.
func (c *Client) Query(ctx context.Context, req Request) (Result, bool, error) {
	if len(req.Names) == 0 {
		return Result{}, false, ErrEmptySelection
	}

	if req.Aggregation == AggregationRaw {
		return c.queryRaw(ctx, req)
	}

	usageQuery, err := renderUsage(req)
	if err != nil {
		return Result{}, false, err
	}
	anyActiveQuery, err := renderAnyActive(req)
	if err != nil {
		return Result{}, false, err
	}
	coverageQuery, err := renderCoverage(req)
	if err != nil {
		return Result{}, false, err
	}
	freshnessQuery, err := renderFreshness(req)
	if err != nil {
		return Result{}, false, err
	}
	for _, q := range []string{usageQuery, anyActiveQuery, coverageQuery, freshnessQuery} {
		if err := checkQuerySize(q); err != nil {
			return Result{}, false, err
		}
	}

	var (
		usageValue                        float64
		usagePresent                      bool
		anyActiveValue, coverageValue     float64
		anyActivePresent, coveragePresent bool
		freshnessValue                    float64
		freshnessPresent                  bool
	)

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		v, present, err := c.run(groupCtx, usageQuery, req.QueryTime)
		usageValue, usagePresent = v, present

		return err
	})
	group.Go(func() error {
		v, present, err := c.run(groupCtx, anyActiveQuery, req.QueryTime)
		anyActiveValue, anyActivePresent = v, present

		return err
	})
	group.Go(func() error {
		v, present, err := c.run(groupCtx, coverageQuery, req.QueryTime)
		coverageValue, coveragePresent = v, present

		return err
	})
	group.Go(func() error {
		v, present, err := c.run(groupCtx, freshnessQuery, req.QueryTime)
		freshnessValue, freshnessPresent = v, present

		return err
	})

	if err := group.Wait(); err != nil {
		return Result{}, false, err
	}

	if !anyActivePresent || anyActiveValue <= 0 {
		// No selected identity was ever active in the window: a
		// genuinely absent metric (metric-gateway.md §3.7), not an
		// error.
		return Result{}, false, nil
	}

	if coveragePresent && coverageValue > 0 {
		return Result{}, false, ErrIncompleteCoverage
	}

	const freshnessGateSeconds = 30
	if freshnessPresent && freshnessValue > freshnessGateSeconds {
		return Result{}, false, ErrStaleData
	}

	if !usagePresent {
		// An active member exists but the usage series itself has no
		// sample: treat as incomplete coverage rather than silently
		// reporting no data for an active target.
		return Result{}, false, ErrIncompleteCoverage
	}
	if err := requireNonNegative(usageValue); err != nil {
		return Result{}, false, err
	}

	return Result{Value: usageValue, Timestamp: req.QueryTime}, true, nil
}

// Ping performs a bounded reachability check against the backend, for use
// as a readiness probe (design.md §10: "a bounded Prometheus reachability
// check"). It only checks that the backend responds, not that any
// particular query is well-formed.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	_, _, err := c.api.Query(ctx, "1", time.Now())

	return err
}

// queryRaw implements Query for AggregationRaw: a single query straight
// against raw cAdvisor/node-exporter series, with no
// any-active/coverage/freshness validation — for clusters that do not run
// the normalized pod_active/pod_cpu_complete/pod_memory_complete or
// node_active/node_complete recording rules (query.go's renderRawUsage,
// metric-gateway.md §8). Because there is no active signal, "no sample" and
// "genuinely absent" are indistinguishable: both return (Result{}, false,
// nil).
func (c *Client) queryRaw(ctx context.Context, req Request) (Result, bool, error) {
	usageQuery, err := renderRawUsage(req)
	if err != nil {
		return Result{}, false, err
	}
	if err := checkQuerySize(usageQuery); err != nil {
		return Result{}, false, err
	}

	usageValue, usagePresent, err := c.run(ctx, usageQuery, req.QueryTime)
	if err != nil {
		return Result{}, false, err
	}
	if !usagePresent {
		return Result{}, false, nil
	}
	if err := requireNonNegative(usageValue); err != nil {
		return Result{}, false, err
	}

	return Result{Value: usageValue, Timestamp: req.QueryTime}, true, nil
}

func (c *Client) run(ctx context.Context, query string, ts time.Time) (float64, bool, error) {
	// Queueing for a slot is bounded by the caller's request deadline
	// only; the per-query timeout starts once the query can actually be
	// sent, so a burst of queued queries does not time out before
	// reaching the backend.
	select {
	case c.queries <- struct{}{}:
		defer func() { <-c.queries }()
	case <-ctx.Done():
		return 0, false, fmt.Errorf("%w: %w", ErrBackend, ctx.Err())
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
		return 0, false, fmt.Errorf("%w: %w", ErrBackend, ctx.Err())
	}

	return decodeSingle(value, warnings, err)
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
