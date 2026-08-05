package ukpmetrics

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCollectEmitsContainerResourceMetricsForPodIPJoin(t *testing.T) {
	exporter, err := NewExporter(Config{
		UKPClient: fakeUKPClient{
			instances: []Instance{{UUID: "uuid-1", Name: "inst-1", PrivateIP: "172.16.0.9"}},
			metrics:   map[string]InstanceMetrics{"uuid-1": {CPUTimeMS: 1234, RSSBytes: 67108864}},
		},
		RuntimeNodeName: "worker-1",
		VirtualNodeName: "kraftlet-worker-1",
		KubeClient: fake.NewSimpleClientset(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns-1"},
			Spec: corev1.PodSpec{
				NodeName:   "kraftlet-worker-1",
				Containers: []corev1.Container{{Name: "app"}},
			},
			Status: corev1.PodStatus{
				PodIP:  "172.16.0.9",
				PodIPs: []corev1.PodIP{{IP: "172.16.0.9"}},
			},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	metrics := gatherMetrics(t, exporter)

	assertContains(t, metrics, `datum_compute_instance_cpu_usage_seconds_total{`)
	assertContains(t, metrics, `container="app"`)
	assertContains(t, metrics, `namespace="ns-1"`)
	assertContains(t, metrics, `node="kraftlet-worker-1"`)
	assertContains(t, metrics, `pod="pod-1"`)
	assertContains(t, metrics, `runtime_node="worker-1"`)
	assertContains(t, metrics, `ukp_instance_uuid="uuid-1"`)
	assertContains(t, metrics, `} 1.234`)
	assertContains(t, metrics, `datum_compute_instance_memory_working_set_bytes{`)
	assertContains(t, metrics, `} 6.7108864e+07`)
}

func TestCollectReadsGuestIPFromWorkspaceWhenInstanceListLacksIP(t *testing.T) {
	platformDir := t.TempDir()
	workspace := filepath.Join(platformDir, "uuid-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "vmm.json"), []byte(`{"boot_args":"netdev.ip=\"172.16.0.10/30:172.16.0.9:172.16.0.9\""}`), 0o644); err != nil {
		t.Fatal(err)
	}

	exporter, err := NewExporter(Config{
		UKPClient: fakeUKPClient{
			instances: []Instance{{UUID: "uuid-1", Name: "inst-1"}},
			metrics:   map[string]InstanceMetrics{"uuid-1": {CPUTimeMS: 2000, RSSBytes: 1024}},
		},
		PlatformDir:     platformDir,
		VirtualNodeName: "kraftlet-worker-1",
		KubeClient: fake.NewSimpleClientset(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns-1"},
			Spec: corev1.PodSpec{
				NodeName:   "kraftlet-worker-1",
				Containers: []corev1.Container{{Name: "app"}},
			},
			Status: corev1.PodStatus{PodIP: "172.16.0.10"},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	metrics := gatherMetrics(t, exporter)

	assertContains(t, metrics, `datum_compute_instance_cpu_usage_seconds_total{`)
	assertContains(t, metrics, `pod="pod-1"`)
	assertContains(t, metrics, `ukp_instance_uuid="uuid-1"`)
	assertContains(t, metrics, `} 2`)
}

func TestCollectSkipsPodsOnOtherVirtualNodes(t *testing.T) {
	exporter, err := NewExporter(Config{
		UKPClient: fakeUKPClient{
			instances: []Instance{{UUID: "uuid-1", PrivateIP: "172.16.0.9"}},
			metrics:   map[string]InstanceMetrics{"uuid-1": {CPUTimeMS: 1234, RSSBytes: 1024}},
		},
		VirtualNodeName: "kraftlet-worker-1",
		KubeClient: fake.NewSimpleClientset(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns-1"},
			Spec:       corev1.PodSpec{NodeName: "kraftlet-other", Containers: []corev1.Container{{Name: "app"}}},
			Status:     corev1.PodStatus{PodIP: "172.16.0.9"},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	metrics := gatherMetrics(t, exporter)
	if strings.Contains(metrics, `datum_compute_instance_cpu_usage_seconds_total{namespace="ns-1"`) {
		t.Fatalf("expected metrics for pod on other virtual node to be skipped, got:\n%s", metrics)
	}
}

func TestCollectSkipsInstanceWhenMetricsFetchFails(t *testing.T) {
	exporter, err := NewExporter(Config{
		UKPClient: fakeUKPClient{
			instances: []Instance{
				{UUID: "uuid-1", PrivateIP: "172.16.0.9"},
				{UUID: "uuid-2", PrivateIP: "172.16.0.10"},
			},
			metrics: map[string]InstanceMetrics{
				"uuid-2": {CPUTimeMS: 2000, RSSBytes: 2048},
			},
			errors: map[string]error{
				"uuid-1": fmt.Errorf("gone"),
			},
		},
		VirtualNodeName: "kraftlet-worker-1",
		KubeClient: fake.NewSimpleClientset(
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns-1"},
				Spec:       corev1.PodSpec{NodeName: "kraftlet-worker-1", Containers: []corev1.Container{{Name: "app"}}},
				Status:     corev1.PodStatus{PodIP: "172.16.0.9"},
			},
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "ns-1"},
				Spec:       corev1.PodSpec{NodeName: "kraftlet-worker-1", Containers: []corev1.Container{{Name: "app"}}},
				Status:     corev1.PodStatus{PodIP: "172.16.0.10"},
			},
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	metrics := gatherMetrics(t, exporter)
	if strings.Contains(metrics, `ukp_instance_uuid="uuid-1"`) {
		t.Fatalf("expected failed instance to be skipped, got:\n%s", metrics)
	}
	assertContains(t, metrics, `pod="pod-2"`)
	assertContains(t, metrics, `ukp_instance_uuid="uuid-2"`)
	assertContains(t, metrics, `} 2`)
}

func gatherMetrics(t *testing.T, exporter *Exporter) string {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewCollector(exporter))
	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 {
		t.Fatalf("expected 200 response, got %d: %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("expected output to contain %q, got:\n%s", want, got)
	}
}

type fakeUKPClient struct {
	instances []Instance
	metrics   map[string]InstanceMetrics
	errors    map[string]error
}

func (c fakeUKPClient) ListInstances(_ context.Context) ([]Instance, error) {
	return c.instances, nil
}

func (c fakeUKPClient) GetInstanceMetrics(_ context.Context, uuid string) (InstanceMetrics, error) {
	if err := c.errors[uuid]; err != nil {
		return InstanceMetrics{}, err
	}
	return c.metrics[uuid], nil
}
