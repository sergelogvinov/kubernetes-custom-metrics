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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clienttesting "k8s.io/client-go/testing"
	metricsv1beta2 "k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
	customfake "k8s.io/metrics/pkg/client/custom_metrics/fake"
)

func metricValue(namespace, name string, uid types.UID, quantity string) metricsv1beta2.MetricValue {
	return metricsv1beta2.MetricValue{
		DescribedObject: corev1.ObjectReference{Namespace: namespace, Name: name, UID: uid},
		Timestamp:       metav1.NewTime(time.Unix(1700000000, 0)),
		Value:           resource.MustParse(quantity),
	}
}

// newFakeClient builds a Client backed by k8s.io/metrics's fake
// CustomMetricsClient, serving fixed fixtures per metric name. A named
// request ("get" with a specific name) returns the one matching item; a
// wildcard request ("*") returns the whole fixture list.
func newFakeClient(t *testing.T, fixtures map[string][]metricsv1beta2.MetricValue) *Client {
	t.Helper()

	fakeClient := &customfake.FakeCustomMetricsClient{}
	fakeClient.AddReactor("get", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		getAction, ok := action.(customfake.GetForAction)
		if !ok {
			return false, nil, nil
		}

		values, ok := fixtures[getAction.GetMetricName()]
		if !ok {
			return true, nil, errors.New("no fixture for metric " + getAction.GetMetricName())
		}

		if getAction.GetName() == "*" {
			return true, &metricsv1beta2.MetricValueList{Items: values}, nil
		}

		for _, v := range values {
			if v.DescribedObject.Name == getAction.GetName() {
				return true, &metricsv1beta2.MetricValueList{Items: []metricsv1beta2.MetricValue{v}}, nil
			}
		}

		return true, &metricsv1beta2.MetricValueList{}, nil
	})

	return &Client{metrics: fakeClient, transport: &cancelTransport{}}
}

func podsDescriptor(t *testing.T) resourceDescriptor {
	t.Helper()

	for _, d := range resourceDescriptors {
		if d.Name == "pods" {
			return d
		}
	}

	t.Fatal("no pods descriptor registered")

	return resourceDescriptor{}
}

func TestCollector_CollectNamed(t *testing.T) {
	client := newFakeClient(t, map[string][]metricsv1beta2.MetricValue{
		"cpu_avg_5m":    {metricValue("prod", "web-0", "u1", "100m")},
		"memory_avg_5m": {metricValue("prod", "web-0", "u1", "200Mi")},
	})

	row, err := NewCollector(client).CollectNamed(context.Background(), podsDescriptor(t), "prod", "web-0", "cpu_avg_5m", "memory_avg_5m")
	if err != nil {
		t.Fatalf("CollectNamed() error = %v", err)
	}

	if row.Namespace != "prod" || row.Name != "web-0" {
		t.Errorf("CollectNamed() identity = %s/%s, want prod/web-0", row.Namespace, row.Name)
	}
	if row.CPU.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("CollectNamed() CPU = %s, want 100m", row.CPU.String())
	}
	if row.Memory.Cmp(resource.MustParse("200Mi")) != 0 {
		t.Errorf("CollectNamed() Memory = %s, want 200Mi", row.Memory.String())
	}
}

func TestCollector_CollectNamed_UIDMismatchRejected(t *testing.T) {
	client := newFakeClient(t, map[string][]metricsv1beta2.MetricValue{
		"cpu_avg_5m":    {metricValue("prod", "web-0", "u1", "100m")},
		"memory_avg_5m": {metricValue("prod", "web-0", "u2", "200Mi")},
	})

	_, err := NewCollector(client).CollectNamed(context.Background(), podsDescriptor(t), "prod", "web-0", "cpu_avg_5m", "memory_avg_5m")
	if err == nil {
		t.Fatal("CollectNamed() error = nil, want a uid-mismatch error (object recreated between cpu and memory requests)")
	}
}

func TestCollector_CollectList(t *testing.T) {
	client := newFakeClient(t, map[string][]metricsv1beta2.MetricValue{
		"cpu_avg_5m": {
			metricValue("prod", "web-0", "u1", "100m"),
			metricValue("prod", "web-1", "u2", "150m"),
		},
		"memory_avg_5m": {
			metricValue("prod", "web-0", "u1", "200Mi"),
			metricValue("prod", "web-1", "u2", "300Mi"),
		},
	})

	rows, err := NewCollector(client).CollectList(context.Background(), podsDescriptor(t), "prod", labels.Everything(), "cpu_avg_5m", "memory_avg_5m")
	if err != nil {
		t.Fatalf("CollectList() error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("CollectList() returned %d rows, want 2", len(rows))
	}
}

func TestCollector_CollectList_EmptyIsEmpty(t *testing.T) {
	client := newFakeClient(t, map[string][]metricsv1beta2.MetricValue{
		"cpu_avg_5m":    {},
		"memory_avg_5m": {},
	})

	rows, err := NewCollector(client).CollectList(context.Background(), podsDescriptor(t), "prod", labels.Everything(), "cpu_avg_5m", "memory_avg_5m")
	if err != nil {
		t.Fatalf("CollectList() error = %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("CollectList() returned %d rows, want 0", len(rows))
	}
}

func TestCollector_CollectList_MismatchedMembershipRejected(t *testing.T) {
	client := newFakeClient(t, map[string][]metricsv1beta2.MetricValue{
		"cpu_avg_5m": {
			metricValue("prod", "web-0", "u1", "100m"),
			metricValue("prod", "web-1", "u2", "150m"),
		},
		"memory_avg_5m": {
			metricValue("prod", "web-0", "u1", "200Mi"),
		},
	})

	_, err := NewCollector(client).CollectList(context.Background(), podsDescriptor(t), "prod", labels.Everything(), "cpu_avg_5m", "memory_avg_5m")
	if err == nil {
		t.Fatal("CollectList() error = nil, want a membership-mismatch error")
	}
}
