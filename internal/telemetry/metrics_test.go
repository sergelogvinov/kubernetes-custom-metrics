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

package telemetry

import "testing"

// TestNew_MetricsAreUsableUnregistered exercises every metric's recording
// methods without calling Register — legacyregistry is a global,
// process-wide singleton that panics on double registration, so tests must
// not call Register (which would collide across -count>1 runs or other
// tests in this package); component-base's metrics are documented as safe
// to record against even before registration.
func TestNew_MetricsAreUsableUnregistered(t *testing.T) {
	m := New()

	m.CacheHits.WithLabelValues("Pod", "cpu_avg_5m").Inc()
	m.CacheMisses.WithLabelValues("Pod", "cpu_avg_5m").Inc()
	m.SingleflightCollapsed.Inc()
	m.PromQueryDuration.WithLabelValues("Pod", "cpu_avg_5m").Observe(0.25)
	m.QueryErrors.WithLabelValues("incomplete-coverage").Inc()
	m.CronJobFallback.Inc()
	m.CronJobNotFound.Inc()
}
