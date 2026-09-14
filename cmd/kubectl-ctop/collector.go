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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta2 "k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
)

// collectedValue is one identity's value for a single metric, preserving
// enough identity and timing to join a CPU value with its memory
// counterpart. The custom-metrics API has no atomic CPU+memory snapshot, so
// CPU and memory are always two independent round trips (design.md §12).
type collectedValue struct {
	Namespace string
	Name      string
	UID       types.UID
	Quantity  resource.Quantity
	Timestamp time.Time
}

// joinedRow pairs one identity's CPU and memory values after both
// independent fetches complete.
type joinedRow struct {
	Namespace   string
	Name        string
	CPU         resource.Quantity
	Memory      resource.Quantity
	EvaluatedAt time.Time
}

// Collector fetches CPU and memory metrics independently through Client and
// joins them by object identity.
type Collector struct {
	client *Client
}

func NewCollector(client *Client) *Collector {
	return &Collector{client: client}
}

// CollectNamed fetches CPU and memory for a single named object.
func (c *Collector) CollectNamed(ctx context.Context, desc resourceDescriptor, namespace, name, cpuMetric, memMetric string) (joinedRow, error) {
	cpuVal, err := c.client.GetForObject(ctx, desc.GroupKind, namespace, name, cpuMetric)
	if err != nil {
		return joinedRow{}, fmt.Errorf("fetching %s: %w", cpuMetric, err)
	}

	memVal, err := c.client.GetForObject(ctx, desc.GroupKind, namespace, name, memMetric)
	if err != nil {
		return joinedRow{}, fmt.Errorf("fetching %s: %w", memMetric, err)
	}

	cpu := valueFromSingle(cpuVal)
	mem := valueFromSingle(memVal)

	if cpu.UID != mem.UID {
		return joinedRow{}, fmt.Errorf("%s/%s was recreated between the cpu and memory requests (uid %s vs %s)",
			namespace, name, cpu.UID, mem.UID)
	}

	return joinedRow{
		Namespace:   cpu.Namespace,
		Name:        cpu.Name,
		CPU:         cpu.Quantity,
		Memory:      mem.Quantity,
		EvaluatedAt: cpu.Timestamp,
	}, nil
}

// CollectList fetches CPU and memory for every object matching selector and
// joins them by UID. A mismatched membership between the two independent
// fetches is rejected outright rather than silently emitting partial rows.
func (c *Collector) CollectList(ctx context.Context, desc resourceDescriptor, namespace string, selector labels.Selector, cpuMetric, memMetric string) ([]joinedRow, error) {
	cpuList, err := c.client.GetForObjects(ctx, desc.GroupKind, namespace, selector, cpuMetric)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", cpuMetric, err)
	}

	memList, err := c.client.GetForObjects(ctx, desc.GroupKind, namespace, selector, memMetric)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", memMetric, err)
	}

	return joinRows(valuesFromList(cpuList), valuesFromList(memList))
}

func valueFromSingle(v *metricsv1beta2.MetricValue) collectedValue {
	return collectedValue{
		Namespace: v.DescribedObject.Namespace,
		Name:      v.DescribedObject.Name,
		UID:       v.DescribedObject.UID,
		Quantity:  v.Value,
		Timestamp: v.Timestamp.Time,
	}
}

func valuesFromList(list *metricsv1beta2.MetricValueList) []collectedValue {
	out := make([]collectedValue, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, collectedValue{
			Namespace: item.DescribedObject.Namespace,
			Name:      item.DescribedObject.Name,
			UID:       item.DescribedObject.UID,
			Quantity:  item.Value,
			Timestamp: item.Timestamp.Time,
		})
	}

	return out
}

func joinRows(cpu, mem []collectedValue) ([]joinedRow, error) {
	memByUID := make(map[types.UID]collectedValue, len(mem))
	for _, m := range mem {
		memByUID[m.UID] = m
	}

	seen := make(map[types.UID]bool, len(cpu))
	rows := make([]joinedRow, 0, len(cpu))

	for _, c := range cpu {
		m, ok := memByUID[c.UID]
		if !ok {
			return nil, fmt.Errorf("cpu and memory metrics disagree on membership: %s/%s (uid %s) has no matching memory sample",
				c.Namespace, c.Name, c.UID)
		}

		seen[c.UID] = true
		rows = append(rows, joinedRow{
			Namespace:   c.Namespace,
			Name:        c.Name,
			CPU:         c.Quantity,
			Memory:      m.Quantity,
			EvaluatedAt: c.Timestamp,
		})
	}

	if len(seen) != len(memByUID) {
		for _, m := range mem {
			if !seen[m.UID] {
				return nil, fmt.Errorf("cpu and memory metrics disagree on membership: %s/%s (uid %s) has no matching cpu sample",
					m.Namespace, m.Name, m.UID)
			}
		}
	}

	return rows, nil
}
