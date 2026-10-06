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

package resolver

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

// resolveNamedCronJob resolves a named CronJob following the fallback order
// in metric-gateway.md §3.4: active Jobs, else recent Jobs within the
// fallback window, else the specific NotFound shape from §3.4.
func (r *Resolver) resolveNamedCronJob(ctx context.Context, namespace, name string) (Resolution, error) {
	cronJob, err := r.getObject(ctx, kindSpecs[KindCronJob], KindCronJob, namespace, name)
	if err != nil {
		return Resolution{}, err
	}

	podNames, usedFallback, err := r.resolveCronJobPodNames(ctx, cronJob)
	if err != nil {
		return Resolution{}, err
	}

	return Resolution{Object: objectRefFrom(cronJob), PodNames: podNames, CronJobFallback: usedFallback}, nil
}

// resolveCronJobPodNames implements the CronJob fallback logic: active Jobs
// (status.active > 0, owner UID match) → else Jobs within the fallback
// window (by status.completionTime or status.startTime) → else
// *NotFoundError (metric-gateway.md §3.4). usedFallback reports whether the
// recent-Jobs fallback (rather than an active Job) produced the result, for
// the caller's gateway_cronjob_fallback_total telemetry.
func (r *Resolver) resolveCronJobPodNames(ctx context.Context, cronJob *unstructured.Unstructured) (names []string, usedFallback bool, err error) {
	namespace, name, cronJobUID := cronJob.GetNamespace(), cronJob.GetName(), cronJob.GetUID()

	jobs, err := r.listObjects(ctx, kindSpecs[KindJob], KindJob, namespace, labels.Everything())
	if err != nil {
		return nil, false, err
	}

	owned := ownedJobs(jobs, cronJobUID)

	selected := filterActiveJobs(owned)
	if len(selected) == 0 {
		usedFallback = true
		selected = filterRecentJobs(owned, r.clock.Now(), r.cronJobFallbackWindow)
	}

	if len(selected) == 0 {
		return nil, false, &NotFoundError{
			Kind:      KindCronJob,
			Namespace: namespace,
			Name:      name,
			Message: fmt.Sprintf("no active or recent (%s) Jobs for CronJob %s",
				formatWindow(r.cronJobFallbackWindow), namespacedName(namespace, name)),
		}
	}

	selectors := make([]labels.Selector, 0, len(selected))
	for i := range selected {
		selector, err := extractSelector(&selected[i])
		if err != nil {
			return nil, false, fmt.Errorf("resolver: Job %s: %w", namespacedName(namespace, selected[i].GetName()), err)
		}
		selectors = append(selectors, selector)
	}

	podNames, err := r.unionPodNames(ctx, namespace, selectors)
	if err != nil {
		return nil, false, err
	}

	return podNames, usedFallback, nil
}

// ownedJobs returns the Jobs in jobs whose controller ownerReference UID
// matches cronJobUID. Ownership is checked by UID, not by name.
func ownedJobs(jobs []unstructured.Unstructured, cronJobUID types.UID) []unstructured.Unstructured {
	var owned []unstructured.Unstructured
	for i := range jobs {
		for _, ref := range jobs[i].GetOwnerReferences() {
			if ref.UID == cronJobUID {
				owned = append(owned, jobs[i])

				break
			}
		}
	}

	return owned
}

func jobActiveCount(job *unstructured.Unstructured) int64 {
	active, found, _ := unstructured.NestedInt64(job.Object, "status", "active") //nolint:errcheck
	if !found {
		return 0
	}

	return active
}

// filterActiveJobs returns Jobs with status.active > 0.
func filterActiveJobs(jobs []unstructured.Unstructured) []unstructured.Unstructured {
	var active []unstructured.Unstructured
	for i := range jobs {
		if jobActiveCount(&jobs[i]) > 0 {
			active = append(active, jobs[i])
		}
	}

	return active
}

// filterRecentJobs returns Jobs whose status.completionTime or
// status.startTime falls within window of now.
func filterRecentJobs(jobs []unstructured.Unstructured, now time.Time, window time.Duration) []unstructured.Unstructured {
	var recent []unstructured.Unstructured
	for i := range jobs {
		if withinWindow(jobTimestamp(&jobs[i], "completionTime"), now, window) ||
			withinWindow(jobTimestamp(&jobs[i], "startTime"), now, window) {
			recent = append(recent, jobs[i])
		}
	}

	return recent
}

func withinWindow(t time.Time, now time.Time, window time.Duration) bool {
	if t.IsZero() {
		return false
	}
	age := now.Sub(t)

	return age >= 0 && age <= window
}

func jobTimestamp(job *unstructured.Unstructured, field string) time.Time {
	raw, found, _ := unstructured.NestedString(job.Object, "status", field) //nolint:errcheck
	if !found {
		return time.Time{}
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}

	return t
}

// formatWindow renders d the way metric-gateway.md §3.4's example message
// does ("24h"), rather than Go's default zero-padded form ("24h0m0s").
func formatWindow(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}

	return d.String()
}
