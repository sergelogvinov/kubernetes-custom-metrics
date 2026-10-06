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
	customv1beta1 "k8s.io/metrics/pkg/apis/custom_metrics/v1beta1"
	customv1beta2 "k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/apiserver"
)

// sigs.k8s.io/custom-metrics-apiserver installs custom.metrics.k8s.io with
// v1beta1 prioritized ahead of v1beta2 (k8s.io/metrics/pkg/apis/custom_metrics/install),
// so discovery would otherwise advertise v1beta1 as the preferred (and, per
// the framework's own discovery-registration loop, only advertised) version.
// This repository serves only v1beta2 (metric-gateway.md §3.6), so we
// change the priority of the shared scheme once at start, and discovery
// shows v1beta2.
func init() {
	if err := apiserver.Scheme.SetVersionPriority(customv1beta2.SchemeGroupVersion, customv1beta1.SchemeGroupVersion); err != nil {
		panic(err)
	}
}
