package stateprojector

import "testing"

func TestDecodeProjectID(t *testing.T) {
	cases := []struct{ encoded, want string }{
		// Real value captured from a live deployment (2026-08-19): namespace
		// "ns-7c30e6d4-..." carries this label and the actual project is
		// "project-htxrg" — nothing like the namespace's own opaque name.
		{"cluster-project-htxrg", "project-htxrg"},
		// EncodeClusterName replaces "/" with "_" for names containing a path.
		{"cluster-org_team", "org/team"},
		{"cluster-", ""},
	}
	for _, c := range cases {
		if got := decodeProjectID(c.encoded); got != c.want {
			t.Errorf("decodeProjectID(%q) = %q, want %q", c.encoded, got, c.want)
		}
	}
}
