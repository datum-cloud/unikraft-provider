// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"strings"
	"testing"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestInstanceServiceName(t *testing.T) {
	long := strings.Repeat("a", 70)

	tests := []struct {
		name      string
		instance  string
		unchanged bool
	}{
		{name: "valid name is unchanged", instance: "web-a1b2c3d4e5-0", unchanged: true},
		{name: "max length name is unchanged", instance: strings.Repeat("a", 63), unchanged: true},
		{name: "digit-leading name", instance: "1password-sync-3f9a0c1b2d-0"},
		{name: "name over 63 characters", instance: long},
		{name: "digit-leading name over 63 characters", instance: "9" + long},
		{name: "subdomain name with dots", instance: "web.example-0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := instanceServiceName(tt.instance)

			if errs := validation.IsDNS1035Label(got); len(errs) > 0 {
				t.Fatalf("instanceServiceName(%q) = %q, not a DNS-1035 label: %v", tt.instance, got, errs)
			}
			if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
				t.Fatalf("instanceServiceName(%q) = %q, not a valid label value: %v", tt.instance, got, errs)
			}
			if tt.unchanged && got != tt.instance {
				t.Errorf("instanceServiceName(%q) = %q, want unchanged", tt.instance, got)
			}
			if !tt.unchanged && got == tt.instance {
				t.Errorf("instanceServiceName(%q) returned the invalid name unchanged", tt.instance)
			}
			if again := instanceServiceName(tt.instance); again != got {
				t.Errorf("instanceServiceName(%q) is not deterministic: %q then %q", tt.instance, got, again)
			}
		})
	}
}

func TestInstanceServiceName_KeepsReadablePrefix(t *testing.T) {
	got := instanceServiceName("1password-sync-3f9a0c1b2d-0")
	if !strings.HasPrefix(got, "i-1password-sync-3f9a0c1b2d-0-") {
		t.Errorf("instanceServiceName = %q, want the Instance name kept behind a letter prefix", got)
	}
}

func TestInstanceServiceName_DistinctInputsStayDistinct(t *testing.T) {
	long := strings.Repeat("a", 70)
	pairs := [][2]string{
		{long + "-0", long + "-1"},
		{"0abc", "i-0abc"},
		{"web.a", "web-a"},
	}
	for _, p := range pairs {
		if a, b := instanceServiceName(p[0]), instanceServiceName(p[1]); a == b {
			t.Errorf("instanceServiceName(%q) and instanceServiceName(%q) collide on %q", p[0], p[1], a)
		}
	}
}

func TestInstanceLabelValue(t *testing.T) {
	for _, name := range []string{"web-a1b2c3d4e5-0", "1password-sync-3f9a0c1b2d-0", strings.Repeat("a", 63)} {
		if got := instanceLabelValue(name); got != name {
			t.Errorf("instanceLabelValue(%q) = %q, want unchanged", name, got)
		}
	}

	long := strings.Repeat("a", 70)
	got := instanceLabelValue(long)
	if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
		t.Fatalf("instanceLabelValue(%q) = %q, not a valid label value: %v", long, got, errs)
	}
	if again := instanceLabelValue(long); again != got {
		t.Errorf("instanceLabelValue(%q) is not deterministic: %q then %q", long, got, again)
	}
}

// TestDigitLeadingInstanceGetsValidService reconciles an Instance whose name
// starts with a digit and checks the Service it gets is a valid name that
// still selects the Instance's Pod.
func TestDigitLeadingInstanceGetsValidService(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	instance := instanceWithPortsAndUID("digit-leading-uid")
	instance.Name = "1password-sync-3f9a0c1b2d-0"

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}
	if _, err := r.reconcileSandboxContainers(ctx, instance); err != nil {
		t.Fatalf("reconcileSandboxContainers returned error: %v", err)
	}

	var pod core.Pod
	if err := cl.Get(ctx, client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}, &pod); err != nil {
		t.Fatalf("failed to get pod: %v", err)
	}

	var svc core.Service
	svcKey := client.ObjectKey{Name: instanceServiceName(instance.Name), Namespace: instance.Namespace}
	if err := cl.Get(ctx, svcKey, &svc); err != nil {
		t.Fatalf("failed to get service: %v", err)
	}
	if errs := validation.IsDNS1035Label(svc.Name); len(errs) > 0 {
		t.Errorf("service name %q is not a DNS-1035 label: %v", svc.Name, errs)
	}
	for k, v := range svc.Spec.Selector {
		if pod.Labels[k] != v {
			t.Errorf("service selector %s=%q does not match pod label %q", k, v, pod.Labels[k])
		}
	}
	if len(svc.Spec.Selector) == 0 {
		t.Error("service has no selector")
	}
}

// TestHandleDeletion_DigitLeadingInstance_DeletesDerivedService verifies that
// deletion looks the Service up by its derived name, so the Service is removed
// and cannot hold the finalizer forever.
func TestHandleDeletion_DigitLeadingInstance_DeletesDerivedService(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)

	now := metav1.Now()
	instance := instanceWithUID("digit-leading-delete-uid")
	instance.Name = "1password-sync-3f9a0c1b2d-0"
	instance.DeletionTimestamp = &now
	instance.Finalizers = []string{instanceFinalizer}

	svc := &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:       instanceServiceName(instance.Name),
			Namespace:  instance.Namespace,
			Finalizers: []string{"test/keep-terminating"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(instance, svc).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	r := &InstanceReconciler{Client: cl, Scheme: s}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Errorf("expected RequeueAfter > 0 while service is terminating, got %+v", result)
	}

	var gotSvc core.Service
	if err := cl.Get(ctx, client.ObjectKeyFromObject(svc), &gotSvc); err != nil {
		if apierrors.IsNotFound(err) {
			t.Fatal("service was removed despite its finalizer; expected it to be terminating")
		}
		t.Fatalf("failed to get service: %v", err)
	}
	if gotSvc.DeletionTimestamp.IsZero() {
		t.Error("expected the derived-name service to have been issued a delete")
	}
}
