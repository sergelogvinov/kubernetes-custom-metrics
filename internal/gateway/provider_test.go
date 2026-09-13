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

package gateway

import (
	"testing"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
)

func TestToMetricValue_CPURoundsUpToMillicores(t *testing.T) {
	item := Item{
		APIVersion: "v1",
		Kind:       "Pod",
		Namespace:  "prod",
		Name:       "web-0",
		UID:        "uid-web-0",
		MetricName: "cpu_avg_5m",
		Value:      0.1871, // cores
		Unit:       catalog.UnitCores,
		Timestamp:  time.Unix(1_700_000_000, 0),
		Window:     5 * time.Minute,
	}

	mv := toMetricValue(item)

	// 0.1871 cores = 187.1 millicores, rounded up to 188m.
	if got, want := mv.Value.String(), "188m"; got != want {
		t.Errorf("Value = %q, want %q", got, want)
	}
	if mv.DescribedObject.APIVersion != "v1" || mv.DescribedObject.Kind != "Pod" ||
		mv.DescribedObject.Namespace != "prod" || mv.DescribedObject.Name != "web-0" ||
		mv.DescribedObject.UID != "uid-web-0" {
		t.Errorf("DescribedObject = %+v", mv.DescribedObject)
	}
	if mv.Metric.Name != "cpu_avg_5m" {
		t.Errorf("Metric.Name = %q", mv.Metric.Name)
	}
	if mv.WindowSeconds == nil || *mv.WindowSeconds != 300 {
		t.Errorf("WindowSeconds = %v, want 300", mv.WindowSeconds)
	}
	if !mv.Timestamp.Time.Equal(item.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", mv.Timestamp.Time, item.Timestamp)
	}
}

func TestToMetricValue_MemoryRoundsUpToWholeBytes(t *testing.T) {
	item := Item{
		Unit:  catalog.UnitBytes,
		Value: 1048576.4, // bytes
	}

	mv := toMetricValue(item)

	if got, want := mv.Value.Value(), int64(1048577); got != want {
		t.Errorf("Value.Value() = %d, want %d", got, want)
	}
}

func TestToMetricValue_ExactValuesRoundTripCleanly(t *testing.T) {
	cpu := toMetricValue(Item{Unit: catalog.UnitCores, Value: 1.0})
	if got, want := cpu.Value.String(), "1"; got != want {
		t.Errorf("CPU Value = %q, want %q", got, want)
	}

	mem := toMetricValue(Item{Unit: catalog.UnitBytes, Value: 1024})
	if got, want := mem.Value.Value(), int64(1024); got != want {
		t.Errorf("Memory Value = %d, want %d", got, want)
	}
}
