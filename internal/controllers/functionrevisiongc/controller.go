package functionrevisiongc

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

const (
	labelFunctionName        = "functions.datumapis.com/function-name"
	defaultRevisionHistoryLimit = int32(10)
)

// Reconciler reconciles Function resources for the purpose of garbage-collecting
// old FunctionRevisions beyond the retention limit.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the controller with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&functionsv1alpha1.Function{}).
		Owns(&functionsv1alpha1.FunctionRevision{}).
		Complete(r)
}

// Reconcile lists all FunctionRevisions owned by this Function and deletes those
// beyond the revisionHistoryLimit that are in the Retired phase.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var fn functionsv1alpha1.Function
	if err := r.Get(ctx, req.NamespacedName, &fn); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get function: %w", err)
	}

	if !fn.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	var revList functionsv1alpha1.FunctionRevisionList
	if err := r.List(ctx, &revList,
		client.InNamespace(fn.Namespace),
		client.MatchingLabels{labelFunctionName: fn.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list function revisions: %w", err)
	}

	// Sort revisions by generation descending (newest first).
	revisions := revList.Items
	sort.Slice(revisions, func(i, j int) bool {
		return revisions[i].Generation > revisions[j].Generation
	})

	limit := defaultRevisionHistoryLimit
	if fn.Spec.RevisionHistoryLimit != nil {
		limit = *fn.Spec.RevisionHistoryLimit
	}

	activeRevision := fn.Status.ActiveRevision

	kept := int32(0)
	for i := range revisions {
		rev := &revisions[i]

		// Always keep the active revision regardless of position.
		if rev.Name == activeRevision {
			kept++
			continue
		}

		if kept < limit {
			kept++
			continue
		}

		// Only delete revisions that are in the Retired phase.
		if rev.Status.Phase != functionsv1alpha1.RevisionPhaseRetired {
			continue
		}

		if err := r.Delete(ctx, rev); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to delete retired revision %s: %w", rev.Name, err)
		}
		log.Info("garbage collected retired function revision", "revision", rev.Name)
	}

	return ctrl.Result{}, nil
}
