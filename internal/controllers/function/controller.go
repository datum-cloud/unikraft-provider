package function

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

const (
	finalizerName      = "functions.datumapis.com/cleanup"
	labelFunctionName  = "functions.datumapis.com/function-name"
	labelGeneration    = "functions.datumapis.com/generation"
	activatorEndpoint  = "http://ufo-activator.datum-system.svc.cluster.local:8080"
)

// Reconciler reconciles Function resources.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the controller with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&functionsv1alpha1.Function{}).
		Owns(&functionsv1alpha1.FunctionRevision{}).
		Watches(
			&computev1alpha.Workload{},
			handler.EnqueueRequestsFromMapFunc(r.workloadToFunction),
		).
		Complete(r)
}

// workloadToFunction maps a Workload back to its owning Function by traversing
// the Workload's owner (FunctionRevision) and then the revision's owner (Function).
func (r *Reconciler) workloadToFunction(ctx context.Context, obj client.Object) []reconcile.Request {
	workload, ok := obj.(*computev1alpha.Workload)
	if !ok {
		return nil
	}
	fnName, ok := workload.Labels[labelFunctionName]
	if !ok {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: fnName, Namespace: workload.Namespace}},
	}
}

// Reconcile processes a Function resource.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var fn functionsv1alpha1.Function
	if err := r.Get(ctx, req.NamespacedName, &fn); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get function: %w", err)
	}

	// Handle deletion.
	if !fn.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &fn)
	}

	// Ensure finalizer is present.
	if !controllerutil.ContainsFinalizer(&fn, finalizerName) {
		controllerutil.AddFinalizer(&fn, finalizerName)
		if err := r.Update(ctx, &fn); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	log.Info("reconciling function")

	// Ensure the desired FunctionRevision exists.
	if err := r.reconcileRevision(ctx, &fn); err != nil {
		return ctrl.Result{}, err
	}

	// Sync status from the active revision.
	if err := r.syncStatus(ctx, &fn); err != nil {
		return ctrl.Result{}, err
	}

	// Manage the HTTPProxy for routing.
	if err := r.reconcileHTTPProxy(ctx, &fn); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileRevision creates the FunctionRevision for the current generation if
// it does not yet exist.
func (r *Reconciler) reconcileRevision(ctx context.Context, fn *functionsv1alpha1.Function) error {
	revisionName := fmt.Sprintf("%s-%d", fn.Name, fn.Generation)

	var rev functionsv1alpha1.FunctionRevision
	err := r.Get(ctx, types.NamespacedName{Name: revisionName, Namespace: fn.Namespace}, &rev)
	if err == nil {
		// Revision already exists. If it's a git source and has no imageRef yet,
		// ensure a FunctionBuild exists.
		if fn.Spec.Source.Git != nil && rev.Spec.ImageRef == "" {
			return r.reconcileBuild(ctx, fn, &rev)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get function revision: %w", err)
	}

	// Build the revision spec.
	imageRef := ""
	if fn.Spec.Source.Image != nil {
		imageRef = fn.Spec.Source.Image.Ref
	}

	rev = functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revisionName,
			Namespace: fn.Namespace,
			Labels: map[string]string{
				labelFunctionName: fn.Name,
				labelGeneration:   fmt.Sprintf("%d", fn.Generation),
			},
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: *fn.Spec.DeepCopy(),
			ImageRef:     imageRef,
		},
	}

	if err := controllerutil.SetControllerReference(fn, &rev, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on revision: %w", err)
	}

	if err := r.Create(ctx, &rev); err != nil {
		return fmt.Errorf("failed to create function revision: %w", err)
	}

	ctrl.LoggerFrom(ctx).Info("created function revision", "revision", revisionName)

	// For git sources, immediately create a build.
	if fn.Spec.Source.Git != nil {
		return r.reconcileBuild(ctx, fn, &rev)
	}
	return nil
}

// reconcileBuild ensures a FunctionBuild exists for the given revision.
func (r *Reconciler) reconcileBuild(ctx context.Context, fn *functionsv1alpha1.Function, rev *functionsv1alpha1.FunctionRevision) error {
	buildName := fmt.Sprintf("build-%s", rev.Name)

	var build functionsv1alpha1.FunctionBuild
	err := r.Get(ctx, types.NamespacedName{Name: buildName, Namespace: rev.Namespace}, &build)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get function build: %w", err)
	}

	build = functionsv1alpha1.FunctionBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildName,
			Namespace: rev.Namespace,
			Labels: map[string]string{
				labelFunctionName:              fn.Name,
				"functions.datumapis.com/revision-name": rev.Name,
			},
		},
		Spec: functionsv1alpha1.FunctionBuildSpec{
			Source:   rev.Spec.FunctionSpec.Source,
			Language: rev.Spec.FunctionSpec.Runtime.Language,
		},
	}

	if err := controllerutil.SetControllerReference(rev, &build, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on function build: %w", err)
	}

	if err := r.Create(ctx, &build); err != nil {
		return fmt.Errorf("failed to create function build: %w", err)
	}

	ctrl.LoggerFrom(ctx).Info("created function build", "build", buildName)
	return nil
}

// syncStatus updates Function.status from the active revision's state.
func (r *Reconciler) syncStatus(ctx context.Context, fn *functionsv1alpha1.Function) error {
	revisionName := fmt.Sprintf("%s-%d", fn.Name, fn.Generation)

	var rev functionsv1alpha1.FunctionRevision
	if err := r.Get(ctx, types.NamespacedName{Name: revisionName, Namespace: fn.Namespace}, &rev); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get function revision for status sync: %w", err)
	}

	patch := client.MergeFrom(fn.DeepCopy())
	fn.Status.ObservedGeneration = fn.Generation

	if rev.Status.Phase == functionsv1alpha1.RevisionPhaseReady {
		fn.Status.ActiveRevision = revisionName
		fn.Status.ReadyRevision = revisionName

		apimeta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
			Type:               functionsv1alpha1.FunctionConditionRevisionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "RevisionReady",
			Message:            fmt.Sprintf("Revision %s is ready", revisionName),
			ObservedGeneration: fn.Generation,
		})
		apimeta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
			Type:               functionsv1alpha1.FunctionConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			Message:            "Function is serving traffic",
			ObservedGeneration: fn.Generation,
		})
	} else {
		apimeta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
			Type:               functionsv1alpha1.FunctionConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             string(rev.Status.Phase),
			Message:            fmt.Sprintf("Revision %s is not yet ready", revisionName),
			ObservedGeneration: fn.Generation,
		})
	}

	if err := r.Status().Patch(ctx, fn, patch); err != nil {
		return fmt.Errorf("failed to patch function status: %w", err)
	}
	return nil
}

// reconcileHTTPProxy ensures an HTTPProxy exists and its backend is correctly
// pointed at either the activator or the workload service.
func (r *Reconciler) reconcileHTTPProxy(ctx context.Context, fn *functionsv1alpha1.Function) error {
	proxyName := fmt.Sprintf("function-%s", fn.Name)

	// Determine the backend endpoint.
	endpoint := activatorEndpoint
	if fn.Status.ActiveRevision != "" {
		var rev functionsv1alpha1.FunctionRevision
		if err := r.Get(ctx, types.NamespacedName{Name: fn.Status.ActiveRevision, Namespace: fn.Namespace}, &rev); err == nil && rev.Status.WorkloadRef != "" {
			// Check workload replica count.
			var workload computev1alpha.Workload
			if err := r.Get(ctx, types.NamespacedName{Name: rev.Status.WorkloadRef, Namespace: fn.Namespace}, &workload); err == nil {
				if workload.Status.Replicas > 0 {
					port := int32(8080)
					if fn.Spec.Runtime.Port != nil {
						port = *fn.Spec.Runtime.Port
					}
					endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", rev.Status.WorkloadRef, fn.Namespace, port)
				}
			}
		}
	}

	ruleName := gatewayv1.SectionName("default")
	desired := networkingv1alpha.HTTPProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyName,
			Namespace: fn.Namespace,
		},
		Spec: networkingv1alpha.HTTPProxySpec{
			Rules: []networkingv1alpha.HTTPProxyRule{
				{
					Name: &ruleName,
					Backends: []networkingv1alpha.HTTPProxyRuleBackend{
						{Endpoint: endpoint},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(fn, &desired, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on http proxy: %w", err)
	}

	var existing networkingv1alpha.HTTPProxy
	err := r.Get(ctx, types.NamespacedName{Name: proxyName, Namespace: fn.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, &desired); err != nil {
			return fmt.Errorf("failed to create http proxy: %w", err)
		}
		ctrl.LoggerFrom(ctx).Info("created http proxy", "proxy", proxyName)
		return r.syncHTTPProxyStatus(ctx, fn, &desired)
	}
	if err != nil {
		return fmt.Errorf("failed to get http proxy: %w", err)
	}

	// Update if backend has changed.
	if len(existing.Spec.Rules) == 0 ||
		len(existing.Spec.Rules[0].Backends) == 0 ||
		existing.Spec.Rules[0].Backends[0].Endpoint != endpoint {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec = desired.Spec
		if err := r.Patch(ctx, &existing, patch); err != nil {
			return fmt.Errorf("failed to patch http proxy: %w", err)
		}
	}

	return r.syncHTTPProxyStatus(ctx, fn, &existing)
}

// syncHTTPProxyStatus copies the canonical hostname from the HTTPProxy status
// back to the Function status.
func (r *Reconciler) syncHTTPProxyStatus(ctx context.Context, fn *functionsv1alpha1.Function, proxy *networkingv1alpha.HTTPProxy) error {
	hostname := proxy.Status.CanonicalHostname
	if hostname == "" && len(proxy.Status.Hostnames) > 0 {
		hostname = string(proxy.Status.Hostnames[0])
	}
	if hostname == "" {
		return nil
	}

	patch := client.MergeFrom(fn.DeepCopy())
	fn.Status.Hostname = hostname
	apimeta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
		Type:               functionsv1alpha1.FunctionConditionRouteConfigured,
		Status:             metav1.ConditionTrue,
		Reason:             "RouteConfigured",
		Message:            fmt.Sprintf("HTTPProxy configured with hostname %s", hostname),
		ObservedGeneration: fn.Generation,
	})
	if err := r.Status().Patch(ctx, fn, patch); err != nil {
		return fmt.Errorf("failed to patch function hostname status: %w", err)
	}
	return nil
}

// reconcileDelete removes managed resources and the finalizer.
func (r *Reconciler) reconcileDelete(ctx context.Context, fn *functionsv1alpha1.Function) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Delete the HTTPProxy if it exists.
	proxyName := fmt.Sprintf("function-%s", fn.Name)
	var proxy networkingv1alpha.HTTPProxy
	if err := r.Get(ctx, types.NamespacedName{Name: proxyName, Namespace: fn.Namespace}, &proxy); err == nil {
		if err := r.Delete(ctx, &proxy); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete http proxy: %w", err)
		}
		log.Info("deleted http proxy", "proxy", proxyName)
	}

	// Remove the finalizer so the Function can be garbage collected.
	controllerutil.RemoveFinalizer(fn, finalizerName)
	if err := r.Update(ctx, fn); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}
