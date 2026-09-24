// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	core "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// TestBuildPodSpecFromContainers_EnvFrom verifies that whole-ConfigMap and
// whole-Secret env sources reach the Pod spec with their Prefix and Optional
// intact, in declaration order, so the guest boots with the environment the
// workload asked for.
func TestBuildPodSpecFromContainers_EnvFrom(t *testing.T) {
	r := &InstanceReconciler{}

	tests := []struct {
		name    string
		envFrom []computev1alpha.EnvFromSource
		want    []core.EnvFromSource
	}{
		{
			name:    "no sources declared",
			envFrom: nil,
			want:    []core.EnvFromSource{},
		},
		{
			name: "configmap source",
			envFrom: []computev1alpha.EnvFromSource{
				{ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "app-config"}},
			},
			want: []core.EnvFromSource{
				{ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "app-config"},
				}},
			},
		},
		{
			name: "secret source",
			envFrom: []computev1alpha.EnvFromSource{
				{SecretRef: &computev1alpha.SecretEnvSource{Name: "app-secrets"}},
			},
			want: []core.EnvFromSource{
				{SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "app-secrets"},
				}},
			},
		},
		{
			name: "prefix preserved on both kinds",
			envFrom: []computev1alpha.EnvFromSource{
				{Prefix: "CFG_", ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "app-config"}},
				{Prefix: "SEC_", SecretRef: &computev1alpha.SecretEnvSource{Name: "app-secrets"}},
			},
			want: []core.EnvFromSource{
				{Prefix: "CFG_", ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "app-config"},
				}},
				{Prefix: "SEC_", SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "app-secrets"},
				}},
			},
		},
		{
			name: "optional true, false, and unset stay distinct",
			envFrom: []computev1alpha.EnvFromSource{
				{ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "cm-optional", Optional: ptr.To(true)}},
				{ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "cm-required", Optional: ptr.To(false)}},
				{ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "cm-unset"}},
				{SecretRef: &computev1alpha.SecretEnvSource{Name: "sec-optional", Optional: ptr.To(true)}},
				{SecretRef: &computev1alpha.SecretEnvSource{Name: "sec-required", Optional: ptr.To(false)}},
				{SecretRef: &computev1alpha.SecretEnvSource{Name: "sec-unset"}},
			},
			want: []core.EnvFromSource{
				{ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "cm-optional"},
					Optional:             ptr.To(true),
				}},
				{ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "cm-required"},
					Optional:             ptr.To(false),
				}},
				{ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "cm-unset"},
				}},
				{SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "sec-optional"},
					Optional:             ptr.To(true),
				}},
				{SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "sec-required"},
					Optional:             ptr.To(false),
				}},
				{SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "sec-unset"},
				}},
			},
		},
		{
			name: "declaration order preserved across mixed kinds",
			envFrom: []computev1alpha.EnvFromSource{
				{ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "base"}},
				{SecretRef: &computev1alpha.SecretEnvSource{Name: "overrides"}},
				{ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "last-word"}},
			},
			want: []core.EnvFromSource{
				{ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "base"},
				}},
				{SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "overrides"},
				}},
				{ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "last-word"},
				}},
			},
		},
		{
			name: "entry naming neither kind is dropped",
			envFrom: []computev1alpha.EnvFromSource{
				{ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "app-config"}},
				{Prefix: "ORPHAN_"},
				{SecretRef: &computev1alpha.SecretEnvSource{Name: "app-secrets"}},
			},
			want: []core.EnvFromSource{
				{ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "app-config"},
				}},
				{SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "app-secrets"},
				}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance := &computev1alpha.Instance{}
			containers := []computev1alpha.SandboxContainer{
				{
					Name:    "app",
					Image:   "index.unikraft.io/datum/app:latest",
					EnvFrom: tc.envFrom,
				},
			}

			spec, err := r.buildPodSpecFromContainers(context.Background(), instance, containers)
			if err != nil {
				t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
			}
			if len(spec.Containers) != 1 {
				t.Fatalf("expected 1 container, got %d", len(spec.Containers))
			}

			assertEnvFromEqual(t, spec.Containers[0].EnvFrom, tc.want)
		})
	}
}

// TestBuildPodSpecFromContainers_EnvAndEnvFromCoexist verifies that per-key env
// vars and whole-source imports are mapped independently, since a container may
// import a ConfigMap wholesale and still override one key literally.
func TestBuildPodSpecFromContainers_EnvAndEnvFromCoexist(t *testing.T) {
	r := &InstanceReconciler{}

	instance := &computev1alpha.Instance{}
	containers := []computev1alpha.SandboxContainer{
		{
			Name:  "app",
			Image: "index.unikraft.io/datum/app:latest",
			Env: []core.EnvVar{
				{Name: "PORT", Value: "8080"},
				{Name: "API_KEY", ValueFrom: &core.EnvVarSource{
					SecretKeyRef: &core.SecretKeySelector{
						LocalObjectReference: core.LocalObjectReference{Name: "app-secrets"},
						Key:                  "api-key",
					},
				}},
			},
			EnvFrom: []computev1alpha.EnvFromSource{
				{ConfigMapRef: &computev1alpha.ConfigMapEnvSource{Name: "app-config"}},
			},
		},
	}

	spec, err := r.buildPodSpecFromContainers(context.Background(), instance, containers)
	if err != nil {
		t.Fatalf("buildPodSpecFromContainers returned error: %v", err)
	}
	c := spec.Containers[0]

	if len(c.Env) != 2 {
		t.Fatalf("Env: want 2 entries, got %d (%v)", len(c.Env), c.Env)
	}
	if c.Env[0].Name != "PORT" || c.Env[0].Value != "8080" {
		t.Errorf("Env[0]: want {PORT 8080}, got %+v", c.Env[0])
	}
	if c.Env[1].ValueFrom == nil || c.Env[1].ValueFrom.SecretKeyRef == nil ||
		c.Env[1].ValueFrom.SecretKeyRef.Key != "api-key" {
		t.Errorf("Env[1]: want SecretKeyRef on api-key, got %+v", c.Env[1])
	}

	assertEnvFromEqual(t, c.EnvFrom, []core.EnvFromSource{
		{ConfigMapRef: &core.ConfigMapEnvSource{
			LocalObjectReference: core.LocalObjectReference{Name: "app-config"},
		}},
	})
}

// assertEnvFromEqual compares EnvFrom entries element-wise, reporting Optional
// pointer differences by value so a nil and a false are not conflated.
func assertEnvFromEqual(t *testing.T, got, want []core.EnvFromSource) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("EnvFrom: want %d entries, got %d (%+v)", len(want), len(got), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Prefix != w.Prefix {
			t.Errorf("EnvFrom[%d].Prefix: want %q, got %q", i, w.Prefix, g.Prefix)
		}
		if (g.ConfigMapRef == nil) != (w.ConfigMapRef == nil) {
			t.Errorf("EnvFrom[%d].ConfigMapRef: want %+v, got %+v", i, w.ConfigMapRef, g.ConfigMapRef)
		} else if w.ConfigMapRef != nil {
			if g.ConfigMapRef.Name != w.ConfigMapRef.Name {
				t.Errorf("EnvFrom[%d].ConfigMapRef.Name: want %q, got %q", i, w.ConfigMapRef.Name, g.ConfigMapRef.Name)
			}
			assertOptionalEqual(t, i, "ConfigMapRef", g.ConfigMapRef.Optional, w.ConfigMapRef.Optional)
		}
		if (g.SecretRef == nil) != (w.SecretRef == nil) {
			t.Errorf("EnvFrom[%d].SecretRef: want %+v, got %+v", i, w.SecretRef, g.SecretRef)
		} else if w.SecretRef != nil {
			if g.SecretRef.Name != w.SecretRef.Name {
				t.Errorf("EnvFrom[%d].SecretRef.Name: want %q, got %q", i, w.SecretRef.Name, g.SecretRef.Name)
			}
			assertOptionalEqual(t, i, "SecretRef", g.SecretRef.Optional, w.SecretRef.Optional)
		}
	}
}

// assertOptionalEqual compares two Optional pointers, treating unset as distinct
// from an explicit false.
func assertOptionalEqual(t *testing.T, i int, field string, got, want *bool) {
	t.Helper()

	switch {
	case want == nil && got != nil:
		t.Errorf("EnvFrom[%d].%s.Optional: want unset, got %v", i, field, *got)
	case want != nil && got == nil:
		t.Errorf("EnvFrom[%d].%s.Optional: want %v, got unset", i, field, *want)
	case want != nil && got != nil && *want != *got:
		t.Errorf("EnvFrom[%d].%s.Optional: want %v, got %v", i, field, *want, *got)
	}
}
