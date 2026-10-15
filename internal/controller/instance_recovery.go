// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Kraftlet and ukpd do not bring an instance back once its start fails, and a
// host reboot makes every restore fail: the reboot removes the instance's TAP
// device, ukpd's restore stops on ENODEV, and only a new kraftlet instance gets
// a new TAP device from CNI. The provider recovers by deleting the instance Pod,
// which kraftlet turns into a new instance, and recreating it.
//
// Recreating a stateful scale-to-zero instance discards its saved snapshot.
// That is accepted: without a TAP device the instance can never wake to use it.
//
// The boot ID is recorded on the first reconcile after the Pod is scheduled,
// since an unscheduled Pod has no host. A Pod first seen after its host
// rebooted records the new boot ID, so only the stuck-start check below can
// recover it.
const (
	// hostBootIDAnnotation records the boot ID of the host an instance Pod's
	// kraftlet runs on. A different boot ID on the host means the host rebooted
	// since, and took the instance's TAP device with it.
	hostBootIDAnnotation = "unikraft.datumapis.com/host-boot-id"

	// kraftletNodePrefix is how the kraftlet DaemonSet names its virtual node:
	// kraftlet-<host node name>.
	kraftletNodePrefix = "kraftlet-"

	// stuckStartGracePeriod is how long a Pod may sit Pending with every
	// container terminated before the provider recreates it. Kraftlet reports
	// a crash loop as CrashLoopBackOff, not terminated, so a crash loop never
	// reaches this state.
	stuckStartGracePeriod = 2 * time.Minute
)

// recoverInstancePod deletes an instance Pod that kraftlet will not start
// again, so the next reconcile recreates it. It reports whether it deleted the
// Pod, and how long until a Pending Pod would be stuck long enough to delete.
func (r *InstanceReconciler) recoverInstancePod(ctx context.Context, pod *core.Pod) (bool, time.Duration, error) {
	logger := log.FromContext(ctx)

	if pod.CreationTimestamp.IsZero() || !pod.DeletionTimestamp.IsZero() {
		return false, 0, nil
	}

	rebooted, err := r.hostRebootedSince(ctx, pod)
	if err != nil {
		return false, 0, err
	}
	if rebooted {
		logger.Info("host rebooted since the instance started; recreating instance pod",
			"pod", pod.Name, "node", pod.Spec.NodeName, "recordedBootID", pod.Annotations[hostBootIDAnnotation])
		return true, 0, r.deleteInstancePod(ctx, pod)
	}

	wait, stuck := stuckStartRemaining(pod, time.Now())
	if !stuck {
		return false, 0, nil
	}
	if wait > 0 {
		return false, wait, nil
	}
	logger.Info("instance pod stopped and was not restarted; recreating instance pod",
		"pod", pod.Name, "node", pod.Spec.NodeName, "gracePeriod", stuckStartGracePeriod)
	return true, 0, r.deleteInstancePod(ctx, pod)
}

// hostRebootedSince compares the host's current boot ID with the one recorded
// on the Pod, recording it first if the Pod has none. A Pod that is not on a
// kraftlet node, or whose host reports no boot ID, is never treated as
// rebooted.
func (r *InstanceReconciler) hostRebootedSince(ctx context.Context, pod *core.Pod) (bool, error) {
	host, ok := strings.CutPrefix(pod.Spec.NodeName, kraftletNodePrefix)
	if !ok || host == "" {
		return false, nil
	}

	var node core.Node
	if err := r.Get(ctx, types.NamespacedName{Name: host}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get host node %s for pod %s: %w", host, pod.Name, err)
	}
	bootID := node.Status.NodeInfo.BootID
	if bootID == "" {
		return false, nil
	}

	switch recorded := pod.Annotations[hostBootIDAnnotation]; recorded {
	case bootID:
		return false, nil
	case "":
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[hostBootIDAnnotation] = bootID
		if err := r.Patch(ctx, pod, patch); err != nil {
			return false, fmt.Errorf("failed to record host boot ID on pod %s: %w", pod.Name, err)
		}
		return false, nil
	default:
		return true, nil
	}
}

func (r *InstanceReconciler) deleteInstancePod(ctx context.Context, pod *core.Pod) error {
	err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("failed to delete instance pod %s: %w", pod.Name, err)
	}
	return nil
}

// stuckStartRemaining reports whether the Pod is Pending with every container
// terminated, which is how kraftlet leaves an instance it stopped and will not
// restart, and how long remains of the grace period. An image pull failure is
// excluded, because kraftlet retries those itself.
func stuckStartRemaining(pod *core.Pod, now time.Time) (time.Duration, bool) {
	if pod.Status.Phase != core.PodPending || len(pod.Status.ContainerStatuses) == 0 {
		return 0, false
	}
	finished := pod.CreationTimestamp.Time
	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil || strings.Contains(t.Message, runtimeImagePullFailed) {
			return 0, false
		}
		if t.FinishedAt.After(finished) {
			finished = t.FinishedAt.Time
		}
	}
	return max(finished.Add(stuckStartGracePeriod).Sub(now), 0), true
}

// hostRebootHandler enqueues every Instance whose Pod runs on the kraftlet of
// a host that just reported a new boot ID.
func (r *InstanceReconciler) hostRebootHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var pods core.PodList
		if err := r.List(ctx, &pods, client.MatchingLabels{"managed-by": "infra-provider-unikraft"}); err != nil {
			log.FromContext(ctx).Error(err, "failed to list instance pods after host reboot", "node", obj.GetName())
			return nil
		}
		kraftletNode := kraftletNodePrefix + obj.GetName()
		var requests []reconcile.Request
		for _, pod := range pods.Items {
			if pod.Spec.NodeName != kraftletNode {
				continue
			}
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      pod.Labels["upstream.instance"],
			}})
		}
		return requests
	})
}

// hostBootIDChanged passes only Node updates that change the boot ID, so the
// node status heartbeats do not reconcile every Instance.
var hostBootIDChanged = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldNode, ok := e.ObjectOld.(*core.Node)
		newNode, ok2 := e.ObjectNew.(*core.Node)
		return ok && ok2 && oldNode.Status.NodeInfo.BootID != newNode.Status.NodeInfo.BootID
	},
}

// recoveryResult folds the stuck-start wait into a reconcile result.
func recoveryResult(wait time.Duration) ctrl.Result {
	if wait > 0 {
		return ctrl.Result{RequeueAfter: wait}
	}
	return ctrl.Result{}
}
