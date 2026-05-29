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

