package locationutil

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// GetLocation returns the location for the provided location reference, and
// whether or not the resource associated with the location should be processed.
func GetLocation(
	ctx context.Context,
	c client.Client,
	locationRef networkingv1alpha.LocationReference,
	locationClassName string,
) (*networkingv1alpha.Location, bool, error) {
	var location networkingv1alpha.Location
	locationObjectKey := client.ObjectKey{
		Namespace: locationRef.Namespace,
		Name:      locationRef.Name,
	}
	if err := c.Get(ctx, locationObjectKey, &location); err != nil {
		return nil, false, fmt.Errorf("failed fetching location: %w", err)
	}

	if location.Spec.Provider.GCP == nil {
		return &location, false, nil
	}

	if len(locationClassName) == 0 {
		return &location, true, nil
	}

	return &location, location.Spec.LocationClassName == locationClassName, nil
}
