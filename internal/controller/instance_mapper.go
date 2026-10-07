package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.datum.net/compute/api/v1alpha"
	core "k8s.io/api/core/v1"
)

// instanceTypeReader fetches the InstanceType object the instance selects from
// the cluster the provider runs in. It returns (nil, nil) when the type is not
// found there, so sizing falls back to the hardcoded catalog. A non-nil error
// is transient and fails the Pod build rather than silently sizing the Pod
// below the footprint the compute controller claimed against.
type instanceTypeReader func(ctx context.Context, name string) (*v1alpha.InstanceType, error)

// resolveContainerResources returns the CPU (millicores) and memory (MiB) to
// set on the downstream Pod container for the given Instance container.
//
// Each dimension (CPU, memory) is resolved independently so that a container
// that sets only a memory limit still gets its explicit memory honoured while
// the catalog supplies CPU (and vice versa). Precedence per dimension:
//  1. Explicit container Limits for that dimension — always wins.
//  2. The published InstanceType object the instance selects, read live via
//     readInstanceType. This is the same catalog the compute controller sizes
//     quota claims from, so the Pod runs at the footprint that was billed.
//  3. The hardcoded catalog (instancetype_catalog.go — a temporary local copy
//     of go.datum.net/compute/pkg/instancetype, see the TODO there) — covers a
//     type not yet available through the reader (and installs that read none).
//  4. Legacy defaults — defaultInstanceMemoryMB for memory; zero (unset) for
//     CPU. Preserves prior behaviour for unknown or empty instanceType.
//
// A transient failure reading the InstanceType returns a non-nil error: the
// Pod must never be programmed at a smaller size than the quota claim assumed.
func resolveContainerResources(
	ctx context.Context,
	instance *v1alpha.Instance,
	container *v1alpha.SandboxContainer,
	readInstanceType instanceTypeReader,
) (cpuMillicores, memoryMiB int64, err error) {
	// Collect any explicit Limits from the container spec.
	var explicitCPUMillicores, explicitMemMiB int64
	if container != nil && container.Resources != nil && container.Resources.Limits != nil {
		if cpu := container.Resources.Limits.Cpu(); cpu != nil && !cpu.IsZero() {
			explicitCPUMillicores = cpu.MilliValue()
		}
		if mem := container.Resources.Limits.Memory(); mem != nil && !mem.IsZero() {
			explicitMemMiB = bytesToRoundedMiB(mem.Value())
		}
	}

	// Explicit values win outright; look up the type catalog only for missing ones.
	if explicitCPUMillicores > 0 && explicitMemMiB > 0 {
		return explicitCPUMillicores, explicitMemMiB, nil
	}

	// The published InstanceType object, when a type is selected and a reader is
	// available. A transient read failure is propagated so the reconcile fails
	// rather than under-size the Pod; a missing type falls through to the
	// hardcoded catalog below.
	var catalogCPU, catalogMem int64
	resolved := false
	if readInstanceType != nil && instance != nil {
		if it := instance.Spec.Runtime.Resources.InstanceType; it != "" {
			t, rerr := readInstanceType(ctx, canonicalInstanceTypeName(it))
			if rerr != nil {
				return 0, 0, fmt.Errorf("reading InstanceType %q: %w", it, rerr)
			}
			if t != nil {
				catalogCPU = t.Spec.Resources.CPU.MilliValue()
				catalogMem = bytesToRoundedMiB(t.Spec.Resources.Memory.Value())
				// A type published with a zero dimension is invalid; fall through
				// to the hardcoded catalog rather than size partially from it.
				resolved = catalogCPU > 0 && catalogMem > 0
			}
		}
	}

	// TODO(instance-type-catalog): two things to delete once compute's
	// instance-type-catalog work (datum-cloud/compute#333) merges and a
	// tagged compute release incorporates it:
	//  1. This hardcoded lookup table, once the live InstanceType CRD read
	//     path above is proven in production. It only covers names the
	//     reader does not hold yet (old installs, types not projected into
	//     this cell); once every cell reliably serves InstanceType objects,
	//     sizing must come from a single source — the published catalog —
	//     and could otherwise drift from the footprint compute claims
	//     against quota.
	//  2. lookupHardcodedInstanceType itself (instancetype_catalog.go), a
	//     temporary local copy of instancetype.Lookup kept only because this
	//     repo has no stable compute release to import that package from
	//     yet. Switch this call back to instancetype.Lookup(it) then.
	if !resolved && instance != nil {
		if it := instance.Spec.Runtime.Resources.InstanceType; it != "" {
			if sizing, ok := lookupHardcodedInstanceType(it); ok {
				catalogCPU, catalogMem = sizing.cpuMillicores, sizing.memoryMiB
				resolved = true
			} else {
				return 0, 0, fmt.Errorf("instance type %q not found in published or fallback catalog", it)
			}
		}
	}

	// Merge: explicit limit wins per-dimension; a resolved catalog fills gaps;
	// legacy default covers memory when neither source has a value.
	resolvedCPU := explicitCPUMillicores
	if resolvedCPU == 0 {
		resolvedCPU = catalogCPU // zero when no source resolved (no fabricated value)
	}

	resolvedMem := explicitMemMiB
	if resolvedMem == 0 {
		if resolved {
			resolvedMem = catalogMem
		} else {
			resolvedMem = int64(defaultInstanceMemoryMB) // legacy fallback
		}
	}

	return resolvedCPU, resolvedMem, nil
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

// bytesToRoundedMiB converts bytes to Mebibytes (MiB) by rounding up.
// Integer division truncates decimals toward zero (e.g. 953.67 -> 953).
// By adding (divisor - 1) before dividing, we implement a ceiling function using pure
// integer math. This ensures that instance memory is never under-sized when
// users request values that do not divide evenly by 1024*1024 (like "1000M").
func bytesToRoundedMiB(bytes int64) int64 {
	return (bytes + 1024*1024 - 1) / (1024 * 1024)
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
