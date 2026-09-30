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
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"github.com/spf13/pflag"
)

func noneChanged(string) bool { return false }

func allChanged(string) bool { return true }

func lookupEnvFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]

		return v, ok
	}
}

func validOptions() *Options {
	o := NewOptions()
	o.Cluster = "test"
	o.PrometheusURL = "https://prometheus.monitoring.svc:9090"

	return o
}

func TestNewOptions_Defaults(t *testing.T) {
	o := NewOptions()

	if o.PrometheusTimeout != defaultPrometheusTimeout {
		t.Errorf("PrometheusTimeout = %s, want %s", o.PrometheusTimeout, defaultPrometheusTimeout)
	}
	if o.CacheTTLShort != 15*time.Second || o.CacheTTLLong != 10*time.Minute {
		t.Errorf("CacheTTLShort/Long = %s/%s", o.CacheTTLShort, o.CacheTTLLong)
	}
	if o.CatalogPath != "/etc/custom-metrics/catalog.yaml" {
		t.Errorf("CatalogPath = %q", o.CatalogPath)
	}
	if o.CronJobFallbackWindow != 24*time.Hour {
		t.Errorf("CronJobFallbackWindow = %s, want 24h", o.CronJobFallbackWindow)
	}
}

func TestResolveEnvironment_EnvAppliesWhenFlagNotSet(t *testing.T) {
	o := NewOptions()

	err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{
		envCluster:             "prod-cluster",
		envPrometheusURL:       "https://prom:9090",
		envPrometheusTimeout:   "7s",
		envPrometheusMaxConns:  "50",
		envRequestTimeout:      "45s",
		envMaxInflightRequests: "64",
		envCacheTTLShort:       "10s",
		envCacheSize:           "500",
		envCacheMaxBytes:       "1048576",
		envCatalogPath:         "/tmp/catalog.yaml",
	}))
	if err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}

	if o.Cluster != "prod-cluster" {
		t.Errorf("Cluster = %q", o.Cluster)
	}
	if o.PrometheusURL != "https://prom:9090" {
		t.Errorf("PrometheusURL = %q", o.PrometheusURL)
	}
	if o.PrometheusTimeout != 7*time.Second {
		t.Errorf("PrometheusTimeout = %s", o.PrometheusTimeout)
	}
	if o.PrometheusMaxConns != 50 {
		t.Errorf("PrometheusMaxConns = %d", o.PrometheusMaxConns)
	}
	if o.RequestTimeout != 45*time.Second {
		t.Errorf("RequestTimeout = %s", o.RequestTimeout)
	}
	if o.MaxInflightRequests != 64 {
		t.Errorf("MaxInflightRequests = %d", o.MaxInflightRequests)
	}
	if o.CacheTTLShort != 10*time.Second {
		t.Errorf("CacheTTLShort = %s", o.CacheTTLShort)
	}
	if o.CacheSize != 500 {
		t.Errorf("CacheSize = %d", o.CacheSize)
	}
	if o.CacheMaxBytes != 1048576 {
		t.Errorf("CacheMaxBytes = %d", o.CacheMaxBytes)
	}
	if o.CatalogPath != "/tmp/catalog.yaml" {
		t.Errorf("CatalogPath = %q", o.CatalogPath)
	}
}

func TestResolveEnvironment_ExplicitFlagBeatsEnv(t *testing.T) {
	o := NewOptions()
	o.Cluster = "from-flag"

	err := o.ResolveEnvironment(allChanged, lookupEnvFrom(map[string]string{
		envCluster: "from-env",
	}))
	if err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}
	if o.Cluster != "from-flag" {
		t.Errorf("Cluster = %q, want from-flag (explicit flag must win)", o.Cluster)
	}
}

func TestResolveEnvironment_DiscoveryMode(t *testing.T) {
	o := NewOptions()
	if o.DiscoveryMode != catalog.DiscoveryMinimal {
		t.Errorf("default DiscoveryMode = %q, want %q", o.DiscoveryMode, catalog.DiscoveryMinimal)
	}

	if err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{envDiscoveryMode: "full"})); err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}
	if o.DiscoveryMode != catalog.DiscoveryFull {
		t.Errorf("DiscoveryMode = %q, want %q", o.DiscoveryMode, catalog.DiscoveryFull)
	}

	o = NewOptions()
	if err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{envDiscoveryMode: " None\n"})); err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}
	if o.DiscoveryMode != catalog.DiscoveryNone {
		t.Errorf("DiscoveryMode = %q, want %q (case and surrounding whitespace ignored)", o.DiscoveryMode, catalog.DiscoveryNone)
	}

	err := NewOptions().ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{envDiscoveryMode: "partial"}))
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("err = %v (%T), want *usageError for an unknown mode", err, err)
	}
	if err != nil && !strings.Contains(err.Error(), "minimal") {
		t.Errorf("err = %v, want it to list the valid modes", err)
	}
}

func TestDiscoveryModeFlag(t *testing.T) {
	parse := func(t *testing.T, args ...string) (*Options, *pflag.FlagSet, error) {
		t.Helper()

		o := NewOptions()
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		fs.SetOutput(io.Discard)
		o.AddFlags(fs)

		return o, fs, fs.Parse(args)
	}

	t.Run("binds and normalizes", func(t *testing.T) {
		o, _, err := parse(t, "--discovery-mode=Full")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if o.DiscoveryMode != catalog.DiscoveryFull {
			t.Errorf("DiscoveryMode = %q, want %q", o.DiscoveryMode, catalog.DiscoveryFull)
		}
	})

	t.Run("rejects unknown mode at parse time", func(t *testing.T) {
		_, _, err := parse(t, "--discovery-mode=partial")
		if err == nil || !strings.Contains(err.Error(), "minimal") {
			t.Errorf("Parse err = %v, want an error listing the valid modes", err)
		}
	})

	t.Run("explicit flag beats env", func(t *testing.T) {
		o, fs, err := parse(t, "--discovery-mode=none")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if err := o.ResolveEnvironment(fs.Changed, lookupEnvFrom(map[string]string{envDiscoveryMode: "full"})); err != nil {
			t.Fatalf("ResolveEnvironment() error = %v", err)
		}
		if o.DiscoveryMode != catalog.DiscoveryNone {
			t.Errorf("DiscoveryMode = %q, want %q (explicit flag must win)", o.DiscoveryMode, catalog.DiscoveryNone)
		}
	})
}

func TestResolveEnvironment_MalformedDurationEnvIsRejected(t *testing.T) {
	o := NewOptions()

	err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{
		envRequestTimeout: "not-a-duration",
	}))
	if err == nil {
		t.Fatal("ResolveEnvironment() succeeded for a malformed duration, want an error")
	}
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("err = %v (%T), want *usageError", err, err)
	}
}

func TestResolveEnvironment_MalformedDurationEnvDoesNotDefeatValidFlag(t *testing.T) {
	o := NewOptions()
	o.RequestTimeout = time.Minute

	err := o.ResolveEnvironment(allChanged, lookupEnvFrom(map[string]string{
		envRequestTimeout: "not-a-duration",
	}))
	if err != nil {
		t.Fatalf("ResolveEnvironment() error = %v, want nil (flag already set, env should not even be consulted)", err)
	}
	if o.RequestTimeout != time.Minute {
		t.Errorf("RequestTimeout = %s, want unchanged 1m", o.RequestTimeout)
	}
}

func TestResolveEnvironment_MalformedIntEnvIsRejected(t *testing.T) {
	o := NewOptions()

	err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{
		envCacheSize: "not-a-number",
	}))
	if err == nil {
		t.Fatal("ResolveEnvironment() succeeded for a malformed int, want an error")
	}
}

func TestValidate_RequiresPrometheusURL(t *testing.T) {
	o := NewOptions()
	if err := o.Validate(); err == nil {
		t.Error("Validate() succeeded with no --prometheus-url, want an error")
	}

	o.PrometheusURL = "https://prometheus:9090"
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil", err)
	}
}

// TestValidate_ClusterIsOptional proves --cluster may be left unset: an
// unset cluster label is omitted from the Prometheus query entirely
// (pkg/prometheus.TestRenderUsage_EmptyClusterOmitsClusterMatcher)
// rather than being a configuration error.
func TestValidate_ClusterIsOptional(t *testing.T) {
	o := NewOptions()
	o.PrometheusURL = "https://prometheus:9090"

	if err := o.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil with --cluster unset", err)
	}
}

func TestValidate_RejectsNonPositiveDurationsAndCounts(t *testing.T) {
	cases := []func(*Options){
		func(o *Options) { o.PrometheusTimeout = 0 },
		func(o *Options) { o.PrometheusMaxConns = 0 },
		func(o *Options) { o.RequestTimeout = -1 },
		func(o *Options) { o.MaxInflightRequests = 0 },
		func(o *Options) { o.MaxSharedComputations = 0 },
		func(o *Options) { o.MaxConcurrentQueries = 0 },
		func(o *Options) { o.CacheTTLShort = 0 },
		func(o *Options) { o.CacheTTLLong = 0 },
		func(o *Options) { o.CacheSize = 0 },
		func(o *Options) { o.CacheMaxBytes = 0 },
		func(o *Options) { o.CronJobFallbackWindow = 0 },
		func(o *Options) { o.DiscoveryMaxMetrics = 0 },
		func(o *Options) { o.DiscoveryMode = "partial" },
	}
	for i, mutate := range cases {
		o := validOptions()
		mutate(o)
		if err := o.Validate(); err == nil {
			t.Errorf("case %d: Validate() succeeded, want an error", i)
		}
	}
}

func TestValidate_RejectsEmptyCatalogPath(t *testing.T) {
	o := validOptions()
	o.CatalogPath = ""

	if err := o.Validate(); err == nil {
		t.Error("Validate() succeeded with an empty catalog path, want an error")
	}
}
