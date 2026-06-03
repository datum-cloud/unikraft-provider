// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	"go.datum.net/unikraft-provider/internal/config"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestBuildPodSpec_DisablesServiceAccountToken verifies that instance Pods are
// hardened against Kubernetes API access: the default ServiceAccount token is
// not auto-mounted, and service link env vars are disabled.
func TestBuildPodSpec_DisablesServiceAccountToken(t *testing.T) {
	ctx := context.Background()
	inst := newTestInstance()
	inst.UID = types.UID("no-sa-token-uid")

	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{})

	spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}

	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want explicit false", spec.AutomountServiceAccountToken)
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Errorf("EnableServiceLinks = %v, want explicit false", spec.EnableServiceLinks)
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
