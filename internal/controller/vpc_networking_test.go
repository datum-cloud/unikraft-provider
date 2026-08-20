// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	netattachv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	computev1alpha "go.datum.net/compute/api/v1alpha"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.datum.net/unikraft-provider/internal/config"
)

const (
	testInterfaceName        = "eth0"
	testNetworkInterfaceName = "vpc-instance-eth0"

	// testNetworkContextName is the network's presence in this location, which
	// the VPC is named after.
	testNetworkContextName = "default-us-central-1"

	// testNetworkInterfaceUID is load-bearing: the controller treats a mismatch
	// as stale rather than binding a recreated interface of the same name.
	testNetworkInterfaceUID = types.UID("ni-uid-1")
)

// vpcTestScheme extends the controller test scheme with the networking, cloud
// and CNI types the VPC path reads and writes.
func vpcTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := testScheme(t)
	if err := networkingv1alpha.AddToScheme(s); err != nil {
		t.Fatalf("failed to add networking scheme: %v", err)
	}
	if err := cloudv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add cloud scheme: %v", err)
	}
	if err := netattachv1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add network attachment scheme: %v", err)
	}
	return s
}

// vpcInstance is an Instance asking for a single interface on a tenant network.
func vpcInstance() *computev1alpha.Instance {
	instance := newTestInstance()
	instance.Name = "vpc-instance"
	instance.Spec.NetworkInterfaces = []computev1alpha.InstanceNetworkInterface{
		{
			Name:    testInterfaceName,
			Network: networkingv1alpha.NetworkRef{Name: "default"},
		},
	}
	return instance
}

// boundClaim is the claim compute creates for the instance's interface, already
// bound to an interface.
func boundClaim(instance *computev1alpha.Instance) *networkingv1alpha.NetworkInterfaceClaim {
	return &networkingv1alpha.NetworkInterfaceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkInterfaceClaimName(instance.Name, testInterfaceName),
			Namespace: instance.Namespace,
		},
		Status: networkingv1alpha.NetworkInterfaceClaimStatus{
			NetworkInterfaceRef: &networkingv1alpha.LocalNetworkInterfaceRef{Name: testNetworkInterfaceName},
		},
	}
}

// allocatedInterface is the NetworkInterface NSO allocated addresses on.
func allocatedInterface(namespace string) *networkingv1alpha.NetworkInterface {
	return &networkingv1alpha.NetworkInterface{
		ObjectMeta: metav1.ObjectMeta{Name: testNetworkInterfaceName, Namespace: namespace, UID: testNetworkInterfaceUID},
		Spec: networkingv1alpha.NetworkInterfaceSpec{
			Network:       networkingv1alpha.LocalNetworkRef{Name: "default"},
			InterfaceName: testInterfaceName,
			MTU:           1400,
			Addresses: []networkingv1alpha.NetworkInterfaceAddress{
				{Family: networkingv1alpha.IPv6Protocol, Address: "2001:db8:a001::/96", Gateway: "2001:db8:a001::1", Primary: true},
				{Family: networkingv1alpha.IPv4Protocol, Address: "10.128.0.2/32", Gateway: "10.128.0.1"},
			},
		},
		Status: networkingv1alpha.NetworkInterfaceStatus{
			NetworkContextRef: &networkingv1alpha.LocalNetworkContextRef{Name: testNetworkContextName},
		},
	}
}

// nadFor is the NAD the VPC controller renders from the VPCAttachment, named
// after the attachment rather than after the interface.
func nadFor(namespace string) *netattachv1.NetworkAttachmentDefinition {
	return &netattachv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: vpcAttachmentName(allocatedInterface(namespace)), Namespace: namespace},
	}
}

func vpcEnabledConfig() *config.UnikraftProvider {
	return &config.UnikraftProvider{
		DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
			EnableVPCNetworking: true,
		},
	}
}

// TestReconcileSandboxContainers_VPCNetworking covers the ordering the design
// requires: no Pod until the NAD the guest attaches to exists, and the Multus
// annotation naming it once it does.
func TestReconcileSandboxContainers_VPCNetworking(t *testing.T) {
	tests := []struct {
		name              string
		cfg               *config.UnikraftProvider
		objects           func(instance *computev1alpha.Instance) []client.Object
		wantPod           bool
		wantRequeue       bool
		wantAnnotationKey string
		wantAnnotation    string
	}{
		{
			name: "pod creation deferred while the claim is unbound",
			cfg:  vpcEnabledConfig(),
			objects: func(instance *computev1alpha.Instance) []client.Object {
				claim := boundClaim(instance)
				claim.Status.NetworkInterfaceRef = nil
				return []client.Object{claim}
			},
			wantPod:     false,
			wantRequeue: true,
		},
		{
			name: "pod creation deferred while the network context is unresolved",
			cfg:  vpcEnabledConfig(),
			objects: func(instance *computev1alpha.Instance) []client.Object {
				networkInterface := allocatedInterface(instance.Namespace)
				networkInterface.Status.NetworkContextRef = nil
				return []client.Object{boundClaim(instance), networkInterface, nadFor(instance.Namespace)}
			},
			wantPod:     false,
			wantRequeue: true,
		},
		{
			name: "pod creation deferred while the NAD is absent",
			cfg:  vpcEnabledConfig(),
			objects: func(instance *computev1alpha.Instance) []client.Object {
				return []client.Object{boundClaim(instance), allocatedInterface(instance.Namespace)}
			},
			wantPod:     false,
			wantRequeue: true,
		},
		{
			name: "pod created and annotated once the NAD exists",
			cfg:  vpcEnabledConfig(),
			objects: func(instance *computev1alpha.Instance) []client.Object {
				return []client.Object{boundClaim(instance), allocatedInterface(instance.Namespace), nadFor(instance.Namespace)}
			},
			wantPod:           true,
			wantAnnotationKey: multusNetworksAnnotation,
			wantAnnotation:    "default/" + testNetworkInterfaceName,
		},
		{
			name: "default-network form is configurable",
			cfg: &config.UnikraftProvider{
				DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
					EnableVPCNetworking:     true,
					MultusNetworkAnnotation: "v1.multus-cni.io/default-network",
				},
			},
			objects: func(instance *computev1alpha.Instance) []client.Object {
				return []client.Object{boundClaim(instance), allocatedInterface(instance.Namespace), nadFor(instance.Namespace)}
			},
			wantPod:           true,
			wantAnnotationKey: "v1.multus-cni.io/default-network",
			wantAnnotation:    "default/" + testNetworkInterfaceName,
		},
		{
			name:    "disabled cell creates the pod with no VPC attachment",
			cfg:     &config.UnikraftProvider{},
			objects: func(instance *computev1alpha.Instance) []client.Object { return nil },
			wantPod: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := vpcTestScheme(t)
			instance := vpcInstance()

			objects := []client.Object{instance}
			objects = append(objects, tt.objects(instance)...)

			cl := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(objects...).
				WithStatusSubresource(&computev1alpha.Instance{}).
				Build()

			r := &InstanceReconciler{Client: cl, Scheme: s, Config: tt.cfg}

			result, err := r.reconcileSandboxContainers(ctx, instance)
			if err != nil {
				t.Fatalf("reconcileSandboxContainers returned error: %v", err)
			}
			if got := result.RequeueAfter > 0; got != tt.wantRequeue {
				t.Errorf("requeue = %v, want %v", got, tt.wantRequeue)
			}

			var pod core.Pod
			err = cl.Get(ctx, client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}, &pod)
			switch {
			case tt.wantPod && err != nil:
				t.Fatalf("expected pod to be created, got error: %v", err)
			case !tt.wantPod && !apierrors.IsNotFound(err):
				t.Fatalf("expected no pod, got err=%v", err)
			}

			if !tt.wantPod {
				return
			}

			got := pod.Annotations[tt.wantAnnotationKey]
			if tt.wantAnnotation == "" {
				if pod.Annotations[multusNetworksAnnotation] != "" {
					t.Errorf("unexpected multus annotation %q", pod.Annotations[multusNetworksAnnotation])
				}
				return
			}
			if got != tt.wantAnnotation {
				t.Errorf("annotation %s = %q, want %q", tt.wantAnnotationKey, got, tt.wantAnnotation)
			}
		})
	}
}

// TestReconcileVPCAttachment_CarriesInterfaceAddresses verifies the attachment
// declares the addresses NSO already allocated, and is owned by the Instance so
// it does not outlive it the way a retained interface does.
func TestReconcileVPCAttachment_CarriesInterfaceAddresses(t *testing.T) {
	ctx := context.Background()
	s := vpcTestScheme(t)
	instance := vpcInstance()

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance, boundClaim(instance), allocatedInterface(instance.Namespace), nadFor(instance.Namespace)).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s, Config: vpcEnabledConfig()}

	networks, ready, err := r.reconcileVPCNetworking(ctx, instance)
	if err != nil {
		t.Fatalf("reconcileVPCNetworking returned error: %v", err)
	}
	if !ready {
		t.Fatal("expected vpc networking to be ready")
	}
	if len(networks) != 1 || networks[0] != "default/"+testNetworkInterfaceName {
		t.Fatalf("networks = %v, want [default/%s]", networks, testNetworkInterfaceName)
	}

	var attachment cloudv1alpha1.VPCAttachment
	key := client.ObjectKey{Name: testNetworkInterfaceName, Namespace: instance.Namespace}
	if err := cl.Get(ctx, key, &attachment); err != nil {
		t.Fatalf("expected vpc attachment to be created: %v", err)
	}

	// The VPC is named after the NetworkContext, not the Network.
	if attachment.Spec.VPC.Name != testNetworkContextName {
		t.Errorf("vpc = %q, want %q", attachment.Spec.VPC.Name, testNetworkContextName)
	}
	if attachment.Spec.Interface.Name != testInterfaceName {
		t.Errorf("interface name = %q, want %q", attachment.Spec.Interface.Name, testInterfaceName)
	}
	if attachment.Spec.Interface.Mode != cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor {
		t.Errorf("interface mode = %q, want Hypervisor", attachment.Spec.Interface.Mode)
	}
	if attachment.Spec.InterfaceRef == nil {
		t.Fatal("expected interfaceRef to be set")
	}
	if attachment.Spec.InterfaceRef.Name != testNetworkInterfaceName {
		t.Errorf("interfaceRef.name = %q, want %q", attachment.Spec.InterfaceRef.Name, testNetworkInterfaceName)
	}
	if attachment.Spec.InterfaceRef.UID != string(testNetworkInterfaceUID) {
		t.Errorf("interfaceRef.uid = %q, want %q", attachment.Spec.InterfaceRef.UID, testNetworkInterfaceUID)
	}
	want := []cloudv1alpha1.IPAddress{"2001:db8:a001::/96", "10.128.0.2/32"}
	if len(attachment.Spec.Interface.Addresses) != len(want) {
		t.Fatalf("addresses = %v, want %v", attachment.Spec.Interface.Addresses, want)
	}
	for i, address := range want {
		if attachment.Spec.Interface.Addresses[i] != address {
			t.Errorf("address[%d] = %q, want %q", i, attachment.Spec.Interface.Addresses[i], address)
		}
	}

	owners := attachment.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "Instance" || owners[0].Name != instance.Name {
		t.Errorf("owner references = %+v, want the Instance", owners)
	}
}

// TestHandleDeletion_DeletesVPCAttachment verifies teardown removes the
// attachment alongside the Pod and Service, rather than leaving it to owner
// reference garbage collection the finalizer would deadlock.
func TestHandleDeletion_DeletesVPCAttachment(t *testing.T) {
	ctx := context.Background()
	s := vpcTestScheme(t)

	instance := vpcInstance()
	instance.Finalizers = []string{instanceFinalizer}
	now := metav1.Now()
	instance.DeletionTimestamp = &now

	attachment := &cloudv1alpha1.VPCAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: testNetworkInterfaceName, Namespace: instance.Namespace},
		Spec: cloudv1alpha1.VPCAttachmentSpec{
			VPC: cloudv1alpha1.VPCRef{Name: "default"},
			Interface: cloudv1alpha1.VPCAttachmentInterface{
				Name:      testInterfaceName,
				Addresses: []cloudv1alpha1.IPAddress{"10.128.0.2/32"},
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance, boundClaim(instance), allocatedInterface(instance.Namespace), attachment).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s, Config: vpcEnabledConfig()}

	if _, err := r.handleDeletion(ctx, instance); err != nil {
		t.Fatalf("handleDeletion returned error: %v", err)
	}

	key := client.ObjectKey{Name: testNetworkInterfaceName, Namespace: instance.Namespace}
	var got cloudv1alpha1.VPCAttachment
	if err := cl.Get(ctx, key, &got); !apierrors.IsNotFound(err) {
		t.Errorf("expected vpc attachment to be deleted, got err=%v", err)
	}
}
