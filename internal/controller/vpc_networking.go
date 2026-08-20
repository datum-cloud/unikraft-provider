// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// vpcNetworkingEnabled reports whether this cell attaches instances to tenant networks.
func (r *InstanceReconciler) vpcNetworkingEnabled() bool {
	return r.Config != nil && r.Config.DownstreamResourceManagement.EnableVPCNetworking
}

// instanceNetworkAnnotations collects the annotations the networking stack
// published for this instance's interfaces, to be carried by the Pod.
//
// The provider follows the instance to its bound NetworkInterfaces and copies
// what they publish. The annotations are opaque: which key delivers the
// interface, and what the value means, belongs to whoever programmed the data
// plane. Nothing here inspects or filters them.
//
// It reports ready=false while any interface is unbound or has published
// nothing yet. The Pod must not be created before then: the annotations are
// resolved at sandbox creation, and this controller builds a Pod spec only on
// creation and never reconciles an existing Pod's spec.
func (r *InstanceReconciler) instanceNetworkAnnotations(
	ctx context.Context,
	instance *computev1alpha.Instance,
) (annotations map[string]string, ready bool, err error) {
	if !r.vpcNetworkingEnabled() {
		return nil, true, nil
	}

	logger := log.FromContext(ctx)

	// Compute publishes one status entry per requested interface, so a short
	// status means the interfaces are still being bound.
	if len(instance.Status.NetworkInterfaces) < len(instance.Spec.NetworkInterfaces) {
		logger.Info("waiting for instance network interfaces to be published", "name", instance.Name)
		return nil, false, nil
	}

	annotations = map[string]string{}

	for _, interfaceStatus := range instance.Status.NetworkInterfaces {
		if interfaceStatus.NetworkInterfaceRef == nil || interfaceStatus.NetworkInterfaceRef.Name == "" {
			logger.Info("network interface not bound yet", "interface", interfaceStatus.Name)
			return nil, false, nil
		}

		var networkInterface networkingv1alpha.NetworkInterface
		key := client.ObjectKey{Namespace: instance.Namespace, Name: interfaceStatus.NetworkInterfaceRef.Name}
		switch err := r.Get(ctx, key, &networkInterface); {
		case apierrors.IsNotFound(err):
			logger.Info("bound network interface not found yet", "name", key.Name)
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("failed to get network interface %s: %w", key.Name, err)
		}

		if len(networkInterface.Status.ConsumerAnnotations) == 0 {
			logger.Info("network interface has published no consumer annotations yet", "name", key.Name)
			return nil, false, nil
		}

		for annotation, value := range networkInterface.Status.ConsumerAnnotations {
			annotations[annotation] = value
		}
	}

	return annotations, true, nil
}
