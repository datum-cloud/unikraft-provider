// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/unikraft-provider/internal/config"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// instanceWithImagePullSecrets returns an Instance whose sandbox pulls from a
// private registry, mirroring what the compute federator writes to the cell
// when a user declares imagePullSecrets on their Workload.
func instanceWithImagePullSecrets(uid types.UID, secretNames ...string) *computev1alpha.Instance {
	inst := instanceWithUID(uid)
	inst.Spec.Runtime.Sandbox.Containers[0].Image = "index.unikraft.io/datum/private-app:latest"
	refs := make([]computev1alpha.LocalSecretReference, 0, len(secretNames))
	for _, n := range secretNames {
		refs = append(refs, computev1alpha.LocalSecretReference{Name: n})
	}
	inst.Spec.Runtime.Sandbox.ImagePullSecrets = refs
	return inst
}

// TestBuildPodSpec_ImagePullSecretsForwarded verifies that secrets declared on
// the sandbox are named on the Pod, in order, so the runtime can authenticate
// to a private registry.
func TestBuildPodSpec_ImagePullSecretsForwarded(t *testing.T) {
	ctx := context.Background()
	inst := instanceWithImagePullSecrets("pull-secret-uid-1", "regcred", "ghcr-creds")

	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{})

	spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}

	want := []core.LocalObjectReference{{Name: "regcred"}, {Name: "ghcr-creds"}}
	if len(spec.ImagePullSecrets) != len(want) {
		t.Fatalf("ImagePullSecrets = %+v, want %+v", spec.ImagePullSecrets, want)
	}
	for i := range want {
		if spec.ImagePullSecrets[i] != want[i] {
			t.Errorf("ImagePullSecrets[%d] = %+v, want %+v", i, spec.ImagePullSecrets[i], want[i])
		}
	}
}

// TestBuildPodSpec_NoImagePullSecrets verifies that an Instance without pull
// secrets leaves the field unset rather than emitting an empty slice.
func TestBuildPodSpec_NoImagePullSecrets(t *testing.T) {
	ctx := context.Background()
	inst := instanceWithUID("pull-secret-uid-2")

	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{})

	spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}

	if spec.ImagePullSecrets != nil {
		t.Errorf("ImagePullSecrets = %+v, want nil", spec.ImagePullSecrets)
	}
}

// TestReconcileSandboxContainers_ImagePullSecretsOnPod verifies the credential
// reference survives the full reconcile onto the created Pod object, which is
// what kraftlet reads when pulling the image.
func TestReconcileSandboxContainers_ImagePullSecretsOnPod(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithImagePullSecrets("pull-secret-uid-3", "regcred")

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{
		Client: cl,
		Scheme: s,
		Config: &config.UnikraftProvider{},
	}

	if _, err := r.reconcileSandboxContainers(ctx, instance); err != nil {
		t.Fatalf("reconcileSandboxContainers returned error: %v", err)
	}

	var pod core.Pod
	if err := cl.Get(ctx, client.ObjectKeyFromObject(instance), &pod); err != nil {
		t.Fatalf("failed to get pod after reconcile: %v", err)
	}

	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "regcred" {
		t.Errorf("pod ImagePullSecrets = %+v, want [{regcred}]", pod.Spec.ImagePullSecrets)
	}
}
