// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/unikraft-provider/internal/config"
)

const (
	unikraftFinalizer = "unikraft.datumapis.com/finalizer"

	defaultInstanceMemoryMB = 1024

	unikraftAnnotationPrefix   = "cloud.unikraft.v1"
	ukcInstanceFqdnsAnnotation = "cloud.unikraft.v1.instances/fqdns"
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

	// Honor scheduling gates. While any gate remains, the instance is not yet
	// ready to be scheduled. Return without requeue — the Instance spec update
	// when a gate is cleared will re-trigger reconciliation.
	if instance.Spec.Controller != nil && len(instance.Spec.Controller.SchedulingGates) > 0 {
		logger.Info("instance has scheduling gates, deferring pod creation",
			"gates", instance.Spec.Controller.SchedulingGates,
		)
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
			Name:      instance.Name,
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

		// Mirror any cloud.unikraft.v1.* annotations from the upstream
		// Instance onto the downstream Pod.
		copyUnikraftAnnotations(instance.Annotations, instancePod.Annotations)

		if instancePod.CreationTimestamp.IsZero() {
			logger.Info("building pod spec for new instance pod", "name", instancePod.Name)
			podSpec, err := r.buildPodSpecFromContainers(ctx, instance, instance.Spec.Runtime.Sandbox.Containers)
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

		// Map memory limit
		memoryMB := mapContainerMemory(sc)
		resources := core.ResourceRequirements{
			Limits: core.ResourceList{
				core.ResourceMemory: *resource.NewQuantity(memoryMB*1024*1024, resource.BinarySI),
			},
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
			Env:          envVars,
			Ports:        ports,
			Resources:    resources,
			VolumeMounts: volumeMounts,
		})
	}

	// Apply node selector: use the operator-supplied override when set, otherwise
	// default to the standard single-node kraftlet label. The default routes all
	// Instance Pods to the node named "kraftlet", which is the convention for a
	// single-node kraftlet deployment. Override via
	// DownstreamResourceManagementConfig.NodeSelector for multi-node or relabelled
	// environments (e.g. Layer-2 e2e where the node has a different hostname label).
	nodeSelector := map[string]string{
		"kubernetes.io/hostname": "kraftlet",
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
		Containers:    containers,
		Volumes:       volumes,
		RestartPolicy: core.RestartPolicyAlways,
		NodeSelector:  nodeSelector,
		Tolerations:   tolerations,
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

		// Mirror any cloud.unikraft.v1.* annotations from the upstream
		// Instance onto the downstream Pod.
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
	logger := log.FromContext(ctx)

	runningCondition := metav1.Condition{
		Type:               computev1alpha.InstanceRunning,
		ObservedGeneration: instance.Generation,
		Reason:             computev1alpha.InstanceRunningReasonRunning,
		Status:             metav1.ConditionTrue,
	}

	// programmedCondition reflects whether the infrastructure provider has
	// successfully provisioned the underlying compute resource. The compute
	// InstanceReconciler derives Ready from Programmed=True + Running=True, so
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
		runningCondition.Status = metav1.ConditionFalse
		runningCondition.Reason = computev1alpha.InstanceRunningReasonStopping
		runningCondition.Message = "Instance is terminating"
		programmedCondition.Status = metav1.ConditionFalse
		programmedCondition.Reason = computev1alpha.InstanceRunningReasonStopping
		programmedCondition.Message = "Instance is terminating"

	default:
		// Derive running and programmed conditions from the underlying runtime phase.
		// Programmed=True means the instance has been accepted and is running;
		// it transitions to False only on terminal failures.
		switch instancePod.Status.Phase {
		case core.PodRunning:
			runningCondition.Status = metav1.ConditionTrue
			runningCondition.Reason = computev1alpha.InstanceRunningReasonRunning
			runningCondition.Message = "Instance is running"
			programmedCondition.Status = metav1.ConditionTrue
			programmedCondition.Reason = computev1alpha.InstanceProgrammedReasonProgrammed
			programmedCondition.Message = "Instance is running"

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
			runningCondition.Status = metav1.ConditionUnknown
			runningCondition.Reason = "Provisioning"
			runningCondition.Message = "Instance is provisioning"
			// Translate the first non-empty container waiting reason into
			// Instance-domain language. Log the raw k8s detail for operators.
			for _, cs := range instancePod.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
					logger.Info("container waiting",
						"k8sReason", cs.State.Waiting.Reason,
						"k8sMessage", cs.State.Waiting.Message,
					)
					runningCondition.Reason, runningCondition.Message =
						translateWaitingReason(cs.State.Waiting.Reason, cs.State.Waiting.Message)
					break
				}
			}
			programmedCondition.Status = metav1.ConditionUnknown
			programmedCondition.Reason = computev1alpha.InstanceProgrammedReasonProgrammingInProgress
			programmedCondition.Message = runningCondition.Message
		case core.PodSucceeded:
			runningCondition.Status = metav1.ConditionFalse
			runningCondition.Reason = computev1alpha.InstanceRunningReasonStopping
			runningCondition.Message = "Instance has stopped"
			programmedCondition.Status = metav1.ConditionFalse
			programmedCondition.Reason = computev1alpha.InstanceRunningReasonStopping
			programmedCondition.Message = "Instance has stopped"
		case core.PodFailed:
			runningCondition.Status = metav1.ConditionFalse
			runningCondition.Reason = "Failed"
			runningCondition.Message = instancePod.Status.Message
			if runningCondition.Message == "" {
				runningCondition.Message = "Instance failed"
			}
			programmedCondition.Status = metav1.ConditionFalse
			programmedCondition.Reason = "Failed"
			programmedCondition.Message = runningCondition.Message
		default:
			runningCondition.Status = metav1.ConditionUnknown
			runningCondition.Reason = "Unknown"
			runningCondition.Message = "Instance state is unknown"
			programmedCondition.Status = metav1.ConditionUnknown
			programmedCondition.Reason = computev1alpha.InstanceProgrammedReasonProgrammingInProgress
			programmedCondition.Message = "Instance state is unknown"
		}
	}

	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, runningCondition) || statusChanged
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
					Name:      instance.Name,
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

func copyUnikraftAnnotations(src, dst map[string]string) {
	for k, v := range src {
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
func (r *InstanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.Instance{}).
		// Watch downstream Pods and map back to Instances via upstream ownership annotations.
		// Pods live in the downstream kraftlet cluster, so we pin this watch to it.
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
