package controller

import (
	"fmt"
	"strconv"

	"go.datum.net/compute/api/v1alpha"
	"unikraft.com/cloud/sdk/pkg/ptr"

	"github.com/unikraft-cloud/k8s-operator/api/v1alpha1/platform"
)

func mapContainerMemory(container *v1alpha.SandboxContainer) int64 {
	if container == nil || container.Resources == nil || container.Resources.Limits == nil || container.Resources.Limits.Memory().IsZero() {
		return int64(defaultInstanceMemoryMB)
	}

	memBytes := container.Resources.Limits.Memory().Value()
	return memBytes / (1024 * 1024)
}

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
