// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/unikraft-provider/internal/config"
)

const (
	// RuntimeClassField is the selectable Instance field the API server matches
	// a runtime class selector against.
	//
	// The path is spelled out here because the compute module this provider
	// pins predates runtime classes. See datum-cloud/unikraft-provider#168.
	RuntimeClassField = "spec.runtime.class"

	// DefaultRuntimeClassName is the class this provider serves unless
	// configuration names another one.
	DefaultRuntimeClassName = "unikernel"
)

// otherRuntimeClasses are the classes the platform offers that this provider
// does not serve.
var otherRuntimeClasses = []string{"general-purpose"}

// ServedRuntimeClass returns the runtime class this provider claims Instances
// for. A deployment names the class so a cell can run a provider per class
// without a provider release. An unnamed class falls back to the class the
// platform serves by default.
func ServedRuntimeClass(cfg *config.UnikraftProvider) string {
	if cfg != nil && cfg.RuntimeClassName != "" {
		return cfg.RuntimeClassName
	}
	return DefaultRuntimeClassName
}

// InstanceCacheSelector returns the field selector that keeps the Instance
// informer from listing and watching another provider's Instances. Two
// providers that both act on one Instance race to create its backing Pod, and
// the loser reconciles forever against a name that already exists.
//
// The selector excludes each class this provider does not serve. Field
// selector terms combine with AND and support only equality, so "unset or
// equal to the served class" has no direct spelling. An unset class reads as
// the empty string, which every exclusion term matches, so excluding the other
// classes leaves exactly the Instances this provider claims.
//
// The cache is where the filter has to live. A controller-runtime predicate
// drops another class's events only after the informer has already stored
// every Instance in the cell, and that memory cost has crash-looped a provider
// in this system.
func InstanceCacheSelector(servedClass string) fields.Selector {
	var terms []fields.Selector
	for _, class := range otherRuntimeClasses {
		if class == servedClass {
			continue
		}
		terms = append(terms, fields.OneTermNotEqualSelector(RuntimeClassField, class))
	}
	return fields.AndSelectors(terms...)
}

// CacheOptions scopes the manager's cache to the Instances this provider
// claims.
func CacheOptions(servedClass string) cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&computev1alpha.Instance{}: {
				Field: InstanceCacheSelector(servedClass),
			},
		},
	}
}
