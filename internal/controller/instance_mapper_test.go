// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// instanceTypeSized returns an InstanceType published with the given CPU and
// memory quantities, for seeding readers in sizing tests.
func instanceTypeSized(cpu, mem string) *computev1alpha.InstanceType {
	return &computev1alpha.InstanceType{
		Spec: computev1alpha.InstanceTypeSpec{
			Resources: computev1alpha.InstanceTypeResources{
				CPU:    resource.MustParse(cpu),
				Memory: resource.MustParse(mem),
			},
		},
	}
}

// TestResolveContainerResources verifies the sizing precedence for Pod container
// resources: explicit Limits > live InstanceType object > hardcoded catalog >
// legacy default. The resolved footprint must equal what compute's quota claim
// accounts for (1 vCPU / 2 GiB for datumcloud-d1-standard-2) so that Pod sizing
// and quota are always consistent.
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
		name      string
		instance  *computev1alpha.Instance
		container *computev1alpha.SandboxContainer
		reader    instanceTypeReader
		wantCPU   int64
		wantMem   int64
		wantErr   bool
	}{
		{
			// Common production shape: instanceType only, no explicit limits and no
			// published object (or none projected yet). The Pod must receive the
			// hardcoded catalog values so it matches the quota claim.
			name:      "d1-standard-2 with no explicit limits → catalog values",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: &computev1alpha.SandboxContainer{},
			wantCPU:   1000, // 1 vCPU
			wantMem:   2048, // 2 GiB
		},
		{
			// Both cpu and memory Limits set explicitly — catalog must not override.
			name:      "explicit cpu+memory limits take precedence over catalog",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: containerWithLimits("500m", "512Mi"),
			wantCPU:   500,
			wantMem:   512,
		},
		{
			// Only memory limit set — memory comes from explicit limit, CPU from catalog.
			name:      "explicit memory only: explicit memory wins, catalog supplies CPU",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: containerWithLimits("", "256Mi"),
			wantCPU:   1000, // catalog d1-standard-2
			wantMem:   256,  // explicit
		},
		{
			// Only CPU limit set — cpu from explicit, memory from catalog.
			name:      "explicit cpu only: explicit cpu wins, catalog supplies memory",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: containerWithLimits("2", ""),
			wantCPU:   2000, // explicit 2 cores
			wantMem:   2048, // catalog d1-standard-2
		},
		{
			// The published InstanceType object is read live and wins over the
			// hardcoded catalog for a name the static table also knows.
			name:      "published instanceType overrides hardcoded catalog",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: &computev1alpha.SandboxContainer{},
			reader: func(_ context.Context, _ string) (*computev1alpha.InstanceType, error) {
				return instanceTypeSized("2000m", "4096Mi"), nil
			},
			wantCPU: 2000,
			wantMem: 4096,
		},
		{
			// Sizing comes from the live catalog even for a type the hardcoded
			// table does not know.
			name:      "published instanceType unknown to hardcoded catalog",
			instance:  instanceWithType("datumcloud-custom-x"),
			container: &computev1alpha.SandboxContainer{},
			reader: func(_ context.Context, _ string) (*computev1alpha.InstanceType, error) {
				return instanceTypeSized("750m", "1024Mi"), nil
			},
			wantCPU: 750,
			wantMem: 1024,
		},
		{
			// A name the reader does not hold falls through to the hardcoded
			// catalog, preserving old installs where the type is not yet projected.
			name:      "instanceType not found → hardcoded catalog fallback",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: &computev1alpha.SandboxContainer{},
			reader: func(_ context.Context, _ string) (*computev1alpha.InstanceType, error) {
				return nil, nil
			},
			wantCPU: 1000,
			wantMem: 2048,
		},
		{
			// Instances stored before the rename carry the slash name
			// ("datumcloud/d1-standard-2"). The name must be canonicalized before
			// the live read: client-go rejects a "/" in a resource name before the
			// request leaves the client, so a reader backed by a real Get errors on
			// the raw name and fails the Pod build. This reader mimics that
			// rejection, so the test fails unless the reader is only ever called
			// with the canonical name.
			name:      "legacy instanceType name is canonicalized before the live read",
			instance:  instanceWithType(legacyD1Standard2InstanceType),
			container: &computev1alpha.SandboxContainer{},
			reader: func(_ context.Context, name string) (*computev1alpha.InstanceType, error) {
				if name != d1Standard2InstanceType {
					return nil, errors.New("live read must use the canonical instance type name")
				}
				return instanceTypeSized("1000m", "2048Mi"), nil
			},
			wantCPU: 1000,
			wantMem: 2048,
		},
		{
			// A type published with a zero dimension is invalid; sizing falls
			// through to the hardcoded catalog rather than fabricating a partial
			// footprint.
			name:      "zero-dimension published type → hardcoded catalog fallback",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: &computev1alpha.SandboxContainer{},
			reader: func(_ context.Context, _ string) (*computev1alpha.InstanceType, error) {
				return instanceTypeSized("0m", "0Mi"), nil
			},
			wantCPU: 1000,
			wantMem: 2048,
		},
		{
			// A transient failure reading the instanceType must fail the resolve,
			// never silently size the Pod below the footprint quota claimed.
			name:      "instanceType read error propagates",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: &computev1alpha.SandboxContainer{},
			reader: func(_ context.Context, _ string) (*computev1alpha.InstanceType, error) {
				return nil, errors.New("transient read failure")
			},
			wantErr: true,
		},
		{
			// Explicit limits for both dimensions win outright; the reader is never
			// consulted, so an erroring reader cannot disturb an explicit sizing.
			name:      "explicit limits win without consulting the reader",
			instance:  instanceWithType("datumcloud-d1-standard-2"),
			container: containerWithLimits("500m", "512Mi"),
			reader: func(_ context.Context, _ string) (*computev1alpha.InstanceType, error) {
				return nil, errors.New("must not be called")
			},
			wantCPU: 500,
			wantMem: 512,
		},
		{
			// Unknown instanceType with no explicit limits → legacy fallback.
			// No fabricated CPU value; memory uses the hardcoded default.
			name:      "unknown instanceType, no limits → legacy default memory, no CPU",
			instance:  instanceWithType("datumcloud-unknown-type-99"),
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
			cpu, mem, err := resolveContainerResources(context.Background(), tc.instance, tc.container, tc.reader)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cpu != tc.wantCPU {
				t.Errorf("cpuMillicores = %d, want %d", cpu, tc.wantCPU)
			}
			if mem != tc.wantMem {
				t.Errorf("memoryMiB = %d, want %d", mem, tc.wantMem)
			}
		})
	}
}

// TestInstanceTypeReaderFromClient verifies the reader produced by
// instanceTypeReaderFromClient: it returns the published object when present and
// (nil, nil) on NotFound so callers fall through to the hardcoded catalog.
func TestInstanceTypeReaderFromClient(t *testing.T) {
	ctx := context.Background()

	seeded := instanceTypeSized("1000m", "2048Mi")
	seeded.Name = "datumcloud-d1-standard-2"

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(seeded).
		Build()

	read := instanceTypeReaderFromClient(cl)

	t.Run("found", func(t *testing.T) {
		got, err := read(ctx, "datumcloud-d1-standard-2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected the published instanceType, got nil")
		}
		if cpu := got.Spec.Resources.CPU.MilliValue(); cpu != 1000 {
			t.Errorf("CPU = %d millicores, want 1000", cpu)
		}
	})

	t.Run("not found returns nil so sizing falls back", func(t *testing.T) {
		got, err := read(ctx, "datumcloud-absent-type")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil for an absent type, got %v", got)
		}
	})

	// A cluster whose API server does not serve the InstanceType kind (its CRD
	// not installed yet) must fall back to the catalog like a missing object,
	// not fail every Pod build.
	t.Run("kind not served returns nil so sizing falls back", func(t *testing.T) {
		noKind := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return &meta.NoKindMatchError{
						GroupKind: computev1alpha.GroupVersion.WithKind("InstanceType").GroupKind(),
					}
				},
			}).
			Build()

		got, err := instanceTypeReaderFromClient(noKind)(ctx, "datumcloud-d1-standard-2")
		if err != nil {
			t.Fatalf("a cluster without the InstanceType kind must not fail sizing, got %v", err)
		}
		if got != nil {
			t.Errorf("expected nil when the kind is not served, got %v", got)
		}
	})
}

// TestBuildPodSpecFromContainers_InstanceTypeSizing verifies that
// buildPodSpecFromContainers sets the instanceType catalog values as Limits
// on the downstream Pod container when the instance is sized by instanceType
// only, reading the published InstanceType object from the client. This ensures
// the Pod footprint equals what the quota claim accounts for.
func TestBuildPodSpecFromContainers_InstanceTypeSizing(t *testing.T) {
	ctx := context.Background()

	published := instanceTypeSized("1000m", "2048Mi")
	published.Name = "datumcloud-d1-standard-2"
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(published).
		Build()
	r := &InstanceReconciler{Client: cl}

	instance := &computev1alpha.Instance{
		Spec: computev1alpha.InstanceSpec{
			Runtime: computev1alpha.InstanceRuntimeSpec{
				Resources: computev1alpha.InstanceRuntimeResources{
					InstanceType: "datumcloud-d1-standard-2",
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

	gotCPULimit := c.Resources.Limits[corev1.ResourceCPU]
	if gotCPULimit.Cmp(wantCPU) != 0 {
		t.Errorf("CPU Limit = %s, want %s", gotCPULimit.String(), wantCPU.String())
	}
	gotMemLimit := c.Resources.Limits[corev1.ResourceMemory]
	if gotMemLimit.Cmp(wantMem) != 0 {
		t.Errorf("Memory Limit = %s, want %s", gotMemLimit.String(), wantMem.String())
	}
	assertZeroRequestsForEveryLimit(t, c)
}

func TestBuildPodSpecFromContainers_ZeroRequests(t *testing.T) {
	ctx := context.Background()
	// A client is required: instanceType sizing reads the published
	// InstanceType object first. None is seeded, so sizing falls back to the
	// hardcoded catalog.
	r := &InstanceReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}

	tests := []struct {
		name         string
		instanceType string
		limits       corev1.ResourceList
		wantLimits   corev1.ResourceList
	}{
		{
			name:         "instanceType sizing",
			instanceType: "datumcloud/d1-standard-2",
			wantLimits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
		},
		{
			name: "explicit limits",
			limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			wantLimits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
		{
			name: "memory only",
			limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			wantLimits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := computev1alpha.SandboxContainer{
				Name:  "app",
				Image: "index.unikraft.io/datum/myapp:latest",
			}
			if tc.limits != nil {
				sc.Resources = &computev1alpha.ContainerResourceRequirements{Limits: tc.limits}
			}
			instance := &computev1alpha.Instance{
				Spec: computev1alpha.InstanceSpec{
					Runtime: computev1alpha.InstanceRuntimeSpec{
						Resources: computev1alpha.InstanceRuntimeResources{InstanceType: tc.instanceType},
						Sandbox: &computev1alpha.SandboxRuntime{
							Containers: []computev1alpha.SandboxContainer{sc},
						},
					},
				},
			}

			spec, err := r.buildPodSpecFromContainers(ctx, instance, instance.Spec.Runtime.Sandbox.Containers)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			c := spec.Containers[0]

			if len(c.Resources.Limits) != len(tc.wantLimits) {
				t.Errorf("Limits = %v, want %v", c.Resources.Limits, tc.wantLimits)
			}
			for name, want := range tc.wantLimits {
				got, ok := c.Resources.Limits[name]
				if !ok || got.Cmp(want) != 0 {
					t.Errorf("%s Limit = %s, want %s", name, got.String(), want.String())
				}
			}
			assertZeroRequestsForEveryLimit(t, c)
		})
	}
}

func assertZeroRequestsForEveryLimit(t *testing.T, c corev1.Container) {
	t.Helper()
	if len(c.Resources.Requests) != len(c.Resources.Limits) {
		t.Errorf("Requests = %v, want one zero entry per Limit %v", c.Resources.Requests, c.Resources.Limits)
	}
	for name := range c.Resources.Limits {
		got, ok := c.Resources.Requests[name]
		if !ok {
			t.Errorf("%s Request absent; the API server would default it to the Limit", name)
			continue
		}
		if !got.IsZero() {
			t.Errorf("%s Request = %s, want 0", name, got.String())
		}
	}
}

// TestBuildPodSpecFromContainers_ExplicitLimitsPreserved verifies that explicit
// container Limits are not overridden by the instanceType catalog, so a workload
// with custom sizing is programmed at its declared footprint.
func TestBuildPodSpecFromContainers_ExplicitLimitsPreserved(t *testing.T) {
	ctx := context.Background()
	r := &InstanceReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}

	instance := &computev1alpha.Instance{
		Spec: computev1alpha.InstanceSpec{
			Runtime: computev1alpha.InstanceRuntimeSpec{
				Resources: computev1alpha.InstanceRuntimeResources{
					InstanceType: "datumcloud-d1-standard-2",
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
