// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.datum.net/unikraft-provider/internal/config"
)

const (
	testInterfaceName        = "eth0"
	testNetworkInterfaceName = "vpc-instance-eth0"
)

// vpcTestScheme extends the controller test scheme with the networking types
// the provider follows the instance to.
func vpcTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := testScheme(t)
	if err := networkingv1alpha.AddToScheme(s); err != nil {
		t.Fatalf("failed to add networking scheme: %v", err)
	}
	return s
}

// vpcInstance is an Instance asking for a single interface on a tenant network,
// with its bound interface already published on status.
func vpcInstance() *computev1alpha.Instance {
	instance := newTestInstance()
	instance.Name = "vpc-instance"
	instance.Spec.NetworkInterfaces = []computev1alpha.InstanceNetworkInterface{
		{
			Name:    testInterfaceName,
			Network: networkingv1alpha.NetworkRef{Name: "default"},
		},
	}
	instance.Status.NetworkInterfaces = []computev1alpha.InstanceNetworkInterfaceStatus{
		{
			Name:                testInterfaceName,
			NetworkInterfaceRef: &networkingv1alpha.LocalNetworkInterfaceRef{Name: testNetworkInterfaceName},
		},
	}
	return instance
}

// publishedInterface is the bound NetworkInterface carrying the annotations the
// networking stack published for whoever consumes it.
func publishedInterface(namespace string, consumerAnnotations map[string]string) *networkingv1alpha.NetworkInterface {
	return &networkingv1alpha.NetworkInterface{
		ObjectMeta: metav1.ObjectMeta{Name: testNetworkInterfaceName, Namespace: namespace},
		Status: networkingv1alpha.NetworkInterfaceStatus{
			ConsumerAnnotations: consumerAnnotations,
		},
	}
}

func vpcEnabledConfig() *config.UnikraftProvider {
	return &config.UnikraftProvider{
		DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
			EnableVPCNetworking: true,
		},
	}
}

// TestReconcileSandboxContainers_NetworkAnnotations covers the ordering the
// design requires — no Pod until the networking stack has published what
// delivers the interface — and that the published annotations are carried
// verbatim, whatever they are.
func TestReconcileSandboxContainers_NetworkAnnotations(t *testing.T) {
	tests := []struct {
		name            string
		cfg             *config.UnikraftProvider
		instance        func() *computev1alpha.Instance
		objects         func(instance *computev1alpha.Instance) []client.Object
		wantPod         bool
		wantRequeue     bool
		wantAnnotations map[string]string
	}{
		{
			name: "pod creation deferred while the interface is unbound",
			cfg:  vpcEnabledConfig(),
			instance: func() *computev1alpha.Instance {
				instance := vpcInstance()
				instance.Status.NetworkInterfaces[0].NetworkInterfaceRef = nil
				return instance
			},
			objects: func(instance *computev1alpha.Instance) []client.Object {
				return []client.Object{publishedInterface(instance.Namespace, map[string]string{"a": "b"})}
			},
			wantPod:     false,
			wantRequeue: true,
		},
		{
			name:     "pod creation deferred while the interface status is unpublished",
			cfg:      vpcEnabledConfig(),
			instance: func() *computev1alpha.Instance { i := vpcInstance(); i.Status.NetworkInterfaces = nil; return i },
			objects: func(instance *computev1alpha.Instance) []client.Object {
				return []client.Object{publishedInterface(instance.Namespace, map[string]string{"a": "b"})}
			},
			wantPod:     false,
			wantRequeue: true,
		},
		{
			name:     "pod creation deferred while no consumer annotations are published",
			cfg:      vpcEnabledConfig(),
			instance: vpcInstance,
			objects: func(instance *computev1alpha.Instance) []client.Object {
				return []client.Object{publishedInterface(instance.Namespace, nil)}
			},
			wantPod:     false,
			wantRequeue: true,
		},
		{
			name:     "published annotations are copied onto the pod",
			cfg:      vpcEnabledConfig(),
			instance: vpcInstance,
			objects: func(instance *computev1alpha.Instance) []client.Object {
				return []client.Object{publishedInterface(instance.Namespace, map[string]string{
					"k8s.v1.cni.cncf.io/networks": "default/vpc-instance-eth0",
					// A key this provider has never heard of must travel too.
					"example.com/some-future-thing": "opaque-value",
				})}
			},
			wantPod: true,
			wantAnnotations: map[string]string{
				"k8s.v1.cni.cncf.io/networks":   "default/vpc-instance-eth0",
				"example.com/some-future-thing": "opaque-value",
			},
		},
		{
			name:     "disabled cell creates the pod and reads no interfaces",
			cfg:      &config.UnikraftProvider{},
			instance: vpcInstance,
			objects:  func(instance *computev1alpha.Instance) []client.Object { return nil },
			wantPod:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := vpcTestScheme(t)
			instance := tt.instance()

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

			for annotation, want := range tt.wantAnnotations {
				if got := pod.Annotations[annotation]; got != want {
					t.Errorf("pod annotation %s = %q, want %q", annotation, got, want)
				}
			}

			if tt.wantAnnotations == nil {
				if got := pod.Annotations["k8s.v1.cni.cncf.io/networks"]; got != "" {
					t.Errorf("unexpected network annotation %q on a cell with networking disabled", got)
				}
			}
		})
	}
}

// TestInstanceNetworkAnnotations_MergesEveryInterface verifies the annotations
// of every bound interface reach the Pod, not just the first.
func TestInstanceNetworkAnnotations_MergesEveryInterface(t *testing.T) {
	ctx := context.Background()
	s := vpcTestScheme(t)

	instance := vpcInstance()
	instance.Spec.NetworkInterfaces = append(instance.Spec.NetworkInterfaces,
		computev1alpha.InstanceNetworkInterface{Name: "eth1", Network: networkingv1alpha.NetworkRef{Name: "default"}})
	instance.Status.NetworkInterfaces = append(instance.Status.NetworkInterfaces,
		computev1alpha.InstanceNetworkInterfaceStatus{
			Name:                "eth1",
			NetworkInterfaceRef: &networkingv1alpha.LocalNetworkInterfaceRef{Name: "vpc-instance-eth1"},
		})

	second := publishedInterface(instance.Namespace, map[string]string{"second/key": "second-value"})
	second.Name = "vpc-instance-eth1"

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance,
			publishedInterface(instance.Namespace, map[string]string{"first/key": "first-value"}),
			second).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s, Config: vpcEnabledConfig()}

	annotations, ready, err := r.instanceNetworkAnnotations(ctx, instance)
	if err != nil {
		t.Fatalf("instanceNetworkAnnotations returned error: %v", err)
	}
	if !ready {
		t.Fatal("expected network annotations to be ready")
	}
	want := map[string]string{"first/key": "first-value", "second/key": "second-value"}
	if len(annotations) != len(want) {
		t.Fatalf("annotations = %v, want %v", annotations, want)
	}
	for annotation, value := range want {
		if annotations[annotation] != value {
			t.Errorf("annotation %s = %q, want %q", annotation, annotations[annotation], value)
		}
	}
}
