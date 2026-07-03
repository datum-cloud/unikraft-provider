// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
)

// AddIndexers adds field indexers for efficient resource queries
// TODO: Add indexers as you implement reconcilers that need them
func AddIndexers(ctx context.Context, mgr ctrl.Manager) error {
	// No indexers yet - add them when you implement reconcilers that need efficient lookups
	// Example:
	//
	// return errors.Join(
	// 	addNetworkContextControllerIndexers(ctx, mgr),
	// )

	return nil
}
