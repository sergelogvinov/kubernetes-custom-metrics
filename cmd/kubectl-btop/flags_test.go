package main

import (
	"errors"
	"testing"
	"time"
)

func noneChanged(string) bool { return false }

func allChanged(string) bool { return true }

func lookupEnvFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestResolveEnvironment_EnvAppliesWhenFlagNotSet(t *testing.T) {
	o := NewOptions()

	err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{
		envWindow:         "1h",
		envRequestTimeout: "1m",
		envNamespace:      "prod",
		envOutput:         "json",
		envContext:        "kind-dev",
	}))
	if err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}

	want := Options{
		Window:         "1h",
		Stat:           defaultStat,
		Namespace:      "prod",
		SortBy:         "cpu",
		Output:         "json",
		RequestTimeout: time.Minute,
		Context:        "kind-dev",
	}
	if *o != want {
		t.Errorf("ResolveEnvironment() = %+v, want %+v", *o, want)
	}
}

func TestResolveEnvironment_FlagOnlyFieldsIgnoreEnv(t *testing.T) {
	o := NewOptions()

	err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{
		"STAT":       "p95",
		"SELECTOR":   "app=web",
		"NO_HEADERS": "true",
	}))
	if err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}

	want := Options{
		Window:         defaultWindow,
		Stat:           defaultStat,
		SortBy:         defaultSortBy,
		Output:         defaultOutput,
		RequestTimeout: defaultRequestTimeout,
	}
	if *o != want {
		t.Errorf("ResolveEnvironment() = %+v, want Stat/Selector/NoHeaders to stay at their flag defaults, got %+v", *o, want)
	}
}

func TestResolveEnvironment_ExplicitFlagBeatsEnv(t *testing.T) {
	o := NewOptions()
	o.Window = "5m" // simulates cobra having already parsed an explicit --window=5m

	err := o.ResolveEnvironment(allChanged, lookupEnvFrom(map[string]string{
		envWindow: "not-a-window",
	}))
	if err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}
	if o.Window != "5m" {
		t.Errorf("Window = %q, want explicit flag value %q to survive a malformed env var", o.Window, "5m")
	}
}

func TestResolveEnvironment_MalformedEnvIsRejected(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"duration", map[string]string{envRequestTimeout: "not-a-duration"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := NewOptions()

			err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(tt.env))
			if err == nil {
				t.Fatalf("ResolveEnvironment() error = nil, want error for %v", tt.env)
			}
			if _, ok := errors.AsType[*usageError](err); !ok {
				t.Errorf("ResolveEnvironment() error = %v, want a *usageError", err)
			}
		})
	}
}

func TestResolveEnvironment_KubeconfigIgnoresEnv(t *testing.T) {
	o := NewOptions()

	err := o.ResolveEnvironment(noneChanged, lookupEnvFrom(map[string]string{
		"KUBECONFIG": "/some/path",
	}))
	if err != nil {
		t.Fatalf("ResolveEnvironment() error = %v", err)
	}
	if o.Kubeconfig != "" {
		t.Errorf("Kubeconfig = %q, want client-go to own KUBECONFIG resolution, not ResolveEnvironment", o.Kubeconfig)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Options)
		wantErr bool
	}{
		{"defaults are valid", func(*Options) {}, false},
		{"bad window", func(o *Options) { o.Window = "not-a-window" }, true},
		{"zero window", func(o *Options) { o.Window = "0m" }, true},
		{"negative window", func(o *Options) { o.Window = "-5m" }, true},
		{"window below 1m minimum", func(o *Options) { o.Window = "30s" }, true},
		{"window exactly 1m minimum is valid", func(o *Options) { o.Window = "1m" }, false},
		{"arbitrary window 26m is valid", func(o *Options) { o.Window = "26m" }, false},
		{"arbitrary window 34m is valid", func(o *Options) { o.Window = "34m" }, false},
		{"arbitrary window 2h is valid", func(o *Options) { o.Window = "2h" }, false},
		{"bad stat", func(o *Options) { o.Stat = "median" }, true},
		{"bad sort-by", func(o *Options) { o.SortBy = "age" }, true},
		{"bad output", func(o *Options) { o.Output = "xml" }, true},
		{"zero request-timeout", func(o *Options) { o.RequestTimeout = 0 }, true},
		{"negative request-timeout", func(o *Options) { o.RequestTimeout = -1 }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := NewOptions()
			tt.mutate(o)

			err := o.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if _, ok := errors.AsType[*usageError](err); !ok {
					t.Errorf("Validate() error = %v, want a *usageError", err)
				}
			}
		})
	}
}
