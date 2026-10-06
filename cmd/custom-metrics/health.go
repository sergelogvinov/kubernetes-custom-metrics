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
	"net/http"
	"time"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/prometheus"
	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/apiserver"
)

// readinessCheckTimeout bounds each individual readyz probe so a slow or
// wedged dependency cannot hang the /readyz handler indefinitely.
const readinessCheckTimeout = 5 * time.Second

// addReadyzChecks adds the gateway's readiness checks to config (taken from
// adapter.Config()): the catalog is loaded, the gateway ServiceAccount can
// read the cluster, and Prometheus is reachable.
// Liveness and discovery stay independent of Prometheus availability —
// only these added readyz checks may depend on it, never /healthz or
// /livez.
func addReadyzChecks(config *apiserver.Config, cat *catalog.Catalog, dynamicClient dynamic.Interface, promClient *prometheus.Client) {
	config.GenericConfig.AddReadyzChecks(
		healthz.NamedCheck("catalog", func(_ *http.Request) error {
			if cat == nil {
				return fmt.Errorf("catalog not loaded")
			}

			return nil
		}),
		healthz.NamedCheck("service-account", func(r *http.Request) error {
			ctx, cancel := context.WithTimeout(r.Context(), readinessCheckTimeout)
			defer cancel()

			nodes := dynamicClient.Resource(resource.Node.GVR())
			if _, err := nodes.List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
				return fmt.Errorf("service account cannot list nodes: %w", err)
			}

			return nil
		}),
		healthz.NamedCheck("prometheus", func(r *http.Request) error {
			ctx, cancel := context.WithTimeout(r.Context(), readinessCheckTimeout)
			defer cancel()

			if err := promClient.Ping(ctx); err != nil {
				return fmt.Errorf("prometheus unreachable: %w", err)
			}

			return nil
		}),
	)
}
