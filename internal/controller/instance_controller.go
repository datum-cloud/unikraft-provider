// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	"go.datum.net/unikraft-provider/internal/downstreamclient"
	milosource "go.miloapis.com/milo/pkg/multicluster-runtime/source"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"

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

	"go.datum.net/unikraft-provider/internal/config"
	computev1alpha "go.datum.net/compute/api/v1alpha"
)

const (
	unikraftFinalizer = "unikraft.datumapis.com/finalizer"

	ukcScaleToZeroPolicyAnnotation         = "cloud.unikraft.v1.instances/scale_to_zero.policy"
	ukcScaleToZeroStatefulAnnotation       = "cloud.unikraft.v1.instances/scale_to_zero.stateful"
	ukcScaleToZeroCoolDownTimeMsAnnotation = "cloud.unikraft.v1.instances/scale_to_zero.cooldown_time_ms"
	ukcInstanceTemplate                    = "cloud.unikraft.v1.instances/template"

	defaultInstanceMemoryMB = 1024
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

	instancePod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      string(instance.UID),
			Namespace: instance.Namespace,
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, downstreamClient, instancePod, func() error {
		if instancePod.Labels == nil {
			instancePod.Labels = map[string]string{}
		}
		instancePod.Labels["managed-by"] = "infra-provider-unikraft"
		instancePod.Labels["upstream.instance"] = instance.Name

		if instancePod.Annotations == nil {
			instancePod.Annotations = map[string]string{}
		}
		instancePod.Annotations[downstreamclient.UpstreamOwnerClusterName] = clusterName
		instancePod.Annotations[downstreamclient.UpstreamOwnerName] = instance.Name
		instancePod.Annotations[downstreamclient.UpstreamOwnerNamespace] = instance.Namespace

		if instancePod.CreationTimestamp.IsZero() {
			logger.Info("building pod spec for new instance pod", "name", instancePod.Name)
			podSpec, err := r.buildPodSpecFromContainers(instance, instance.Spec.Runtime.Sandbox.Containers)
			if err != nil {
				return err
			}
			instancePod.Spec = podSpec
			return nil
		}

		logger.Info("skipping pod spec reconciliation; pod already exists",
			"name", instancePod.Name,
			"creationTimestamp", instancePod.CreationTimestamp,
		)
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create/update pod for instance %s: %w", instance.Name, err)
	}

	logger.Info("reconciled instance pod",
		"result", result,
		"name", instancePod.Name,
		"containers", len(instance.Spec.Runtime.Sandbox.Containers),
		"phase", instancePod.Status.Phase,
		"message", instancePod.Status.Message,
	)

	if err := r.reconcileInstanceService(ctx, downstreamClient, clusterName, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile service for instance %s: %w", instance.Name, err)
	}

	if err := r.syncInstancePowerState(ctx, upstreamClient, instance, instancePod); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to sync instance power state: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) buildPodSpecFromContainers(
	instance *computev1alpha.Instance,
	sandboxContainers []computev1alpha.SandboxContainer,
) (core.PodSpec, error) {
	containers := make([]core.Container, 0, len(sandboxContainers))
	for i := range sandboxContainers {
		sc := &sandboxContainers[i]

		// Map environment variables from container
		envVars := make([]core.EnvVar, 0, len(sc.Env))
		for _, env := range sc.Env {
			envVars = append(envVars, core.EnvVar{
				Name:  env.Name,
				Value: env.Value,
			})
		}

		// Map ports from container
		ports := make([]core.ContainerPort, 0, len(sc.Ports))
		for _, p := range sc.Ports {
			ports = append(ports, core.ContainerPort{
				Name:          p.Name,
				ContainerPort: p.Port,
			})
		}

		// Map memory limit
		memoryMB := mapContainerMemory(sc)
		resources := core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceMemory: *resource.NewQuantity(memoryMB*1024*1024, resource.BinarySI),
			},
		}

		containers = append(containers, core.Container{
			Name:      sc.Name,
			Image:     sc.Image,
			Env:       envVars,
			Ports:     ports,
			Resources: resources,
		})
	}

	spec := core.PodSpec{
		Containers:    containers,
		RestartPolicy: core.RestartPolicyAlways,
		NodeSelector: map[string]string{
			"kubernetes.io/hostname": "kraftlet",
		},
		Tolerations: []core.Toleration{
			{
				Key:      "virtual-kubelet.io/provider",
				Operator: "Equal",
				Value:    "ukc",
				Effect:   "NoSchedule",
			},
		},
	}

	return spec, nil
}

func (r *InstanceReconciler) reconcileInstanceService(
	ctx context.Context,
	downstreamClient client.Client,
	clusterName string,
	instance *computev1alpha.Instance,
) error {
	logger := log.FromContext(ctx)

	type containerPort struct {
		name string
		port int32
	}

	var allPorts []containerPort
	if instance.Spec.Runtime.Sandbox != nil {
		for _, c := range instance.Spec.Runtime.Sandbox.Containers {
			for _, p := range c.Ports {
				allPorts = append(allPorts, containerPort{name: p.Name, port: p.Port})
			}
		}
	}

	svc := &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}

	if len(allPorts) == 0 {
		if err := downstreamClient.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete obsolete service: %w", err)
		}
		return nil
	}

	servicePorts := make([]core.ServicePort, 0, len(allPorts))
	if len(allPorts) == 1 {
		p := allPorts[0]
		name := p.name
		if name == "" {
			name = "http"
		}
		servicePorts = append(servicePorts, core.ServicePort{
			Name:       name,
			Port:       443,
			TargetPort: intstr.FromInt32(p.port),
			Protocol:   core.ProtocolTCP,
		})
	} else {
		for _, p := range allPorts {
			name := p.name
			if name == "" {
				name = fmt.Sprintf("port-%d", p.port)
			}
			servicePorts = append(servicePorts, core.ServicePort{
				Name:       name,
				Port:       p.port,
				TargetPort: intstr.FromInt32(p.port),
				Protocol:   core.ProtocolTCP,
			})
		}
	}

	result, err := controllerutil.CreateOrPatch(ctx, downstreamClient, svc, func() error {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations[downstreamclient.UpstreamOwnerClusterName] = clusterName
		svc.Annotations[downstreamclient.UpstreamOwnerName] = instance.Name
		svc.Annotations[downstreamclient.UpstreamOwnerNamespace] = instance.Namespace

		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels["managed-by"] = "infra-provider-unikraft"
		svc.Labels["upstream.instance"] = instance.Name

		svc.Spec.Selector = map[string]string{
			"upstream.instance": instance.Name,
		}
		svc.Spec.Ports = servicePorts
		if svc.Spec.Type == "" {
			svc.Spec.Type = core.ServiceTypeClusterIP
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create/update service: %w", err)
	}

	logger.Info("reconciled instance service",
		"result", result,
		"name", svc.Name,
		"ports", len(servicePorts),
	)

	return nil
}

func (r *InstanceReconciler) syncInstancePowerState(
	ctx context.Context,
	upstreamClient client.Client,
	instance *computev1alpha.Instance,
	instancePod *core.Pod,
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

	switch {
	case !instance.DeletionTimestamp.IsZero():
		runningCondition.Status = metav1.ConditionFalse
		runningCondition.Reason = computev1alpha.InstanceRunningReasonStopping
		runningCondition.Message = "Instance is being deleted"
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = "Terminating"
		readyCondition.Message = "Instance is being deleted"

	default:
		// Derive running condition from the pod phase.
		switch instancePod.Status.Phase {
		case core.PodRunning:
			runningCondition.Status = metav1.ConditionTrue
			runningCondition.Reason = computev1alpha.InstanceRunningReasonRunning
			runningCondition.Message = "Pod is running"
		case core.PodPending:
			runningCondition.Status = metav1.ConditionUnknown
			runningCondition.Reason = "Pending"
			runningCondition.Message = "Pod is pending"
			// Surface a more useful message from container waiting states, if any.
			for _, cs := range instancePod.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
					runningCondition.Reason = cs.State.Waiting.Reason
					if cs.State.Waiting.Message != "" {
						runningCondition.Message = cs.State.Waiting.Message
					}
					break
				}
			}
		case core.PodSucceeded:
			runningCondition.Status = metav1.ConditionFalse
			runningCondition.Reason = computev1alpha.InstanceRunningReasonStopping
			runningCondition.Message = "Pod completed"
		case core.PodFailed:
			runningCondition.Status = metav1.ConditionFalse
			runningCondition.Reason = "Failed"
			runningCondition.Message = instancePod.Status.Message
			if runningCondition.Message == "" {
				runningCondition.Message = "Pod failed"
			}
		default:
			runningCondition.Status = metav1.ConditionUnknown
			runningCondition.Reason = "Unknown"
			runningCondition.Message = "Pod phase is unknown"
		}

		readyCondition.Status = metav1.ConditionUnknown
		readyCondition.Reason = "Unknown"
		for _, c := range instancePod.Status.Conditions {
			if c.Type != core.PodReady {
				continue
			}
			readyCondition.Status = metav1.ConditionStatus(c.Status)
			if c.Reason != "" {
				readyCondition.Reason = c.Reason
			} else {
				readyCondition.Reason = "PodReady"
			}
			readyCondition.Message = c.Message
			break
		}
	}

	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, runningCondition) || statusChanged
	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, readyCondition) || statusChanged

	var networkIP string
	if len(instancePod.Status.PodIPs) > 0 {
		networkIP = instancePod.Status.PodIPs[0].IP
	}
	if networkIP != "" {
		if len(instance.Status.NetworkInterfaces) == 0 {
			instance.Status.NetworkInterfaces = make([]computev1alpha.InstanceNetworkInterfaceStatus, 1)
			statusChanged = true
		}
		if instance.Status.NetworkInterfaces[0].Assignments.NetworkIP == nil ||
			*instance.Status.NetworkInterfaces[0].Assignments.NetworkIP != networkIP {
			instance.Status.NetworkInterfaces[0].Assignments.NetworkIP = &networkIP
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
			podInstance := &core.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s", instance.UID),
					Namespace: instance.Namespace,
				},
			}

			if err := downstreamClient.Delete(ctx, podInstance); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to delete unikraft instance for container %d: %w", idx, err)
				}
			}
			logger.Info("deleted downstream unikraft instance", "container-idx", idx)
		}
	}

	// Delete the downstream Service that was created alongside the pod (if any).
	svc := &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}
	if err := downstreamClient.Delete(ctx, svc); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete downstream service: %w", err)
		}
	} else {
		logger.Info("deleted downstream service", "name", svc.Name)
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(instance, unikraftFinalizer)
	if err := upstreamClient.Update(ctx, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.Instance{}).
		WatchesRawSource(milosource.MustNewClusterSource(r.DownstreamCluster, &core.Pod{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*core.Pod, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, instancePod *core.Pod) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := instancePod.Annotations[downstreamclient.UpstreamOwnerClusterName]
				upstreamName := instancePod.Annotations[downstreamclient.UpstreamOwnerName]
				upstreamNamespace := instancePod.Annotations[downstreamclient.UpstreamOwnerNamespace]

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
