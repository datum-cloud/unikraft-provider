// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// injectInterfacesAnnotation asks the networking stack to wire this Pod up to
// the interfaces its Instance requested. How that happens, and what it is
// delivered with, belongs to whoever serves the webhook that matches on it.
const injectInterfacesAnnotation = "networking.datumapis.com/inject-interfaces"

// requestsInterfaceInjection reports whether an Instance Pod should carry the
// opt-in annotation.
//
// Only stamped when an interface is genuinely wanted. The webhook's
// objectSelector matches on exactly this annotation, and that narrowness is what
// makes its failurePolicy Fail safe: an outage blocks the Pods that need an
// interface rather than every Pod in the cell.
func (r *InstanceReconciler) requestsInterfaceInjection(instance *computev1alpha.Instance) bool {
	if r.Config == nil || !r.Config.DownstreamResourceManagement.EnableVPCNetworking {
		return false
	}
	return len(instance.Spec.NetworkInterfaces) > 0
}
