// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

func TestInstancePodRequestsSurviveAPIServerDefaulting(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via make test")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()

	instance := &computev1alpha.Instance{
		Spec: computev1alpha.InstanceSpec{
			Runtime: computev1alpha.InstanceRuntimeSpec{
				Resources: computev1alpha.InstanceRuntimeResources{InstanceType: "datumcloud/d1-standard-2"},
				Sandbox: &computev1alpha.SandboxRuntime{
					Containers: []computev1alpha.SandboxContainer{
						{Name: "app", Image: "index.unikraft.io/datum/myapp:latest"},
					},
				},
			},
		},
	}
	spec, err := (&InstanceReconciler{}).buildPodSpecFromContainers(ctx, instance, instance.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("build pod spec: %v", err)
	}

	built := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "zero-requests", Namespace: "default"},
		Spec:       spec,
	}
	if err := c.Create(ctx, built); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	stored := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(built), stored); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	got := stored.Spec.Containers[0].Resources
	for name, want := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:    "1",
		corev1.ResourceMemory: "2Gi",
	} {
		limit := got.Limits[name]
		if limit.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("stored %s Limit = %s, want %s", name, limit.String(), want)
		}
		request, ok := got.Requests[name]
		if !ok || !request.IsZero() {
			t.Errorf("stored %s Request = %s (present=%t), want 0", name, request.String(), ok)
		}
	}

	omitted := built.DeepCopy()
	omitted.ObjectMeta = metav1.ObjectMeta{Name: "omitted-requests", Namespace: "default"}
	omitted.Spec.Containers[0].Resources.Requests = nil
	if err := c.Create(ctx, omitted); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(omitted), stored); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	defaulted := stored.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	if defaulted.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("omitted memory Request defaulted to %s, want 2Gi; the defaulting trap no longer holds", defaulted.String())
	}
}
