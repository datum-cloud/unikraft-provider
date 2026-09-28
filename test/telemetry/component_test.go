// SPDX-License-Identifier: AGPL-3.0-only

package telemetry

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestNodeTelemetryComponent verifies additive composition and validates both
// signal pipelines with the same collector version as the platform contract.
func TestNodeTelemetryComponent(t *testing.T) {
	binary := os.Getenv("OTELCOL_BIN")
	if binary == "" {
		t.Skip("set OTELCOL_BIN to Collector Contrib 0.144.0 to validate the component")
	}
	kustomize := os.Getenv("KUSTOMIZE_BIN")
	if kustomize == "" {
		kustomize = "kustomize"
	}
	output, err := exec.Command(kustomize, "build", "fixture").CombinedOutput()
	if err != nil {
		t.Fatalf("compose Unikraft component: %v\n%s", err, output)
	}
	var collector struct {
		Spec struct {
			Image        string           `json:"image"`
			HostNetwork  bool             `json:"hostNetwork"`
			DNSPolicy    string           `json:"dnsPolicy"`
			Env          []map[string]any `json:"env"`
			Volumes      []map[string]any `json:"volumes"`
			VolumeMounts []map[string]any `json:"volumeMounts"`
			Config       map[string]any   `json:"config"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(output, &collector); err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "0.144.0") ||
		!strings.HasSuffix(collector.Spec.Image, ":0.144.0") {
		t.Fatalf("collector executable and fixture must use 0.144.0: %s (%v)", version, err)
	}
	if !collector.Spec.HostNetwork || collector.Spec.DNSPolicy != "ClusterFirstWithHostNet" {
		t.Fatal("runtime scrapes require host networking and cluster DNS")
	}
	// Appending the service mount and credentials must keep platform entries.
	assertNames(t, collector.Spec.Volumes, "storage", "unikraft-data")
	assertNames(t, collector.Spec.VolumeMounts, "storage", "unikraft-data")
	assertNames(t, collector.Spec.Env, "K8S_NODE_NAME", "LOGS_OTLP_ENDPOINT", "METRICS_RW_ENDPOINT",
		"UKP_HOST_IP", "UKP_API_TOKEN", "UKP_METRICS_TOKEN")
	config := collector.Spec.Config
	storage := config["extensions"].(map[string]any)["file_storage"].(map[string]any)
	storage["directory"] = t.TempDir()
	receivers := config["receivers"].(map[string]any)
	for _, receiver := range []string{"filelog/kubernetes", "filelog/unikraft", "prometheus/unikraft"} {
		if receivers[receiver] == nil {
			t.Errorf("missing receiver %s", receiver)
		}
	}
	processors := config["processors"].(map[string]any)
	metadata := processors["k8sattributes/unikraft"].(map[string]any)
	filter := metadata["filter"].(map[string]any)
	if filter["node_from_env_var"] != nil || filter["node"] != nil {
		t.Error("virtual Unikraft Pods must be associated across nodes")
	}
	service := config["service"].(map[string]any)
	pipelines := service["pipelines"].(map[string]any)
	for _, pipeline := range []string{"logs/platform", "logs/unikraft", "metrics/unikraft"} {
		if pipelines[pipeline] == nil {
			t.Errorf("missing pipeline %s", pipeline)
		}
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "validate", "--config", path)
	cmd.Env = append(os.Environ(), "K8S_NODE_NAME=fixture-node", "LOGS_OTLP_ENDPOINT=localhost:4317",
		"METRICS_RW_ENDPOINT=http://localhost:8429/api/v1/write", "UKP_HOST_IP=127.0.0.1",
		"UKP_API_TOKEN=fixture-token", "UKP_METRICS_TOKEN=fixture-token")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("validate composed collector: %v\n%s", err, output)
	}
}

func assertNames(t *testing.T, entries []map[string]any, names ...string) {
	t.Helper()
	seen := make(map[string]int)
	for _, entry := range entries {
		seen[entry["name"].(string)]++
	}
	for _, name := range names {
		if seen[name] != 1 {
			t.Errorf("entry %s occurs %d times, want once", name, seen[name])
		}
	}
}
