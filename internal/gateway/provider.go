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
	"context"
	"math"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/metrics/pkg/apis/custom_metrics"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/provider"
)

// Provider implements provider.CustomMetricsProvider directly on top of
// Service — there is no adapter layer translating one provider contract
// into another (design.md §3 rule 2).
type Provider struct {
	catalog *catalog.Catalog
	service *Service
}

var _ provider.CustomMetricsProvider = (*Provider)(nil)

// NewProvider builds a Provider serving cat's discovery entries and
// answering queries through svc.
func NewProvider(cat *catalog.Catalog, svc *Service) *Provider {
	return &Provider{catalog: cat, service: svc}
}

// ListAllMetrics implements provider.CustomMetricsProvider.
func (p *Provider) ListAllMetrics() []provider.CustomMetricInfo {
	return p.catalog.Entries()
}

// GetMetricByName implements provider.CustomMetricsProvider.
func (p *Provider) GetMetricByName(ctx context.Context, name types.NamespacedName, info provider.CustomMetricInfo, metricSelector labels.Selector) (*custom_metrics.MetricValue, error) {
	result, err := p.service.Get(ctx, Request{
		Verb:           "get",
		Namespace:      name.Namespace,
		GroupResource:  info.GroupResource,
		Name:           name.Name,
		Metric:         info.Metric,
		ObjectSelector: labels.Everything(),
		MetricSelector: metricSelector,
	})
	if err != nil {
		return nil, err
	}

	if len(result.Items) != 1 {
		return nil, provider.NewMetricNotFoundForError(info.GroupResource, info.Metric, name.Name)
	}

	return toMetricValue(result.Items[0]), nil
}

// GetMetricBySelector implements provider.CustomMetricsProvider.
func (p *Provider) GetMetricBySelector(
	ctx context.Context,
	namespace string,
	selector labels.Selector,
	info provider.CustomMetricInfo,
	metricSelector labels.Selector,
) (*custom_metrics.MetricValueList, error) {
	result, err := p.service.Get(ctx, Request{
		Verb:           "list",
		Namespace:      namespace,
		GroupResource:  info.GroupResource,
		Metric:         info.Metric,
		ObjectSelector: selector,
		MetricSelector: metricSelector,
	})
	if err != nil {
		return nil, err
	}

	items := make([]custom_metrics.MetricValue, len(result.Items))
	for i, item := range result.Items {
		items[i] = *toMetricValue(item)
	}

	return &custom_metrics.MetricValueList{Items: items}, nil
}

// toMetricValue converts one internal Item into the wire value type
// (design.md §6 step 9). CPU quantities use DecimalSI, rounded up to
// millicores; memory uses BinarySI, rounded up to whole bytes
// (metric-gateway.md §3.2) — pkg/prometheus has already rejected
// negative/non-finite values, so this conversion only ever rounds up.
func toMetricValue(item Item) *custom_metrics.MetricValue {
	windowSeconds := int64(item.Window / time.Second)

	return &custom_metrics.MetricValue{
		DescribedObject: custom_metrics.ObjectReference{
			APIVersion: item.APIVersion,
			Kind:       item.Kind,
			Namespace:  item.Namespace,
			Name:       item.Name,
			UID:        item.UID,
		},
		Metric:        custom_metrics.MetricIdentifier{Name: item.MetricName},
		Timestamp:     metav1.NewTime(item.Timestamp),
		WindowSeconds: &windowSeconds,
		Value:         quantityForItem(item.Unit, item.Value),
	}
}

func quantityForItem(unit catalog.Unit, value float64) resource.Quantity {
	if unit == catalog.UnitCores {
		return *resource.NewMilliQuantity(int64(math.Ceil(value*1000)), resource.DecimalSI)
	}

	return *resource.NewQuantity(int64(math.Ceil(value)), resource.BinarySI)
}
