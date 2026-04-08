package functionrevision

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

const (
	labelFunctionName = "functions.datumapis.com/function-name"
	defaultInstanceType = "datumcloud/d1-standard-2"
	defaultMemory       = "256Mi"
	defaultNetwork      = "default"
)

// Reconciler reconciles FunctionRevision resources.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the controller with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&functionsv1alpha1.FunctionRevision{}).
		Owns(&computev1alpha.Workload{}).
		Complete(r)
}

// Reconcile processes a FunctionRevision resource.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var rev functionsv1alpha1.FunctionRevision
	if err := r.Get(ctx, req.NamespacedName, &rev); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get function revision: %w", err)
	}

	if !rev.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	log.Info("reconciling function revision")

	// If the imageRef is not yet set, the revision is waiting for a build.
	if rev.Spec.ImageRef == "" {
		return r.reconcileBuilding(ctx, &rev)
	}

	// imageRef is set — ensure the Workload exists.
	return r.reconcileWorkload(ctx, &rev)
}

// reconcileBuilding handles the case where a revision is waiting for a build.
func (r *Reconciler) reconcileBuilding(ctx context.Context, rev *functionsv1alpha1.FunctionRevision) (ctrl.Result, error) {
	// Look up the FunctionBuild to see if it has produced an imageRef.
	buildName := fmt.Sprintf("build-%s", rev.Name)
	var build functionsv1alpha1.FunctionBuild
	if err := r.Get(ctx, types.NamespacedName{Name: buildName, Namespace: rev.Namespace}, &build); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setPhase(ctx, rev, functionsv1alpha1.RevisionPhasePending, "Waiting for build to be created")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get function build: %w", err)
	}

	switch build.Status.Phase {
	case functionsv1alpha1.BuildPhaseSucceeded:
		if build.Status.ImageRef == "" {
			return r.setPhase(ctx, rev, functionsv1alpha1.RevisionPhaseBuilding, "Build succeeded but image ref not yet available")
		}
		// Patch the revision spec with the imageRef produced by the build.
		patch := client.MergeFrom(rev.DeepCopy())
		rev.Spec.ImageRef = build.Status.ImageRef
		rev.Spec.BuildRef = buildName
		if err := r.Patch(ctx, rev, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to patch revision imageRef: %w", err)
		}
		return ctrl.Result{}, nil

	case functionsv1alpha1.BuildPhaseFailed:
		return r.setPhase(ctx, rev, functionsv1alpha1.RevisionPhaseFailed, build.Status.Message)

	default:
		return r.setPhase(ctx, rev, functionsv1alpha1.RevisionPhaseBuilding, "Build is in progress")
	}
}

// reconcileWorkload creates or updates the Workload for a revision with a known imageRef.
func (r *Reconciler) reconcileWorkload(ctx context.Context, rev *functionsv1alpha1.FunctionRevision) (ctrl.Result, error) {
	workloadName := rev.Name

	memory := defaultMemory
	if rev.Spec.FunctionSpec.Runtime.Resources.Memory != "" {
		memory = rev.Spec.FunctionSpec.Runtime.Resources.Memory
	}

	memQty, err := resource.ParseQuantity(memory)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to parse memory quantity %q: %w", memory, err)
	}

	port := int32(8080)
	if rev.Spec.FunctionSpec.Runtime.Port != nil {
		port = *rev.Spec.FunctionSpec.Runtime.Port
	}

	fnName := rev.Labels[labelFunctionName]

	desired := computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName,
			Namespace: rev.Namespace,
			Labels: map[string]string{
				labelFunctionName: fnName,
			},
		},
		Spec: computev1alpha.WorkloadSpec{
			Template: computev1alpha.InstanceTemplateSpec{
				Spec: computev1alpha.InstanceSpec{
					Runtime: computev1alpha.InstanceRuntimeSpec{
						Resources: computev1alpha.InstanceRuntimeResources{
							InstanceType: defaultInstanceType,
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: memQty,
							},
						},
						VirtualMachine: &computev1alpha.VirtualMachineRuntime{
							Ports: []computev1alpha.NamedPort{
								{Name: "http", Port: port},
							},
						},
					},
					NetworkInterfaces: []computev1alpha.InstanceNetworkInterface{
						{
							Network: networkingv1alpha.NetworkRef{
								Name:      defaultNetwork,
								Namespace: rev.Namespace,
							},
						},
					},
					Volumes: []computev1alpha.InstanceVolume{
						{
							Name: "boot",
							VolumeSource: computev1alpha.VolumeSource{
								Disk: &computev1alpha.DiskTemplateVolumeSource{
									Template: &computev1alpha.DiskTemplateVolumeSourceTemplate{
										Spec: computev1alpha.DiskSpec{
											Resources: &computev1alpha.DiskResourceRequirements{
												Requests: corev1.ResourceList{
													corev1.ResourceStorage: resource.MustParse("1Gi"),
												},
											},
											Populator: &computev1alpha.DiskPopulator{
												Image: &computev1alpha.ImageDiskPopulator{
													Name: rev.Spec.ImageRef,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			Placements: []computev1alpha.WorkloadPlacement{
				{
					Name:      "default",
					CityCodes: []string{"ANY"},
					ScaleSettings: computev1alpha.HorizontalScaleSettings{
						MinReplicas: 0,
					},
				},
			},
		},
	}

	// Attach the boot volume to the VM.
	bootAttachment := "boot"
	desired.Spec.Template.Spec.Runtime.VirtualMachine.VolumeAttachments = []computev1alpha.VolumeAttachment{
		{Name: bootAttachment},
	}

	if err := controllerutil.SetControllerReference(rev, &desired, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner reference on workload: %w", err)
	}

	var existing computev1alpha.Workload
	err = r.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: rev.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, &desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create workload: %w", err)
		}
		ctrl.LoggerFrom(ctx).Info("created workload", "workload", workloadName)
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get workload: %w", err)
	}

	// Sync status from the Workload.
	return ctrl.Result{}, r.syncStatusFromWorkload(ctx, rev, &existing)
}

// syncStatusFromWorkload updates the FunctionRevision status based on the
// Workload's availability condition.
func (r *Reconciler) syncStatusFromWorkload(ctx context.Context, rev *functionsv1alpha1.FunctionRevision, workload *computev1alpha.Workload) error {
	patch := client.MergeFrom(rev.DeepCopy())

	workloadAvailable := apimeta.IsStatusConditionTrue(workload.Status.Conditions, computev1alpha.WorkloadAvailable)

	if workloadAvailable {
		rev.Status.Phase = functionsv1alpha1.RevisionPhaseReady
		rev.Status.WorkloadRef = workload.Name
		apimeta.SetStatusCondition(&rev.Status.Conditions, metav1.Condition{
			Type:    functionsv1alpha1.FunctionRevisionConditionWorkloadReady,
			Status:  metav1.ConditionTrue,
			Reason:  "WorkloadAvailable",
			Message: fmt.Sprintf("Workload %s is available", workload.Name),
		})
		apimeta.SetStatusCondition(&rev.Status.Conditions, metav1.Condition{
			Type:    functionsv1alpha1.FunctionRevisionConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  "Ready",
			Message: "Revision is ready to serve traffic",
		})
	} else {
		rev.Status.Phase = functionsv1alpha1.RevisionPhasePending
		rev.Status.WorkloadRef = workload.Name
		apimeta.SetStatusCondition(&rev.Status.Conditions, metav1.Condition{
			Type:    functionsv1alpha1.FunctionRevisionConditionWorkloadReady,
			Status:  metav1.ConditionFalse,
			Reason:  "WorkloadNotAvailable",
			Message: fmt.Sprintf("Workload %s is not yet available", workload.Name),
		})
		apimeta.SetStatusCondition(&rev.Status.Conditions, metav1.Condition{
			Type:    functionsv1alpha1.FunctionRevisionConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "WorkloadNotReady",
			Message: "Waiting for workload to become available",
		})
	}

	if err := r.Status().Patch(ctx, rev, patch); err != nil {
		return fmt.Errorf("failed to patch revision status: %w", err)
	}
	return nil
}

// setPhase is a helper that updates only the revision phase and returns an
// empty result.
func (r *Reconciler) setPhase(ctx context.Context, rev *functionsv1alpha1.FunctionRevision, phase functionsv1alpha1.RevisionPhase, message string) (ctrl.Result, error) {
	if rev.Status.Phase == phase {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(rev.DeepCopy())
	rev.Status.Phase = phase
	if err := r.Status().Patch(ctx, rev, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch revision phase to %s: %w", phase, err)
	}
	ctrl.LoggerFrom(ctx).Info("updated revision phase", "phase", phase, "message", message)
	return ctrl.Result{}, nil
}
