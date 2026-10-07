// SPDX-License-Identifier: AGPL-3.0-only

package controller

// TODO(instance-type-catalog): this is a temporary local copy of
// go.datum.net/compute/pkg/instancetype's D1Standard2/Lookup, made because the
// compute module version this provider currently pins (and even the compute
// module resolved via its local `replace` directive in go.mod, which points at
// an unreleased working copy and must never be committed) does not give this
// repo a stable import to depend on for that package. Importing it today would
// tie this repo's build to compute's local, unreleased state.
//
// Once compute's instance-type-catalog work merges (datum-cloud/compute#333,
// PR datum-cloud/compute#377) and a tagged compute release incorporates it,
// delete this file, remove the `replace` directive from go.mod, bump the
// go.datum.net/compute requirement to that release, and change the one call
// site in instance_mapper.go back to instancetype.Lookup. Sizing must come
// from a single source of truth; this duplication is only tolerated until
// that release exists.
const (
	// d1Standard2InstanceType mirrors instancetype.D1Standard2.
	d1Standard2InstanceType = "datumcloud-d1-standard-2"

	// legacyD1Standard2InstanceType mirrors instancetype.LegacyD1Standard2 —
	// the name d1Standard2InstanceType was published under before instance
	// type names became valid Kubernetes object names. Instances stored
	// before the rename still carry it.
	legacyD1Standard2InstanceType = "datumcloud/d1-standard-2"
)

// instanceTypeAliases mirrors instancetype's unexported aliases map: retired
// instance type names, keyed to the name that replaced them.
var instanceTypeAliases = map[string]string{
	legacyD1Standard2InstanceType: d1Standard2InstanceType,
}

// instanceTypeSizing mirrors instancetype.Resources.
type instanceTypeSizing struct {
	// cpuMillicores is the CPU allocation in millicores. 1000 millicores is
	// one virtual CPU (vCPU).
	cpuMillicores int64

	// memoryMiB is the memory allocation in mebibytes (MiB).
	memoryMiB int64
}

// hardcodedInstanceTypeCatalog mirrors instancetype's unexported catalog map.
var hardcodedInstanceTypeCatalog = map[string]instanceTypeSizing{
	d1Standard2InstanceType: {
		cpuMillicores: 1000, // 1 vCPU
		memoryMiB:     2048, // 2 GiB
	},
}

// canonicalInstanceTypeName mirrors instancetype.Canonical: it maps a name
// retired by a rename to the name that replaced it, and returns any other
// name, including an unknown one, unchanged.
func canonicalInstanceTypeName(name string) string {
	if canonical, ok := instanceTypeAliases[name]; ok {
		return canonical
	}
	return name
}

// lookupHardcodedInstanceType mirrors instancetype.Lookup: it returns the
// sizing for an instance type name and reports whether the catalog contains
// the name, resolving a retired name to the sizing of the name that replaced
// it.
func lookupHardcodedInstanceType(name string) (instanceTypeSizing, bool) {
	sizing, ok := hardcodedInstanceTypeCatalog[canonicalInstanceTypeName(name)]
	return sizing, ok
}
