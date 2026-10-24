// SPDX-License-Identifier: AGPL-3.0-only

package config

import "testing"

func TestInstanceDNSConfig_Validate(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg     *InstanceDNSConfig
		wantErr bool
	}{
		"nil is unset":              {cfg: nil},
		"ipv4":                      {cfg: &InstanceDNSConfig{Nameservers: []string{"1.1.1.1"}}},
		"ipv6 only":                 {cfg: &InstanceDNSConfig{Nameservers: []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}}},
		"mixed with searches":       {cfg: &InstanceDNSConfig{Nameservers: []string{"2606:4700:4700::1111", "1.1.1.1"}, Searches: []string{"example.internal"}}},
		"no nameservers":            {cfg: &InstanceDNSConfig{}, wantErr: true},
		"too many nameservers":      {cfg: &InstanceDNSConfig{Nameservers: []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4"}}, wantErr: true},
		"hostname not ip":           {cfg: &InstanceDNSConfig{Nameservers: []string{"dns.google"}}, wantErr: true},
		"port suffix":               {cfg: &InstanceDNSConfig{Nameservers: []string{"[2606:4700:4700::1111]:53"}}, wantErr: true},
		"ipv6 zone":                 {cfg: &InstanceDNSConfig{Nameservers: []string{"fe80::1%eth0"}}, wantErr: true},
		"injected search directive": {cfg: &InstanceDNSConfig{Nameservers: []string{"1.1.1.1"}, Searches: []string{"example.org\nnameserver 127.0.0.1"}}, wantErr: true},
		"search comment":            {cfg: &InstanceDNSConfig{Nameservers: []string{"1.1.1.1"}, Searches: []string{"example.org#ignored"}}, wantErr: true},
		"empty search domain":       {cfg: &InstanceDNSConfig{Nameservers: []string{"1.1.1.1"}, Searches: []string{""}}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
