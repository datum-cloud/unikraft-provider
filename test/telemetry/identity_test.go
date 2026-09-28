// SPDX-License-Identifier: AGPL-3.0-only

package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// Exercise real VM files and container-ID association, not just OTTL syntax.
// The virtual Pods are deliberately on a different node from the collector.
func TestInstanceLogProjectIdentity(t *testing.T) {
	binary := os.Getenv("OTELCOL_BIN")
	if binary == "" {
		t.Skip("set OTELCOL_BIN to Collector Contrib 0.144.0")
	}
	const (
		projectLabel   = "resourcemanager.miloapis.com/project-name"
		clusterLabel   = "meta.datumapis.com/upstream-cluster-name"
		namespaceLabel = "meta.datumapis.com/upstream-namespace"
	)
	cases := []struct {
		name            string
		labels          map[string]string
		project         string
		missingInstance bool
	}{
		{name: "upstream-only", labels: map[string]string{clusterLabel: "cluster-project-a", namespaceLabel: "default"}, project: "project-a"},
		{name: "explicit-only", labels: map[string]string{projectLabel: "project-explicit", namespaceLabel: "default"}, project: "project-explicit"},
		{name: "empty-explicit", labels: map[string]string{projectLabel: "", clusterLabel: "cluster-project-empty", namespaceLabel: "default"}, project: "project-empty"},
		{name: "explicit-precedence", labels: map[string]string{projectLabel: "project-preferred", clusterLabel: "cluster-project-other", namespaceLabel: "default"}, project: "project-preferred"},
		{name: "prefixed-explicit", labels: map[string]string{projectLabel: "cluster-project-literal", clusterLabel: "cluster-project-literal", namespaceLabel: "default"}, project: "cluster-project-literal"},
		{name: "missing-project", labels: map[string]string{namespaceLabel: "default"}},
		{name: "invalid-cluster", labels: map[string]string{clusterLabel: "project-invalid", namespaceLabel: "default"}},
		{name: "empty-cluster", labels: map[string]string{clusterLabel: "cluster-", namespaceLabel: "default"}},
		{name: "missing-namespace", labels: map[string]string{clusterLabel: "cluster-project-nonamespace"}},
		{name: "missing-instance", labels: map[string]string{clusterLabel: "cluster-project-noinstance", namespaceLabel: "default"}, missingInstance: true},
	}
	dir := t.TempDir()
	pods := make([]corev1.Pod, 0, len(cases))
	namespaces := make([]corev1.Namespace, 0, len(cases))
	for i, tc := range cases {
		id := fmt.Sprintf("%08d-1111-1111-1111-111111111111", i)
		podLabels := map[string]string{"managed-by": "infra-provider-unikraft", "upstream.instance": "same-name"}
		if tc.missingInstance {
			delete(podLabels, "upstream.instance")
		}
		pods = append(pods, corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "same-name", Namespace: tc.name, UID: types.UID(id), ResourceVersion: "1", Labels: podLabels},
			Spec:       corev1.PodSpec{NodeName: "kraftlet-virtual-node", Containers: []corev1.Container{{Name: "app"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: "ukp://" + id}}},
		})
		namespaces = append(namespaces, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tc.name, ResourceVersion: "1", Labels: tc.labels}})
		logDir := filepath.Join(dir, "ukp", "data", "platform", id)
		if err := os.MkdirAll(logDir, 0700); err != nil {
			t.Fatal(err)
		}
		// Application content cannot override the namespace-derived project.
		body := tc.name + ` {"project_name":"project-spoof","datum.project.name":"project-spoof"}`
		if err := os.WriteFile(filepath.Join(logDir, "vm.log"), []byte(body+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	kube := identityKubernetesAPI(t, pods, namespaces)
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fixture
  cluster: {server: %s}
contexts:
- name: fixture
  context: {cluster: fixture, user: fixture}
current-context: fixture
users:
- name: fixture
  user: {}
`, kube.URL)), 0600); err != nil {
		t.Fatal(err)
	}

	kustomize := os.Getenv("KUSTOMIZE_BIN")
	if kustomize == "" {
		kustomize = "kustomize"
	}
	rendered, err := exec.Command(kustomize, "build", "fixture").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, rendered)
	}
	var collector struct {
		Spec struct {
			Config map[string]any `json:"config"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(rendered, &collector); err != nil {
		t.Fatal(err)
	}
	config := collector.Spec.Config
	config["extensions"].(map[string]any)["file_storage"].(map[string]any)["directory"] = dir
	outputPath := filepath.Join(dir, "exported.jsonl")
	config["exporters"] = map[string]any{"file/test": map[string]any{"path": outputPath}}
	service := config["service"].(map[string]any)
	pipeline := service["pipelines"].(map[string]any)["logs/unikraft"].(map[string]any)
	pipeline["exporters"] = []string{"file/test"}
	service["pipelines"] = map[string]any{"logs/unikraft": pipeline}
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "/var/lib/ukp", filepath.Join(dir, "ukp"))
	text = strings.ReplaceAll(text, "auth_type: serviceAccount", "auth_type: kubeConfig")
	configPath := filepath.Join(dir, "collector.yaml")
	if err := os.WriteFile(configPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", configPath)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig, "K8S_NODE_NAME=physical-node",
		"UKP_HOST_IP=127.0.0.1", "UKP_API_TOKEN=test-token", "UKP_METRICS_TOKEN=test-token")
	var collectorOutput bytes.Buffer
	cmd.Stdout, cmd.Stderr = &collectorOutput, &collectorOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("collector failed: %v\n%s", err, collectorOutput.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("collector did not stop")
		}
		if t.Failed() {
			t.Log(collectorOutput.String())
		}
	})
	started := time.Now()
	deadline := started.Add(20 * time.Second)
	for {
		records := readIdentityRecords(t, outputPath)
		ready := true
		for _, tc := range cases {
			attrs, found := records[tc.name]
			if tc.project == "" {
				if found {
					t.Fatalf("%s exported without resolved identity: %v", tc.name, attrs)
				}
				continue
			}
			if !found {
				ready = false
				continue
			}
			for key, want := range map[string]string{
				"project_name": tc.project, "datum.project.name": tc.project,
				"datum.instance.namespace": "default", "datum.instance.name": "same-name",
				"k8s.node.name": "physical-node",
			} {
				if attrs[key] != want {
					t.Fatalf("%s: %s = %q, want %q", tc.name, key, attrs[key], want)
				}
			}
		}
		// Allow all file polls and the file exporter's flush to complete before
		// concluding that the invalid-identity records stayed excluded.
		if ready && time.Since(started) >= 3*time.Second {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for project logs; received %v", records)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func identityKubernetesAPI(t *testing.T, pods []corev1.Pod, namespaces []corev1.Namespace) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		switch r.URL.Path {
		case "/api/v1/pods":
			selector, err := labels.Parse(r.URL.Query().Get("labelSelector"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			selected := make([]corev1.Pod, 0, len(pods))
			for _, pod := range pods {
				if selector.Matches(labels.Set(pod.Labels)) {
					selected = append(selected, pod)
				}
			}
			_ = json.NewEncoder(w).Encode(corev1.PodList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
				ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: selected,
			})
		case "/api/v1/namespaces":
			_ = json.NewEncoder(w).Encode(corev1.NamespaceList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NamespaceList"},
				ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: namespaces,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func readIdentityRecords(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	records := make(map[string]map[string]string)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return records
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	for {
		var batch struct {
			ResourceLogs []struct {
				Resource struct {
					Attributes []struct {
						Key   string
						Value struct{ StringValue string }
					}
				}
				ScopeLogs []struct {
					LogRecords []struct{ Body struct{ StringValue string } }
				}
			}
		}
		if err := decoder.Decode(&batch); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return records // A concurrent flush may still be writing a batch.
			}
			t.Fatal(err)
		}
		for _, resource := range batch.ResourceLogs {
			attrs := make(map[string]string)
			for _, attr := range resource.Resource.Attributes {
				attrs[attr.Key] = attr.Value.StringValue
			}
			for _, scope := range resource.ScopeLogs {
				for _, record := range scope.LogRecords {
					name, _, _ := strings.Cut(record.Body.StringValue, " ")
					records[name] = attrs
				}
			}
		}
	}
}
