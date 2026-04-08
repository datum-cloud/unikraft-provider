// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

// AddIndexers adds field indexers for efficient resource queries
// TODO: Add indexers as you implement reconcilers that need them
func AddIndexers(ctx context.Context, mgr mcmanager.Manager) error {
	// No indexers yet - add them when you implement reconcilers that need efficient lookups
	// Example from GCP provider (uncomment when network-services-operator CRDs are installed):
	//
	// return errors.Join(
	// 	addNetworkContextControllerIndexers(ctx, mgr),
	// )

	return nil
}

// Example indexer for NetworkContext (requires network-services-operator CRDs):
//
// const networkContextControllerNetworkUIDIndex = "networkContextControllerNetworkUIDIndex"
//
// func addNetworkContextControllerIndexers(ctx context.Context, mgr mcmanager.Manager) error {
// 	if err := mgr.GetFieldIndexer().IndexField(ctx, &networkingv1alpha.NetworkContext{}, 
// 		networkContextControllerNetworkUIDIndex, networkContextControllerNetworkUIDIndexFunc); err != nil {
// 		return fmt.Errorf("failed to add network context controller indexer %q: %w", 
// 			networkContextControllerNetworkUIDIndex, err)
// 	}
// 	return nil
// }
//
// func networkContextControllerNetworkUIDIndexFunc(o client.Object) []string {
// 	if networkRef := metav1.GetControllerOf(o); networkRef != nil {
// 		return []string{fmt.Sprintf("network-%s", networkRef.UID)}
// 	}
// 	return nil
// }
