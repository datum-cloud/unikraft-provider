// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	netattachv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	computev1alpha "go.datum.net/compute/api/v1alpha"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	// defaultInterfaceName mirrors the compute API default for an interface that
	// does not name itself.
	defaultInterfaceName = "eth0"

	// maxObjectNameLength is the DNS subdomain limit a derived name must respect.
	maxObjectNameLength = 253

	// claimNameHashLen is the digest length appended by the truncation fallback.
	claimNameHashLen = 12
)

// instanceInterfaceName is the device name an interface presents to the guest.
func instanceInterfaceName(networkInterface computev1alpha.InstanceNetworkInterface) string {
	if networkInterface.Name == "" {
		return defaultInterfaceName
	}
	return networkInterface.Name
}

// networkInterfaceClaimName derives the claim name compute gives an instance's
// interface. It must match compute's own derivation exactly, digest fallback
// included, or the provider looks up a claim that does not exist.
func networkInterfaceClaimName(instanceName, interfaceName string) string {
	candidate := instanceName + "-" + interfaceName
	if len(candidate) <= maxObjectNameLength && len(validation.IsDNS1123Subdomain(candidate)) == 0 {
		return candidate
	}

	sum := sha256.Sum256([]byte(instanceName + "\x00" + interfaceName))
	suffix := hex.EncodeToString(sum[:])[:claimNameHashLen]

	prefix := instanceName
	if limit := maxObjectNameLength - 1 - claimNameHashLen; len(prefix) > limit {
		prefix = prefix[:limit]
	}
	return strings.TrimRight(prefix, "-.") + "-" + suffix
}

// vpcNetworkingEnabled reports whether this cell attaches instances to tenant VPCs.
func (r *InstanceReconciler) vpcNetworkingEnabled() bool {
	return r.Config != nil && r.Config.DownstreamResourceManagement.EnableVPCNetworking
}

// multusAnnotationKey is the annotation form used to attach the Pod to its
// NetworkAttachmentDefinitions.
func (r *InstanceReconciler) multusAnnotationKey() string {
	if r.Config != nil && r.Config.DownstreamResourceManagement.MultusNetworkAnnotation != "" {
		return r.Config.DownstreamResourceManagement.MultusNetworkAnnotation
	}
	return multusNetworksAnnotation
}

// reconcileVPCNetworking puts the instance on the tenant VPCs its interfaces
// belong to: one VPCAttachment per interface, and the list of
// NetworkAttachmentDefinitions the Pod must reference.
//
// It reports ready=false while any piece is still missing. The NAD is created
// by the VPC controller, not here, and it must exist and be complete before the
// sandbox: Multus resolves the annotation at sandbox creation, this controller
// builds a Pod spec only on creation, and galactic fails CNI ADD outright on a
// missing or incomplete NAD.
func (r *InstanceReconciler) reconcileVPCNetworking(
	ctx context.Context,
	instance *computev1alpha.Instance,
) (networks []string, ready bool, err error) {
	if !r.vpcNetworkingEnabled() {
		return nil, true, nil
	}

	logger := log.FromContext(ctx)
	ready = true

	for _, specInterface := range instance.Spec.NetworkInterfaces {
		interfaceName := instanceInterfaceName(specInterface)

		networkInterface, found, err := r.resolveNetworkInterface(ctx, instance, interfaceName)
		if err != nil {
			return nil, false, err
		}
		if !found {
			logger.Info("network interface not bound yet", "interface", interfaceName)
			ready = false
			continue
		}

		// The VPC is named after the network's presence in this location, which
		// NSO records on the interface while fulfilling the claim.
		if networkInterface.Status.NetworkContextRef == nil || networkInterface.Status.NetworkContextRef.Name == "" {
			logger.Info("network context not resolved yet", "interface", interfaceName)
			ready = false
			continue
		}

		attachmentName, err := r.reconcileVPCAttachment(ctx, instance, networkInterface)
		if err != nil {
			return nil, false, err
		}

		// The NAD renders from the VPCAttachment and is named after it.
		var nad netattachv1.NetworkAttachmentDefinition
		key := client.ObjectKey{Namespace: instance.Namespace, Name: attachmentName}
		switch err := r.Get(ctx, key, &nad); {
		case apierrors.IsNotFound(err):
			logger.Info("network attachment definition not created yet", "name", attachmentName)
			ready = false
			continue
		case err != nil:
			return nil, false, fmt.Errorf("failed to get network attachment definition %s: %w", attachmentName, err)
		}

		networks = append(networks, fmt.Sprintf("%s/%s", nad.Namespace, nad.Name))
	}

	if !ready {
		return nil, false, nil
	}
	return networks, true, nil
}

// resolveNetworkInterface follows the instance's interface to the
// NetworkInterface NSO bound to it, which carries the addresses, gateway and MTU.
func (r *InstanceReconciler) resolveNetworkInterface(
	ctx context.Context,
	instance *computev1alpha.Instance,
	interfaceName string,
) (*networkingv1alpha.NetworkInterface, bool, error) {
	claimName := networkInterfaceClaimName(instance.Name, interfaceName)

	var claim networkingv1alpha.NetworkInterfaceClaim
	key := client.ObjectKey{Namespace: instance.Namespace, Name: claimName}
	switch err := r.Get(ctx, key, &claim); {
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("failed to get network interface claim %s: %w", claimName, err)
	}

	if claim.Status.NetworkInterfaceRef == nil {
		return nil, false, nil
	}

	var networkInterface networkingv1alpha.NetworkInterface
	key = client.ObjectKey{Namespace: instance.Namespace, Name: claim.Status.NetworkInterfaceRef.Name}
	switch err := r.Get(ctx, key, &networkInterface); {
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("failed to get network interface %s: %w", key.Name, err)
	}

	if len(networkInterface.Spec.Addresses) == 0 {
		return nil, false, nil
	}

	return &networkInterface, true, nil
}

// reconcileVPCAttachment declares the instance's presence on the VPC for one
// interface, carrying the addresses NSO already allocated.
//
// The attachment is owned by the Instance rather than the NetworkInterface:
// under reclaimPolicy Retain the interface deliberately outlives the instance,
// and the attachment must not.
func (r *InstanceReconciler) reconcileVPCAttachment(
	ctx context.Context,
	instance *computev1alpha.Instance,
	networkInterface *networkingv1alpha.NetworkInterface,
) (string, error) {
	logger := log.FromContext(ctx)

	addresses := make([]cloudv1alpha1.IPAddress, 0, len(networkInterface.Spec.Addresses))
	for _, address := range networkInterface.Spec.Addresses {
		addresses = append(addresses, cloudv1alpha1.IPAddress(address.Address))
	}

	interfaceName := networkInterface.Spec.InterfaceName
	if interfaceName == "" {
		interfaceName = defaultInterfaceName
	}

	attachment := &cloudv1alpha1.VPCAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vpcAttachmentName(networkInterface),
			Namespace: instance.Namespace,
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, r.Client, attachment, func() error {
		attachment.Spec.VPC = cloudv1alpha1.VPCRef{Name: networkInterface.Status.NetworkContextRef.Name}

		// The controller dereferences this for the addresses, gateway and MTU, and
		// treats a UID mismatch as stale rather than binding a recreated interface.
		attachment.Spec.InterfaceRef = &cloudv1alpha1.NetworkInterfaceRef{
			Name: networkInterface.Name,
			UID:  string(networkInterface.UID),
		}

		attachment.Spec.Interface = cloudv1alpha1.VPCAttachmentInterface{
			Name: interfaceName,
			// A Unikraft guest is a microVM: the interface goes to the VMM as a
			// device rather than into the pod's network namespace.
			Mode:      cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor,
			Addresses: addresses,
		}

		return controllerutil.SetControllerReference(instance, attachment, r.Scheme)
	})
	if err != nil {
		return "", fmt.Errorf("failed to create/update vpc attachment %s: %w", attachment.Name, err)
	}

	logger.Info("reconciled vpc attachment",
		"result", result,
		"name", attachment.Name,
		"addresses", len(addresses),
	)
	return attachment.Name, nil
}

// vpcAttachmentName names the attachment, and so the NAD rendered from it,
// after the NetworkInterface it realizes.
func vpcAttachmentName(networkInterface *networkingv1alpha.NetworkInterface) string {
	return networkInterface.Name
}

// deleteVPCAttachments tears down the attachments backing a deleting instance.
//
// Deleted explicitly rather than left to owner-reference garbage collection,
// which would not run until the Instance leaves etcd — and the provider
// finalizer holds it there.
func (r *InstanceReconciler) deleteVPCAttachments(ctx context.Context, instance *computev1alpha.Instance) error {
	if !r.vpcNetworkingEnabled() {
		return nil
	}

	for _, specInterface := range instance.Spec.NetworkInterfaces {
		interfaceName := instanceInterfaceName(specInterface)

		// The attachment is named after the NetworkInterface, so resolve it the
		// same way it was created. If the claim is already gone the owner
		// reference collects the attachment once the Instance leaves etcd.
		networkInterface, found, err := r.resolveNetworkInterface(ctx, instance, interfaceName)
		if err != nil {
			return err
		}
		if !found {
			continue
		}

		attachment := &cloudv1alpha1.VPCAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: vpcAttachmentName(networkInterface), Namespace: instance.Namespace},
		}
		if err := r.Delete(ctx, attachment); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete vpc attachment %s: %w", attachment.Name, err)
		}
	}

	return nil
}
