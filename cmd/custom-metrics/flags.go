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
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/prometheus"
	"github.com/spf13/pflag"
)

const (
	flagCluster               = "cluster"
	flagPrometheusURL         = "prometheus-url"
	flagPrometheusTimeout     = "prometheus-timeout"
	flagPrometheusMaxConns    = "prometheus-max-conns"
	flagPrometheusCAFile      = "prometheus-ca-file"
	flagPrometheusTokenFile   = "prometheus-token-file"
	flagRequestTimeout        = "request-timeout"
	flagMaxInflightRequests   = "max-inflight-requests"
	flagMaxSharedComputations = "max-shared-computations"
	flagMaxConcurrentQueries  = "max-concurrent-queries"
	flagCacheTTLShort         = "cache-ttl-short"
	flagCacheTTLLong          = "cache-ttl-long"
	flagCacheSize             = "cache-size"
	flagCacheMaxBytes         = "cache-max-bytes"
	flagCatalogPath           = "catalog-path"
	flagCronJobFallbackWindow = "cronjob-fallback-window"
	flagDiscoveryMaxMetrics   = "discovery-max-metrics"
	flagDiscoveryMode         = "discovery-mode"

	envCluster               = "CLUSTER"
	envPrometheusURL         = "PROMETHEUS_URL"
	envPrometheusTimeout     = "PROMETHEUS_TIMEOUT"
	envPrometheusMaxConns    = "PROMETHEUS_MAX_CONNS"
	envPrometheusCAFile      = "PROMETHEUS_CA_FILE"
	envPrometheusTokenFile   = "PROMETHEUS_TOKEN_FILE"
	envRequestTimeout        = "REQUEST_TIMEOUT"
	envMaxInflightRequests   = "MAX_INFLIGHT"
	envMaxSharedComputations = "MAX_COMPUTATIONS"
	envMaxConcurrentQueries  = "MAX_QUERIES"
	envCacheTTLShort         = "CACHE_TTL_SHORT"
	envCacheTTLLong          = "CACHE_TTL_LONG"
	envCacheSize             = "CACHE_SIZE"
	envCacheMaxBytes         = "CACHE_MAX_BYTES"
	envCatalogPath           = "CATALOG_PATH"
	envCronJobFallbackWindow = "CRONJOB_FALLBACK"
	envDiscoveryMaxMetrics   = "DISCOVERY_MAX"
	envDiscoveryMode         = "DISCOVERY_MODE"
)

const (
	defaultPrometheusTimeout     = prometheus.DefaultTimeout
	defaultPrometheusMaxConns    = prometheus.DefaultMaxConns
	defaultRequestTimeout        = 10 * time.Second
	defaultMaxInflightRequests   = 128
	defaultMaxSharedComputations = 32
	defaultMaxConcurrentQueries  = prometheus.DefaultMaxConcurrentQueries
	defaultCacheTTLShort         = 15 * time.Second
	defaultCacheTTLLong          = 10 * time.Minute
	defaultCacheSize             = 10000
	defaultCacheMaxBytes         = 64 << 20
	defaultCatalogPath           = "/etc/custom-metrics/catalog.yaml"
	defaultCronJobFallbackWindow = 24 * time.Hour
	defaultDiscoveryMaxMetrics   = 1000
	defaultDiscoveryMode         = catalog.DiscoveryMinimal
)

// Options holds the final value of every gateway-specific flag. A flag wins
// over an environment variable, and an environment variable wins over the
// default.
// Serving, authentication, authorization, and Kubernetes-access flags are
// not redefined here — they come from basecmd.AdapterBase (metric-gateway.md
// §6.1).
type Options struct {
	Cluster string

	PrometheusURL       string
	PrometheusTimeout   time.Duration
	PrometheusMaxConns  int
	PrometheusCAFile    string
	PrometheusTokenFile string

	RequestTimeout        time.Duration
	MaxInflightRequests   int
	MaxSharedComputations int
	MaxConcurrentQueries  int

	CacheTTLShort time.Duration
	CacheTTLLong  time.Duration
	CacheSize     int
	CacheMaxBytes int64

	CatalogPath           string
	CronJobFallbackWindow time.Duration
	DiscoveryMaxMetrics   int
	DiscoveryMode         catalog.DiscoveryMode
}

// NewOptions returns Options bound to their documented hardcoded defaults.
func NewOptions() *Options {
	return &Options{
		PrometheusTimeout:     defaultPrometheusTimeout,
		PrometheusMaxConns:    defaultPrometheusMaxConns,
		RequestTimeout:        defaultRequestTimeout,
		MaxInflightRequests:   defaultMaxInflightRequests,
		MaxSharedComputations: defaultMaxSharedComputations,
		MaxConcurrentQueries:  defaultMaxConcurrentQueries,
		CacheTTLShort:         defaultCacheTTLShort,
		CacheTTLLong:          defaultCacheTTLLong,
		CacheSize:             defaultCacheSize,
		CacheMaxBytes:         defaultCacheMaxBytes,
		CatalogPath:           defaultCatalogPath,
		CronJobFallbackWindow: defaultCronJobFallbackWindow,
		DiscoveryMaxMetrics:   defaultDiscoveryMaxMetrics,
		DiscoveryMode:         defaultDiscoveryMode,
	}
}

// AddFlags registers every gateway-specific flag on fs, bound directly to
// o's fields, alongside (but not replacing) basecmd.AdapterBase's own
// flags on the same *pflag.FlagSet.
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.Cluster, flagCluster, o.Cluster, "Exact backend cluster label; omitted from the Prometheus query entirely when unset")
	fs.StringVar(&o.PrometheusURL, flagPrometheusURL, o.PrometheusURL, "Prometheus backend endpoint")
	fs.DurationVar(&o.PrometheusTimeout, flagPrometheusTimeout, o.PrometheusTimeout, "Per-query timeout against Prometheus")
	fs.IntVar(&o.PrometheusMaxConns, flagPrometheusMaxConns, o.PrometheusMaxConns, "Prometheus HTTP connection pool size")
	fs.StringVar(&o.PrometheusCAFile, flagPrometheusCAFile, o.PrometheusCAFile, "CA bundle verifying the Prometheus backend (default: system roots)")
	fs.StringVar(&o.PrometheusTokenFile, flagPrometheusTokenFile, o.PrometheusTokenFile, "Optional bearer-token file for the Prometheus backend")
	fs.DurationVar(&o.RequestTimeout, flagRequestTimeout, o.RequestTimeout, "Total per-computation deadline")
	fs.IntVar(&o.MaxInflightRequests, flagMaxInflightRequests, o.MaxInflightRequests, "Admitted metric requests, including singleflight waiters")
	fs.IntVar(&o.MaxSharedComputations, flagMaxSharedComputations, o.MaxSharedComputations, "Live distinct shared computations, including queued work")
	fs.IntVar(&o.MaxConcurrentQueries, flagMaxConcurrentQueries, o.MaxConcurrentQueries, "Global concurrent backend query limit")
	fs.DurationVar(&o.CacheTTLShort, flagCacheTTLShort, o.CacheTTLShort, "Response cache TTL for window<=1h")
	fs.DurationVar(&o.CacheTTLLong, flagCacheTTLLong, o.CacheTTLLong, "Response cache TTL for window>1h")
	fs.IntVar(&o.CacheSize, flagCacheSize, o.CacheSize, "Response cache entry cap")
	fs.Int64Var(&o.CacheMaxBytes, flagCacheMaxBytes, o.CacheMaxBytes, "Response cache accounted byte cap")
	fs.StringVar(&o.CatalogPath, flagCatalogPath, o.CatalogPath, "Path to the catalog ConfigMap YAML")
	fs.DurationVar(&o.CronJobFallbackWindow, flagCronJobFallbackWindow, o.CronJobFallbackWindow, "CronJob recent-Job fallback lookback")
	fs.IntVar(&o.DiscoveryMaxMetrics, flagDiscoveryMaxMetrics, o.DiscoveryMaxMetrics, "Discovery resource/metric entry cap")
	fs.Var((*discoveryModeValue)(&o.DiscoveryMode), flagDiscoveryMode,
		"Metrics advertised in discovery: full, minimal (one example per base and resource), or none; all names are served regardless")
}

// discoveryModeValue adapts catalog.DiscoveryMode to pflag.Value so an
// invalid --discovery-mode is rejected at parse time, with the same
// normalization and error as DISCOVERY_MODE.
type discoveryModeValue catalog.DiscoveryMode

func (v *discoveryModeValue) String() string { return string(*v) }
func (v *discoveryModeValue) Type() string   { return "string" }

func (v *discoveryModeValue) Set(s string) error {
	m, err := catalog.ParseDiscoveryMode(s)
	if err != nil {
		return err
	}
	*v = discoveryModeValue(m)

	return nil
}

// ResolveEnvironment overrides every field whose flag was not explicitly
// set with its documented environment variable (metric-gateway.md §6.1).
// changed reports whether a given flag was explicitly passed; lookupEnv is
// injected so tests never mutate the process environment.
func (o *Options) ResolveEnvironment(changed func(name string) bool, lookupEnv func(string) (string, bool)) error {
	if !changed(flagCluster) {
		if v, ok := lookupEnv(envCluster); ok {
			o.Cluster = v
		}
	}
	if !changed(flagPrometheusURL) {
		if v, ok := lookupEnv(envPrometheusURL); ok {
			o.PrometheusURL = v
		}
	}
	if err := resolveDurationEnv(&o.PrometheusTimeout, changed, lookupEnv, flagPrometheusTimeout, envPrometheusTimeout); err != nil {
		return err
	}
	if err := resolveIntEnv(&o.PrometheusMaxConns, changed, lookupEnv, flagPrometheusMaxConns, envPrometheusMaxConns); err != nil {
		return err
	}
	if !changed(flagPrometheusCAFile) {
		if v, ok := lookupEnv(envPrometheusCAFile); ok {
			o.PrometheusCAFile = v
		}
	}
	if !changed(flagPrometheusTokenFile) {
		if v, ok := lookupEnv(envPrometheusTokenFile); ok {
			o.PrometheusTokenFile = v
		}
	}
	if err := resolveDurationEnv(&o.RequestTimeout, changed, lookupEnv, flagRequestTimeout, envRequestTimeout); err != nil {
		return err
	}
	if err := resolveIntEnv(&o.MaxInflightRequests, changed, lookupEnv, flagMaxInflightRequests, envMaxInflightRequests); err != nil {
		return err
	}
	if err := resolveIntEnv(&o.MaxSharedComputations, changed, lookupEnv, flagMaxSharedComputations, envMaxSharedComputations); err != nil {
		return err
	}
	if err := resolveIntEnv(&o.MaxConcurrentQueries, changed, lookupEnv, flagMaxConcurrentQueries, envMaxConcurrentQueries); err != nil {
		return err
	}
	if err := resolveDurationEnv(&o.CacheTTLShort, changed, lookupEnv, flagCacheTTLShort, envCacheTTLShort); err != nil {
		return err
	}
	if err := resolveDurationEnv(&o.CacheTTLLong, changed, lookupEnv, flagCacheTTLLong, envCacheTTLLong); err != nil {
		return err
	}
	if err := resolveIntEnv(&o.CacheSize, changed, lookupEnv, flagCacheSize, envCacheSize); err != nil {
		return err
	}
	if !changed(flagCacheMaxBytes) {
		if v, ok := lookupEnv(envCacheMaxBytes); ok {
			n, err := parsePositiveInt64(v)
			if err != nil {
				return envError(flagCacheMaxBytes, envCacheMaxBytes, v)
			}
			o.CacheMaxBytes = n
		}
	}
	if !changed(flagCatalogPath) {
		if v, ok := lookupEnv(envCatalogPath); ok {
			o.CatalogPath = v
		}
	}
	if err := resolveDurationEnv(&o.CronJobFallbackWindow, changed, lookupEnv, flagCronJobFallbackWindow, envCronJobFallbackWindow); err != nil {
		return err
	}
	if err := resolveIntEnv(&o.DiscoveryMaxMetrics, changed, lookupEnv, flagDiscoveryMaxMetrics, envDiscoveryMaxMetrics); err != nil {
		return err
	}
	if !changed(flagDiscoveryMode) {
		if v, ok := lookupEnv(envDiscoveryMode); ok {
			m, err := catalog.ParseDiscoveryMode(v)
			if err != nil {
				return &usageError{err: fmt.Errorf("environment variable %s (--%s): %w", envDiscoveryMode, flagDiscoveryMode, err)}
			}
			o.DiscoveryMode = m
		}
	}

	return nil
}

func resolveDurationEnv(field *time.Duration, changed func(string) bool, lookupEnv func(string) (string, bool), flag, env string) error {
	if changed(flag) {
		return nil
	}
	v, ok := lookupEnv(env)
	if !ok {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return envError(flag, env, v)
	}
	*field = d

	return nil
}

func resolveIntEnv(field *int, changed func(string) bool, lookupEnv func(string) (string, bool), flag, env string) error {
	if changed(flag) {
		return nil
	}
	v, ok := lookupEnv(env)
	if !ok {
		return nil
	}
	n, err := parsePositiveInt(v)
	if err != nil {
		return envError(flag, env, v)
	}
	*field = n

	return nil
}

func parsePositiveInt(s string) (int, error) {
	n, err := strconv.ParseInt(s, 10, 64)

	return int(n), err
}

func parsePositiveInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// Validate rejects malformed effective values regardless of whether they
// came from a flag or an environment variable.
func (o *Options) Validate() error {
	if o.PrometheusURL == "" {
		return &usageError{err: fmt.Errorf("--%s is required", flagPrometheusURL)}
	}
	if _, err := url.Parse(o.PrometheusURL); err != nil {
		return &usageError{err: fmt.Errorf("--%s: %w", flagPrometheusURL, err)}
	}
	if o.PrometheusTimeout <= 0 {
		return positiveError(flagPrometheusTimeout, o.PrometheusTimeout)
	}
	if o.PrometheusMaxConns <= 0 {
		return positiveError(flagPrometheusMaxConns, o.PrometheusMaxConns)
	}
	if o.RequestTimeout <= 0 {
		return positiveError(flagRequestTimeout, o.RequestTimeout)
	}
	if o.MaxInflightRequests <= 0 {
		return positiveError(flagMaxInflightRequests, o.MaxInflightRequests)
	}
	if o.MaxSharedComputations <= 0 {
		return positiveError(flagMaxSharedComputations, o.MaxSharedComputations)
	}
	if o.MaxConcurrentQueries <= 0 {
		return positiveError(flagMaxConcurrentQueries, o.MaxConcurrentQueries)
	}
	if o.CacheTTLShort <= 0 {
		return positiveError(flagCacheTTLShort, o.CacheTTLShort)
	}
	if o.CacheTTLLong <= 0 {
		return positiveError(flagCacheTTLLong, o.CacheTTLLong)
	}
	if o.CacheSize <= 0 {
		return positiveError(flagCacheSize, o.CacheSize)
	}
	if o.CacheMaxBytes <= 0 {
		return positiveError(flagCacheMaxBytes, o.CacheMaxBytes)
	}
	if o.CatalogPath == "" {
		return &usageError{err: fmt.Errorf("--%s is required", flagCatalogPath)}
	}
	if o.CronJobFallbackWindow <= 0 {
		return positiveError(flagCronJobFallbackWindow, o.CronJobFallbackWindow)
	}
	if o.DiscoveryMaxMetrics <= 0 {
		return positiveError(flagDiscoveryMaxMetrics, o.DiscoveryMaxMetrics)
	}
	if !o.DiscoveryMode.Valid() {
		return &usageError{err: fmt.Errorf("--%s must be one of %v, got %q", flagDiscoveryMode, catalog.DiscoveryModes(), o.DiscoveryMode)}
	}

	return nil
}

func positiveError(flag string, value any) error {
	return &usageError{err: fmt.Errorf("--%s must be positive, got %v", flag, value)}
}

func envError(flag, env, value string) error {
	return &usageError{err: fmt.Errorf("invalid value %q for environment variable %s (--%s)", value, env, flag)}
}
