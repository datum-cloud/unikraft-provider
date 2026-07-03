// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// TestBuildPodSpecFromContainers_CommandArgs verifies that Command and Args from
// SandboxContainer are passed through to the corresponding core.Container field
// verbatim, and that neither field is set when the container spec leaves them nil.
func TestBuildPodSpecFromContainers_CommandArgs(t *testing.T) {
	r := &InstanceReconciler{}

	tests := []struct {
		name        string
		command     []string
		args        []string
		wantCommand []string
		wantArgs    []string
	}{
		{
			name:        "neither set — image default honored",
			command:     nil,
			args:        nil,
			wantCommand: nil,
			wantArgs:    nil,
		},
		{
			name:        "command only",
			command:     []string{"/usr/bin/bun"},
			args:        nil,
			wantCommand: []string{"/usr/bin/bun"},
			wantArgs:    nil,
		},
		{
			name:        "args only",
			command:     nil,
			args:        []string{"run", "/usr/src/server.ts"},
			wantCommand: nil,
			wantArgs:    []string{"run", "/usr/src/server.ts"},
		},
		{
			name:        "command + args both set",
			command:     []string{"/usr/bin/bun"},
			args:        []string{"run", "/usr/src/server.ts"},
			wantCommand: []string{"/usr/bin/bun"},
			wantArgs:    []string{"run", "/usr/src/server.ts"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance := &computev1alpha.Instance{}
			containers := []computev1alpha.SandboxContainer{
				{
					Name:    "app",
					Image:   "example.com/myapp:latest",
					Command: tc.command,
					Args:    tc.args,
				},
			}

			spec, err := r.buildPodSpecFromContainers(context.Background(), instance, containers)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(spec.Containers) != 1 {
				t.Fatalf("expected 1 container, got %d", len(spec.Containers))
			}
			c := spec.Containers[0]

			if !sliceEqual(c.Command, tc.wantCommand) {
				t.Errorf("Command: want %v, got %v", tc.wantCommand, c.Command)
			}
			if !sliceEqual(c.Args, tc.wantArgs) {
				t.Errorf("Args: want %v, got %v", tc.wantArgs, c.Args)
			}
		})
	}
}

// TestBuildPodSpecFromContainers_OtherFieldsPassthrough verifies env, ports, and
// memory pass through correctly alongside Command/Args, ensuring the addition
// didn't disturb existing mapping logic.
func TestBuildPodSpecFromContainers_OtherFieldsPassthrough(t *testing.T) {
	r := &InstanceReconciler{}

	tcp := corev1.Protocol("TCP")
	instance := &computev1alpha.Instance{}
	containers := []computev1alpha.SandboxContainer{
		{
			Name:    "web",
			Image:   "example.com/web:latest",
			Command: []string{"/bin/serve"},
			Args:    []string{"--port=8080"},
			Env:     []corev1.EnvVar{{Name: "PORT", Value: "8080"}},
			Ports:   []computev1alpha.NamedPort{{Name: "http", Port: 8080, Protocol: &tcp}},
			Resources: &computev1alpha.ContainerResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		},
	}

	spec, err := r.buildPodSpecFromContainers(context.Background(), instance, containers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(spec.Containers))
	}
	c := spec.Containers[0]

	if !sliceEqual(c.Command, []string{"/bin/serve"}) {
		t.Errorf("Command: want [/bin/serve], got %v", c.Command)
	}
	if !sliceEqual(c.Args, []string{"--port=8080"}) {
		t.Errorf("Args: want [--port=8080], got %v", c.Args)
	}
	if len(c.Env) != 1 || c.Env[0].Name != "PORT" || c.Env[0].Value != "8080" {
		t.Errorf("Env: want [{PORT 8080}], got %v", c.Env)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("Ports: want port 8080, got %v", c.Ports)
	}
	memLimit := c.Resources.Limits[corev1.ResourceMemory]
	wantMem := resource.MustParse("256Mi")
	if memLimit.Cmp(wantMem) != 0 {
		t.Errorf("Memory limit: want 256Mi, got %v", memLimit.String())
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
