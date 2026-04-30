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

func assignContainerServices(ports []v1alpha.NamedPort) []platform.Service {
	services := make([]platform.Service, 0)
	for _, port := range ports {
		services = append(services, platform.Service{
			DestinationPort: ptr.Ptr(uint32(port.Port)),
			Port:            443,
			Handlers:        []platform.ServiceHandlers{platform.ServiceHandlersHttp, platform.ServiceHandlersTls},
		})
	}

	services = append(services, platform.Service{
		DestinationPort: ptr.Ptr(uint32(443)),
		Port:            80,
		Handlers:        []platform.ServiceHandlers{platform.ServiceHandlersHttp, platform.ServiceHandlersRedirect},
	})

	return services
}

func mapInstanceScaleToZero(annotations map[string]string, services []platform.Service) (*platform.CreateInstanceRequestScaleToZero, error) {
	if len(services) == 0 {
		return &platform.CreateInstanceRequestScaleToZero{
			Policy: ptr.Ptr(platform.CreateInstanceRequestScaleToZeroPolicyOff),
		}, nil
	}

	policyValue, hasPolicy := annotations[ukcScaleToZeroPolicyAnnotation]
	statefulValue, hasStateful := annotations[ukcScaleToZeroStatefulAnnotation]
	coolDownValue, hasCoolDown := annotations[ukcScaleToZeroCoolDownTimeMsAnnotation]

	res := &platform.CreateInstanceRequestScaleToZero{
		Policy:         ptr.Ptr(defaultScaleToZeroPolicy),
		Stateful:       ptr.Ptr(defaultScaleToZeroStateful),
		CooldownTimeMs: ptr.Ptr(defaultScaleToZeroCooldownTimeMs),
	}

	var err error

	if hasPolicy {
		res.Policy, err = optionalPtr(policyValue, func(s string) (platform.CreateInstanceRequestScaleToZeroPolicy, error) {
			return platform.CreateInstanceRequestScaleToZeroPolicy(policyValue), nil
		})
		if err != nil {
			return nil, err
		}
	}

	if hasStateful {
		res.Stateful, err = optionalPtr(statefulValue, func(s string) (bool, error) {
			return strconv.ParseBool(statefulValue)
		})
		if err != nil {
			return nil, fmt.Errorf("error parsing stateful scale to zero annotation as bool: %s", err)
		}
	}

	if hasCoolDown {
		res.CooldownTimeMs, err = optionalPtr(coolDownValue, func(s string) (int32, error) {
			n, err := strconv.ParseInt(s, 10, 32)
			return int32(n), err
		})
		if err != nil {
			return nil, fmt.Errorf("error parsing scale to zero cooldown annotation as int: %s", err)
		}
	}

	return res, nil
}

func mapInstanceRoms(instance *v1alpha.Instance) []platform.CreateInstanceRequestRom {
	if instance == nil {
		return nil
	}

	roms := make([]platform.CreateInstanceRequestRom, 0)

	for _, volume := range instance.Spec.Volumes {
		if volume.ConfigMap != nil {
			roms = append(roms, platform.CreateInstanceRequestRom{
				Name:  ptr.Ptr(volume.Name),
				Image: volume.ConfigMap.Name,
			})
		}
	}

	return roms
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
