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
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.datum.net/unikraft-provider/internal/config"
)

const testHost = "compute-1"

func hostNode(bootID string) *core.Node {
	return &core.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testHost},
		Status:     core.NodeStatus{NodeInfo: core.NodeSystemInfo{BootID: bootID}},
	}
}

// instancePodOn builds a running instance Pod scheduled to the given node, with
// the given recorded boot ID ("" for none).
func instancePodOn(nodeName, recordedBootID string) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-instance",
			Namespace:         "default",
			UID:               types.UID("pod-uid"),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
			Labels: map[string]string{
				"managed-by":        "infra-provider-unikraft",
				"upstream.instance": "test-instance",
			},
			Annotations: map[string]string{},
		},
		Spec:   core.PodSpec{NodeName: nodeName},
		Status: core.PodStatus{Phase: core.PodRunning},
	}
	if recordedBootID != "" {
		pod.Annotations[hostBootIDAnnotation] = recordedBootID
	}
	return pod
}

// stopInstancePod puts the Pod in the state kraftlet leaves an instance ukpd
// stopped and kraftlet will not restart.
func stopInstancePod(pod *core.Pod, finishedAt time.Time, message string) *core.Pod {
	pod.Status.Phase = core.PodPending
	pod.Status.ContainerStatuses = []core.ContainerStatus{{
		Name: "app",
		State: core.ContainerState{Terminated: &core.ContainerStateTerminated{
			ExitCode:   1,
			Reason:     "Error",
			Message:    message,
			FinishedAt: metav1.NewTime(finishedAt),
		}},
	}}
	return pod
}

func recoveryReconciler(t *testing.T, objs ...client.Object) (*InstanceReconciler, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{})
	r.Client = cl
	return r, cl
}

func podExists(t *testing.T, cl client.Client) (*core.Pod, bool) {
	t.Helper()
	var pod core.Pod
	err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-instance"}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return &pod, true
}

func TestRecoverInstancePod_HostReboot(t *testing.T) {
	tests := []struct {
		name        string
		nodeName    string
		recorded    string
		hostBootID  string
		wantDeleted bool
		wantBootID  string
	}{
		{
			name:        "boot ID changed since the pod started",
			nodeName:    kraftletNodePrefix + testHost,
			recorded:    "boot-a",
			hostBootID:  "boot-b",
			wantDeleted: true,
		},
		{
			name:       "same boot",
			nodeName:   kraftletNodePrefix + testHost,
			recorded:   "boot-a",
			hostBootID: "boot-a",
			wantBootID: "boot-a",
		},
		{
			name:       "first sight records the boot ID",
			nodeName:   kraftletNodePrefix + testHost,
			hostBootID: "boot-a",
			wantBootID: "boot-a",
		},
		{
			name:       "host reports no boot ID",
			nodeName:   kraftletNodePrefix + testHost,
			recorded:   "boot-a",
			hostBootID: "",
			wantBootID: "boot-a",
		},
		{
			name:       "pod not on a kraftlet node",
			nodeName:   testHost,
			recorded:   "boot-a",
			hostBootID: "boot-b",
			wantBootID: "boot-a",
		},
		{
			name:     "pod not scheduled yet",
			nodeName: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := instancePodOn(tt.nodeName, tt.recorded)
			r, cl := recoveryReconciler(t, pod, hostNode(tt.hostBootID))

			deleted, wait, err := r.recoverInstancePod(context.Background(), pod)
			if err != nil {
				t.Fatalf("recoverInstancePod: %v", err)
			}
			if deleted != tt.wantDeleted {
				t.Errorf("deleted = %v, want %v", deleted, tt.wantDeleted)
			}
			if wait != 0 {
				t.Errorf("wait = %v, want 0 for a running pod", wait)
			}

			got, exists := podExists(t, cl)
			if exists == tt.wantDeleted {
				t.Fatalf("pod exists = %v after deleted = %v", exists, tt.wantDeleted)
			}
			if exists && got.Annotations[hostBootIDAnnotation] != tt.wantBootID {
				t.Errorf("recorded boot ID = %q, want %q", got.Annotations[hostBootIDAnnotation], tt.wantBootID)
			}
		})
	}
}

func TestRecoverInstancePod_HostNodeMissing(t *testing.T) {
	pod := instancePodOn(kraftletNodePrefix+testHost, "boot-a")
	r, cl := recoveryReconciler(t, pod)

	deleted, _, err := r.recoverInstancePod(context.Background(), pod)
	if err != nil {
		t.Fatalf("recoverInstancePod: %v", err)
	}
	if deleted {
		t.Error("deleted a pod whose host node is missing")
	}
	if _, exists := podExists(t, cl); !exists {
		t.Error("pod was removed")
	}
}

func TestRecoverInstancePod_StuckStart(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		finishedAt  time.Time
		message     string
		wantDeleted bool
		wantWait    bool
	}{
		{
			name:        "stopped past the grace period",
			finishedAt:  now.Add(-stuckStartGracePeriod - time.Second),
			message:     "Instance is stopped, unknown stop",
			wantDeleted: true,
		},
		{
			name:       "stopped within the grace period",
			finishedAt: now.Add(-time.Second),
			message:    "Instance is stopped, unknown stop",
			wantWait:   true,
		},
		{
			name:       "image pull failure is left to kraftlet",
			finishedAt: now.Add(-time.Hour),
			message:    runtimeImagePullFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := stopInstancePod(instancePodOn(kraftletNodePrefix+testHost, "boot-a"), tt.finishedAt, tt.message)
			r, cl := recoveryReconciler(t, pod, hostNode("boot-a"))

			deleted, wait, err := r.recoverInstancePod(context.Background(), pod)
			if err != nil {
				t.Fatalf("recoverInstancePod: %v", err)
			}
			if deleted != tt.wantDeleted {
				t.Errorf("deleted = %v, want %v", deleted, tt.wantDeleted)
			}
			if (wait > 0) != tt.wantWait {
				t.Errorf("wait = %v, want a wait: %v", wait, tt.wantWait)
			}
			if wait > stuckStartGracePeriod {
				t.Errorf("wait = %v, longer than the grace period", wait)
			}
			if _, exists := podExists(t, cl); exists == tt.wantDeleted {
				t.Errorf("pod exists = %v after deleted = %v", exists, tt.wantDeleted)
			}
		})
	}
}

func TestStuckStartRemaining(t *testing.T) {
	now := time.Now()
	long := now.Add(-time.Hour)

	crashLooping := instancePodOn(kraftletNodePrefix+testHost, "")
	crashLooping.Status.Phase = core.PodPending
	crashLooping.Status.ContainerStatuses = []core.ContainerStatus{{
		Name:  "app",
		State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}

	running := stopInstancePod(instancePodOn(kraftletNodePrefix+testHost, ""), long, "stopped")
	running.Status.Phase = core.PodRunning

	noStatuses := instancePodOn(kraftletNodePrefix+testHost, "")
	noStatuses.Status.Phase = core.PodPending

	for name, pod := range map[string]*core.Pod{
		"crash loop":          crashLooping,
		"running pod":         running,
		"no container status": noStatuses,
	} {
		t.Run(name, func(t *testing.T) {
			if _, stuck := stuckStartRemaining(pod, now); stuck {
				t.Error("reported stuck")
			}
		})
	}
}

func TestHostBootIDChanged(t *testing.T) {
	update := func(oldID, newID string) event.UpdateEvent {
		return event.UpdateEvent{ObjectOld: hostNode(oldID), ObjectNew: hostNode(newID)}
	}
	if !hostBootIDChanged.Update(update("boot-a", "boot-b")) {
		t.Error("boot ID change was filtered out")
	}
	if hostBootIDChanged.Update(update("boot-a", "boot-a")) {
		t.Error("status update with the same boot ID passed")
	}
}

func TestHostRebootHandler_EnqueuesInstancesOnThatHost(t *testing.T) {
	onHost := instancePodOn(kraftletNodePrefix+testHost, "boot-a")
	elsewhere := instancePodOn(kraftletNodePrefix+"compute-2", "boot-a")
	elsewhere.Name = "other-instance"
	elsewhere.Labels["upstream.instance"] = "other-instance"
	r, _ := recoveryReconciler(t, onHost, elsewhere)

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	r.hostRebootHandler().Update(context.Background(),
		event.UpdateEvent{ObjectOld: hostNode("boot-a"), ObjectNew: hostNode("boot-b")}, q)

	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-instance"}}
	if q.Len() != 1 {
		t.Fatalf("enqueued %d requests, want 1", q.Len())
	}
	if got, _ := q.Get(); got != want {
		t.Errorf("enqueued %v, want %v", got, want)
	}
}
