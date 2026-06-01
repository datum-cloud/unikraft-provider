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

// podPendingWithWaiting builds a pending pod whose first container has the
// given waiting reason and message, mirroring what the kubelet reports when
// an image pull fails, a container crashes, etc.
func podPendingWithWaiting(k8sReason, k8sMessage string) *core.Pod {
	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status: core.PodStatus{
			Phase: core.PodPending,
			ContainerStatuses: []core.ContainerStatus{
				{
					Name: "app",
					State: core.ContainerState{
						Waiting: &core.ContainerStateWaiting{
							Reason:  k8sReason,
							Message: k8sMessage,
						},
					},
				},
			},
		},
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
// sets the Programmed condition according to the underlying runtime phase, matching
// the contract expected by compute's reconcileInstanceReadyCondition.
func TestSyncInstancePowerState_ProgrammedCondition(t *testing.T) {
	tests := []struct {
		name                 string
		pod                  *core.Pod
		wantProgrammed       metav1.ConditionStatus
		wantProgrammedReason string
	}{
		{
			name:                 "instance running sets Programmed=True",
			pod:                  podWithPhase(core.PodRunning),
			wantProgrammed:       metav1.ConditionTrue,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammed,
		},
		{
			name:                 "instance provisioning keeps Programmed=Unknown",
			pod:                  podWithPhase(core.PodPending),
			wantProgrammed:       metav1.ConditionUnknown,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammingInProgress,
		},
		{
			name:                 "instance failed sets Programmed=False",
			pod:                  podWithPhase(core.PodFailed),
			wantProgrammed:       metav1.ConditionFalse,
			wantProgrammedReason: "Failed",
		},
		{
			name:                 "instance stopped sets Programmed=False",
			pod:                  podWithPhase(core.PodSucceeded),
			wantProgrammed:       metav1.ConditionFalse,
			wantProgrammedReason: computev1alpha.InstanceRunningReasonStopping,
		},
		{
			name:                 "instance state unknown keeps Programmed=Unknown",
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

	// Use a running instance; if Ready were ever written it would appear here.
	if err := r.syncInstancePowerState(context.Background(), fakeClient, instance, podWithPhase(core.PodRunning)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ready := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha.InstanceReady)
	if ready != nil {
		t.Errorf("provider must not write the Ready condition (owned by compute), but found: %+v", ready)
	}
}

// TestSyncInstancePowerState_InstanceRunning_RunningConditionTrue verifies the
// Running condition is set correctly when the instance is running, since compute
// requires Running=True (after Programmed=True) to set Ready=True.
func TestSyncInstancePowerState_InstanceRunning_RunningConditionTrue(t *testing.T) {
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

// TestTranslateWaitingReason verifies that known k8s container waiting reasons
// are translated into Instance-domain reason/message pairs, and that no raw k8s
// string ever reaches a condition field.
func TestTranslateWaitingReason(t *testing.T) {
	tests := []struct {
		k8sReason     string
		k8sMessage    string
		wantReason    string
		wantMessage   string
		wantNotReason string // raw k8s string that must NOT appear as the reason
	}{
		{
			k8sReason:     "ImagePullBackOff",
			k8sMessage:    "Back-off pulling image",
			wantReason:    "ImageUnavailable",
			wantMessage:   "The instance image could not be pulled",
			wantNotReason: "ImagePullBackOff",
		},
		{
			k8sReason:     "ErrImagePull",
			k8sMessage:    "rpc error: ...",
			wantReason:    "ImageUnavailable",
			wantMessage:   "The instance image could not be pulled",
			wantNotReason: "ErrImagePull",
		},
		{
			k8sReason:     "ImageInspectError",
			wantReason:    "ImageUnavailable",
			wantMessage:   "The instance image could not be pulled",
			wantNotReason: "ImageInspectError",
		},
		{
			k8sReason:     "InvalidImageName",
			wantReason:    "ImageUnavailable",
			wantMessage:   "The instance image could not be pulled",
			wantNotReason: "InvalidImageName",
		},
		{
			k8sReason:     "RegistryUnavailable",
			wantReason:    "ImageUnavailable",
			wantMessage:   "The instance image could not be pulled",
			wantNotReason: "RegistryUnavailable",
		},
		{
			k8sReason:     "CrashLoopBackOff",
			k8sMessage:    "back-off 5m0s restarting failed container",
			wantReason:    "InstanceCrashing",
			wantMessage:   "The instance is repeatedly failing to start",
			wantNotReason: "CrashLoopBackOff",
		},
		{
			k8sReason:     "CreateContainerError",
			wantReason:    "ConfigurationError",
			wantMessage:   "The instance could not be started due to a configuration error",
			wantNotReason: "CreateContainerError",
		},
		{
			k8sReason:     "CreateContainerConfigError",
			wantReason:    "ConfigurationError",
			wantMessage:   "The instance could not be started due to a configuration error",
			wantNotReason: "CreateContainerConfigError",
		},
		{
			k8sReason:     "ContainerCreating",
			wantReason:    "Provisioning",
			wantMessage:   "Instance is provisioning",
			wantNotReason: "ContainerCreating",
		},
		{
			k8sReason:     "PodInitializing",
			wantReason:    "Provisioning",
			wantMessage:   "Instance is provisioning",
			wantNotReason: "PodInitializing",
		},
		{
			// Unknown/arbitrary k8s reason — must fall back to generic, never pass through.
			k8sReason:     "SomeInternalKubernetesError",
			k8sMessage:    "internal details operators should not see",
			wantReason:    "Provisioning",
			wantMessage:   "Instance is provisioning",
			wantNotReason: "SomeInternalKubernetesError",
		},
	}

	for _, tc := range tests {
		t.Run(tc.k8sReason, func(t *testing.T) {
			reason, message := translateWaitingReason(tc.k8sReason, tc.k8sMessage)

			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if message != tc.wantMessage {
				t.Errorf("message = %q, want %q", message, tc.wantMessage)
			}
			if reason == tc.wantNotReason {
				t.Errorf("raw k8s reason %q leaked into condition reason — must be translated", tc.wantNotReason)
			}
		})
	}
}

// TestSyncInstancePowerState_WaitingReasonTranslation verifies that
// syncInstancePowerState translates container waiting reasons end-to-end: the
// Running condition must carry domain language, never raw k8s strings.
func TestSyncInstancePowerState_WaitingReasonTranslation(t *testing.T) {
	tests := []struct {
		name          string
		pod           *core.Pod
		wantReason    string
		wantMessage   string
		wantNotReason string
	}{
		{
			name:          "ImagePullBackOff → ImageUnavailable in Running condition",
			pod:           podPendingWithWaiting("ImagePullBackOff", "Back-off pulling image"),
			wantReason:    "ImageUnavailable",
			wantMessage:   "The instance image could not be pulled",
			wantNotReason: "ImagePullBackOff",
		},
		{
			name:          "CrashLoopBackOff → InstanceCrashing in Running condition",
			pod:           podPendingWithWaiting("CrashLoopBackOff", "back-off 5m0s restarting failed container"),
			wantReason:    "InstanceCrashing",
			wantMessage:   "The instance is repeatedly failing to start",
			wantNotReason: "CrashLoopBackOff",
		},
		{
			name:          "unknown k8s reason → generic Provisioning, raw string never set",
			pod:           podPendingWithWaiting("SomeObscureK8sReason", "internal details"),
			wantReason:    "Provisioning",
			wantMessage:   "Instance is provisioning",
			wantNotReason: "SomeObscureK8sReason",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance := newTestInstance()
			r := &InstanceReconciler{}
			fakeClient := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(instance).
				WithStatusSubresource(&computev1alpha.Instance{}).
				Build()

			if err := r.syncInstancePowerState(context.Background(), fakeClient, instance, tc.pod); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			running := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha.InstanceRunning)
			if running == nil {
				t.Fatal("expected Running condition to be set")
			}
			if running.Reason != tc.wantReason {
				t.Errorf("Running.Reason = %q, want %q", running.Reason, tc.wantReason)
			}
			if running.Message != tc.wantMessage {
				t.Errorf("Running.Message = %q, want %q", running.Message, tc.wantMessage)
			}
			if running.Reason == tc.wantNotReason {
				t.Errorf("raw k8s reason %q leaked into Running condition", tc.wantNotReason)
			}
			if running.Message == tc.pod.Status.ContainerStatuses[0].State.Waiting.Message {
				t.Errorf("raw k8s message leaked into Running condition: %q", running.Message)
			}
		})
	}
}
