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

// TestResolveContainerResources verifies the three-tier sizing precedence for
// Pod container resources: explicit Limits > instanceType catalog > legacy
// default. The catalog values must equal what compute's quota claim accounts
// for (1 vCPU / 2 GiB for datumcloud/d1-standard-2) so that Pod sizing and
// quota are always consistent.
func TestResolveContainerResources(t *testing.T) {
	instanceWithType := func(instanceType string) *computev1alpha.Instance {
		return &computev1alpha.Instance{
			Spec: computev1alpha.InstanceSpec{
				Runtime: computev1alpha.InstanceRuntimeSpec{
					Resources: computev1alpha.InstanceRuntimeResources{
						InstanceType: instanceType,
					},
				},
			},
		}
	}

	containerWithLimits := func(cpu, mem string) *computev1alpha.SandboxContainer {
		sc := &computev1alpha.SandboxContainer{
			Resources: &computev1alpha.ContainerResourceRequirements{
				Limits: corev1.ResourceList{},
			},
		}
		if cpu != "" {
			sc.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if mem != "" {
			sc.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(mem)
		}
		return sc
	}

	tests := []struct {
		name          string
		instance      *computev1alpha.Instance
		container     *computev1alpha.SandboxContainer
		wantCPU       int64
		wantMem       int64
	}{
		{
			// Common production shape: instanceType only, no explicit limits.
			// The Pod must receive the catalog values so it matches the quota claim.
			name:      "d1-standard-2 with no explicit limits → catalog values",
			instance:  instanceWithType("datumcloud/d1-standard-2"),
			container: &computev1alpha.SandboxContainer{},
			wantCPU:   1000, // 1 vCPU
			wantMem:   2048, // 2 GiB
		},
		{
			// Both cpu and memory Limits set explicitly — catalog must not override.
			name:      "explicit cpu+memory limits take precedence over catalog",
			instance:  instanceWithType("datumcloud/d1-standard-2"),
			container: containerWithLimits("500m", "512Mi"),
			wantCPU:   500,
			wantMem:   512,
		},
		{
			// Only memory limit set — memory comes from explicit limit, CPU from catalog.
			name:      "explicit memory only: explicit memory wins, catalog supplies CPU",
			instance:  instanceWithType("datumcloud/d1-standard-2"),
			container: containerWithLimits("", "256Mi"),
			wantCPU:   1000, // catalog d1-standard-2
			wantMem:   256,  // explicit
		},
		{
			// Only CPU limit set — cpu from explicit, memory from catalog.
			name:      "explicit cpu only: explicit cpu wins, catalog supplies memory",
			instance:  instanceWithType("datumcloud/d1-standard-2"),
			container: containerWithLimits("2", ""),
			wantCPU:   2000, // explicit 2 cores
			wantMem:   2048, // catalog d1-standard-2
		},
		{
			// Unknown instanceType with no explicit limits → legacy fallback.
			// No fabricated CPU value; memory uses the hardcoded default.
			name:      "unknown instanceType, no limits → legacy default memory, no CPU",
			instance:  instanceWithType("datumcloud/unknown-type-99"),
			container: &computev1alpha.SandboxContainer{},
			wantCPU:   0,
			wantMem:   int64(defaultInstanceMemoryMB),
		},
		{
			// No instanceType, no explicit limits → same legacy fallback.
			name:      "empty instanceType, no limits → legacy default memory, no CPU",
			instance:  instanceWithType(""),
			container: &computev1alpha.SandboxContainer{},
			wantCPU:   0,
			wantMem:   int64(defaultInstanceMemoryMB),
		},
		{
			// nil instance (defensive) → legacy fallback.
			name:      "nil instance → legacy default memory, no CPU",
			instance:  nil,
			container: &computev1alpha.SandboxContainer{},
			wantCPU:   0,
			wantMem:   int64(defaultInstanceMemoryMB),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem := resolveContainerResources(tc.instance, tc.container)
			if cpu != tc.wantCPU {
				t.Errorf("cpuMillicores = %d, want %d", cpu, tc.wantCPU)
			}
			if mem != tc.wantMem {
				t.Errorf("memoryMiB = %d, want %d", mem, tc.wantMem)
			}
		})
	}
}

// TestBuildPodSpecFromContainers_InstanceTypeSizing verifies that
// buildPodSpecFromContainers sets both Requests and Limits for cpu and memory
// on the downstream Pod container when the instance is sized by instanceType
// only. This ensures the Pod footprint equals what the quota claim accounts for.
func TestBuildPodSpecFromContainers_InstanceTypeSizing(t *testing.T) {
	ctx := context.Background()
	r := &InstanceReconciler{}

	instance := &computev1alpha.Instance{
		Spec: computev1alpha.InstanceSpec{
			Runtime: computev1alpha.InstanceRuntimeSpec{
				Resources: computev1alpha.InstanceRuntimeResources{
					InstanceType: "datumcloud/d1-standard-2",
				},
				Sandbox: &computev1alpha.SandboxRuntime{
					Containers: []computev1alpha.SandboxContainer{
						{
							Name:  "app",
							Image: "index.unikraft.io/datum/myapp:latest",
							// No Resources set — instanceType drives sizing.
						},
					},
				},
			},
		},
	}

	spec, err := r.buildPodSpecFromContainers(ctx, instance, instance.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(spec.Containers))
	}
	c := spec.Containers[0]

	wantCPU := resource.MustParse("1000m")
	wantMem := resource.MustParse("2048Mi")

	// Both Limits and Requests must be set with catalog values.
	gotCPULimit := c.Resources.Limits[corev1.ResourceCPU]
	if gotCPULimit.Cmp(wantCPU) != 0 {
		t.Errorf("CPU Limit = %s, want %s", gotCPULimit.String(), wantCPU.String())
	}
	gotMemLimit := c.Resources.Limits[corev1.ResourceMemory]
	if gotMemLimit.Cmp(wantMem) != 0 {
		t.Errorf("Memory Limit = %s, want %s", gotMemLimit.String(), wantMem.String())
	}
	gotCPUReq := c.Resources.Requests[corev1.ResourceCPU]
	if gotCPUReq.Cmp(wantCPU) != 0 {
		t.Errorf("CPU Request = %s, want %s", gotCPUReq.String(), wantCPU.String())
	}
	gotMemReq := c.Resources.Requests[corev1.ResourceMemory]
	if gotMemReq.Cmp(wantMem) != 0 {
		t.Errorf("Memory Request = %s, want %s", gotMemReq.String(), wantMem.String())
	}
}

// TestBuildPodSpecFromContainers_ExplicitLimitsPreserved verifies that explicit
// container Limits are not overridden by the instanceType catalog, so a workload
// with custom sizing is programmed at its declared footprint.
func TestBuildPodSpecFromContainers_ExplicitLimitsPreserved(t *testing.T) {
	ctx := context.Background()
	r := &InstanceReconciler{}

	instance := &computev1alpha.Instance{
		Spec: computev1alpha.InstanceSpec{
			Runtime: computev1alpha.InstanceRuntimeSpec{
				Resources: computev1alpha.InstanceRuntimeResources{
					InstanceType: "datumcloud/d1-standard-2",
				},
				Sandbox: &computev1alpha.SandboxRuntime{
					Containers: []computev1alpha.SandboxContainer{
						{
							Name:  "app",
							Image: "index.unikraft.io/datum/myapp:latest",
							Resources: &computev1alpha.ContainerResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	spec, err := r.buildPodSpecFromContainers(ctx, instance, instance.Spec.Runtime.Sandbox.Containers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := spec.Containers[0]

	wantCPU := resource.MustParse("500m")
	wantMem := resource.MustParse("512Mi")

	gotCPU := c.Resources.Limits[corev1.ResourceCPU]
	if gotCPU.Cmp(wantCPU) != 0 {
		t.Errorf("CPU Limit = %s, want %s (explicit value must not be overridden by catalog)",
			gotCPU.String(), wantCPU.String())
	}
	gotMem := c.Resources.Limits[corev1.ResourceMemory]
	if gotMem.Cmp(wantMem) != 0 {
		t.Errorf("Memory Limit = %s, want %s (explicit value must not be overridden by catalog)",
			gotMem.String(), wantMem.String())
	}
}

// sliceEqual reports whether two string slices are element-wise equal.
// nil and an empty slice are treated as equivalent.
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
