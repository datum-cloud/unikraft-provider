// SPDX-License-Identifier: AGPL-3.0-only

// Package controller – single-client behavior tests.
//
// These tests cover behavior introduced by the BUG-1 (multi-cluster removal)
// and BUG-2 (conflict-loop starvation) fixes:
//
//  1. A Pod transitioning to Running re-enqueues via Owns() and causes the
//     Instance to surface Programmed=True / Running=True using the local
//     client only (no upstreamClient/downstreamClient split).
//  2. The status write is a scoped MergeFrom patch; QuotaGranted/Ready
//     owned by compute's quota controller survive and are not overwritten.
//  3. An IsConflict on the status patch produces Requeue:true with nil error
//     (no hot-loop backoff; the reconciler is not responsible for retrying).
//  4. Pod and Service carry a controller ownerReference to the Instance so
//     Owns() re-enqueues on pod phase changes and native GC handles cleanup.
package controller

import (
	"context"
	"fmt"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// instanceWithUID returns a minimal Instance with a non-empty UID so that
// controllerutil.SetControllerReference can stamp a valid ownerReference.
func instanceWithUID(uid types.UID) *computev1alpha.Instance {
	inst := newTestInstance()
	inst.UID = uid
	return inst
}

// instanceWithPortsAndUID returns an Instance that declares a named container
// port so reconcileInstanceService will create a Service for it.
func instanceWithPortsAndUID(uid types.UID) *computev1alpha.Instance {
	inst := instanceWithUID(uid)
	inst.Spec.Runtime.Sandbox.Containers[0].Ports = []computev1alpha.NamedPort{
		{Name: "http", Port: 8080},
	}
	return inst
}

// runningPodForInstance returns a Pod that already exists in the cluster with
// Status.Phase=Running. The fake client preserves pod status across
// CreateOrPatch so the reconciler sees the Running phase when it looks up the
// pod inside reconcileSandboxContainers.
func runningPodForInstance(instance *computev1alpha.Instance) *core.Pod {
	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: core.PodSpec{
			Containers: []core.Container{{Name: "app", Image: "oci.unikraft.io/official/nginx:latest"}},
		},
		Status: core.PodStatus{Phase: core.PodRunning},
	}
}

// findCondition is a convenience wrapper around apimeta.FindStatusCondition
// that fails the test if the condition is absent.
func findCondition(t *testing.T, conditions []metav1.Condition, condType string) metav1.Condition {
	t.Helper()
	c := apimeta.FindStatusCondition(conditions, condType)
	if c == nil {
		t.Fatalf("expected condition %q to be set, got nil", condType)
	}
	return *c
}

// ---------------------------------------------------------------------------
// BUG-1 fix: single local client, Owns() re-enqueue
// ---------------------------------------------------------------------------

// TestReconcileSandboxContainers_RunningPod_SetsConditions is the primary
// regression test for BUG-1. It verifies the full reconcile path when a Pod
// already exists with Status.Phase=Running:
//
//   - reconcileSandboxContainers uses r.Client (one client, local cluster)
//   - the pod phase is observed correctly without any upstreamClient split
//   - Programmed=True and Running=True are set on the Instance
func TestReconcileSandboxContainers_RunningPod_SetsConditions(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithUID("run-uid-1")
	pod := runningPodForInstance(instance)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance, pod).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}

	result, err := r.reconcileSandboxContainers(ctx, instance)
	if err != nil {
		t.Fatalf("reconcileSandboxContainers returned error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected empty Result (no requeue), got %+v", result)
	}

	// Programmed must be True.
	programmed := findCondition(t, instance.Status.Conditions, computev1alpha.InstanceProgrammed)
	if programmed.Status != metav1.ConditionTrue {
		t.Errorf("Programmed.Status = %q, want True", programmed.Status)
	}
	if programmed.Reason != computev1alpha.InstanceProgrammedReasonProgrammed {
		t.Errorf("Programmed.Reason = %q, want %q", programmed.Reason, computev1alpha.InstanceProgrammedReasonProgrammed)
	}

	// Running must be True.
	running := findCondition(t, instance.Status.Conditions, computev1alpha.InstanceRunning)
	if running.Status != metav1.ConditionTrue {
		t.Errorf("Running.Status = %q, want True", running.Status)
	}
	if running.Reason != computev1alpha.InstanceRunningReasonRunning {
		t.Errorf("Running.Reason = %q, want %q", running.Reason, computev1alpha.InstanceRunningReasonRunning)
	}
}

// TestReconcileSandboxContainers_PendingPod_SetsUnknown verifies that when the
// Pod is still Pending the Instance conditions are Unknown (not True).
func TestReconcileSandboxContainers_PendingPod_SetsUnknown(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithUID("pend-uid-1")
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace},
		Spec:       core.PodSpec{Containers: []core.Container{{Name: "app", Image: "img"}}},
		Status:     core.PodStatus{Phase: core.PodPending},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance, pod).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}

	_, err := r.reconcileSandboxContainers(ctx, instance)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	programmed := findCondition(t, instance.Status.Conditions, computev1alpha.InstanceProgrammed)
	if programmed.Status != metav1.ConditionUnknown {
		t.Errorf("Programmed.Status = %q, want Unknown for PodPending", programmed.Status)
	}
	running := findCondition(t, instance.Status.Conditions, computev1alpha.InstanceRunning)
	if running.Status != metav1.ConditionUnknown {
		t.Errorf("Running.Status = %q, want Unknown for PodPending", running.Status)
	}
}

// ---------------------------------------------------------------------------
// BUG-2 fix: scoped MergeFrom patch, conflict → requeue
// ---------------------------------------------------------------------------

// TestSyncInstancePowerState_PatchPreservesStoredConditions verifies that the
// stored representation in the API server (read back via fake client.Get) still
// contains QuotaGranted=True after syncInstancePowerState runs. This catches a
// regression where a full Status().Update() would overwrite the field (BUG-2).
func TestSyncInstancePowerState_PatchPreservesStoredConditions(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithUID("patch-uid-1")
	// Pre-seed both QuotaGranted and Ready as if the compute quota controller
	// already wrote them before this reconcile runs.
	instance.Status.Conditions = []metav1.Condition{
		{
			Type:               computev1alpha.InstanceQuotaGranted,
			Status:             metav1.ConditionTrue,
			Reason:             "QuotaAvailable",
			ObservedGeneration: 1,
		},
		{
			Type:               computev1alpha.InstanceReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Running",
			ObservedGeneration: 1,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}

	if err := r.syncInstancePowerState(ctx, instance, podWithPhase(core.PodRunning)); err != nil {
		t.Fatalf("syncInstancePowerState returned unexpected error: %v", err)
	}

	// Read back from the fake API server — this is the source of truth.
	var stored computev1alpha.Instance
	if err := cl.Get(ctx, client.ObjectKeyFromObject(instance), &stored); err != nil {
		t.Fatalf("failed to get instance after patch: %v", err)
	}

	// QuotaGranted must still be True (the patch must not have overwritten it).
	qg := apimeta.FindStatusCondition(stored.Status.Conditions, computev1alpha.InstanceQuotaGranted)
	if qg == nil {
		t.Fatal("QuotaGranted condition lost from stored instance after patch")
	}
	if qg.Status != metav1.ConditionTrue {
		t.Errorf("stored QuotaGranted.Status = %q, want True (must not be overwritten by provider patch)", qg.Status)
	}

	// Ready must still be True.
	ready := apimeta.FindStatusCondition(stored.Status.Conditions, computev1alpha.InstanceReady)
	if ready == nil {
		t.Fatal("Ready condition lost from stored instance after patch")
	}
	if ready.Status != metav1.ConditionTrue {
		t.Errorf("stored Ready.Status = %q, want True (must not be overwritten by provider patch)", ready.Status)
	}

	// Provider-owned conditions must also be present and correct.
	programmed := apimeta.FindStatusCondition(stored.Status.Conditions, computev1alpha.InstanceProgrammed)
	if programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Errorf("stored Programmed should be True, got %v", programmed)
	}
	running := apimeta.FindStatusCondition(stored.Status.Conditions, computev1alpha.InstanceRunning)
	if running == nil || running.Status != metav1.ConditionTrue {
		t.Errorf("stored Running should be True, got %v", running)
	}
}

// TestReconcileSandboxContainers_ConflictOnStatusPatch verifies that when the
// status patch returns a 409 Conflict, reconcileSandboxContainers returns
// ctrl.Result{Requeue: true} with a nil error. This prevents exponential
// backoff hot-loops from a single Instance starving the worker pool (BUG-2).
func TestReconcileSandboxContainers_ConflictOnStatusPatch(t *testing.T) {
	tests := []struct {
		name        string
		patchErr    error
		wantRequeue bool
		wantErr     bool
	}{
		{
			name: "409 conflict → requeue:true, nil error",
			patchErr: apierrors.NewConflict(
				computev1alpha.GroupVersion.WithResource("instances").GroupResource(),
				"test-instance",
				fmt.Errorf("resource version mismatch"),
			),
			wantRequeue: true,
			wantErr:     false,
		},
		{
			name:        "no error → no requeue",
			patchErr:    nil,
			wantRequeue: false,
			wantErr:     false,
		},
		{
			name:        "non-conflict server error → returns error",
			patchErr:    apierrors.NewInternalError(fmt.Errorf("etcd unavailable")),
			wantRequeue: false,
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testScheme(t)

			instance := instanceWithUID("conflict-uid-1")
			// Pre-seed a running pod so reconcileSandboxContainers proceeds to the
			// status patch step without failing earlier.
			pod := runningPodForInstance(instance)

			baseClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(instance, pod).
				WithStatusSubresource(&computev1alpha.Instance{}).
				Build()

			var cl client.Client
			if tc.patchErr != nil {
				// Inject a fixed error on every SubResource patch call so we can
				// test the conflict-handling branch without a real API server.
				injectedErr := tc.patchErr
				cl = interceptor.NewClient(baseClient, interceptor.Funcs{
					SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
						return injectedErr
					},
				})
			} else {
				cl = baseClient
			}

			r := &InstanceReconciler{Client: cl, Scheme: s}

			result, err := r.reconcileSandboxContainers(ctx, instance)

			if tc.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got: %v", err)
			}

			gotRequeue := result.Requeue || result.RequeueAfter != 0
			if gotRequeue != tc.wantRequeue {
				t.Errorf("result.Requeue = %v (RequeueAfter=%v), want wantRequeue=%v",
					result.Requeue, result.RequeueAfter, tc.wantRequeue)
			}
		})
	}
}

// TestReconcileSandboxContainers_ConflictResult_IsNotError specifically asserts
// that the ctrl.Result returned on conflict satisfies ctrl.Result{Requeue:true}
// and that err == nil, matching the contract the reconciler framework expects
// for a simple requeue (no error rate-limiting).
func TestReconcileSandboxContainers_ConflictResult_IsNotError(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithUID("conflict-noerr-1")
	pod := runningPodForInstance(instance)

	baseClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance, pod).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	conflictErr := apierrors.NewConflict(
		computev1alpha.GroupVersion.WithResource("instances").GroupResource(),
		instance.Name,
		fmt.Errorf("resourceVersion mismatch"),
	)
	cl := interceptor.NewClient(baseClient, interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			return conflictErr
		},
	})

	r := &InstanceReconciler{Client: cl, Scheme: s}

	result, err := r.reconcileSandboxContainers(ctx, instance)

	// The contract: conflict → nil error so the framework does not apply
	// exponential back-off to this requeue.
	if err != nil {
		t.Errorf("conflict must produce nil error (not %v) to avoid exponential back-off", err)
	}
	// The contract: Requeue:true so the reconciler re-runs without delay.
	wantResult := ctrl.Result{Requeue: true}
	if result != wantResult {
		t.Errorf("result = %+v, want %+v", result, wantResult)
	}
}

// ---------------------------------------------------------------------------
// BUG-1 fix: ownerReference instead of upstream-* annotations
// ---------------------------------------------------------------------------

// TestPodCarriesControllerOwnerReference verifies that after reconciling an
// Instance, the resulting Pod has a controller ownerReference pointing to the
// Instance. Owns(&core.Pod{}) relies on ownerReferences to re-enqueue the
// Instance when the Pod phase changes; without this the BUG-1 dead pod-watch
// would silently return.
func TestPodCarriesControllerOwnerReference(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithUID("owner-ref-pod-uid")

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}

	if _, err := r.reconcileSandboxContainers(ctx, instance); err != nil {
		t.Fatalf("reconcileSandboxContainers returned error: %v", err)
	}

	// Read the Pod back from the API server.
	var pod core.Pod
	if err := cl.Get(ctx, client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}, &pod); err != nil {
		t.Fatalf("failed to get pod after reconcile: %v", err)
	}

	// Verify the controller ownerReference.
	ownerRefs := pod.OwnerReferences
	if len(ownerRefs) == 0 {
		t.Fatal("pod has no ownerReferences; expected a controller ownerReference to the Instance")
	}

	var found bool
	for _, ref := range ownerRefs {
		if ref.Name == instance.Name && ref.UID == instance.UID {
			found = true
			if ref.Controller == nil || !*ref.Controller {
				t.Error("ownerReference.Controller must be true so Owns() re-enqueues on pod events")
			}
			if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
				t.Error("ownerReference.BlockOwnerDeletion must be true (set by SetControllerReference)")
			}
			break
		}
	}
	if !found {
		t.Errorf("pod ownerReferences %v do not contain an entry for instance %s/%s",
			ownerRefs, instance.Name, instance.UID)
	}

	// Verify the legacy upstream-* annotations are NOT set (they are the old
	// multi-cluster routing mechanism that caused BUG-1).
	for k := range pod.Annotations {
		if k == "meta.datumapis.com/upstream-cluster-name" || k == "meta.datumapis.com/upstream-namespace" {
			t.Errorf("pod carries legacy upstream annotation %q which must not be set in single-cell mode", k)
		}
	}
}

// TestServiceCarriesControllerOwnerReference verifies that the Service created
// for an Instance with container ports has a controller ownerReference to the
// Instance. This allows Owns(&core.Service{}) to work and ensures GC handles
// the Service when the Instance is deleted.
func TestServiceCarriesControllerOwnerReference(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithPortsAndUID("owner-ref-svc-uid")

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}

	if _, err := r.reconcileSandboxContainers(ctx, instance); err != nil {
		t.Fatalf("reconcileSandboxContainers returned error: %v", err)
	}

	// Read the Service back from the API server.
	var svc core.Service
	if err := cl.Get(ctx, client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}, &svc); err != nil {
		t.Fatalf("failed to get service after reconcile: %v", err)
	}

	// Verify the controller ownerReference.
	ownerRefs := svc.OwnerReferences
	if len(ownerRefs) == 0 {
		t.Fatal("service has no ownerReferences; expected a controller ownerReference to the Instance")
	}

	var found bool
	for _, ref := range ownerRefs {
		if ref.Name == instance.Name && ref.UID == instance.UID {
			found = true
			if ref.Controller == nil || !*ref.Controller {
				t.Error("service ownerReference.Controller must be true so Owns() re-enqueues on service events")
			}
			break
		}
	}
	if !found {
		t.Errorf("service ownerReferences %v do not contain an entry for instance %s/%s",
			ownerRefs, instance.Name, instance.UID)
	}

	// Verify the legacy upstream-* annotations are NOT set.
	for k := range svc.Annotations {
		if k == "meta.datumapis.com/upstream-cluster-name" || k == "meta.datumapis.com/upstream-namespace" {
			t.Errorf("service carries legacy upstream annotation %q which must not be set in single-cell mode", k)
		}
	}
}

// TestPodAndService_SameNamespaceAsInstance verifies that Pod and Service are
// created in the same namespace as the Instance. In single-cell mode there is
// no namespace mapping; co-location in the same namespace is a prerequisite for
// GC via ownerReferences.
func TestPodAndService_SameNamespaceAsInstance(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithPortsAndUID("ns-check-uid")
	instance.Namespace = "custom-ns"
	// ObjectMeta.Namespace must be in the scheme; fake client handles arbitrary namespaces.

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}

	if _, err := r.reconcileSandboxContainers(ctx, instance); err != nil {
		t.Fatalf("reconcileSandboxContainers returned error: %v", err)
	}

	var pod core.Pod
	if err := cl.Get(ctx, client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}, &pod); err != nil {
		t.Fatalf("pod not found in instance namespace %q: %v", instance.Namespace, err)
	}
	if pod.Namespace != instance.Namespace {
		t.Errorf("pod.Namespace = %q, want %q", pod.Namespace, instance.Namespace)
	}

	var svc core.Service
	if err := cl.Get(ctx, client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}, &svc); err != nil {
		t.Fatalf("service not found in instance namespace %q: %v", instance.Namespace, err)
	}
	if svc.Namespace != instance.Namespace {
		t.Errorf("service.Namespace = %q, want %q", svc.Namespace, instance.Namespace)
	}
}

// TestReconcile_DeletedInstance_Noop verifies that reconciliation of a deleted
// instance (DeletionTimestamp set) returns immediately without error. With
// ownerRef-based GC the provider has no finalizer to process.
func TestReconcile_DeletedInstance_Noop(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	now := metav1.Now()
	instance := instanceWithUID("deleted-uid-1")
	instance.DeletionTimestamp = &now
	instance.Finalizers = []string{"foregroundDeletion"} // prevents actual removal in fake client

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	if err != nil {
		t.Errorf("expected nil error for deleted instance, got: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected empty Result for deleted instance, got: %+v", result)
	}
}
