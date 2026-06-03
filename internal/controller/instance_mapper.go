package controller

import (
	"go.datum.net/compute/api/v1alpha"
)

// ukcInstanceTypeSpec holds the vCPU and memory dimensions for a named
// platform instance type as seen by the unikraft provider.
type ukcInstanceTypeSpec struct {
	// cpuMillicores is the number of CPU millicores (1000 = 1 vCPU).
	cpuMillicores int64
	// memoryMiB is the amount of RAM in mebibytes.
	memoryMiB int64
}

// ukcInstanceTypeCatalog maps platform instance type names to their resource
// dimensions. These values must stay in sync with the compute controller's
// instanceTypeCatalog (internal/controller/instance_controller.go) — the two
// catalogs are the single source of truth for what quota claims and what the
// running Pod actually receives. When new instance types are added, update
// both catalogs with the same values.
//
// datumcloud/d1-standard-2: 1 vCPU, 2 GiB RAM.
var ukcInstanceTypeCatalog = map[string]ukcInstanceTypeSpec{
	"datumcloud/d1-standard-2": {
		cpuMillicores: 1000, // 1 vCPU
		memoryMiB:     2048, // 2 GiB
	},
}

// resolveContainerResources returns the CPU (millicores) and memory (MiB) to
// set on the downstream Pod container for the given Instance container.
//
// Each dimension (CPU, memory) is resolved independently so that a container
// that sets only a memory limit still gets its explicit memory honoured while
// the catalog supplies CPU (and vice versa). Precedence per dimension:
//  1. Explicit container Limits for that dimension — always wins.
//  2. instanceType catalog — used when the instance is sized by instanceType
//     only, without an explicit limit for that dimension. This is the common
//     production case and ensures the Pod receives the same resource footprint
//     that the quota claim accounts for.
//  3. Legacy defaults — defaultInstanceMemoryMB for memory; zero (unset) for
//     CPU. Preserves prior behaviour for unknown or empty instanceType.
func resolveContainerResources(instance *v1alpha.Instance, container *v1alpha.SandboxContainer) (cpuMillicores, memoryMiB int64) {
	// Collect any explicit Limits from the container spec.
	var explicitCPUMillicores, explicitMemMiB int64
	if container != nil && container.Resources != nil && container.Resources.Limits != nil {
		if cpu := container.Resources.Limits.Cpu(); cpu != nil && !cpu.IsZero() {
			explicitCPUMillicores = cpu.MilliValue()
		}
		if mem := container.Resources.Limits.Memory(); mem != nil && !mem.IsZero() {
			explicitMemMiB = mem.Value() / (1024 * 1024)
		}
	}

	// Explicit values win outright; look up the catalog only for missing ones.
	if explicitCPUMillicores > 0 && explicitMemMiB > 0 {
		return explicitCPUMillicores, explicitMemMiB
	}

	// Attempt catalog lookup for any dimension not covered by explicit limits.
	var catalogCPU, catalogMem int64
	if instance != nil {
		it := instance.Spec.Runtime.Resources.InstanceType
		if it != "" {
			if spec, ok := ukcInstanceTypeCatalog[it]; ok {
				catalogCPU = spec.cpuMillicores
				catalogMem = spec.memoryMiB
			}
		}
	}

	// Merge: explicit limit wins per-dimension; catalog fills gaps; legacy
	// default covers memory when neither source has a value.
	resolvedCPU := explicitCPUMillicores
	if resolvedCPU == 0 {
		resolvedCPU = catalogCPU // zero when instanceType is unknown (no fabricated value)
	}

	resolvedMem := explicitMemMiB
	if resolvedMem == 0 {
		if catalogMem > 0 {
			resolvedMem = catalogMem
		} else {
			resolvedMem = int64(defaultInstanceMemoryMB) // legacy fallback
		}
	}

	return resolvedCPU, resolvedMem
}

// translateWaitingReason converts a raw Kubernetes container waiting reason and
// message into Instance-domain reason and message strings. Users should never
// see Kubernetes-internal jargon (ImagePullBackOff, CrashLoopBackOff, etc.) in
// their Instance status; this function ensures all waiting states are expressed
// in platform terms.
//
// The caller is responsible for logging the original k8s reason and message at
// a debug/info level for operator visibility before calling this function.
func translateWaitingReason(k8sReason, _ string) (reason, message string) {
	switch k8sReason {
	case "ImagePullBackOff", "ErrImagePull", "ImageInspectError",
		"InvalidImageName", "RegistryUnavailable":
		return "ImageUnavailable", "The instance image could not be pulled"
	case "CrashLoopBackOff":
		return "InstanceCrashing", "The instance is repeatedly failing to start"
	case "CreateContainerError", "CreateContainerConfigError":
		return "ConfigurationError", "The instance could not be started due to a configuration error"
	case "ContainerCreating", "PodInitializing":
		return "Provisioning", "Instance is provisioning"
	default:
		return "Provisioning", "Instance is provisioning"
	}
}
