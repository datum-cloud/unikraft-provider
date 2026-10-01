// SPDX-License-Identifier: AGPL-3.0-only

package controller

import "testing"

// TestLookupHardcodedInstanceType locks the behavior this file exists to
// mirror from go.datum.net/compute/pkg/instancetype.Lookup: a known name
// resolves, a retired name resolves to the same sizing as the name that
// replaced it, and an unknown name yields no sizing.
func TestLookupHardcodedInstanceType(t *testing.T) {
	current, ok := lookupHardcodedInstanceType(d1Standard2InstanceType)
	if !ok {
		t.Fatalf("lookupHardcodedInstanceType(%q) must resolve", d1Standard2InstanceType)
	}
	if current.cpuMillicores != 1000 || current.memoryMiB != 2048 {
		t.Errorf("sizing = %+v, want 1000 millicores / 2048 MiB", current)
	}

	legacy, ok := lookupHardcodedInstanceType(legacyD1Standard2InstanceType)
	if !ok {
		t.Fatalf("lookupHardcodedInstanceType(%q) must resolve", legacyD1Standard2InstanceType)
	}
	if legacy != current {
		t.Errorf("retired name sized %+v, want the same as %q: %+v", legacy, d1Standard2InstanceType, current)
	}

	if _, ok := lookupHardcodedInstanceType("datumcloud-unknown-type"); ok {
		t.Error("lookupHardcodedInstanceType of an unknown name must report not-found")
	}
}
