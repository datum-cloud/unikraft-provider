// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderCorefile runs the coredns entrypoint with nft and coredns replaced by
// shims, so the test sees the Corefile the real CoreDNS would be handed.
func renderCorefile(t *testing.T, ukpConf string, env map[string]string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shim("nft", "exit 0")
	// The entrypoint execs coredns last; echo its -conf back instead of serving.
	shim("coredns", `while [ $# -gt 0 ]; do if [ "$1" = -conf ]; then cat "$2"; exit 0; fi; shift; done; exit 1`)

	conf := filepath.Join(dir, "ukp.conf")
	if err := os.WriteFile(conf, []byte(ukpConf), 0o644); err != nil {
		t.Fatal(err)
	}

	script, err := filepath.Abs("../../build/ukp-runtime/coredns-redirect.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, script)
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"UKP_CONF=" + conf,
		"UKP_RUNTIME=" + dir,
		"COREDNS_BIN=" + filepath.Join(bin, "coredns"),
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("coredns-redirect.sh failed: %v\n%s", err, out)
	}
	return string(out)
}

func forwardLine(t *testing.T, corefile string) string {
	t.Helper()
	for _, line := range strings.Split(corefile, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "forward ") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no forward directive in rendered Corefile:\n%s", corefile)
	return ""
}

func TestCorednsRedirect_UpstreamDefault(t *testing.T) {
	out := renderCorefile(t, "", nil)
	if got, want := forwardLine(t, out), "forward . 127.0.0.53:53"; got != want {
		t.Errorf("forward = %q, want %q", got, want)
	}
}

func TestCorednsRedirect_UpstreamFromConf(t *testing.T) {
	out := renderCorefile(t, `UKP_DNS_UPSTREAM="1.1.1.1:53"`+"\n", map[string]string{"UKP_DNS_UPSTREAM": ""})
	if got, want := forwardLine(t, out), "forward . 1.1.1.1:53"; got != want {
		t.Errorf("forward = %q, want %q", got, want)
	}
}

// The container env must win over ukp.conf, which sets the same variable
// unconditionally, and IPv6 upstreams must reach the Corefile verbatim.
func TestCorednsRedirect_UpstreamEnvOverridesConfIPv6(t *testing.T) {
	upstream := "[2606:4700:4700::1111]:53 [2001:4860:4860::8888]:53"
	out := renderCorefile(t, `UKP_DNS_UPSTREAM="127.0.0.53:53"`+"\n", map[string]string{"UKP_DNS_UPSTREAM": upstream})
	if got, want := forwardLine(t, out), "forward . "+upstream; got != want {
		t.Errorf("forward = %q, want %q", got, want)
	}

	bare := "2606:4700:4700::1111 2001:4860:4860::8888"
	out = renderCorefile(t, "", map[string]string{"UKP_DNS_UPSTREAM": bare})
	if got, want := forwardLine(t, out), "forward . "+bare; got != want {
		t.Errorf("forward = %q, want %q", got, want)
	}
}
