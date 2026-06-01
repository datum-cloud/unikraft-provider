// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/unikraft-provider/internal/config"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ---------------------------------------------------------------------------
// Task 1: listInstanceRequests — cluster-name unit test
// ---------------------------------------------------------------------------

// TestListInstanceRequests_CarriesClusterName verifies that listInstanceRequests
// enqueues one mcreconcile.Request per Instance in the namespace and that each
// request carries the supplied cluster name verbatim.
//
// This directly tests the seam extracted from instancesInNamespace; the earlier
// bug was an empty clusterName reaching the mapper because
// TypedEnqueueRequestsFromMapFunc's ctx does not carry the cluster name.
// listInstanceRequests takes clusterName as an explicit parameter and stamps it
// on every returned request, so the test exercises that contract.
func TestListInstanceRequests_CarriesClusterName(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)
	const clusterName = "project-abc123"
	const namespace = "default"

	inst1 := &computev1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-1", Namespace: namespace},
	}
	inst2 := &computev1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-2", Namespace: namespace},
	}
	// Instance in a different namespace — must NOT appear in results.
	instOther := &computev1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-other", Namespace: "other-ns"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(inst1, inst2, instOther).
		Build()

	reqs, err := listInstanceRequests(ctx, cl, clusterName, namespace)
	if err != nil {
		t.Fatalf("listInstanceRequests returned unexpected error: %v", err)
	}

	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests (one per Instance in namespace %q), got %d", namespace, len(reqs))
	}

	reqsByName := make(map[string]struct{}, len(reqs))
	for _, r := range reqs {
		if r.ClusterName != clusterName {
			t.Errorf("request for %q has ClusterName = %q, want %q",
				r.Name, r.ClusterName, clusterName)
		}
		if r.Namespace != namespace {
			t.Errorf("request for %q has Namespace = %q, want %q",
				r.Name, r.Namespace, namespace)
		}
		reqsByName[r.Name] = struct{}{}
	}

	for _, name := range []string{"inst-1", "inst-2"} {
		if _, ok := reqsByName[name]; !ok {
			t.Errorf("expected request for Instance %q, not found in results", name)
		}
	}

	if _, ok := reqsByName["inst-other"]; ok {
		t.Error("Instance from a different namespace must not appear in results")
	}
}

// TestListInstanceRequests_EmptyNamespace verifies that an empty namespace
// returns zero requests without error.
func TestListInstanceRequests_EmptyNamespace(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	cl := fake.NewClientBuilder().WithScheme(s).Build()

	reqs, err := listInstanceRequests(ctx, cl, "cluster-xyz", "no-instances-here")
	if err != nil {
		t.Fatalf("unexpected error for empty namespace: %v", err)
	}
	if len(reqs) != 0 {
		t.Errorf("expected 0 requests for empty namespace, got %d", len(reqs))
	}
}

// ---------------------------------------------------------------------------
// Task 3: NodeSelector / Tolerations override
// ---------------------------------------------------------------------------

// TestBuildPodSpec_DefaultNodeSelector verifies that when no NodeSelector
// override is set in config, the pod spec carries the default kraftlet
// hostname selector and ukc toleration.
func TestBuildPodSpec_DefaultNodeSelector(t *testing.T) {
	ctx := context.Background()
	inst := newTestInstance()
	inst.UID = types.UID("default-ns-uid")

	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{})

	spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}

	if v, ok := spec.NodeSelector["kubernetes.io/hostname"]; !ok || v != "kraftlet" {
		t.Errorf("default NodeSelector: want kubernetes.io/hostname=kraftlet, got %v", spec.NodeSelector)
	}

	if len(spec.Tolerations) != 1 {
		t.Fatalf("expected 1 default toleration, got %d", len(spec.Tolerations))
	}
	tol := spec.Tolerations[0]
	if tol.Key != "virtual-kubelet.io/provider" || tol.Value != "ukc" || tol.Effect != core.TaintEffectNoSchedule {
		t.Errorf("unexpected default toleration: %+v", tol)
	}
}

// TestBuildPodSpec_NodeSelectorOverride verifies that when NodeSelector is set
// in config it replaces the default kraftlet selector entirely.
func TestBuildPodSpec_NodeSelectorOverride(t *testing.T) {
	ctx := context.Background()
	inst := newTestInstance()
	inst.UID = types.UID("override-ns-uid")

	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{
		NodeSelector: map[string]string{"node-role": "kraftlet", "zone": "us-east"},
	})

	spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}

	if len(spec.NodeSelector) != 2 {
		t.Fatalf("expected 2 node selector entries from override, got %d", len(spec.NodeSelector))
	}
	if spec.NodeSelector["node-role"] != "kraftlet" {
		t.Errorf("NodeSelector node-role = %q, want kraftlet", spec.NodeSelector["node-role"])
	}
	if spec.NodeSelector["zone"] != "us-east" {
		t.Errorf("NodeSelector zone = %q, want us-east", spec.NodeSelector["zone"])
	}

	// Default hostname selector must not bleed through.
	if _, ok := spec.NodeSelector["kubernetes.io/hostname"]; ok {
		t.Error("default kubernetes.io/hostname must not appear when NodeSelector override is set")
	}
}

// TestBuildPodSpec_TolerationsOverride verifies that when Tolerations is set
// in config it replaces the default ukc toleration entirely.
func TestBuildPodSpec_TolerationsOverride(t *testing.T) {
	ctx := context.Background()
	inst := newTestInstance()
	inst.UID = types.UID("override-tol-uid")

	customTol := core.Toleration{
		Key:      "custom-taint",
		Operator: core.TolerationOpEqual,
		Value:    "true",
		Effect:   core.TaintEffectNoSchedule,
	}

	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{
		Tolerations: []core.Toleration{customTol},
	})

	spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}

	if len(spec.Tolerations) != 1 {
		t.Fatalf("expected 1 toleration from override, got %d", len(spec.Tolerations))
	}
	if spec.Tolerations[0].Key != "custom-taint" {
		t.Errorf("Toleration.Key = %q, want custom-taint", spec.Tolerations[0].Key)
	}
	// Default ukc taint must not bleed through.
	for _, tol := range spec.Tolerations {
		if tol.Value == "ukc" {
			t.Error("default ukc toleration must not appear when Tolerations override is set")
		}
	}
}
