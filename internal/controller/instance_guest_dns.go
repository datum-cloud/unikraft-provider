// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"reflect"
	"strings"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/unikraft-provider/internal/config"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	guestDNSVolume = "unikraft-guest-dns"
	guestDNSPath   = "/run/datum-dns"
	// Use a mounted script rather than sh -c: application arguments remain
	// arguments, including spaces and shell metacharacters. exec preserves
	// application signal handling and exit status.
	guestDNSStart = "#!/bin/sh\nset -eu\ncat " + guestDNSPath + "/resolv.conf > /etc/resolv.conf\nexec \"$@\"\n"
)

func guestDNSConfigMap(instance *computev1alpha.Instance, dns *config.InstanceDNSConfig) *core.ConfigMap {
	var resolver strings.Builder
	for _, ns := range dns.Nameservers {
		fmt.Fprintf(&resolver, "nameserver %s\n", ns)
	}
	if len(dns.Searches) > 0 {
		fmt.Fprintf(&resolver, "search %s\n", strings.Join(dns.Searches, " "))
	}
	// Version the file contents per Instance. A configuration change must not
	// change startup behavior for an existing Pod when its guest restarts.
	sum := sha256.Sum256([]byte(string(instance.UID) + "\x00" + instance.Namespace + "/" + instance.Name + "\x00" + resolver.String() + guestDNSStart))
	return &core.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("unikraft-dns-%x", sum[:16]),
			Namespace: instance.Namespace,
		},
		Immutable: ptr.To(true),
		Data: map[string]string{
			"resolv.conf": resolver.String(),
			"start.sh":    guestDNSStart,
		},
	}
}

func initializeGuestDNS(spec *core.PodSpec, configMapName string) error {
	for _, v := range spec.Volumes {
		if v.Name == guestDNSVolume {
			return fmt.Errorf("instanceDNS.initializeGuest: volume name %q is reserved", guestDNSVolume)
		}
	}
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if len(c.Command) == 0 || c.Command[0] == "" {
			return fmt.Errorf("instanceDNS.initializeGuest: container %q requires an explicit command; image-default entrypoints are not supported", c.Name)
		}
		for _, mount := range c.VolumeMounts {
			for _, reserved := range []string{guestDNSPath, "/etc/resolv.conf"} {
				if pathsOverlap(mount.MountPath, reserved) {
					return fmt.Errorf("instanceDNS.initializeGuest: container %q mount %q conflicts with %s", c.Name, mount.MountPath, reserved)
				}
			}
		}
		c.Command = append([]string{"/bin/sh", guestDNSPath + "/start.sh"}, c.Command...)
		c.VolumeMounts = append(c.VolumeMounts, core.VolumeMount{
			Name: guestDNSVolume, MountPath: guestDNSPath, ReadOnly: true,
		})
	}
	// kraftlet mounts ConfigMaps as directories; subPath cannot replace a
	// single file on the tested runtime. The wrapper copies the file instead.
	spec.Volumes = append(spec.Volumes, core.Volume{
		Name: guestDNSVolume,
		VolumeSource: core.VolumeSource{ConfigMap: &core.ConfigMapVolumeSource{
			LocalObjectReference: core.LocalObjectReference{Name: configMapName},
		}},
	})
	return nil
}

func pathsOverlap(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}

func (r *InstanceReconciler) ensureGuestDNSConfigMap(ctx context.Context, instance *computev1alpha.Instance, dns *config.InstanceDNSConfig) error {
	cm := guestDNSConfigMap(instance, dns)
	if err := controllerutil.SetControllerReference(instance, cm, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create guest DNS ConfigMap: %w", err)
		}
		// Read directly from the API, avoiding an informer over tenant data.
		var existing core.ConfigMap
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(cm), &existing); err != nil {
			return fmt.Errorf("get guest DNS ConfigMap: %w", err)
		}
		if !metav1.IsControlledBy(&existing, instance) || existing.DeletionTimestamp != nil ||
			!ptr.Deref(existing.Immutable, false) || !reflect.DeepEqual(existing.Data, cm.Data) || len(existing.BinaryData) != 0 {
			return fmt.Errorf("guest DNS ConfigMap %s already exists with different ownership or contents", cm.Name)
		}
	}
	return nil
}
