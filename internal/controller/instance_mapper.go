package controller

import (
	"fmt"
	"strconv"
	"strings"

	"go.datum.net/compute/api/v1alpha"
	core "k8s.io/api/core/v1"
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

// runtimeImagePullFailed is the runtime's text for a failed image pull.
const runtimeImagePullFailed = "image pull failed"

// containerStartFailure reports the first container the runtime is failing to
// start. Transient states such as ContainerCreating are not failures.
func containerStartFailure(instance *v1alpha.Instance, pod *core.Pod) (reason, message string, failing bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && strings.Contains(t.Message, runtimeImagePullFailed) {
			return v1alpha.InstanceProgrammedReasonImageUnavailable, imagePullFailure(instance, cs), true
		}
		if cs.State.Waiting == nil {
			continue
		}
		reason, _ := translateWaitingReason(cs.State.Waiting.Reason, cs.State.Waiting.Message)
		switch reason {
		case v1alpha.InstanceProgrammedReasonImageUnavailable:
			return reason, imagePullFailure(instance, cs), true
		case v1alpha.InstanceProgrammedReasonInstanceCrashing:
			message := fmt.Sprintf("Container %q keeps exiting", cs.Name)
			if t := cs.LastTerminationState.Terminated; t != nil {
				message += fmt.Sprintf(" (last exit code %d)", t.ExitCode)
			}
			return reason, message + "; restarting", true
		case v1alpha.InstanceProgrammedReasonConfigurationError:
			return reason, fmt.Sprintf("Container %q could not be started due to a configuration error; retrying", cs.Name), true
		}
	}
	return "", "", false
}

// imagePullFailure names the image as the user wrote it (the runtime reports a
// rewritten form) and the pull secrets it was tried with.
func imagePullFailure(instance *v1alpha.Instance, cs core.ContainerStatus) string {
	image := cs.Image
	var secrets []string
	if sandbox := instance.Spec.Runtime.Sandbox; sandbox != nil {
		for _, c := range sandbox.Containers {
			if c.Name == cs.Name {
				image = c.Image
			}
		}
		for _, ref := range sandbox.ImagePullSecrets {
			secrets = append(secrets, strconv.Quote(ref.Name))
		}
	}
	message := fmt.Sprintf("Image %q for container %q could not be pulled", image, cs.Name)
	if len(secrets) == 0 {
		return message + " anonymously"
	}
	return message + " using credentials from " + strings.Join(secrets, ", ")
}
