package imageconvert

import (
	"net/netip"
	"testing"
)

func TestEgressAllowed(t *testing.T) {
	e := Egress{Denied: []netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")}}
	tests := []struct {
		addr    string
		allowed bool
	}{
		{"104.16.0.1:443", true},
		{"[2606:4700::1]:443", true},
		{"127.0.0.1:5000", false},
		{"[::1]:5051", false},
		{"[::ffff:127.0.0.1]:5000", false},
		{"10.0.0.5:8080", false},
		{"172.16.0.1:443", false},
		{"192.168.1.1:443", false},
		{"169.254.169.254:80", false},
		{"100.64.0.1:443", false},
		{"0.0.0.0:443", false},
		{"[fd00::1]:443", false},
		{"[fe80::1]:443", false},
		{"203.0.113.7:443", false},
		{"203.0.113.8:443", true},
	}
	for _, tt := range tests {
		if err := e.allowed(tt.addr); (err == nil) != tt.allowed {
			t.Errorf("allowed(%q) = %v, want allowed %v", tt.addr, err, tt.allowed)
		}
	}
}
