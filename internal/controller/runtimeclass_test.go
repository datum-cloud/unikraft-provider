// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/unikraft-provider/internal/config"
)

// TestInstanceCacheSelector verifies the selector the informer sends to the
// API server. The selector is the only thing deciding which Instances this
// provider handles, so it must exclude another provider's class while still
// matching an instance whose class is unset.
func TestInstanceCacheSelector(t *testing.T) {
	selector := InstanceCacheSelector(DefaultRuntimeClassName)

	if want := "spec.runtime.class!=general-purpose"; selector.String() != want {
		t.Errorf("selector = %q, want %q", selector.String(), want)
	}
	if selector.Empty() {
		t.Fatal("selector is empty, so the informer would list every Instance in the cell")
	}

	for _, tc := range []struct {
		name  string
		class string
		want  bool
	}{
		{name: "unset class", class: "", want: true},
		{name: "served class", class: "unikernel", want: true},
		{name: "another provider's class", class: "general-purpose", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := selector.Matches(fields.Set{computev1alpha.InstanceRuntimeClassField: tc.class})
			if got != tc.want {
				t.Errorf("selector.Matches(class=%q) = %v, want %v", tc.class, got, tc.want)
			}
		})
	}
}

// TestInstanceCacheSelectorExcludesEveryOtherClass verifies that the selector
// covers every class the provider does not serve, so adding a class to the
// platform cannot silently leave this provider claiming it.
func TestInstanceCacheSelectorExcludesEveryOtherClass(t *testing.T) {
	selector := InstanceCacheSelector(DefaultRuntimeClassName)

	for _, class := range otherRuntimeClasses {
		if selector.Matches(fields.Set{computev1alpha.InstanceRuntimeClassField: class}) {
			t.Errorf("selector matches class %q, which this provider does not serve", class)
		}
	}
}

// TestInstanceCacheSelectorForOtherServedClass verifies that a provider
// configured for another class does not exclude the class it serves.
func TestInstanceCacheSelectorForOtherServedClass(t *testing.T) {
	selector := InstanceCacheSelector("general-purpose")

	if !selector.Matches(fields.Set{computev1alpha.InstanceRuntimeClassField: "general-purpose"}) {
		t.Error("selector excludes the class the provider was configured to serve")
	}
}

// TestCacheOptionsScopesInstances verifies that the class filter reaches the
// informer as a field selector. A filter applied anywhere else still leaves
// the cache holding every Instance in the cell.
func TestCacheOptionsScopesInstances(t *testing.T) {
	opts := CacheOptions(DefaultRuntimeClassName)

	var byObject cache.ByObject
	var found bool
	for obj, cfg := range opts.ByObject {
		if _, isInstance := obj.(*computev1alpha.Instance); isInstance {
			byObject, found = cfg, true
			break
		}
	}
	if !found {
		t.Fatal("cache options do not scope Instances")
	}
	if byObject.Field == nil {
		t.Fatal("Instance cache has no field selector")
	}
	if got, want := byObject.Field.String(), InstanceCacheSelector(DefaultRuntimeClassName).String(); got != want {
		t.Errorf("Instance cache field selector = %q, want %q", got, want)
	}
	if byObject.Label != nil && !byObject.Label.Empty() {
		t.Errorf("Instance cache carries a label selector %q, want class selection by field only", byObject.Label)
	}
}

// TestServedRuntimeClass verifies that the served class comes from
// configuration and falls back to the platform default.
func TestServedRuntimeClass(t *testing.T) {
	if got := ServedRuntimeClass(nil); got != DefaultRuntimeClassName {
		t.Errorf("ServedRuntimeClass(nil) = %q, want %q", got, DefaultRuntimeClassName)
	}
	if got := ServedRuntimeClass(&config.UnikraftProvider{}); got != DefaultRuntimeClassName {
		t.Errorf("ServedRuntimeClass with no class configured = %q, want %q", got, DefaultRuntimeClassName)
	}
	if got := ServedRuntimeClass(&config.UnikraftProvider{RuntimeClassName: "custom-class"}); got != "custom-class" {
		t.Errorf("ServedRuntimeClass = %q, want custom-class", got)
	}
}
