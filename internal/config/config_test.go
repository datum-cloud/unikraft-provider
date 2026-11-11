// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// TestEnableCNIDefaulting decodes config the way cmd/main.go does, so an
// explicit false must survive defaulting while an unset field defaults on.
func TestEnableCNIDefaulting(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := RegisterDefaults(scheme); err != nil {
		t.Fatal(err)
	}
	codecs := serializer.NewCodecFactory(scheme, serializer.EnableStrict)

	for name, tc := range map[string]struct {
		field string
		want  bool
	}{
		"unset defaults on": {field: "", want: true},
		"explicit true":     {field: "enableCNI: true", want: true},
		"explicit false":    {field: "enableCNI: false", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			data := []byte("apiVersion: apiserver.config.datumapis.com/v1alpha1\n" +
				"kind: UnikraftProvider\n" +
				"downstreamResourceManagement:\n" +
				"  " + tc.field + "\n")
			var cfg UnikraftProvider
			if err := runtime.DecodeInto(codecs.UniversalDecoder(), data, &cfg); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := cfg.DownstreamResourceManagement.EnableCNI
			if got == nil || *got != tc.want {
				t.Errorf("EnableCNI = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInstanceDNSConfig_Validate(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg     *InstanceDNSConfig
		wantErr bool
	}{
		"nil is unset":         {cfg: nil},
		"ipv4":                 {cfg: &InstanceDNSConfig{Nameservers: []string{"1.1.1.1"}}},
		"ipv6 only":            {cfg: &InstanceDNSConfig{Nameservers: []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}}},
		"mixed with searches":  {cfg: &InstanceDNSConfig{Nameservers: []string{"2606:4700:4700::1111", "1.1.1.1"}, Searches: []string{"example.internal"}}},
		"no nameservers":       {cfg: &InstanceDNSConfig{}, wantErr: true},
		"too many nameservers": {cfg: &InstanceDNSConfig{Nameservers: []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4"}}, wantErr: true},
		"hostname not ip":      {cfg: &InstanceDNSConfig{Nameservers: []string{"dns.google"}}, wantErr: true},
		"port suffix":          {cfg: &InstanceDNSConfig{Nameservers: []string{"[2606:4700:4700::1111]:53"}}, wantErr: true},
		"ipv6 zone":            {cfg: &InstanceDNSConfig{Nameservers: []string{"fe80::1%eth0"}}, wantErr: true},
		"empty search domain":  {cfg: &InstanceDNSConfig{Nameservers: []string{"1.1.1.1"}, Searches: []string{""}}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
