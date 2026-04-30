// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"go.datum.net/unikraft-provider/internal/downstreamclient"
	milosource "go.miloapis.com/milo/pkg/multicluster-runtime/source"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	"unikraft.com/cloud/sdk/pkg/ptr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	unikraftv1alpha1 "github.com/unikraft-cloud/k8s-operator/api/v1alpha1"
	"github.com/unikraft-cloud/k8s-operator/api/v1alpha1/platform"
	"go.datum.net/unikraft-provider/internal/config"
	computev1alpha "go.datum.net/compute/api/v1alpha"
)

const (
	unikraftFinalizer = "unikraft.datumapis.com/finalizer"

	ukcScaleToZeroPolicyAnnotation         = "cloud.unikraft.v1.instances/scale_to_zero.policy"
	ukcScaleToZeroStatefulAnnotation       = "cloud.unikraft.v1.instances/scale_to_zero.stateful"
	ukcScaleToZeroCoolDownTimeMsAnnotation = "cloud.unikraft.v1.instances/scale_to_zero.cooldown_time_ms"
	ukcInstanceTemplate                    = "cloud.unikraft.v1.instances/template"

	defaultInstanceMemoryMB          = 1024
	defaultScaleToZeroPolicy         = platform.CreateInstanceRequestScaleToZeroPolicyOn
	defaultScaleToZeroStateful       = false
	defaultScaleToZeroCooldownTimeMs = int32(1000)
)

type InstanceReconciler struct {
	mgr               mcmanager.Manager
	Config            *config.UnikraftProvider
	LocationClassName string
	DownstreamCluster cluster.Cluster
}

// Reconcile implements the reconciliation logic
func (r *InstanceReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("reconciling instance", "cluster", req.ClusterName, "name", req.Name, "namespace", req.Namespace)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)
	upstreamClient := cl.GetClient()

	var instance computev1alpha.Instance
	if err := upstreamClient.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("instance not found, may have been deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get instance: %w", err)
	}

	downstreamClient := r.DownstreamCluster.GetClient()

	if !instance.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&instance, unikraftFinalizer) {
			return r.handleDeletion(ctx, upstreamClient, downstreamClient, &instance)
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&instance, unikraftFinalizer) {
		controllerutil.AddFinalizer(&instance, unikraftFinalizer)
		if err := upstreamClient.Update(ctx, &instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Only handle sandbox instances
	if instance.Spec.Runtime.Sandbox == nil {
		logger.Info("skipping non-sandbox instance")
		return ctrl.Result{}, nil
	}

	// Create one Unikraft instance per container in the sandbox
	return r.reconcileSandboxContainers(ctx, req.ClusterName, upstreamClient, downstreamClient, &instance)
}

func (r *InstanceReconciler) reconcileSandboxContainers(
	ctx context.Context,
	clusterName string,
	upstreamClient client.Client,
	downstreamClient client.Client,
	instance *computev1alpha.Instance,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if instance.Spec.Runtime.Sandbox == nil {
		return ctrl.Result{}, fmt.Errorf("sandbox runtime is nil")
	}

	var unikraftInstance *unikraftv1alpha1.Instance

	// Create one Unikraft Instance per container
	for idx, container := range instance.Spec.Runtime.Sandbox.Containers {
		unikraftInstance = &unikraftv1alpha1.Instance{
			ObjectMeta: metav1.ObjectMeta{
				// Use a unique name based on upstream instance UID and container index
				Name:      fmt.Sprintf("instance-%s-container-%d", instance.UID, idx),
				Namespace: instance.Namespace,
				Annotations: map[string]string{
					downstreamclient.UpstreamOwnerClusterName: clusterName,
					downstreamclient.UpstreamOwnerName:        instance.Name,
					downstreamclient.UpstreamOwnerNamespace:   instance.Namespace,
				},
				Labels: map[string]string{
					"managed-by":         "infra-provider-unikraft",
					"upstream.instance":  instance.Name,
					"upstream.container": container.Name,
				},
			},
		}

		var unikraftVolume *unikraftv1alpha1.Volume
		for _, volume := range instance.Spec.Volumes {
			unikraftVolume = &unikraftv1alpha1.Volume{
				ObjectMeta: metav1.ObjectMeta{
					// Use a unique name based on upstream instance UID and container index
					Name:      volume.Name,
					Namespace: instance.Namespace,
					Annotations: map[string]string{
						downstreamclient.UpstreamOwnerClusterName: clusterName,
						downstreamclient.UpstreamOwnerName:        instance.Name,
						downstreamclient.UpstreamOwnerNamespace:   instance.Namespace,
					},
					Labels: map[string]string{
						"managed-by":         "infra-provider-unikraft",
						"upstream.instance":  instance.Name,
						"upstream.container": container.Name,
					},
				},
			}

			_, err := controllerutil.CreateOrPatch(ctx, downstreamClient, unikraftVolume, func() error {
				unikraftVolume.Spec.Name = ptr.Ptr(volume.Name)
				unikraftVolume.Spec.SizeMb = 100
				return nil
			})

			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create/update unikraft instance for container %s: %w", container.Name, err)
			}
		}

		result, err := controllerutil.CreateOrPatch(ctx, downstreamClient, unikraftInstance, func() error {
			// Convert container to Unikraft CreateInstanceRequest
			unikraftSpec, err := r.buildUnikraftSpecFromContainer(instance, &container)
			if err != nil {
				return err
			}

			unikraftInstance.Spec = unikraftSpec
			return nil
		})

		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create/update unikraft instance for container %s: %w", container.Name, err)
		}

		logger.Info("reconciled unikraft instance",
			"result", result,
			"name", unikraftInstance.Name,
			"container", container.Name,
			"status", unikraftInstance.Status.Status,
			"message", unikraftInstance.Status.Message,
		)

		// TODO: Sync status from unikraftInstance back to upstream instance
		// After CreateOrPatch, unikraftInstance contains the full state from the cluster
		// including unikraftInstance.Status which has the Unikraft instance state
	}

	if err := r.syncInstancePowerState(ctx, upstreamClient, instance, unikraftInstance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to sync instance power state: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) buildUnikraftSpecFromContainer(
	instance *computev1alpha.Instance,
	container *computev1alpha.SandboxContainer,
) (platform.CreateInstanceRequest, error) {
	name := ptr.Ptr(unikraftInstanceName(instance.Name, container.Name))

	scaleToZero, err := mapInstanceScaleToZero(instance.Annotations, assignContainerServices(container.Ports))
	if err != nil {
		return platform.CreateInstanceRequest{}, err
	}

	if instanceTemplate, ok := instance.Annotations[ukcInstanceTemplate]; ok {
		var serviceGroup *platform.CreateInstanceRequestServiceGroup

		if len(container.Ports) > 0 {
			serviceGroup = &platform.CreateInstanceRequestServiceGroup{
				Services: assignContainerServices(container.Ports),
			}
		}

		return platform.CreateInstanceRequest{
			Name:         name,
			ScaleToZero:  scaleToZero,
			ServiceGroup: serviceGroup,
			Template:     &platform.CreateInstanceRequestTemplate{NameOrUUID: &platform.CreateInstanceRequestTemplateNameOrUUID{Name: instanceTemplate}},
			Roms:         mapInstanceRoms(instance),
		}, nil
	}

	image := container.Image

	spec := platform.CreateInstanceRequest{
		Name:      ptr.Ptr(unikraftInstanceName(instance.Name, container.Name)),
		Image:     &image,
		Autostart: ptr.Ptr(true),
	}

	// Map instance type to memory/vcpus
	if instance.Spec.Runtime.Resources.InstanceType != "" {
		// TODO: Add instance type mapping
		// For now, use defaults
		spec.MemoryMb = ptr.Ptr(int64(128))
		spec.Vcpus = ptr.Ptr(int32(1))
	}

	// Map environment variables from container
	spec.Env = make(map[string]string)
	for _, env := range container.Env {
		spec.Env[env.Name] = env.Value
	}

	if len(container.Ports) > 0 {
		spec.ServiceGroup = &platform.CreateInstanceRequestServiceGroup{
			Services: assignContainerServices(container.Ports),
		}
	}

	spec.MemoryMb = ptr.Ptr(mapContainerMemory(container))
	spec.ScaleToZero = scaleToZero

	for _, attachment := range container.VolumeAttachments {
		if attachment.MountPath == nil {
			continue
		}

		spec.Volumes = append(spec.Volumes, platform.CreateInstanceRequestVolume{
			Name: ptr.Ptr(attachment.Name),
			At:   *attachment.MountPath,
		})
	}

	spec.Args = []string{"/usr/bin/bun run /usr/src/server.ts"}

	return spec, nil
}

func (r *InstanceReconciler) syncInstancePowerState(
	ctx context.Context,
	upstreamClient client.Client,
	instance *computev1alpha.Instance,
	ukcInstance *unikraftv1alpha1.Instance,
) error {
	runningCondition := metav1.Condition{
		Type:               computev1alpha.InstanceRunning,
		ObservedGeneration: instance.Generation,
		Reason:             computev1alpha.InstanceRunningReasonRunning,
		Status:             metav1.ConditionTrue,
	}

	readyCondition := metav1.Condition{
		Type:               computev1alpha.InstanceReady,
		ObservedGeneration: instance.Generation,
		Reason:             "Ready",
		Status:             metav1.ConditionUnknown,
	}

	statusChanged := false

	if !instance.DeletionTimestamp.IsZero() {
		runningCondition.Reason = computev1alpha.InstanceRunningReasonStopping
		runningCondition.Message = "Instance is being deleted"
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = "Terminating"
		readyCondition.Message = "Instance is being deleted"
	} else {
		if ukcInstance.Status.Status != nil && *ukcInstance.Status.Status == platform.ResponseStatusSUCCESS {
			runningCondition.Status = metav1.ConditionTrue
			runningCondition.Message = "Instance is running"
			readyCondition.Status = metav1.ConditionTrue
			readyCondition.Message = "Instance is ready"
		}

		if ukcInstance.Status.Status != nil && *ukcInstance.Status.Status == platform.ResponseStatusERROR {
			runningCondition.Status = metav1.ConditionFalse
			runningCondition.Message = "Instance is not running"
			readyCondition.Status = metav1.ConditionFalse
			readyCondition.Reason = "Error"
			readyCondition.Message = "Instance is not ready"
		}
	}

	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, runningCondition) || statusChanged
	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, readyCondition) || statusChanged

	if externalIP := r.extractExternalIPFromUnikraftInstance(ukcInstance); externalIP != "" {
		if len(instance.Status.NetworkInterfaces) == 0 {
			instance.Status.NetworkInterfaces = make([]computev1alpha.InstanceNetworkInterfaceStatus, 1)
			statusChanged = true
		}
		if instance.Status.NetworkInterfaces[0].Assignments.ExternalIP == nil ||
			*instance.Status.NetworkInterfaces[0].Assignments.ExternalIP != externalIP {
			instance.Status.NetworkInterfaces[0].Assignments.ExternalIP = &externalIP
			statusChanged = true
		}
	}

	if statusChanged {
		if err := upstreamClient.Status().Update(ctx, instance); err != nil {
			return fmt.Errorf("failed to update instance status: %w", err)
		}
	}

	return nil
}

func (r *InstanceReconciler) extractExternalIPFromUnikraftInstance(ukcInstance *unikraftv1alpha1.Instance) string {
	if ukcInstance.Status.Data == nil || len(ukcInstance.Status.Data.Instances) == 0 {
		return ""
	}

	instance := ukcInstance.Status.Data.Instances[0]
	if instance.ServiceGroup == nil || len(instance.ServiceGroup.Domains) == 0 {
		return ""
	}

	domain := instance.ServiceGroup.Domains[0]
	if domain.Fqdn != nil {
		return *domain.Fqdn
	}

	return ""
}

func (r *InstanceReconciler) handleDeletion(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamClient client.Client,
	instance *computev1alpha.Instance,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Delete all downstream Unikraft instances for all containers
	if instance.Spec.Runtime.Sandbox != nil {
		for idx := range instance.Spec.Runtime.Sandbox.Containers {
			unikraftInstance := &unikraftv1alpha1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("instance-%s-container-%d", instance.UID, idx),
					Namespace: instance.Namespace,
				},
			}

			if err := downstreamClient.Delete(ctx, unikraftInstance); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to delete unikraft instance for container %d: %w", idx, err)
				}
			}
			logger.Info("deleted downstream unikraft instance", "container-idx", idx)
		}
	}

	if instance.Spec.Volumes != nil {
		for idx, volume := range instance.Spec.Volumes {
			unikraftVolume := &unikraftv1alpha1.Volume{
				ObjectMeta: metav1.ObjectMeta{
					Name:      volume.Name,
					Namespace: instance.Namespace,
				},
			}

			if err := downstreamClient.Delete(ctx, unikraftVolume); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to delete unikraft volume %d: %w", idx, err)
				}
			}
			logger.Info("deleted downstream unikraft instance", "container-idx", idx)
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(instance, unikraftFinalizer)
	if err := upstreamClient.Update(ctx, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

func unikraftInstanceName(instanceName, containerName string) string {
	return fmt.Sprintf("%s-%s", instanceName, containerName)
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.Instance{}).
		WatchesRawSource(milosource.MustNewClusterSource(r.DownstreamCluster, &unikraftv1alpha1.Instance{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*unikraftv1alpha1.Instance, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, instance *unikraftv1alpha1.Instance) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := instance.Annotations[downstreamclient.UpstreamOwnerClusterName]
				upstreamName := instance.Annotations[downstreamclient.UpstreamOwnerName]
				upstreamNamespace := instance.Annotations[downstreamclient.UpstreamOwnerNamespace]

				if upstreamClusterName == "" || upstreamName == "" || upstreamNamespace == "" {
					logger.Info("Unikraft instance is missing upstream ownership metadata")
					return nil
				}

				return []mcreconcile.Request{
					{
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: upstreamNamespace,
								Name:      upstreamName,
							},
						},
						ClusterName: upstreamClusterName,
					},
				}
			})
		})).
		Named("instance").
		Complete(r)
}

//func (r *InstanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {
//	r.mgr = mgr
//
//	return mcbuilder.ControllerManagedBy(mgr).
//		For(&computev1alpha.Instance{}).
//		Named("instance").
//		Complete(r)
//}
