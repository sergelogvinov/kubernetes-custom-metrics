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

// Package main's RBAC markers describe exactly the permissions
// internal/resolver needs to resolve Pods, Nodes and the six supported
// workload kinds (metric-gateway.md §3.3) with the gateway's own
// ServiceAccount. They are consumed by controller-gen (design.md §13) and
// carry no runtime behavior of their own.
//
// +kubebuilder:rbac:groups="",resources=pods;nodes,verbs=get;list
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list
package main
