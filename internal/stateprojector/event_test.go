package stateprojector

import "testing"

func TestExtractTransitionKeys(t *testing.T) {
	cases := []struct {
		data           map[string]any
		wantUUID       string
		wantOld, wantN string
	}{
		// The payload ukpd emits before 0.14: vm / prev / new.
		{map[string]any{"vm": "dda7fe99-387a-4d81-80f2-35e2b51ee5c5", "prev": "running", "new": "stopping"},
			"dda7fe99-387a-4d81-80f2-35e2b51ee5c5", "running", "stopping"},
		{map[string]any{"vm": "dda7fe99-387a-4d81-80f2-35e2b51ee5c5", "prev": "starting", "new": "running"},
			"dda7fe99-387a-4d81-80f2-35e2b51ee5c5", "starting", "running"},
		// Unrecognized keys read nothing — no guessing at alternate spellings
		// that have never actually been observed.
		{map[string]any{"instance_uuid": "u2", "old_state": "starting", "state": "running"}, "", "", ""},
	}
	for _, c := range cases {
		u, old, n := extractTransition(stateChange{Data: c.data})
		if u != c.wantUUID || old != c.wantOld || n != c.wantN {
			t.Errorf("extractTransition(%v) = (%q,%q,%q), want (%q,%q,%q)", c.data, u, old, n, c.wantUUID, c.wantOld, c.wantN)
		}
	}
	// uuid fallback scans an appended uuid-shaped token when "vm" is absent.
	u, _, _ := extractTransition(stateChange{Data: map[string]any{"instance": "31020cdc-eee4-440a-8b0b-57c4690ae3d0"}})
	if u != "31020cdc-eee4-440a-8b0b-57c4690ae3d0" {
		t.Errorf("uuid fallback = %q", u)
	}
	// ukp-platform 0.14+ carries the uuid as object.uuid.
	ev := stateChange{Data: map[string]any{"prev": "starting", "new": "running"}}
	ev.Object.UUID = "17fba7de-094a-4968-8779-64432dacb47e"
	if got, _, _ := extractTransition(ev); got != ev.Object.UUID {
		t.Errorf("extractTransition(%+v) uuid = %q, want %q", ev, got, ev.Object.UUID)
	}
}
