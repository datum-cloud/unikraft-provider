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

// platformAllocatedInterface is the interface status compute publishes once the
// platform has allocated the Instance a tenant network address: the address
// itself, plus the NetworkInterface reference that interface injection reads
// before it admits the Instance Pod.
func platformAllocatedInterface() computev1alpha.InstanceNetworkInterfaceStatus {
	networkIP := "10.128.0.7"
	return computev1alpha.InstanceNetworkInterfaceStatus{
		Name:                "eth0",
		NetworkInterfaceRef: &networkingv1alpha.LocalNetworkInterfaceRef{Name: "test-instance-eth0"},
		Assignments: computev1alpha.InstanceNetworkInterfaceAssignmentsStatus{
			NetworkIP: &networkIP,
		},
	}
}

// runningPodWithClusterIP is a Pod the cell's own network has addressed, which
// is the address the provider would publish if it owned the field.
func runningPodWithClusterIP() *core.Pod {
	pod := podWithPhase(core.PodRunning)
	pod.Status.PodIPs = []core.PodIP{{IP: "172.16.4.9"}}
	return pod
}

// TestSyncInstancePowerState_PlatformAddressesSurvive verifies that the
// provider leaves the addresses compute publishes alone. Replacing them with
// the Pod's cluster address both misreports where the Instance is reachable and
// drops the NetworkInterface reference that admits the next Instance Pod.
func TestSyncInstancePowerState_PlatformAddressesSurvive(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.UnikraftProvider
	}{
		{
			name: "the cell wires instances onto tenant networks",
			cfg:  vpcEnabledConfig(),
		},
		{
			name: "the platform allocates addresses before this provider opts in",
			cfg:  &config.UnikraftProvider{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := testScheme(t)
			instance := instanceRequestingInterface()
			published := platformAllocatedInterface()
			instance.Status.NetworkInterfaces = []computev1alpha.InstanceNetworkInterfaceStatus{published}

			cl := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(instance).
				WithStatusSubresource(&computev1alpha.Instance{}).
				Build()

			r := &InstanceReconciler{Client: cl, Scheme: s, Config: tt.cfg}

			if err := r.syncInstancePowerState(ctx, instance, runningPodWithClusterIP()); err != nil {
				t.Fatalf("syncInstancePowerState returned error: %v", err)
			}

			if len(instance.Status.NetworkInterfaces) != 1 {
				t.Fatalf("network interfaces = %d entries, want 1", len(instance.Status.NetworkInterfaces))
			}
			got := instance.Status.NetworkInterfaces[0]
			if got.NetworkInterfaceRef == nil {
				t.Fatal("the NetworkInterface reference compute published was dropped")
			}
			if got.NetworkInterfaceRef.Name != published.NetworkInterfaceRef.Name {
				t.Errorf("NetworkInterface reference = %q, want %q",
					got.NetworkInterfaceRef.Name, published.NetworkInterfaceRef.Name)
			}
			if got.Assignments.NetworkIP == nil || *got.Assignments.NetworkIP != *published.Assignments.NetworkIP {
				t.Errorf("network address = %v, want the address the platform allocated %q",
					got.Assignments.NetworkIP, *published.Assignments.NetworkIP)
			}
		})
	}
}

// TestSyncInstancePowerState_PublishesPodAddressWithoutTenantNetworking
// verifies that a cell with no tenant networking still reports where the
// Instance is reachable, because there the Pod's address is the only one it
// has.
func TestSyncInstancePowerState_PublishesPodAddressWithoutTenantNetworking(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)
	instance := newTestInstance()

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s, Config: &config.UnikraftProvider{}}

	if err := r.syncInstancePowerState(ctx, instance, runningPodWithClusterIP()); err != nil {
		t.Fatalf("syncInstancePowerState returned error: %v", err)
	}

	if len(instance.Status.NetworkInterfaces) != 1 {
		t.Fatalf("network interfaces = %d entries, want 1", len(instance.Status.NetworkInterfaces))
	}
	networkIP := instance.Status.NetworkInterfaces[0].Assignments.NetworkIP
	if networkIP == nil || *networkIP != "172.16.4.9" {
		t.Errorf("network address = %v, want the address the cell assigned the Instance", networkIP)
	}
}
