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
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	clienttesting "k8s.io/client-go/testing"
	clock "k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
)

func newFakeClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClient(scheme.Scheme, objs...)
}

func stringSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}

	return m
}

// --- Pod / Node direct lookup ----------------------------------------------

func TestResolve_PodNamed(t *testing.T) {
	client := newFakeClient(&corev1.Pod{
		Namespace: "prod", Name: "web-0", UID: types.UID("uid-web-0"),
	})
	r := New(client)

	got, err := r.Resolve(context.Background(), Target{Kind: KindPod, Namespace: "prod", Name: "web-0"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Object.UID != "uid-web-0" || got[0].Object.Namespace != "prod" || got[0].Object.Name != "web-0" {
		t.Errorf("Object = %+v", got[0].Object)
	}
	if want := []string{"web-0"}; !equalNames(got[0].PodNames, want) {
		t.Errorf("PodNames = %v, want %v", got[0].PodNames, want)
	}
}

func TestResolve_NodeNamed(t *testing.T) {
	client := newFakeClient(&corev1.Node{
		Name: "worker-1", UID: types.UID("uid-worker-1"),
	})
	r := New(client)

	got, err := r.Resolve(context.Background(), Target{Kind: KindNode, Name: "worker-1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Object.UID != "uid-worker-1" {
		t.Errorf("Object.UID = %q, want uid-worker-1", got[0].Object.UID)
	}
	if len(got[0].PodNames) != 0 {
		t.Errorf("PodNames = %v, want empty for a Node target", got[0].PodNames)
	}
}

func TestResolve_NamedNotFound(t *testing.T) {
	client := newFakeClient()
	r := New(client)

	_, err := r.Resolve(context.Background(), Target{Kind: KindPod, Namespace: "prod", Name: "does-not-exist"})

	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want *NotFoundError", err)
	}
	if notFound.Kind != KindPod || notFound.Namespace != "prod" || notFound.Name != "does-not-exist" {
		t.Errorf("NotFoundError = %+v", notFound)
	}
}

// --- Workload matchExpressions selectors -----------------------------------

func TestResolve_WorkloadMatchExpressionsSelector(t *testing.T) {
	selector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"frontend"}},
		},
	}

	deploy := &appsv1.Deployment{
		Namespace: "prod", Name: "web", UID: types.UID("uid-deploy-web"),
		Spec: appsv1.DeploymentSpec{Selector: selector},
	}
	matching1 := pod("prod", "web-0", "uid-web-0", map[string]string{"tier": "frontend"})
	matching2 := pod("prod", "web-1", "uid-web-1", map[string]string{"tier": "frontend"})
	nonMatching := pod("prod", "cache-0", "uid-cache-0", map[string]string{"tier": "cache"})

	client := newFakeClient(deploy, matching1, matching2, nonMatching)
	r := New(client)

	got, err := r.Resolve(context.Background(), Target{Kind: KindDeployment, Namespace: "prod", Name: "web"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Object.UID != "uid-deploy-web" {
		t.Errorf("Object.UID = %q, want uid-deploy-web", got[0].Object.UID)
	}
	want := []string{"web-0", "web-1"}
	if !equalNames(got[0].PodNames, want) {
		t.Errorf("PodNames = %v, want %v", got[0].PodNames, want)
	}
}

func TestResolve_EachWorkloadKindUsesItsSpecSelector(t *testing.T) {
	labelSelector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	matching := pod("prod", "web-0", "uid-web-0", map[string]string{"app": "web"})

	cases := []struct {
		kind Kind
		obj  runtime.Object
	}{
		{KindDeployment, &appsv1.Deployment{
			Namespace: "prod", Name: "web", UID: types.UID("uid-w"),
			Spec: appsv1.DeploymentSpec{Selector: labelSelector},
		}},
		{KindStatefulSet, &appsv1.StatefulSet{
			Namespace: "prod", Name: "web", UID: types.UID("uid-w"),
			Spec: appsv1.StatefulSetSpec{Selector: labelSelector},
		}},
		{KindDaemonSet, &appsv1.DaemonSet{
			Namespace: "prod", Name: "web", UID: types.UID("uid-w"),
			Spec: appsv1.DaemonSetSpec{Selector: labelSelector},
		}},
		{KindJob, &batchv1.Job{
			Namespace: "prod", Name: "web", UID: types.UID("uid-w"),
			Spec: batchv1.JobSpec{Selector: labelSelector},
		}},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			client := newFakeClient(tc.obj, matching.DeepCopy())
			r := New(client)

			got, err := r.Resolve(context.Background(), Target{Kind: tc.kind, Namespace: "prod", Name: "web"})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(got) != 1 || len(got[0].PodNames) != 1 || got[0].PodNames[0] != "web-0" {
				t.Errorf("got = %+v, want one Resolution with PodNames=[web-0]", got)
			}
		})
	}
}

// --- Pod name union across overlapping selectors ----------------------------

func TestUnionPodNames_DedupesOverlappingSelectors(t *testing.T) {
	podA := pod("prod", "a", "uid-a", map[string]string{"group": "x"})
	podB := pod("prod", "b", "uid-b", map[string]string{"group": "x", "extra": "y"})
	podC := pod("prod", "c", "uid-c", map[string]string{"extra": "y"})

	client := newFakeClient(podA, podB, podC)
	r := New(client)

	selX, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"group": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	selY, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"extra": "y"}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.unionPodNames(context.Background(), "prod", []labels.Selector{selX, selY})
	if err != nil {
		t.Fatalf("unionPodNames: %v", err)
	}

	want := []string{"a", "b", "c"}
	if !equalNames(got, want) {
		t.Errorf("got = %v, want %v (b matched by both selectors, must not duplicate)", got, want)
	}
}

// --- CronJob active / recent / 404 branching -------------------------------

func TestResolveCronJob_PrefersActiveJobs(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cronJob := &batchv1.CronJob{
		Namespace: "prod", Name: "nightly", UID: types.UID("uid-cronjob"),
	}
	activeJob := ownedJob("prod", "nightly-123", "uid-job-active", "uid-cronjob", batchv1.JobStatus{Active: 1})
	activeJob.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"job": "active"}}
	recentJob := ownedJob("prod", "nightly-100", "uid-job-recent", "uid-cronjob", batchv1.JobStatus{
		CompletionTime: new(metav1.NewTime(now.Add(-1 * time.Hour))),
	})
	recentJob.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"job": "recent"}}

	activePod := pod("prod", "active-pod", "uid-active-pod", map[string]string{"job": "active"})
	recentPod := pod("prod", "recent-pod", "uid-recent-pod", map[string]string{"job": "recent"})

	client := newFakeClient(cronJob, activeJob, recentJob, activePod, recentPod)
	r := New(client, WithClock(clocktesting.NewFakePassiveClock(now)))

	got, err := r.Resolve(context.Background(), Target{Kind: KindCronJob, Namespace: "prod", Name: "nightly"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	// Only the active Job's pods are selected; the recent one is ignored
	// because an active Job exists (metric-gateway.md §3.4 step 1).
	want := []string{"active-pod"}
	if !equalNames(got[0].PodNames, want) {
		t.Errorf("PodNames = %v, want %v", got[0].PodNames, want)
	}
	if got[0].CronJobFallback {
		t.Error("CronJobFallback = true, want false when an active Job was used")
	}
}

func TestResolveCronJob_FallsBackToRecentJobs(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cronJob := &batchv1.CronJob{
		Namespace: "prod", Name: "nightly", UID: types.UID("uid-cronjob"),
	}
	recentJob := ownedJob("prod", "nightly-100", "uid-job-recent", "uid-cronjob", batchv1.JobStatus{
		CompletionTime: new(metav1.NewTime(now.Add(-2 * time.Hour))),
	})
	recentJob.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"job": "recent"}}
	tooOldJob := ownedJob("prod", "nightly-1", "uid-job-old", "uid-cronjob", batchv1.JobStatus{
		CompletionTime: new(metav1.NewTime(now.Add(-48 * time.Hour))),
	})
	tooOldJob.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"job": "old"}}

	recentPod := pod("prod", "recent-pod", "uid-recent-pod", map[string]string{"job": "recent"})
	oldPod := pod("prod", "old-pod", "uid-old-pod", map[string]string{"job": "old"})

	client := newFakeClient(cronJob, recentJob, tooOldJob, recentPod, oldPod)
	r := New(client, WithClock(clocktesting.NewFakePassiveClock(now)), WithCronJobFallbackWindow(24*time.Hour))

	got, err := r.Resolve(context.Background(), Target{Kind: KindCronJob, Namespace: "prod", Name: "nightly"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := []string{"recent-pod"}
	if !equalNames(got[0].PodNames, want) {
		t.Errorf("PodNames = %v, want %v (only the within-window Job)", got[0].PodNames, want)
	}
	if !got[0].CronJobFallback {
		t.Error("CronJobFallback = false, want true when the recent-Jobs fallback was used")
	}
}

func TestResolveCronJob_NoActiveOrRecentIs404(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cronJob := &batchv1.CronJob{
		Namespace: "prod", Name: "nightly", UID: types.UID("uid-cronjob"),
	}
	tooOldJob := ownedJob("prod", "nightly-1", "uid-job-old", "uid-cronjob", batchv1.JobStatus{
		CompletionTime: new(metav1.NewTime(now.Add(-48 * time.Hour))),
	})

	client := newFakeClient(cronJob, tooOldJob)
	r := New(client, WithClock(clocktesting.NewFakePassiveClock(now)), WithCronJobFallbackWindow(24*time.Hour))

	_, err := r.Resolve(context.Background(), Target{Kind: KindCronJob, Namespace: "prod", Name: "nightly"})

	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want *NotFoundError", err)
	}
	if want := "no active or recent (24h) Jobs for CronJob prod/nightly"; notFound.Message != want {
		t.Errorf("Message = %q, want %q", notFound.Message, want)
	}
}

func TestResolveCronJob_UnrelatedJobIgnored(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cronJob := &batchv1.CronJob{
		Namespace: "prod", Name: "nightly", UID: types.UID("uid-cronjob"),
	}
	// Same name prefix, but owned by a different CronJob UID: ownership must
	// be checked by UID, not name (design.md §7).
	unrelated := ownedJob("prod", "nightly-999", "uid-job-unrelated", "uid-other-cronjob", batchv1.JobStatus{Active: 1})

	client := newFakeClient(cronJob, unrelated)
	r := New(client, WithClock(clocktesting.NewFakePassiveClock(now)))

	_, err := r.Resolve(context.Background(), Target{Kind: KindCronJob, Namespace: "prod", Name: "nightly"})

	if _, ok := errors.AsType[*NotFoundError](err); !ok {
		t.Fatalf("err = %v, want *NotFoundError (unrelated Job must not count)", err)
	}
}

// --- Pod/Job deletion and recreation with a new UID ------------------------

func TestResolve_PodRecreationChangesUID(t *testing.T) {
	client := newFakeClient(pod("prod", "web-0", "uid-web-0-gen1", nil))
	r := New(client)

	first, err := r.Resolve(context.Background(), Target{Kind: KindPod, Namespace: "prod", Name: "web-0"})
	if err != nil {
		t.Fatalf("Resolve (gen1): %v", err)
	}
	if first[0].Object.UID != "uid-web-0-gen1" {
		t.Fatalf("gen1 UID = %q", first[0].Object.UID)
	}

	ctx := context.Background()
	if err := client.Resource(kindSpecs[KindPod].gvr).Namespace("prod").Delete(ctx, "web-0", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting gen1 pod: %v", err)
	}
	recreated := pod("prod", "web-0", "uid-web-0-gen2", nil)
	if _, err := client.Resource(kindSpecs[KindPod].gvr).Namespace("prod").Create(ctx, toUnstructured(t, recreated), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating gen2 pod: %v", err)
	}

	second, err := r.Resolve(ctx, Target{Kind: KindPod, Namespace: "prod", Name: "web-0"})
	if err != nil {
		t.Fatalf("Resolve (gen2): %v", err)
	}
	if second[0].Object.UID != "uid-web-0-gen2" {
		t.Errorf("gen2 UID = %q, want uid-web-0-gen2 (stale gen1 UID must not leak)", second[0].Object.UID)
	}
}

// --- Wildcard listing -------------------------------------------------------

func TestResolve_WildcardPodsSortedAndFiltered(t *testing.T) {
	client := newFakeClient(
		pod("prod", "b", "uid-b", map[string]string{"app": "web"}),
		pod("prod", "a", "uid-a", map[string]string{"app": "web"}),
		pod("prod", "other", "uid-other", map[string]string{"app": "other"}),
	)
	r := New(client)

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.Resolve(context.Background(), Target{Kind: KindPod, Namespace: "prod", ObjectSelector: selector})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Object.Name != "a" || got[1].Object.Name != "b" {
		t.Errorf("order = [%s, %s], want [a, b]", got[0].Object.Name, got[1].Object.Name)
	}
}

func TestResolve_WildcardCronJobOmitsNotFoundInstead(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	withJobs := &batchv1.CronJob{Namespace: "prod", Name: "has-runs", UID: types.UID("uid-cj-1")}
	withoutJobs := &batchv1.CronJob{Namespace: "prod", Name: "no-runs", UID: types.UID("uid-cj-2")}
	activeJob := ownedJob("prod", "has-runs-1", "uid-job-1", "uid-cj-1", batchv1.JobStatus{Active: 1})
	activeJob.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"job": "1"}}
	activePod := pod("prod", "pod-1", "uid-pod-1", map[string]string{"job": "1"})

	client := newFakeClient(withJobs, withoutJobs, activeJob, activePod)
	r := New(client, WithClock(clocktesting.NewFakePassiveClock(now)))

	got, err := r.Resolve(context.Background(), Target{Kind: KindCronJob, Namespace: "prod", ObjectSelector: labels.Everything()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (the CronJob with no active/recent Jobs must be omitted, not error)", len(got))
	}
	if got[0].Object.Name != "has-runs" {
		t.Errorf("got[0].Object.Name = %q, want has-runs", got[0].Object.Name)
	}
}

func TestResolve_WildcardEmptyIsEmptyNotError(t *testing.T) {
	client := newFakeClient()
	r := New(client)

	got, err := r.Resolve(context.Background(), Target{Kind: KindPod, Namespace: "prod", ObjectSelector: labels.Everything()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// --- ServiceAccount permission failures map to ForbiddenError, not 403 -----

func TestResolve_ForbiddenMapsToForbiddenError(t *testing.T) {
	client := newFakeClient()
	client.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "web-0", errors.New("RBAC denied"))
	})
	r := New(client)

	_, err := r.Resolve(context.Background(), Target{Kind: KindPod, Namespace: "prod", Name: "web-0"})

	if _, ok := errors.AsType[*ForbiddenError](err); !ok {
		t.Fatalf("err = %v, want *ForbiddenError", err)
	}
}

// --- Pagination across continue-token pages --------------------------------

func TestListObjects_FollowsContinueTokens(t *testing.T) {
	client := newFakeClient(
		pod("prod", "a", "uid-a", nil),
		pod("prod", "b", "uid-b", nil),
		pod("prod", "c", "uid-c", nil),
	)

	// The fake dynamic client's tracker does not itself paginate; simulate a
	// server that does by splitting the full list across two pages the
	// first time it is called.
	calls := 0
	client.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		listAction, ok := action.(clienttesting.ListActionImpl)
		if !ok {
			return false, nil, nil
		}

		full, err := client.Tracker().List(listAction.GetResource(), listAction.GetKind(), listAction.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		typed, ok := full.(*unstructured.UnstructuredList)
		if !ok {
			t.Fatalf("tracker list returned %T, want *unstructured.UnstructuredList", full)
		}

		calls++
		page := typed.DeepCopy()
		if calls == 1 {
			page.Items = typed.Items[:2]
			page.SetContinue("page-2")
		} else {
			page.Items = typed.Items[2:]
			page.SetContinue("")
		}

		return true, page, nil
	})

	r := New(client, WithPageSize(2))

	objs, err := r.listObjects(context.Background(), kindSpecs[KindPod], KindPod, "prod", labels.Everything())
	if err != nil {
		t.Fatalf("listObjects: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("len(objs) = %d, want 3 across two pages", len(objs))
	}
	if calls != 2 {
		t.Errorf("reactor called %d times, want 2 (one per page)", calls)
	}
}

// --- helpers -----------------------------------------------------------

func pod(namespace, name, uid string, podLabels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		Namespace: namespace,
		Name:      name,
		UID:       types.UID(uid),
		Labels:    podLabels,
	}
}

func ownedJob(namespace, name, uid, ownerUID string, status batchv1.JobStatus) *batchv1.Job {
	return &batchv1.Job{
		Namespace: namespace,
		Name:      name,
		UID:       types.UID(uid),
		OwnerReferences: []metav1.OwnerReference{
			{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       "irrelevant-for-uid-matching",
				UID:        types.UID(ownerUID),
				Controller: new(true),
			},
		},
		Status: status,
	}
}

func toUnstructured(t *testing.T, obj runtime.Object) *unstructured.Unstructured {
	t.Helper()

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("converting to unstructured: %v", err)
	}

	return &unstructured.Unstructured{Object: raw}
}

//go:fix inline
func ptrTime(t metav1.Time) *metav1.Time { return new(t) }

//go:fix inline
func boolPtr(b bool) *bool { return new(b) }

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	gotSet, wantSet := stringSet(got...), stringSet(want...)
	if len(gotSet) != len(wantSet) {
		return false
	}
	for n := range wantSet {
		if !gotSet[n] {
			return false
		}
	}
	// PodNames must also be sorted for determinism.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			return false
		}
	}

	return true
}

var _ = clock.RealClock{} // ensure the clock import is used even if tests are trimmed
