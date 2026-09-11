// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

const (
	// podRecreateBaseDelay is the wait before the second consecutive recreate of
	// the same Instance's Pod. The first recreate is immediate, so an isolated
	// VM failure recovers without delay.
	podRecreateBaseDelay = 30 * time.Second

	// podRecreateMaxDelay caps the backoff so a host that is broken for a long
	// time still gets retried, rather than being abandoned.
	podRecreateMaxDelay = 10 * time.Minute

	// podRecreateSettleDelay gives the API server time to finish removing the
	// deleted Pod before the next reconcile rebuilds it.
	podRecreateSettleDelay = 2 * time.Second

	// instanceRecoveringReason marks an Instance whose Pod keeps terminating.
	// Recreation is still being retried, but an operator needs to see why the
	// instance is not available.
	instanceRecoveringReason = "InstanceCrashing"
)

// recoverTerminatedPod replaces a backing Pod whose container has terminated
// unexpectedly.
//
// The Pod is recreated rather than left in place because a terminated kraftlet
// Pod is terminal: the VMM behind it is gone and nothing restarts it, so the
// customer's instance stays down until a new Pod is scheduled. Recreation is
// rate limited per Instance so a failure that reproduces on every attempt —
// a broken CNI on the node, for example — backs off instead of churning Pods.
//
// The second return value reports whether the caller should stop reconciling
// and honor the returned result.
func (r *InstanceReconciler) recoverTerminatedPod(
	ctx context.Context,
	instance *computev1alpha.Instance,
) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	var pod core.Pod
	key := client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}
	if err := r.Get(ctx, key, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			// The Pod is absent, either because the Instance is new or because a
			// previous recreate already removed it. Do not clear the attempt
			// history here: that would reset the backoff on every cycle and
			// defeat the rate limit.
			return ctrl.Result{}, false, nil
		}
		return ctrl.Result{}, false, fmt.Errorf("failed to get pod for instance %s: %w", instance.Name, err)
	}

	reason, terminated := podTerminatedUnexpectedly(&pod)
	if !terminated {
		if pod.Status.Phase == core.PodRunning {
			// The instance is serving again, so start the next failure from a
			// clean slate.
			r.podRecreates.forget(instance.UID)
		}
		return ctrl.Result{}, false, nil
	}

	wait, attempt := r.podRecreates.attempt(instance.UID, time.Now())
	if wait > 0 {
		logger.Info("deferring instance pod recreate; previous attempts failed",
			"name", instance.Name,
			"reason", reason,
			"attempts", attempt,
			"retryIn", wait,
		)
		if err := r.markInstanceRecovering(ctx, instance, attempt); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, true, nil
			}
			return ctrl.Result{}, true, fmt.Errorf("failed to report recovery status for instance %s: %w", instance.Name, err)
		}
		return ctrl.Result{RequeueAfter: wait}, true, nil
	}

	logger.Info("recreating instance pod after unexpected termination",
		"name", instance.Name,
		"reason", reason,
		"attempts", attempt,
	)

	// Guard the delete on the observed UID so a Pod that was already replaced
	// between the Get above and here is not removed.
	if err := r.Delete(ctx, &pod, client.Preconditions{UID: &pod.UID}); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, true, fmt.Errorf("failed to delete terminated pod for instance %s: %w", instance.Name, err)
	}

	return ctrl.Result{RequeueAfter: podRecreateSettleDelay}, true, nil
}

// podTerminatedUnexpectedly reports whether an Instance's Pod has stopped in a
// way it cannot recover from, along with a short description for logging.
//
// A Pod that is being deleted, or that ran to completion, is left alone: the
// first is already on its way out and the second finished the work it was asked
// to do.
func podTerminatedUnexpectedly(pod *core.Pod) (string, bool) {
	if !pod.DeletionTimestamp.IsZero() {
		return "", false
	}
	if pod.Status.Phase == core.PodSucceeded {
		return "", false
	}

	for _, cs := range pod.Status.ContainerStatuses {
		terminated := cs.State.Terminated
		if terminated == nil || terminated.ExitCode == 0 {
			continue
		}
		return fmt.Sprintf("container %s terminated with exit code %d (%s)",
			cs.Name, terminated.ExitCode, terminated.Reason), true
	}

	if pod.Status.Phase == core.PodFailed {
		return "pod failed", true
	}

	return "", false
}

// markInstanceRecovering records on the Instance that its Pod keeps terminating,
// so repeated recreate failures are visible to the customer and to operators
// rather than only appearing in provider logs.
func (r *InstanceReconciler) markInstanceRecovering(
	ctx context.Context,
	instance *computev1alpha.Instance,
	attempts int,
) error {
	base := instance.DeepCopy()

	message := fmt.Sprintf("The instance stopped unexpectedly and has failed to restart %d times", attempts)
	condition := metav1.Condition{
		Type:               computev1alpha.InstanceAvailable,
		ObservedGeneration: instance.Generation,
		Status:             metav1.ConditionFalse,
		Reason:             instanceRecoveringReason,
		Message:            message,
	}
	changed := meta.SetStatusCondition(&instance.Status.Conditions, condition)

	condition.Type = computev1alpha.InstanceProgrammed
	changed = meta.SetStatusCondition(&instance.Status.Conditions, condition) || changed

	if !changed {
		return nil
	}

	// Optimistic-lock merge patch, matching syncInstancePowerState: a concurrent
	// write by the compute quota controller yields a conflict rather than
	// silently clobbering fields this provider does not own.
	return r.Status().Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// podRecreateTracker records consecutive Pod recreate attempts per Instance so
// that a failure which reproduces every time backs off instead of recreating
// the Pod on every reconcile.
type podRecreateTracker struct {
	mu    sync.Mutex
	state map[types.UID]podRecreateAttempt
}

type podRecreateAttempt struct {
	count int
	last  time.Time
}

// attempt reports how long to wait before the next recreate and the number of
// attempts made, counting the one being authorized. A zero wait means the
// caller may recreate now.
func (t *podRecreateTracker) attempt(uid types.UID, now time.Time) (time.Duration, int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.state == nil {
		t.state = map[types.UID]podRecreateAttempt{}
	}

	if prev, ok := t.state[uid]; ok {
		if wait := prev.last.Add(podRecreateBackoff(prev.count)).Sub(now); wait > 0 {
			return wait, prev.count
		}
	}

	next := podRecreateAttempt{count: t.state[uid].count + 1, last: now}
	t.state[uid] = next
	return 0, next.count
}

// forget drops an Instance's attempt history, both when the instance recovers
// and when it is deleted, so the map does not grow without bound.
func (t *podRecreateTracker) forget(uid types.UID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.state, uid)
}

// podRecreateBackoff doubles the wait per consecutive failure, up to a cap.
func podRecreateBackoff(failures int) time.Duration {
	switch {
	case failures < 1:
		return 0
	case failures > 16:
		return podRecreateMaxDelay
	}

	delay := podRecreateBaseDelay << (failures - 1)
	if delay > podRecreateMaxDelay {
		return podRecreateMaxDelay
	}
	return delay
}
