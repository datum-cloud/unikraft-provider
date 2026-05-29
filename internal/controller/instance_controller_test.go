// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	core "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestInstance returns a minimal Instance for testing status sync.
func newTestInstance() *computev1alpha.Instance {
	return &computev1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-instance",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: computev1alpha.InstanceSpec{
			Runtime: computev1alpha.InstanceRuntimeSpec{
				Sandbox: &computev1alpha.SandboxRuntime{
					Containers: []computev1alpha.SandboxContainer{
						{Name: "app", Image: "oci.unikraft.io/official/nginx:latest"},
					},
				},
			},
		},
		Status: computev1alpha.InstanceStatus{
			Conditions: []metav1.Condition{},
		},
	}
}

func podWithPhase(phase core.PodPhase) *core.Pod {
	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     core.PodStatus{Phase: phase},
	}
}

// testScheme returns a scheme with the compute and core types registered,
// suitable for use with the fake client in unit tests.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add clientgo scheme: %v", err)
	}
	if err := computev1alpha.AddToScheme(s); err != nil {
		t.Fatalf("failed to add compute scheme: %v", err)
	}
	return s
}

// TestSyncInstancePowerState_ProgrammedCondition verifies that syncInstancePowerState
// sets the Programmed condition according to the Pod phase, matching the contract
// expected by compute's reconcileInstanceReadyCondition.
func TestSyncInstancePowerState_ProgrammedCondition(t *testing.T) {
	tests := []struct {
		name                 string
		pod                  *core.Pod
		wantProgrammed       metav1.ConditionStatus
		wantProgrammedReason string
	}{
		{
			name:                 "Pod running sets Programmed=True",
			pod:                  podWithPhase(core.PodRunning),
			wantProgrammed:       metav1.ConditionTrue,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammed,
		},
		{
			name:                 "Pod pending keeps Programmed=Unknown",
			pod:                  podWithPhase(core.PodPending),
			wantProgrammed:       metav1.ConditionUnknown,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammingInProgress,
		},
		{
			name:                 "Pod failed sets Programmed=False",
			pod:                  podWithPhase(core.PodFailed),
			wantProgrammed:       metav1.ConditionFalse,
			wantProgrammedReason: "Failed",
		},
		{
			name:                 "Pod succeeded sets Programmed=False",
			pod:                  podWithPhase(core.PodSucceeded),
			wantProgrammed:       metav1.ConditionFalse,
			wantProgrammedReason: computev1alpha.InstanceRunningReasonStopping,
		},
		{
			name:                 "Pod phase empty (unscheduled) keeps Programmed=Unknown",
			pod:                  podWithPhase(""),
			wantProgrammed:       metav1.ConditionUnknown,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammingInProgress,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance := newTestInstance()
			r := &InstanceReconciler{}

			// Use a fake client — syncInstancePowerState only calls Status().Update().
			fakeClient := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(instance).
				WithStatusSubresource(&computev1alpha.Instance{}).
				Build()

			err := r.syncInstancePowerState(context.Background(), fakeClient, instance, tc.pod)
			if err != nil {
				t.Fatalf("syncInstancePowerState returned unexpected error: %v", err)
			}

			programmed := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha.InstanceProgrammed)
			if programmed == nil {
				t.Fatal("expected Programmed condition to be set, got nil")
			}
			if programmed.Status != tc.wantProgrammed {
				t.Errorf("Programmed.Status = %q, want %q", programmed.Status, tc.wantProgrammed)
			}
			if programmed.Reason != tc.wantProgrammedReason {
				t.Errorf("Programmed.Reason = %q, want %q", programmed.Reason, tc.wantProgrammedReason)
			}
			if programmed.ObservedGeneration != instance.Generation {
				t.Errorf("Programmed.ObservedGeneration = %d, want %d", programmed.ObservedGeneration, instance.Generation)
			}
		})
	}
}

// TestSyncInstancePowerState_NoReadyConditionWritten verifies that the provider
// does NOT write a Ready condition, since compute's InstanceReconciler owns Ready
// and derives it from Programmed + Running. Writing Ready here would race.
func TestSyncInstancePowerState_NoReadyConditionWritten(t *testing.T) {
	instance := newTestInstance()
	r := &InstanceReconciler{}
	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	pod := podWithPhase(core.PodRunning)
	if err := r.syncInstancePowerState(context.Background(), fakeClient, instance, pod); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ready := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha.InstanceReady)
	if ready != nil {
		t.Errorf("provider must not write the Ready condition (owned by compute), but found: %+v", ready)
	}
}

// TestSyncInstancePowerState_PodRunning_RunningConditionTrue verifies the
// Running condition is also set correctly when the Pod is running, since compute
// requires Running=True (after Programmed=True) to set Ready=True.
func TestSyncInstancePowerState_PodRunning_RunningConditionTrue(t *testing.T) {
	instance := newTestInstance()
	r := &InstanceReconciler{}
	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	if err := r.syncInstancePowerState(context.Background(), fakeClient, instance, podWithPhase(core.PodRunning)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	running := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha.InstanceRunning)
	if running == nil {
		t.Fatal("expected Running condition to be set")
	}
	if running.Status != metav1.ConditionTrue {
		t.Errorf("Running.Status = %q, want True", running.Status)
	}
}
