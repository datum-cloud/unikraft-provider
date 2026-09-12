// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// injectInterfacesLabel asks the networking stack to wire this Pod up to the
// interfaces its Instance requested. How that happens, and what it is delivered
// with, belongs to whoever serves the webhook that matches on it. A label, not
// an annotation, because a webhook objectSelector can only select on labels.
const injectInterfacesLabel = "networking.datumapis.com/inject-interfaces"

// requestsInterfaceInjection reports whether an Instance Pod should carry the
// opt-in label.
//
// Only stamped when an interface is genuinely wanted. The webhook's
// objectSelector matches on exactly this label, and that narrowness is what
// makes its failurePolicy Fail safe: an outage blocks the Pods that need an
// interface rather than every Pod in the cell.
func (r *InstanceReconciler) requestsInterfaceInjection(instance *computev1alpha.Instance) bool {
	return r.vpcNetworkingEnabled() && len(instance.Spec.NetworkInterfaces) > 0
}

// vpcNetworkingEnabled reports whether this cell wires Instances onto tenant
// networks.
func (r *InstanceReconciler) vpcNetworkingEnabled() bool {
	return r.Config != nil && r.Config.DownstreamResourceManagement.EnableVPCNetworking
}

// providerOwnsInterfaceStatus reports whether the Pod's addresses are the
// Instance's addresses, and therefore whether this provider may publish them.
//
// Compute owns the field wherever the platform allocates the addresses. It
// publishes the tenant network address and the NetworkInterface reference that
// interface injection reads to admit the next Instance Pod. Overwriting that
// entry with the Pod's cluster addresses drops the reference, and the Instance
// never starts.
//
// The published reference is the second test because a cell can run the
// platform's address allocation before this provider opts in. Deferring on that
// evidence keeps the provider out of the field during a rollout.
func (r *InstanceReconciler) providerOwnsInterfaceStatus(instance *computev1alpha.Instance) bool {
	if r.vpcNetworkingEnabled() {
		return false
	}

	for _, networkInterface := range instance.Status.NetworkInterfaces {
		if networkInterface.NetworkInterfaceRef != nil {
			return false
		}
	}

	return true
}
