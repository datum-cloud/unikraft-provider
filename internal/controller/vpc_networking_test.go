// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	core "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.datum.net/unikraft-provider/internal/config"
)

// instanceRequestingInterface is an Instance asking for one interface on a
// tenant network.
func instanceRequestingInterface() *computev1alpha.Instance {
	instance := newTestInstance()
	instance.Spec.NetworkInterfaces = []computev1alpha.InstanceNetworkInterface{
		{Network: networkingv1alpha.NetworkRef{Name: "default"}},
	}
	return instance
}

func vpcEnabledConfig() *config.UnikraftProvider {
	return &config.UnikraftProvider{
		DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
			EnableVPCNetworking: true,
		},
	}
}

// TestReconcileSandboxContainers_InterfaceInjectionLabel verifies the provider's
// entire networking contribution: one opt-in label, stamped only when an
// interface is genuinely wanted, and no waiting on anything.
func TestReconcileSandboxContainers_InterfaceInjectionLabel(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *config.UnikraftProvider
		instance  func() *computev1alpha.Instance
		wantLabel string
	}{
		{
			name:      "stamped when the instance requests an interface",
			cfg:       vpcEnabledConfig(),
			instance:  instanceRequestingInterface,
			wantLabel: "true",
		},
		{
			name:     "not stamped when the instance requests no interfaces",
			cfg:      vpcEnabledConfig(),
			instance: newTestInstance,
		},
		{
			name:     "not stamped when the feature is disabled",
			cfg:      &config.UnikraftProvider{},
			instance: instanceRequestingInterface,
		},
		{
			name:     "not stamped when the provider has no config",
			instance: instanceRequestingInterface,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := testScheme(t)
			instance := tt.instance()

			cl := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(instance).
				WithStatusSubresource(&computev1alpha.Instance{}).
				Build()

			r := &InstanceReconciler{Client: cl, Scheme: s, Config: tt.cfg}

			result, err := r.reconcileSandboxContainers(ctx, instance)
			if err != nil {
				t.Fatalf("reconcileSandboxContainers returned error: %v", err)
			}

			// Networking never defers the Pod: the scheduling gate compute holds
			// until the data plane is prepared is the whole ordering guarantee.
			if result.RequeueAfter != 0 {
				t.Errorf("RequeueAfter = %v, want 0", result.RequeueAfter)
			}

			var pod core.Pod
			key := client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}
			if err := cl.Get(ctx, key, &pod); err != nil {
				t.Fatalf("expected pod to be created, got error: %v", err)
			}

			// A label, because the webhook's objectSelector cannot select on
			// annotations.
			if got := pod.Labels[injectInterfacesLabel]; got != tt.wantLabel {
				t.Errorf("label %s = %q, want %q", injectInterfacesLabel, got, tt.wantLabel)
			}
			if got := pod.Annotations[injectInterfacesLabel]; got != "" {
				t.Errorf("opt-in must not be stamped as an annotation, got %q", got)
			}
		})
	}
}
