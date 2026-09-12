// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// instanceJSONWithRuntimeClass is the wire form of an Instance that names a
// runtime class, as an API server serves it.
const instanceJSONWithRuntimeClass = `{
  "apiVersion": "compute.datumapis.com/v1alpha",
  "kind": "Instance",
  "metadata": {"name": "test-instance", "namespace": "default"},
  "spec": {
    "runtime": {
      "class": "unikernel",
      "sandbox": {
        "containers": [{"name": "app", "image": "oci.unikraft.io/official/nginx:latest"}]
      }
    }
  }
}`

// TestInstanceDecodeEncodePreservesRuntimeClass guards the compute API version
// this provider compiles against. Types that predate the runtime class have
// nowhere to hold it, and the class is then absent from anything the provider
// writes back.
func TestInstanceDecodeEncodePreservesRuntimeClass(t *testing.T) {
	var instance computev1alpha.Instance
	if err := json.Unmarshal([]byte(instanceJSONWithRuntimeClass), &instance); err != nil {
		t.Fatalf("failed to decode instance: %v", err)
	}

	if got := instance.Spec.Runtime.Class; got != "unikernel" {
		t.Fatalf("decoded runtime class = %q, want %q", got, "unikernel")
	}

	encoded, err := json.Marshal(&instance)
	if err != nil {
		t.Fatalf("failed to encode instance: %v", err)
	}

	var roundTripped map[string]any
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("failed to decode the re-encoded instance: %v", err)
	}
	spec, _ := roundTripped["spec"].(map[string]any)
	runtimeSpec, _ := spec["runtime"].(map[string]any)
	if got := runtimeSpec["class"]; got != "unikernel" {
		t.Errorf("re-encoded runtime class = %v, want %q", got, "unikernel")
	}
}

// TestFinalizerUpdatePreservesRuntimeClass verifies that the write the provider
// makes to claim an Instance leaves the runtime class intact. The finalizer
// update sends the whole object, so a field the provider cannot represent is
// erased from the stored Instance.
func TestFinalizerUpdatePreservesRuntimeClass(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)

	stored := decodeInstance(t, scheme, instanceJSONWithRuntimeClass)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stored).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	key := client.ObjectKeyFromObject(stored)

	var instance computev1alpha.Instance
	if err := fakeClient.Get(ctx, key, &instance); err != nil {
		t.Fatalf("failed to get instance: %v", err)
	}
	controllerutil.AddFinalizer(&instance, instanceFinalizer)
	if err := fakeClient.Update(ctx, &instance); err != nil {
		t.Fatalf("failed to update instance: %v", err)
	}

	var after computev1alpha.Instance
	if err := fakeClient.Get(ctx, key, &after); err != nil {
		t.Fatalf("failed to re-read instance: %v", err)
	}
	if got := after.Spec.Runtime.Class; got != "unikernel" {
		t.Errorf("runtime class after the provider's write = %q, want %q", got, "unikernel")
	}
	if !controllerutil.ContainsFinalizer(&after, instanceFinalizer) {
		t.Error("finalizer is absent, so the write under test did not happen")
	}
}

// decodeInstance decodes an Instance manifest through the scheme the provider
// gives its client, so the test exercises the same codecs the provider uses.
func decodeInstance(t *testing.T, scheme *runtime.Scheme, manifest string) *computev1alpha.Instance {
	t.Helper()

	instance := &computev1alpha.Instance{}
	if err := json.Unmarshal([]byte(manifest), instance); err != nil {
		t.Fatalf("failed to decode instance: %v", err)
	}
	if _, _, err := scheme.ObjectKinds(instance); err != nil {
		t.Fatalf("instance is not registered in the scheme: %v", err)
	}
	instance.TypeMeta = metav1.TypeMeta{}
	return instance
}
