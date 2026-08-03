// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/unikraft-provider/internal/config"
)

const (
	defaultInstanceMemoryMB = 1024

	unikraftAnnotationPrefix   = "cloud.unikraft.v1"
	ukcInstanceFqdnsAnnotation = "cloud.unikraft.v1.instances/fqdns"

	// ukcCniEnabledAnnotation opts an Instance's Pod into kraftlet's
	// remote-CNI integration. It is a platform decision (config.EnableCNI),
	// not a tenant-facing knob, so it is excluded from the generic
	// Instance->Pod annotation passthrough below and set by the controller
	// instead.
	ukcCniEnabledAnnotation = "cloud.unikraft.v1.instances/cni-enabled"

	// instanceFinalizer gates Instance deletion on teardown of the backing Pod
	// (and Service). The provider holds this finalizer until it has deleted the
	// Pod and observed it fully removed, so the Instance — and the upstream
	// WorkloadDeployment/Workload that wait on it — is never removed before the
	// downstream Pod is gone.
	instanceFinalizer = "unikraft.datumapis.com/finalizer"
)

type InstanceReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Config            *config.UnikraftProvider
	LocationClassName string
}

// Reconcile implements the reconciliation logic
func (r *InstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("reconciling instance", "name", req.Name, "namespace", req.Namespace)

	var instance computev1alpha.Instance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("instance not found, may have been deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get instance: %w", err)
	}

	// A deleting Instance is handled by the finalizer path, which tears down the
	// backing Pod/Service and waits for them to be gone before releasing the
	// Instance. This must run before the sandbox/scheduling-gate short-circuits
	// below so teardown is never skipped.
	if !instance.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &instance)
	}

	// Only handle sandbox instances
	if instance.Spec.Runtime.Sandbox == nil {
		logger.Info("skipping non-sandbox instance")
		return ctrl.Result{}, nil
	}

	// Honor scheduling gates. While any gate remains, the instance is not yet
	// ready to be scheduled. Return without requeue — the Instance spec update
	// when a gate is cleared will re-trigger reconciliation.
	if instance.Spec.Controller != nil && len(instance.Spec.Controller.SchedulingGates) > 0 {
		logger.Info("instance has scheduling gates, deferring pod creation",
			"gates", instance.Spec.Controller.SchedulingGates,
		)
		return ctrl.Result{}, nil
	}

	// If instance.Status.Suspended is true, delete the backing Pod (which stops
	// the process) but keep the Instance object, its finalizer, and the Service.
	// On resume (Suspended=false), reconcileSandboxContainers will recreate the Pod.
	if instance.Status.Suspended {
		return r.reconcileSuspended(ctx, &instance)
	}

	// Ensure our finalizer is present before creating any backing resources, so
	// that teardown is always routed through handleDeletion and ordered behind
	// the Pod's removal. Adding it here (rather than on every Instance) means
	// non-sandbox and still-gated Instances — which have no backing Pod — are not
	// burdened with a finalizer.
	if !controllerutil.ContainsFinalizer(&instance, instanceFinalizer) {
		controllerutil.AddFinalizer(&instance, instanceFinalizer)
		if err := r.Update(ctx, &instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer to instance %s: %w", instance.Name, err)
		}
	}

	return r.reconcileSandboxContainers(ctx, &instance)
}

// handleDeletion tears down the backing Pod and Service for a deleting Instance
// and removes the provider finalizer only once they are confirmed gone.
//
// The Pod/Service are deleted explicitly rather than via owner-reference garbage
// collection: GC would not reclaim them until the Instance is removed from etcd,
// but the Instance cannot be removed while this finalizer is held — relying on GC
// here would deadlock. Explicit deletion drives teardown; the owner reference set
// on the Pod/Service remains only as a backstop for abnormal cases (e.g. a crash
// before the finalizer was added).
func (r *InstanceReconciler) handleDeletion(ctx context.Context, instance *computev1alpha.Instance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(instance, instanceFinalizer) {
		// Either the Instance never had a backing Pod (non-sandbox or still
		// scheduling-gated) or it was already finalized. Nothing to clean up.
		return ctrl.Result{}, nil
	}

	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete pod for instance %s: %w", instance.Name, err)
	}

	svc := &core.Service{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete service for instance %s: %w", instance.Name, err)
	}

	// Gate: do not release the Instance until its backing Pod (and Service) are
	// fully gone. While either still exists, requeue and wait. The Owns(Pod)/
	// Owns(Service) watches also re-trigger reconciliation on their final removal;
	// the RequeueAfter is a backstop in case that event is missed.
	if pending, err := r.backingResourcesPending(ctx, instance); err != nil {
		return ctrl.Result{}, err
	} else if pending {
		logger.Info("waiting for backing resources to terminate before finalizing instance", "name", instance.Name)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	controllerutil.RemoveFinalizer(instance, instanceFinalizer)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from instance %s: %w", instance.Name, err)
	}
	logger.Info("backing resources gone; removed finalizer, instance may be deleted", "name", instance.Name)
	return ctrl.Result{}, nil
}

// backingResourcesPending reports whether the Instance's backing Pod or Service
// still exists in the API.
func (r *InstanceReconciler) backingResourcesPending(ctx context.Context, instance *computev1alpha.Instance) (bool, error) {
	key := client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}

	var pod core.Pod
	switch err := r.Get(ctx, key, &pod); {
	case err == nil:
		return true, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("failed to check backing pod for instance %s: %w", instance.Name, err)
	}

	var svc core.Service
	switch err := r.Get(ctx, key, &svc); {
	case err == nil:
		return true, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("failed to check backing service for instance %s: %w", instance.Name, err)
	}

	return false, nil
}

// reconcileSuspended deletes the backing Pod to stop the instance process without
// deleting the Instance object itself, its finalizer, or the Service.
func (r *InstanceReconciler) reconcileSuspended(ctx context.Context, instance *computev1alpha.Instance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete pod for suspended instance %s: %w", instance.Name, err)
	}

	logger.Info("instance suspended (pod deleted)", "name", instance.Name)
	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) reconcileSandboxContainers(
	ctx context.Context,
	instance *computev1alpha.Instance,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if instance.Spec.Runtime.Sandbox == nil {
		return ctrl.Result{}, fmt.Errorf("sandbox runtime is nil")
	}

	instancePod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, r.Client, instancePod, func() error {
		if instancePod.Labels == nil {
			instancePod.Labels = map[string]string{}
		}
		instancePod.Labels["managed-by"] = "infra-provider-unikraft"
		instancePod.Labels["upstream.instance"] = instance.Name

		if instancePod.Annotations == nil {
			instancePod.Annotations = map[string]string{}
		}

		// Mirror any cloud.unikraft.v1.* annotations from the upstream
		// Instance onto the downstream Pod, aside from the platform-controlled
		// ones set explicitly below.
		copyUnikraftAnnotations(instance.Annotations, instancePod.Annotations)

		// CNI is a platform decision, not a tenant one: set it from provider
		// config rather than letting it flow through from the Instance, so a
		// tenant can't enable or disable it by setting the annotation
		// themselves.
		if r.Config != nil && r.Config.DownstreamResourceManagement.EnableCNI {
			instancePod.Annotations[ukcCniEnabledAnnotation] = "true"
		} else {
			delete(instancePod.Annotations, ukcCniEnabledAnnotation)
		}

		if instancePod.CreationTimestamp.IsZero() {
			logger.Info("building pod spec for new instance pod", "name", instancePod.Name)
			podSpec, err := r.buildPodSpecFromContainers(ctx, instance, instance.Spec.Runtime.Sandbox.Containers)
			if err != nil {
				return err
			}
			instancePod.Spec = podSpec
		} else {
			logger.Info("skipping pod spec reconciliation; pod already exists",
				"name", instancePod.Name,
				"creationTimestamp", instancePod.CreationTimestamp,
			)
		}

		if err := controllerutil.SetControllerReference(instance, instancePod, r.Scheme); err != nil {
			return err
		}
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

	if err := r.reconcileInstanceService(ctx, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile service for instance %s: %w", instance.Name, err)
	}

	if err := r.syncInstancePowerState(ctx, instance, instancePod); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to sync instance power state: %w", err)
	}

	return ctrl.Result{}, nil
}

// buildPodSpecFromContainers translates an Instance's sandbox spec into a
// Kubernetes PodSpec suitable for the kraftlet virtual-kubelet.
//
// Translation rules:
//   - Instance volumes with ConfigMap/Secret sources → Pod volumes (direct
//     assignment; the corev1 source types are identical).
//   - Disk sources are skipped — kraftlet does not support them at this time.
//   - Per-container VolumeAttachments with a non-nil MountPath → VolumeMounts.
//   - Container env vars carry both Value and ValueFrom through faithfully.
//
// Volume and env references (ConfigMap/Secret names) are resolved at mount time
// by kraftlet using its own node/kubelet identity. The provider only references
// them by name in the Pod spec; no data is read or mirrored by the provider.
//
// TODO(Phase 3b): EnvFrom mapping is deferred. compute's SandboxContainer does
// not yet expose an EnvFrom field. When it is added (planned for v1 API), the
// mapping here will require field-by-field translation from
// computev1alpha.EnvFromSource to core.EnvFromSource — it is NOT a simple
// ValueFrom-style passthrough because the two types are not identical.
func (r *InstanceReconciler) buildPodSpecFromContainers(
	ctx context.Context,
	instance *computev1alpha.Instance,
	sandboxContainers []computev1alpha.SandboxContainer,
) (core.PodSpec, error) {
	logger := log.FromContext(ctx)

	// Build the Pod-level volume list from the Instance spec.
	volumes := make([]core.Volume, 0, len(instance.Spec.Volumes))
	for _, iv := range instance.Spec.Volumes {
		switch {
		case iv.ConfigMap != nil:
			volumes = append(volumes, core.Volume{
				Name: iv.Name,
				VolumeSource: core.VolumeSource{
					ConfigMap: iv.ConfigMap,
				},
			})
		case iv.Secret != nil:
			volumes = append(volumes, core.Volume{
				Name: iv.Name,
				VolumeSource: core.VolumeSource{
					Secret: iv.Secret,
				},
			})
		case iv.Disk != nil:
			// Disk-backed volumes are not supported by kraftlet; skip and log so
			// operators can see the omission without a hard failure.
			logger.Info("skipping disk-backed volume (not supported by kraftlet)",
				"instance", instance.Name,
				"volume", iv.Name,
			)
		}
	}

	containers := make([]core.Container, 0, len(sandboxContainers))
	for i := range sandboxContainers {
		sc := &sandboxContainers[i]

		// Map environment variables from container. Carry ValueFrom through
		// faithfully so secret/configmap key refs work inside the Pod. Only
		// the literal Value field was previously forwarded; this is corrected here.
		envVars := make([]core.EnvVar, 0, len(sc.Env))
		for _, env := range sc.Env {
			envVars = append(envVars, core.EnvVar{
				Name:      env.Name,
				Value:     env.Value,
				ValueFrom: env.ValueFrom,
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

		// Resolve CPU and memory from the container spec or the instanceType
		// catalog. Using requests == limits ensures kraftlet sees a guaranteed
		// QoS class and the Pod's resource footprint matches what quota claimed.
		cpuMillicores, memoryMB := resolveContainerResources(instance, sc)
		memQ := *resource.NewQuantity(memoryMB*1024*1024, resource.BinarySI)
		resourceList := core.ResourceList{
			core.ResourceMemory: memQ,
		}
		if cpuMillicores > 0 {
			resourceList[core.ResourceCPU] = *resource.NewMilliQuantity(cpuMillicores, resource.DecimalSI)
		}
		resources := core.ResourceRequirements{
			Requests: resourceList.DeepCopy(),
			Limits:   resourceList,
		}

		// Map volume attachments to volume mounts. Only attachments with a
		// non-nil MountPath are included; attachments without a MountPath are
		// device references (e.g. raw disk) that kraftlet does not handle.
		volumeMounts := make([]core.VolumeMount, 0, len(sc.VolumeAttachments))
		for _, va := range sc.VolumeAttachments {
			if va.MountPath == nil {
				continue
			}
			volumeMounts = append(volumeMounts, core.VolumeMount{
				Name:      va.Name,
				MountPath: *va.MountPath,
			})
		}

		containers = append(containers, core.Container{
			Name:         sc.Name,
			Image:        sc.Image,
			Command:      sc.Command,
			Args:         sc.Args,
			Env:          envVars,
			Ports:        ports,
			Resources:    resources,
			VolumeMounts: volumeMounts,
		})
	}

	// Apply node selector: use the operator-supplied override when set, otherwise
	// default to the per-host Kraftlet virtual-kubelet label. The default places
	// guests on any per-host virtual-kubelet node (kraftlet-<host>), selected by
	// label rather than a single hard-coded node name. Override via
	// DownstreamResourceManagementConfig.NodeSelector for multi-node or relabelled
	// environments (e.g. Layer-2 e2e where the node has a different hostname label).
	nodeSelector := map[string]string{
		"unikraft.com/virtual-kubelet": "true",
	}
	if r.Config != nil && len(r.Config.DownstreamResourceManagement.NodeSelector) > 0 {
		nodeSelector = r.Config.DownstreamResourceManagement.NodeSelector
	}

	// Apply tolerations: use the operator-supplied override when set, otherwise
	// default to the ukc virtual-kubelet taint that kraftlet nodes carry.
	tolerations := []core.Toleration{
		{
			Key:      "virtual-kubelet.io/provider",
			Operator: "Equal",
			Value:    "ukc",
			Effect:   "NoSchedule",
		},
	}
	if r.Config != nil && len(r.Config.DownstreamResourceManagement.Tolerations) > 0 {
		tolerations = r.Config.DownstreamResourceManagement.Tolerations
	}

	spec := core.PodSpec{
		Containers: containers,
		Volumes:    volumes,
		// Sandbox workloads must not be granted Kubernetes API access. Disable
		// projection of the default ServiceAccount token so no credential is
		// mounted into the instance Pod (the apiserver still assigns the default
		// SA identity, but without a token it cannot authenticate).
		AutomountServiceAccountToken: ptr.To(false),
		EnableServiceLinks:           ptr.To(false),
		RestartPolicy:                core.RestartPolicyAlways,
		NodeSelector:                 nodeSelector,
		Tolerations:                  tolerations,
	}

	return spec, nil
}

func (r *InstanceReconciler) reconcileInstanceService(
	ctx context.Context,
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
		if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
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

	result, err := controllerutil.CreateOrPatch(ctx, r.Client, svc, func() error {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}

		// Mirror any cloud.unikraft.v1.* annotations from the upstream
		// Instance onto the downstream Service.
		copyUnikraftAnnotations(instance.Annotations, svc.Annotations)

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

		if err := controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
			return err
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
	instance *computev1alpha.Instance,
	instancePod *core.Pod,
) error {
	logger := log.FromContext(ctx)

	// Take the base snapshot BEFORE any status mutation so the patch only
	// includes fields this controller owns (Programmed, Available,
	// ObservedTemplateHash, NetworkInterfaces). QuotaGranted/Ready are owned
	// by the compute quota controller and must not be overwritten.
	base := instance.DeepCopy()

	availableCondition := metav1.Condition{
		Type:               computev1alpha.InstanceAvailable,
		ObservedGeneration: instance.Generation,
		Reason:             computev1alpha.InstanceAvailableReasonAvailable,
		Status:             metav1.ConditionTrue,
	}

	// programmedCondition reflects whether the infrastructure provider has
	// successfully provisioned the underlying compute resource. The compute
	// InstanceReconciler derives Ready from Programmed=True + Available=True, so
	// this provider is responsible for setting Programmed and must NOT write
	// the Ready condition directly (that would race with compute's derivation).
	programmedCondition := metav1.Condition{
		Type:               computev1alpha.InstanceProgrammed,
		ObservedGeneration: instance.Generation,
		Status:             metav1.ConditionUnknown,
		Reason:             computev1alpha.InstanceProgrammedReasonProgrammingInProgress,
		Message:            "Instance is provisioning",
	}

	statusChanged := false

	switch {
	case !instance.DeletionTimestamp.IsZero():
		availableCondition.Status = metav1.ConditionFalse
		availableCondition.Reason = computev1alpha.InstanceAvailableReasonStopping
		availableCondition.Message = "Instance is terminating"
		programmedCondition.Status = metav1.ConditionFalse
		programmedCondition.Reason = computev1alpha.InstanceAvailableReasonStopping
		programmedCondition.Message = "Instance is terminating"

	default:
		// Derive running and programmed conditions from the underlying runtime phase.
		// Programmed=True means the instance has been accepted and is running;
		// it transitions to False only on terminal failures.
		switch instancePod.Status.Phase {
		case core.PodRunning:
			availableCondition.Status = metav1.ConditionTrue
			availableCondition.Reason = computev1alpha.InstanceAvailableReasonAvailable
			availableCondition.Message = "Instance is available"
			programmedCondition.Status = metav1.ConditionTrue
			programmedCondition.Reason = computev1alpha.InstanceProgrammedReasonProgrammed
			programmedCondition.Message = "Instance is available"

			// The instance has been programmed, so record the template hash the
			// provider acted on. Compute counts an instance toward its current
			// replicas only when ObservedTemplateHash matches the desired hash
			// it stamped on Spec.Controller.TemplateHash, so echo that value
			// back to keep rolling-update/template-version tracking accurate.
			if instance.Spec.Controller != nil {
				if instance.Status.Controller == nil {
					instance.Status.Controller = &computev1alpha.InstanceControllerStatus{}
				}
				if instance.Status.Controller.ObservedTemplateHash != instance.Spec.Controller.TemplateHash {
					instance.Status.Controller.ObservedTemplateHash = instance.Spec.Controller.TemplateHash
					statusChanged = true
				}
			}
		case core.PodPending:
			availableCondition.Status = metav1.ConditionUnknown
			availableCondition.Reason = "Provisioning"
			availableCondition.Message = "Instance is provisioning"
			// Translate the first non-empty container waiting reason into
			// Instance-domain language. Log the raw k8s detail for operators.
			for _, cs := range instancePod.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
					logger.Info("container waiting",
						"k8sReason", cs.State.Waiting.Reason,
						"k8sMessage", cs.State.Waiting.Message,
					)
					availableCondition.Reason, availableCondition.Message =
						translateWaitingReason(cs.State.Waiting.Reason, cs.State.Waiting.Message)
					break
				}
			}
			programmedCondition.Status = metav1.ConditionUnknown
			programmedCondition.Reason = computev1alpha.InstanceProgrammedReasonProgrammingInProgress
			programmedCondition.Message = availableCondition.Message
		case core.PodSucceeded:
			availableCondition.Status = metav1.ConditionFalse
			availableCondition.Reason = computev1alpha.InstanceAvailableReasonStopping
			availableCondition.Message = "Instance has stopped"
			programmedCondition.Status = metav1.ConditionFalse
			programmedCondition.Reason = computev1alpha.InstanceAvailableReasonStopping
			programmedCondition.Message = "Instance has stopped"
		case core.PodFailed:
			availableCondition.Status = metav1.ConditionFalse
			availableCondition.Reason = "Failed"
			availableCondition.Message = instancePod.Status.Message
			if availableCondition.Message == "" {
				availableCondition.Message = "Instance failed"
			}
			programmedCondition.Status = metav1.ConditionFalse
			programmedCondition.Reason = "Failed"
			programmedCondition.Message = availableCondition.Message
		default:
			availableCondition.Status = metav1.ConditionUnknown
			availableCondition.Reason = "Unknown"
			availableCondition.Message = "Instance state is unknown"
			programmedCondition.Status = metav1.ConditionUnknown
			programmedCondition.Reason = computev1alpha.InstanceProgrammedReasonProgrammingInProgress
			programmedCondition.Message = "Instance state is unknown"
		}
	}

	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, availableCondition) || statusChanged
	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, programmedCondition) || statusChanged

	var networkIP string
	if len(instancePod.Status.PodIPs) > 0 {
		networkIP = instancePod.Status.PodIPs[0].IP
	}

	serviceFqdns := extractServiceFqdnsFromPod(instance, instancePod)

	desiredInterfaces := buildNetworkInterfaceStatus(networkIP, serviceFqdns)
	if !networkInterfacesEqual(instance.Status.NetworkInterfaces, desiredInterfaces) {
		instance.Status.NetworkInterfaces = desiredInterfaces
		statusChanged = true
	}

	if statusChanged {
		// Use an optimistic-lock merge patch so that a concurrent write by the
		// compute quota controller (updating QuotaGranted/Ready between our Get
		// and Patch) yields a 409 Conflict rather than silently clobbering their
		// update. The caller handles IsConflict by requeueing, which causes a
		// fresh Get before the next patch attempt (BUG-2 fix).
		if err := r.Status().Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}

	return nil
}

func copyUnikraftAnnotations(src, dst map[string]string) {
	for k, v := range src {
		if k == ukcCniEnabledAnnotation {
			// Platform-controlled; set by the caller from provider config, not
			// copied from the tenant-facing Instance.
			continue
		}
		if strings.HasPrefix(k, unikraftAnnotationPrefix) {
			dst[k] = v
		}
	}
}

type podFqdns map[string]struct {
	PrivateFqdn string `json:"privateFqdn"`
	ServiceFqdn string `json:"serviceFqdn"`
}

func extractServiceFqdnsFromPod(instance *computev1alpha.Instance, pod *core.Pod) []string {
	if pod == nil {
		return nil
	}
	raw, ok := pod.Annotations[ukcInstanceFqdnsAnnotation]
	if !ok || raw == "" {
		return nil
	}

	var fqdns podFqdns
	if err := json.Unmarshal([]byte(raw), &fqdns); err != nil {
		return nil
	}

	out := make([]string, 0, len(fqdns))
	seen := make(map[string]struct{}, len(fqdns))

	add := func(name string) {
		if _, dup := seen[name]; dup {
			return
		}
		entry, found := fqdns[name]
		if !found || entry.ServiceFqdn == "" {
			return
		}
		seen[name] = struct{}{}
		out = append(out, entry.ServiceFqdn)
	}

	if instance != nil && instance.Spec.Runtime.Sandbox != nil {
		for _, c := range instance.Spec.Runtime.Sandbox.Containers {
			add(c.Name)
		}
	}

	remaining := make([]string, 0, len(fqdns))
	for name := range fqdns {
		if _, alreadyConsumed := seen[name]; alreadyConsumed {
			continue
		}
		remaining = append(remaining, name)
	}
	sort.Strings(remaining)
	for _, name := range remaining {
		add(name)
	}

	return out
}

func buildNetworkInterfaceStatus(networkIP string, serviceFqdns []string) []computev1alpha.InstanceNetworkInterfaceStatus {
	if networkIP == "" && len(serviceFqdns) == 0 {
		return nil
	}

	var ipPtr *string
	if networkIP != "" {
		ip := networkIP
		ipPtr = &ip
	}

	if len(serviceFqdns) == 0 {
		return []computev1alpha.InstanceNetworkInterfaceStatus{
			{
				Assignments: computev1alpha.InstanceNetworkInterfaceAssignmentsStatus{
					NetworkIP: ipPtr,
				},
			},
		}
	}

	out := make([]computev1alpha.InstanceNetworkInterfaceStatus, 0, len(serviceFqdns))
	for _, fqdn := range serviceFqdns {
		f := fqdn
		out = append(out, computev1alpha.InstanceNetworkInterfaceStatus{
			Assignments: computev1alpha.InstanceNetworkInterfaceAssignmentsStatus{
				NetworkIP:  ipPtr,
				ExternalIP: &f,
			},
		})
	}
	return out
}

func networkInterfacesEqual(a, b []computev1alpha.InstanceNetworkInterfaceStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strPtrEqual(a[i].Assignments.NetworkIP, b[i].Assignments.NetworkIP) {
			return false
		}
		if !strPtrEqual(a[i].Assignments.ExternalIP, b[i].Assignments.ExternalIP) {
			return false
		}
	}
	return true
}

func strPtrEqual(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha.Instance{}).
		Owns(&core.Pod{}).
		Owns(&core.Service{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Named("instance").
		Complete(r)
}
