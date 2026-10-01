// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"reflect"
	"testing"

	"go.datum.net/unikraft-provider/internal/config"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestBuildPodSpec_InstanceDNSUnset verifies that without a deployment-time
// resolver choice the Pod's DNS fields stay empty, so the runtime's own
// default resolver keeps governing.
func TestBuildPodSpec_InstanceDNSUnset(t *testing.T) {
	ctx := context.Background()
	inst := newTestInstance()
	inst.UID = types.UID("dns-unset-uid")

	for name, cfg := range map[string]config.DownstreamResourceManagementConfig{
		"nil":   {},
		"empty": {InstanceDNS: &config.InstanceDNSConfig{}},
	} {
		t.Run(name, func(t *testing.T) {
			r := reconcilerWithConfig(cfg)
			spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
			if err != nil {
				t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
			}
			if spec.DNSPolicy != "" {
				t.Errorf("DNSPolicy = %q, want unset", spec.DNSPolicy)
			}
			if spec.DNSConfig != nil {
				t.Errorf("DNSConfig = %+v, want nil", spec.DNSConfig)
			}
		})
	}
}

// TestBuildPodSpec_InstanceDNSSet verifies that a configured resolver list is
// carried on every Instance Pod verbatim, IPv6 literals included, with
// policy None so nothing else is appended to it.
func TestBuildPodSpec_InstanceDNSSet(t *testing.T) {
	ctx := context.Background()
	inst := newTestInstance()
	inst.UID = types.UID("dns-set-uid")

	nameservers := []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}
	searches := []string{"svc.example.internal", "example.internal"}
	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{
		InstanceDNS: &config.InstanceDNSConfig{
			Nameservers: nameservers,
			Searches:    searches,
		},
	})

	spec, err := r.buildPodSpecFromContainers(ctx, inst, inst.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}

	if spec.DNSPolicy != core.DNSNone {
		t.Errorf("DNSPolicy = %q, want %q", spec.DNSPolicy, core.DNSNone)
	}
	if spec.DNSConfig == nil {
		t.Fatal("DNSConfig = nil, want nameservers and searches")
	}
	if !reflect.DeepEqual(spec.DNSConfig.Nameservers, nameservers) {
		t.Errorf("Nameservers = %v, want %v", spec.DNSConfig.Nameservers, nameservers)
	}
	if !reflect.DeepEqual(spec.DNSConfig.Searches, searches) {
		t.Errorf("Searches = %v, want %v", spec.DNSConfig.Searches, searches)
	}

	// The Pod must not alias the provider's config, which outlives it.
	spec.DNSConfig.Nameservers[0] = "mutated"
	if r.Config.DownstreamResourceManagement.InstanceDNS.Nameservers[0] == "mutated" {
		t.Error("Pod DNSConfig aliases the provider config slice")
	}
}
