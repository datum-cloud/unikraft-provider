package controller

import (
	"go.datum.net/compute/api/v1alpha"
)

func mapContainerMemory(container *v1alpha.SandboxContainer) int64 {
	if container == nil || container.Resources == nil || container.Resources.Limits == nil || container.Resources.Limits.Memory().IsZero() {
		return int64(defaultInstanceMemoryMB)
	}

	memBytes := container.Resources.Limits.Memory().Value()
	return memBytes / (1024 * 1024)
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

// optionalPtr parses val using parser and returns a pointer to the result.
// Returns nil (no error) when val is empty. Used by scale-to-zero annotation
// parsing when that path is re-wired on the Kraftlet Pod path.
//
//nolint:unused
func optionalPtr[T any](val string, parser func(string) (T, error)) (*T, error) {
	if len(val) == 0 {
		return nil, nil
	}

	v, err := parser(val)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

