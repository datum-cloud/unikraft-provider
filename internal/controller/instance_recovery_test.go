// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// These tests cover self-healing of an Instance whose backing VM died. Before
// the fix, reconcileSandboxContainers keyed only on Pod.CreationTimestamp and
// logged "skipping pod spec reconciliation; pod already exists" forever while
// the instance stayed down.

// terminatedPodForInstance returns an existing Pod whose container exited
// non-zero, the shape a kraftlet Pod takes when its VMM stops unexpectedly.
func terminatedPodForInstance(instance *computev1alpha.Instance, exitCode int32) *core.Pod {
	pod := runningPodForInstance(instance)
	pod.UID = types.UID(string(instance.UID) + "-pod")
	pod.Status.ContainerStatuses = []core.ContainerStatus{{
		Name: "app",
		State: core.ContainerState{
			Terminated: &core.ContainerStateTerminated{
				ExitCode: exitCode,
				Reason:   "Error",
			},
		},
	}}
	return pod
}

func recoveryTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testSchemeWithAll(t)).
		WithObjects(objs...).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()
}

// TestRecoverTerminatedPod_RecreatesDeadPod is the primary regression test: an
// instance whose container terminated with a non-zero exit code gets its Pod
// replaced, so the instance comes back without operator intervention.
func TestRecoverTerminatedPod_RecreatesDeadPod(t *testing.T) {
	ctx := context.Background()
	instance := instanceWithUID("terminated-uid")
	instance.Finalizers = []string{instanceFinalizer}
	pod := terminatedPodForInstance(instance, 1)

	cl := recoveryTestClient(t, instance, pod)
	r := &InstanceReconciler{Client: cl, Scheme: testSchemeWithAll(t)}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if res.RequeueAfter != podRecreateSettleDelay {
		t.Errorf("RequeueAfter = %v, want %v so the Pod is rebuilt on the next pass", res.RequeueAfter, podRecreateSettleDelay)
	}

	var got core.Pod
	err = cl.Get(ctx, client.ObjectKeyFromObject(pod), &got)
	if err == nil && got.DeletionTimestamp.IsZero() {
		t.Fatal("terminated pod still present and not deleted; instance cannot self-heal")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error reading pod: %v", err)
	}

	// The second pass must rebuild the Pod rather than leave the instance down.
	if err := cl.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("failed to clear pod: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("second Reconcile returned error: %v", err)
	}
	var rebuilt core.Pod
	if err := cl.Get(ctx, client.ObjectKeyFromObject(pod), &rebuilt); err != nil {
		t.Fatalf("expected pod to be recreated, got: %v", err)
	}
	if len(rebuilt.Spec.Containers) == 0 {
		t.Error("recreated pod has no containers; spec was not rebuilt")
	}
}

// TestRecoverTerminatedPod_LeavesHealthyPod verifies the loop does not disturb
// instances that are running or still starting.
func TestRecoverTerminatedPod_LeavesHealthyPod(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*core.Pod)
	}{
		{
			name:   "running",
			mutate: func(p *core.Pod) { p.Status.Phase = core.PodRunning },
		},
		{
			name: "starting",
			mutate: func(p *core.Pod) {
				p.Status.Phase = core.PodPending
				p.Status.ContainerStatuses = []core.ContainerStatus{{
					Name:  "app",
					State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"}},
				}}
			},
		},
		{
			name: "completed",
			mutate: func(p *core.Pod) {
				p.Status.Phase = core.PodSucceeded
				p.Status.ContainerStatuses = []core.ContainerStatus{{
					Name:  "app",
					State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}},
				}}
			},
		},
		{
			name: "terminated cleanly",
			mutate: func(p *core.Pod) {
				p.Status.ContainerStatuses = []core.ContainerStatus{{
					Name:  "app",
					State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}},
				}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			instance := instanceWithUID(types.UID("healthy-" + tc.name))
			instance.Finalizers = []string{instanceFinalizer}
			pod := runningPodForInstance(instance)
			pod.UID = types.UID("healthy-pod-" + tc.name)
			tc.mutate(pod)

			cl := recoveryTestClient(t, instance, pod)
			r := &InstanceReconciler{Client: cl, Scheme: testSchemeWithAll(t)}

			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}

			var got core.Pod
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
				t.Fatalf("pod was removed but should have been left alone: %v", err)
			}
			if !got.DeletionTimestamp.IsZero() {
				t.Error("pod was marked for deletion but should have been left alone")
			}
		})
	}
}

// TestRecoverTerminatedPod_IgnoresDeletingPod verifies a Pod already on its way
// out is not deleted again, so teardown is not disturbed.
func TestRecoverTerminatedPod_IgnoresDeletingPod(t *testing.T) {
	instance := instanceWithUID("deleting-uid")
	pod := terminatedPodForInstance(instance, 1)
	now := metav1.Now()
	pod.DeletionTimestamp = &now

	if reason, terminated := podTerminatedUnexpectedly(pod); terminated {
		t.Errorf("deleting pod reported as needing recreation: %q", reason)
	}
}

// TestRecoverTerminatedPod_SuspendedInstanceNotResurrected verifies that the
// recovery path never fights a deliberate platform suspension: a suspended
// Instance keeps its Pod deleted instead of being restarted.
func TestRecoverTerminatedPod_SuspendedInstanceNotResurrected(t *testing.T) {
	ctx := context.Background()
	instance := instanceWithUID("suspended-uid")
	instance.Finalizers = []string{instanceFinalizer}
	instance.Status.Suspended = true
	pod := terminatedPodForInstance(instance, 1)

	cl := recoveryTestClient(t, instance, pod)
	r := &InstanceReconciler{Client: cl, Scheme: testSchemeWithAll(t)}

	// Prime the tracker so a leaked entry would be observable.
	r.podRecreates.attempt(instance.UID, time.Now())

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if err := cl.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("failed to clear pod: %v", err)
	}

	// A second pass must not bring the instance back while it stays suspended.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("second Reconcile returned error: %v", err)
	}

	var got core.Pod
	err := cl.Get(ctx, client.ObjectKeyFromObject(pod), &got)
	if err == nil && got.DeletionTimestamp.IsZero() {
		t.Fatal("suspended instance was resurrected")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error reading pod: %v", err)
	}

	if _, attempt := r.podRecreates.attempt(instance.UID, time.Now()); attempt != 1 {
		t.Errorf("suspension left recreate history behind: attempt = %d, want 1", attempt)
	}
}

// TestRecoverTerminatedPod_BacksOffOnRepeatedFailure verifies that an instance
// which fails to come back — the broken-CNI case — is retried on a growing
// delay rather than recreated on every reconcile.
func TestRecoverTerminatedPod_BacksOffOnRepeatedFailure(t *testing.T) {
	ctx := context.Background()
	instance := instanceWithUID("flapping-uid")
	instance.Finalizers = []string{instanceFinalizer}
	pod := terminatedPodForInstance(instance, 1)

	cl := recoveryTestClient(t, instance, pod)
	r := &InstanceReconciler{Client: cl, Scheme: testSchemeWithAll(t)}

	// First failure recreates immediately.
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	if err != nil {
		t.Fatalf("first Reconcile returned error: %v", err)
	}
	if res.RequeueAfter != podRecreateSettleDelay {
		t.Fatalf("first RequeueAfter = %v, want %v", res.RequeueAfter, podRecreateSettleDelay)
	}

	// The replacement Pod dies the same way. The next reconcile must wait rather
	// than delete the Pod again.
	replacement := terminatedPodForInstance(instance, 1)
	replacement.UID = types.UID("flapping-pod-2")
	if err := cl.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("failed to clear pod: %v", err)
	}
	if err := cl.Create(ctx, replacement); err != nil {
		t.Fatalf("failed to seed replacement pod: %v", err)
	}

	res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	if err != nil {
		t.Fatalf("second Reconcile returned error: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > podRecreateBaseDelay {
		t.Errorf("second RequeueAfter = %v, want a backoff of at most %v", res.RequeueAfter, podRecreateBaseDelay)
	}

	var got core.Pod
	if err := cl.Get(ctx, client.ObjectKeyFromObject(replacement), &got); err != nil {
		t.Fatalf("replacement pod was deleted during backoff: %v", err)
	}
	if !got.DeletionTimestamp.IsZero() {
		t.Error("replacement pod deleted during backoff; recreation is hot-looping")
	}

	// Repeated failure must be visible on the Instance, not only in the logs.
	var updated computev1alpha.Instance
	if err := cl.Get(ctx, client.ObjectKeyFromObject(instance), &updated); err != nil {
		t.Fatalf("failed to read instance: %v", err)
	}
	cond := findCondition(t, updated.Status.Conditions, computev1alpha.InstanceAvailable)
	if cond.Status != metav1.ConditionFalse || cond.Reason != instanceRecoveringReason {
		t.Errorf("Available = %s/%s, want False/%s", cond.Status, cond.Reason, instanceRecoveringReason)
	}
}

// TestRecoverTerminatedPod_BackoffResetsOnRecovery verifies that an instance
// which comes back healthy starts its next failure from an immediate retry.
func TestRecoverTerminatedPod_BackoffResetsOnRecovery(t *testing.T) {
	ctx := context.Background()
	instance := instanceWithUID("recovered-uid")
	instance.Finalizers = []string{instanceFinalizer}
	pod := runningPodForInstance(instance)
	pod.UID = types.UID("recovered-pod")

	cl := recoveryTestClient(t, instance, pod)
	r := &InstanceReconciler{Client: cl, Scheme: testSchemeWithAll(t)}
	r.podRecreates.attempt(instance.UID, time.Now())

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if wait, attempt := r.podRecreates.attempt(instance.UID, time.Now()); wait != 0 || attempt != 1 {
		t.Errorf("after recovery: wait = %v, attempt = %d; want 0, 1", wait, attempt)
	}
}

func TestPodRecreateBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: 0},
		{failures: 1, want: podRecreateBaseDelay},
		{failures: 2, want: 2 * podRecreateBaseDelay},
		{failures: 3, want: 4 * podRecreateBaseDelay},
		{failures: 10, want: podRecreateMaxDelay},
		{failures: 1000, want: podRecreateMaxDelay},
	}

	for _, tc := range cases {
		if got := podRecreateBackoff(tc.failures); got != tc.want {
			t.Errorf("podRecreateBackoff(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

func TestPodRecreateTracker_Attempt(t *testing.T) {
	var tracker podRecreateTracker
	uid := types.UID("tracker-uid")
	start := time.Now()

	if wait, attempt := tracker.attempt(uid, start); wait != 0 || attempt != 1 {
		t.Fatalf("first attempt: wait = %v, attempt = %d; want 0, 1", wait, attempt)
	}
	if wait, attempt := tracker.attempt(uid, start); wait != podRecreateBaseDelay || attempt != 1 {
		t.Fatalf("immediate retry: wait = %v, attempt = %d; want %v, 1", wait, attempt, podRecreateBaseDelay)
	}
	if wait, attempt := tracker.attempt(uid, start.Add(podRecreateBaseDelay)); wait != 0 || attempt != 2 {
		t.Fatalf("after backoff: wait = %v, attempt = %d; want 0, 2", wait, attempt)
	}

	tracker.forget(uid)
	if wait, attempt := tracker.attempt(uid, start); wait != 0 || attempt != 1 {
		t.Fatalf("after forget: wait = %v, attempt = %d; want 0, 1", wait, attempt)
	}
}
