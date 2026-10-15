// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	"go.datum.net/unikraft-provider/internal/config"
	"k8s.io/utils/ptr"
)

// TestBuildPodSpec_TerminationGracePeriod verifies that the instance's grace
// period is carried on the Pod, and that an unset one leaves the Kubernetes
// default in place.
func TestBuildPodSpec_TerminationGracePeriod(t *testing.T) {
	ctx := context.Background()
	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{})

	inst := newTestInstance()
	inst.Spec.Runtime.TerminationGracePeriodSeconds = ptr.To[int64](45)
	spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}
	if got := spec.TerminationGracePeriodSeconds; got == nil || *got != 45 {
		t.Errorf("TerminationGracePeriodSeconds = %v, want 45", got)
	}

	unset := newTestInstance()
	spec, err = r.buildPodSpecFromContainers(ctx, unset, unset.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}
	if spec.TerminationGracePeriodSeconds != nil {
		t.Errorf("TerminationGracePeriodSeconds = %d, want unset", *spec.TerminationGracePeriodSeconds)
	}
}
