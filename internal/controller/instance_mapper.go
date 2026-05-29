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

