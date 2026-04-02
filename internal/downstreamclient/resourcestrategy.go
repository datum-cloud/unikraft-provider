// SPDX-License-Identifier: AGPL-3.0-only

package downstreamclient

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ResourceStrategy defines how resources are managed in the downstream cluster.
// This interface allows for different strategies like:
// - Same namespace in the same cluster
// - Mapped namespaces in the downstream cluster
// - Single target namespace for all resources
type ResourceStrategy interface {
	// Get retrieves a resource from the downstream cluster using the strategy's mapping logic.
	Get(ctx context.Context, key client.ObjectKey, obj client.Object) error

	// Create creates a resource in the downstream cluster using the strategy's mapping logic.
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error

	// Update updates a resource in the downstream cluster using the strategy's mapping logic.
	Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error

	// Delete deletes a resource from the downstream cluster using the strategy's mapping logic.
	Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error

	// Patch patches a resource in the downstream cluster using the strategy's mapping logic.
	Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error

	// List lists resources from the downstream cluster using the strategy's mapping logic.
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}
