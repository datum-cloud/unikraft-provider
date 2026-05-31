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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"

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

	// referencedDataLabel is the label applied by the compute resolver to
	// companion ConfigMaps and Secrets that have been delivered to the cell
	// namespace. The provider uses this label to discover and mirror companions
	// into the downstream kraftlet cluster.
	//
	// Tracks compute's ReferencedDataLabel constant (not yet exported in v0.6.0).
	referencedDataLabel = "compute.datumapis.com/referenced-data"

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

	// Mirror companion ConfigMaps/Secrets from the upstream cell namespace into
	// the downstream Pod namespace before creating the Pod. Kraftlet resolves
	// volume/env references from the cluster where the Pod runs (the downstream
	// cluster), so companions must be present there first.
	//
	// When SameCluster=true (lab/single-cluster), the downstream IS the upstream;
	// companions are already reachable and mirroring would be a no-op — skip it.
	if !r.Config.DownstreamResourceManagement.SameCluster {
		if err := r.mirrorCompanions(ctx, upstreamClient, downstreamClient, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to mirror companion objects for instance %s: %w", instance.Name, err)
		}
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

// mirrorCompanions copies labeled companion ConfigMaps and Secrets from the
// upstream cell namespace into the downstream Pod namespace. The resolver stamps
// companions with the referencedDataLabel; listing by that label finds exactly
// the set this Instance needs without any name recomputation.
//
// If listing upstream companions fails, an error is returned and Pod creation is
// deferred until the next reconcile. Completeness enforcement (blocking Instance
// scheduling until all referenced companions are present) is handled by the
// ReferencedData scheduling gate on the upstream cell side, not here.
//
// After mirroring, any downstream companion that no longer exists upstream is
// pruned — but only if no other surviving (non-deleting) Instance in the
// namespace still references it.
//
// ownerReferences are stripped from mirrored copies: the downstream cluster has
// no knowledge of upstream objects and GC should be driven by the provider's own
// deletion path.
func (r *InstanceReconciler) mirrorCompanions(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamClient client.Client,
	instance *computev1alpha.Instance,
) error {
	logger := log.FromContext(ctx)

	companionSelector := labels.SelectorFromSet(labels.Set{referencedDataLabel: "true"})
	listOpts := &client.ListOptions{
		Namespace:     instance.Namespace,
		LabelSelector: companionSelector,
	}

	// Mirror ConfigMaps and track which names are required upstream.
	var cmList core.ConfigMapList
	if err := upstreamClient.List(ctx, &cmList, listOpts); err != nil {
		return fmt.Errorf("failed to list companion ConfigMaps in namespace %s: %w", instance.Namespace, err)
	}
	requiredCMs := make(map[string]struct{}, len(cmList.Items))
	for i := range cmList.Items {
		src := &cmList.Items[i]
		requiredCMs[src.Name] = struct{}{}
		mirror := &core.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      src.Name,
				Namespace: instance.Namespace,
			},
		}
		_, err := controllerutil.CreateOrPatch(ctx, downstreamClient, mirror, func() error {
			if mirror.Labels == nil {
				mirror.Labels = map[string]string{}
			}
			mirror.Labels[referencedDataLabel] = "true"
			mirror.Data = src.Data
			mirror.BinaryData = src.BinaryData
			// Strip ownerReferences: upstream owners don't exist in the downstream cluster.
			mirror.OwnerReferences = nil
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to mirror companion ConfigMap %s/%s: %w", instance.Namespace, src.Name, err)
		}
		logger.V(1).Info("mirrored companion ConfigMap",
			"name", src.Name,
			"namespace", instance.Namespace,
		)
	}

	// Mirror Secrets and track which names are required upstream.
	var secretList core.SecretList
	if err := upstreamClient.List(ctx, &secretList, listOpts); err != nil {
		return fmt.Errorf("failed to list companion Secrets in namespace %s: %w", instance.Namespace, err)
	}
	requiredSecrets := make(map[string]struct{}, len(secretList.Items))
	for i := range secretList.Items {
		src := &secretList.Items[i]
		requiredSecrets[src.Name] = struct{}{}
		mirror := &core.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      src.Name,
				Namespace: instance.Namespace,
			},
		}
		_, err := controllerutil.CreateOrPatch(ctx, downstreamClient, mirror, func() error {
			if mirror.Labels == nil {
				mirror.Labels = map[string]string{}
			}
			mirror.Labels[referencedDataLabel] = "true"
			mirror.Data = src.Data
			mirror.Type = src.Type
			// Strip ownerReferences: upstream owners don't exist in the downstream cluster.
			mirror.OwnerReferences = nil
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to mirror companion Secret %s/%s: %w", instance.Namespace, src.Name, err)
		}
		logger.V(1).Info("mirrored companion Secret",
			"name", src.Name,
			"namespace", instance.Namespace,
		)
	}

	// Prune downstream mirrors that no longer exist upstream, but only when no
	// other surviving (non-deleting) Instance in the namespace still references
	// them. This ensures that companions shared across multiple Instances are
	// not removed while siblings still need them.
	if err := r.pruneOrphanMirrors(ctx, upstreamClient, downstreamClient, instance, requiredCMs, requiredSecrets); err != nil {
		return fmt.Errorf("failed to prune orphan mirrors for instance %s: %w", instance.Name, err)
	}

	return nil
}

// pruneOrphanMirrors deletes downstream mirrored companions that are no longer
// present upstream (i.e. their names are not in requiredCMs / requiredSecrets),
// provided no other surviving Instance in the namespace still references them.
func (r *InstanceReconciler) pruneOrphanMirrors(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamClient client.Client,
	instance *computev1alpha.Instance,
	requiredCMs map[string]struct{},
	requiredSecrets map[string]struct{},
) error {
	logger := log.FromContext(ctx)

	// Build the union of companion names referenced by all surviving
	// (non-deleting) Instances in the same namespace, excluding the current
	// instance (whose required set is already reflected in requiredCMs /
	// requiredSecrets passed in). This prevents deleting a companion that a
	// sibling Instance still needs.
	survivingCMs, survivingSecrets, err := r.companionsReferencedBySurvivingInstances(ctx, upstreamClient, instance)
	if err != nil {
		return err
	}

	companionSelector := labels.SelectorFromSet(labels.Set{referencedDataLabel: "true"})

	// Prune orphan ConfigMaps.
	var downstreamCMs core.ConfigMapList
	if err := downstreamClient.List(ctx, &downstreamCMs, &client.ListOptions{
		Namespace:     instance.Namespace,
		LabelSelector: companionSelector,
	}); err != nil {
		return fmt.Errorf("failed to list downstream ConfigMaps for pruning: %w", err)
	}
	for i := range downstreamCMs.Items {
		cm := &downstreamCMs.Items[i]
		if _, required := requiredCMs[cm.Name]; required {
			continue // still needed by this instance
		}
		if _, sibling := survivingCMs[cm.Name]; sibling {
			continue // still needed by a sibling instance
		}
		if err := downstreamClient.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to prune orphan companion ConfigMap %s/%s: %w", cm.Namespace, cm.Name, err)
		}
		logger.V(1).Info("pruned orphan companion ConfigMap",
			"name", cm.Name,
			"namespace", cm.Namespace,
		)
	}

	// Prune orphan Secrets.
	var downstreamSecrets core.SecretList
	if err := downstreamClient.List(ctx, &downstreamSecrets, &client.ListOptions{
		Namespace:     instance.Namespace,
		LabelSelector: companionSelector,
	}); err != nil {
		return fmt.Errorf("failed to list downstream Secrets for pruning: %w", err)
	}
	for i := range downstreamSecrets.Items {
		s := &downstreamSecrets.Items[i]
		if _, required := requiredSecrets[s.Name]; required {
			continue // still needed by this instance
		}
		if _, sibling := survivingSecrets[s.Name]; sibling {
			continue // still needed by a sibling instance
		}
		if err := downstreamClient.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to prune orphan companion Secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		logger.V(1).Info("pruned orphan companion Secret",
			"name", s.Name,
			"namespace", s.Namespace,
		)
	}

	return nil
}

// deleteDownstreamCompanions removes mirrored companion ConfigMaps and Secrets
// from the downstream namespace during Instance deletion. Only companions that
// are no longer referenced by any other surviving (non-deleting) Instance in
// the namespace are deleted — companions shared with siblings are preserved.
func (r *InstanceReconciler) deleteDownstreamCompanions(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamClient client.Client,
	instance *computev1alpha.Instance,
) error {
	logger := log.FromContext(ctx)

	// Compute the union of companion names still referenced by surviving sibling
	// Instances. The current instance is already deleting (DeletionTimestamp is
	// set), so it is excluded from the sibling list; its companions are candidates
	// for deletion unless a sibling also references them.
	survivingCMs, survivingSecrets, err := r.companionsReferencedBySurvivingInstances(ctx, upstreamClient, instance)
	if err != nil {
		return err
	}

	companionSelector := labels.SelectorFromSet(labels.Set{referencedDataLabel: "true"})
	listOpts := &client.ListOptions{
		Namespace:     instance.Namespace,
		LabelSelector: companionSelector,
	}

	var cmList core.ConfigMapList
	if err := downstreamClient.List(ctx, &cmList, listOpts); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to list downstream companion ConfigMaps: %w", err)
	}
	for i := range cmList.Items {
		cm := &cmList.Items[i]
		if _, sibling := survivingCMs[cm.Name]; sibling {
			logger.V(1).Info("skipping companion ConfigMap deletion — still referenced by a sibling instance",
				"name", cm.Name, "namespace", cm.Namespace)
			continue
		}
		if err := downstreamClient.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete downstream companion ConfigMap %s/%s: %w", cm.Namespace, cm.Name, err)
		}
		logger.V(1).Info("deleted downstream companion ConfigMap", "name", cm.Name, "namespace", cm.Namespace)
	}

	var secretList core.SecretList
	if err := downstreamClient.List(ctx, &secretList, listOpts); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to list downstream companion Secrets: %w", err)
	}
	for i := range secretList.Items {
		s := &secretList.Items[i]
		if _, sibling := survivingSecrets[s.Name]; sibling {
			logger.V(1).Info("skipping companion Secret deletion — still referenced by a sibling instance",
				"name", s.Name, "namespace", s.Namespace)
			continue
		}
		if err := downstreamClient.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete downstream companion Secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		logger.V(1).Info("deleted downstream companion Secret", "name", s.Name, "namespace", s.Namespace)
	}

	return nil
}

// companionsReferencedBySurvivingInstances returns the namespace-wide union of
// companion ConfigMap names and Secret names that are referenced (labeled with
// referencedDataLabel) upstream by any surviving (non-deleting) Instance in the
// same namespace, excluding the provided instance itself.
//
// This is a namespace-union computation, not per-instance accounting. The
// function lists all labeled companions found upstream for each surviving sibling
// and merges them into a single set. It is intentionally conservative: it favors
// a companion leak (leaving an object behind) over an over-deletion (removing an
// object that a sibling still needs). This invariant makes both the pruning path
// (mirrorCompanions) and the deletion path (handleDeletion) safe to call
// independently — neither can remove a companion while any sibling references it.
//
// "Surviving" means the Instance does not have a DeletionTimestamp set.
func (r *InstanceReconciler) companionsReferencedBySurvivingInstances(
	ctx context.Context,
	upstreamClient client.Client,
	instance *computev1alpha.Instance,
) (cms map[string]struct{}, secrets map[string]struct{}, err error) {
	var siblingList computev1alpha.InstanceList
	if err := upstreamClient.List(ctx, &siblingList, client.InNamespace(instance.Namespace)); err != nil {
		return nil, nil, fmt.Errorf("failed to list sibling instances in namespace %s: %w", instance.Namespace, err)
	}

	companionSelector := labels.SelectorFromSet(labels.Set{referencedDataLabel: "true"})

	cms = make(map[string]struct{})
	secrets = make(map[string]struct{})

	for i := range siblingList.Items {
		sibling := &siblingList.Items[i]
		// Skip the deleting instance itself and any other instances that are
		// also being deleted.
		if sibling.Name == instance.Name {
			continue
		}
		if !sibling.DeletionTimestamp.IsZero() {
			continue
		}

		// List the companions that exist upstream in the sibling's namespace and
		// add them to the surviving sets.
		var sibCMs core.ConfigMapList
		if err := upstreamClient.List(ctx, &sibCMs, &client.ListOptions{
			Namespace:     sibling.Namespace,
			LabelSelector: companionSelector,
		}); err != nil {
			return nil, nil, fmt.Errorf("failed to list upstream ConfigMaps for sibling %s: %w", sibling.Name, err)
		}
		for j := range sibCMs.Items {
			cms[sibCMs.Items[j].Name] = struct{}{}
		}

		var sibSecrets core.SecretList
		if err := upstreamClient.List(ctx, &sibSecrets, &client.ListOptions{
			Namespace:     sibling.Namespace,
			LabelSelector: companionSelector,
		}); err != nil {
			return nil, nil, fmt.Errorf("failed to list upstream Secrets for sibling %s: %w", sibling.Name, err)
		}
		for j := range sibSecrets.Items {
			secrets[sibSecrets.Items[j].Name] = struct{}{}
		}
	}

	return cms, secrets, nil
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

	// Delete mirrored companion ConfigMaps/Secrets from the downstream namespace.
	// Only needed when companions were actually mirrored (SameCluster=false).
	if !r.Config.DownstreamResourceManagement.SameCluster {
		if err := r.deleteDownstreamCompanions(ctx, upstreamClient, downstreamClient, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete downstream companions for instance %s: %w", instance.Name, err)
		}
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
		// Watch upstream (cell) companion ConfigMaps labeled with referencedDataLabel
		// and enqueue all Instances in the same namespace. Companions live in the
		// upstream cell clusters (same clusters as Instances), so this watch engages
		// with all provider clusters via the standard mcbuilder.Watches path.
		// When a companion is rotated or newly delivered, mirroring is re-triggered.
		//
		// We use the raw TypedEventHandlerFunc form (func(clusterName, cl) → handler)
		// rather than mchandler.TypedEnqueueRequestsFromMapFunc so that we capture
		// the cluster name from the framework-provided closure parameter. The map-func
		// ctx passed to TypedEnqueueRequestsFromMapFunc does NOT carry the cluster
		// (ClusterFrom(ctx) returns ""), so GetCluster("") would error and the watch
		// would silently enqueue nothing.
		Watches(&core.ConfigMap{}, mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request](
			func(clusterName string, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
				return handler.TypedEnqueueRequestsFromMapFunc(
					func(ctx context.Context, cm client.Object) []mcreconcile.Request {
						if cm.GetLabels()[referencedDataLabel] != "true" {
							return nil
						}
						return r.instancesInNamespace(ctx, clusterName, cm.GetNamespace())
					})
			}),
		).
		// Watch upstream (cell) companion Secrets labeled with referencedDataLabel.
		Watches(&core.Secret{}, mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request](
			func(clusterName string, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
				return handler.TypedEnqueueRequestsFromMapFunc(
					func(ctx context.Context, s client.Object) []mcreconcile.Request {
						if s.GetLabels()[referencedDataLabel] != "true" {
							return nil
						}
						return r.instancesInNamespace(ctx, clusterName, s.GetNamespace())
					})
			}),
		).
		Named("instance").
		Complete(r)
}

// instancesInNamespace lists all Instances in the given namespace on the named
// cluster and returns them as reconcile requests. Used by companion watches to
// re-trigger any Instance that may need a fresh mirror pass.
//
// Cluster resolution is done via r.mgr.GetCluster to obtain the upstream client.
// The listing and request-mapping logic is delegated to listInstanceRequests so
// it can be unit-tested independently of the manager.
func (r *InstanceReconciler) instancesInNamespace(ctx context.Context, clusterName, namespace string) []mcreconcile.Request {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, clusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster for companion watch", "cluster", clusterName)
		return nil
	}

	reqs, err := listInstanceRequests(ctx, cl.GetClient(), clusterName, namespace)
	if err != nil {
		logger.Error(err, "failed to list instances for companion watch", "namespace", namespace)
		return nil
	}
	return reqs
}

// listInstanceRequests lists all Instances in the given namespace using the
// provided client and maps each one to an mcreconcile.Request carrying the
// given clusterName. It is a pure function with no manager dependency so it
// can be unit-tested directly.
func listInstanceRequests(ctx context.Context, cl client.Client, clusterName, namespace string) ([]mcreconcile.Request, error) {
	var instanceList computev1alpha.InstanceList
	if err := cl.List(ctx, &instanceList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list instances in namespace %s: %w", namespace, err)
	}

	reqs := make([]mcreconcile.Request, 0, len(instanceList.Items))
	for _, inst := range instanceList.Items {
		reqs = append(reqs, mcreconcile.Request{
			Request: reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: inst.Namespace,
					Name:      inst.Name,
				},
			},
			ClusterName: clusterName,
		})
	}
	return reqs, nil
}
