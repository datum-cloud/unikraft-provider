// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/unikraft-provider/internal/config"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func guestDNSTestConfig() *config.InstanceDNSConfig {
	return &config.InstanceDNSConfig{
		InitializeGuest: true,
		Nameservers:     []string{"2606:4700:4700::1111", "2001:4860:4860::8888"},
		Searches:        []string{"example.internal"},
	}
}

func TestGuestDNSStartup(t *testing.T) {
	inst := instanceWithVolumes()
	c := &inst.Spec.Runtime.Sandbox.Containers[0]
	c.Command = []string{"/app/start", "command argument"}
	c.Args = []string{"", "a b", "$(touch /tmp/unwanted)", "'quoted'", "line\nbreak"}
	inst.Spec.Runtime.Sandbox.Containers = append(inst.Spec.Runtime.Sandbox.Containers, *c.DeepCopy())
	inst.Spec.Runtime.Sandbox.Containers[1].Name = "second"
	original := inst.DeepCopy()
	for _, enabled := range []bool{false, true} {
		dns := guestDNSTestConfig()
		dns.InitializeGuest = enabled
		r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{InstanceDNS: dns})
		spec, err := r.buildPodSpecFromContainers(context.Background(), inst, inst.Spec.Runtime.Sandbox.Containers)
		if err != nil {
			t.Fatal(err)
		}
		for i, container := range spec.Containers {
			wantCommand := original.Spec.Runtime.Sandbox.Containers[i].Command
			if enabled {
				wantCommand = append([]string{"/bin/sh", "/run/datum-dns/start.sh"}, wantCommand...)
			}
			if !reflect.DeepEqual(container.Command, wantCommand) || !reflect.DeepEqual(container.Args, original.Spec.Runtime.Sandbox.Containers[i].Args) {
				t.Fatalf("enabled=%v: command or args changed: %#v %#v", enabled, container.Command, container.Args)
			}
			if enabled {
				mount := container.VolumeMounts[len(container.VolumeMounts)-1]
				if mount.MountPath != "/run/datum-dns" || !mount.ReadOnly || mount.SubPath != "" {
					t.Fatalf("unexpected mount: %+v", mount)
				}
			}
		}
		wantVolumes := len(original.Spec.Volumes)
		if enabled {
			wantVolumes++
		}
		if len(spec.Volumes) != wantVolumes {
			t.Fatalf("got %d volumes, want %d", len(spec.Volumes), wantVolumes)
		}
	}
	if !reflect.DeepEqual(inst, original) {
		t.Fatal("mutated Instance")
	}
	cm := guestDNSConfigMap(inst, guestDNSTestConfig())
	if got, want := cm.Data["resolv.conf"], "nameserver 2606:4700:4700::1111\nnameserver 2001:4860:4860::8888\nsearch example.internal\n"; got != want {
		t.Fatalf("resolver = %q, want %q", got, want)
	}
}

func TestGuestDNSRejectsIncompatibleContainers(t *testing.T) {
	cases := map[string]core.PodSpec{
		"explicit command": {Containers: []core.Container{{Name: "app"}}},
		"reserved":         {Volumes: []core.Volume{{Name: guestDNSVolume}}},
	}
	for _, mount := range []string{"/", "/run", "/run/datum-dns/", "/run/datum-dns/start.sh", "/etc", "/etc/resolv.conf", "/etc/../etc/resolv.conf"} {
		cases["conflicts "+mount] = core.PodSpec{Containers: []core.Container{{Name: "app", Command: []string{"/app"}, VolumeMounts: []core.VolumeMount{{MountPath: mount}}}}}
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			err := initializeGuestDNS(&spec, "dns")
			if err == nil || !strings.Contains(err.Error(), strings.Fields(name)[0]) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestGuestDNSReconcile(t *testing.T) {
	ctx := context.Background()
	inst := instanceWithUID("guest-dns-reconcile")
	inst.Spec.Runtime.Sandbox.Containers[0].Command = []string{"/app"}
	dns := guestDNSTestConfig()
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst).WithStatusSubresource(&computev1alpha.Instance{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if pod, ok := obj.(*core.Pod); ok {
				var cm core.ConfigMap
				if err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Spec.Volumes[0].ConfigMap.Name}, &cm); err != nil {
					t.Fatalf("DNS must exist before Pod creation: %v", err)
				}
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	r := reconcilerWithConfig(config.DownstreamResourceManagementConfig{InstanceDNS: dns})
	r.Client, r.APIReader, r.Scheme = cl, cl, scheme
	for range 2 {
		if _, err := r.reconcileSandboxContainers(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	var pod core.Pod
	if err := cl.Get(ctx, client.ObjectKeyFromObject(inst), &pod); err != nil {
		t.Fatal(err)
	}
	var cm core.ConfigMap
	if err := cl.Get(ctx, client.ObjectKey{Namespace: inst.Namespace, Name: pod.Spec.Volumes[0].ConfigMap.Name}, &cm); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&cm, inst) || cm.Immutable == nil || !*cm.Immutable {
		t.Fatalf("DNS ConfigMap must be immutable and owned: %+v", cm.ObjectMeta)
	}
	// The API reader handles a retry after ConfigMap creation but before Pod creation.
	if err := r.ensureGuestDNSConfigMap(ctx, inst, dns); err != nil {
		t.Fatal(err)
	}
	// Real API servers set this timestamp; the fake client does not.
	pod.CreationTimestamp = metav1.Now()
	if err := cl.Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	before := pod.Spec.DeepCopy()
	dns.Nameservers = []string{"2001:4860:4860::8844"}
	if _, err := r.reconcileSandboxContainers(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(inst), &pod); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, &pod.Spec) {
		t.Fatal("configuration change altered existing Pod")
	}
	var cms core.ConfigMapList
	if err := cl.List(ctx, &cms); err != nil {
		t.Fatal(err)
	}
	if len(cms.Items) != 1 {
		t.Fatal("existing Pod created an unused DNS ConfigMap")
	}
}

func TestGuestDNSConfigMapCollision(t *testing.T) {
	for _, collision := range []string{"ownership", "contents", "mutable"} {
		t.Run(collision, func(t *testing.T) {
			inst := instanceWithUID("guest-dns-owner")
			dns := guestDNSTestConfig()
			scheme := testScheme(t)
			cl := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := &InstanceReconciler{Client: cl, APIReader: cl, Scheme: scheme}
			if err := r.ensureGuestDNSConfigMap(context.Background(), inst, dns); err != nil {
				t.Fatal(err)
			}
			cm := guestDNSConfigMap(inst, dns)
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(cm), cm); err != nil {
				t.Fatal(err)
			}
			switch collision {
			case "ownership":
				cm.OwnerReferences = nil
			case "contents":
				cm.Data["start.sh"] = "wrong"
			case "mutable":
				cm.Immutable = nil
			}
			if err := cl.Update(context.Background(), cm); err != nil {
				t.Fatal(err)
			}
			if err := r.ensureGuestDNSConfigMap(context.Background(), inst, dns); err == nil {
				t.Fatal("accepted conflicting ConfigMap")
			}
			var after core.ConfigMap
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(cm), &after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cm.Data, after.Data) || !reflect.DeepEqual(cm.OwnerReferences, after.OwnerReferences) {
				t.Fatal("overwrote conflicting ConfigMap")
			}
		})
	}
}
