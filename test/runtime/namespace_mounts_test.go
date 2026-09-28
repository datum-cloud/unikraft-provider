// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// A private /host/run mount retains copies of existing namespace mounts
// across CNI cleanup. Keep both views in sync without allowing writes or
// mount propagation back to the host through the Multus socket's parent.
func TestRemoteCNINamespaceMounts(t *testing.T) {
	data, err := os.ReadFile("../../config/components/ukp-remote-cni/daemonset.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var daemonSet appsv1.DaemonSet
	if err := yaml.UnmarshalStrict(data, &daemonSet); err != nil {
		t.Fatal(err)
	}
	var container *corev1.Container
	for i := range daemonSet.Spec.Template.Spec.Containers {
		candidate := &daemonSet.Spec.Template.Spec.Containers[i]
		if candidate.Name == "remote-cni" {
			container = candidate
		}
	}
	if container == nil {
		t.Fatal("remote-cni container not found")
	}
	for _, want := range []struct {
		name, path, source string
		readOnly           bool
		propagation        corev1.MountPropagationMode
	}{
		{"host-run", "/host/run", "/run", true, corev1.MountPropagationHostToContainer},
		{"run-netns", "/run/netns", "/run/netns", false, corev1.MountPropagationBidirectional},
	} {
		t.Run(want.name, func(t *testing.T) {
			foundMount, foundVolume := false, false
			for _, mount := range container.VolumeMounts {
				if mount.Name != want.name {
					continue
				}
				foundMount = true
				if mount.MountPath != want.path || mount.ReadOnly != want.readOnly ||
					mount.MountPropagation == nil || *mount.MountPropagation != want.propagation {
					t.Errorf("incorrect namespace mount: %+v", mount)
				}
			}
			for _, volume := range daemonSet.Spec.Template.Spec.Volumes {
				if volume.Name == want.name && volume.HostPath != nil && volume.HostPath.Path == want.source {
					foundVolume = true
				}
			}
			if !foundMount || !foundVolume {
				t.Fatal("required host namespace mount or volume missing")
			}
		})
	}
}
