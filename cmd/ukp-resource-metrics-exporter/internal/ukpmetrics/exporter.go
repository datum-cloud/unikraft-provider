package ukpmetrics

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

const defaultContainerName = "instance"

var bootIPPattern = regexp.MustCompile(`netdev\.ip=[^0-9]*([0-9][0-9.]*)`)

var (
	cpuUsageDesc = prometheus.NewDesc(
		"datum_compute_instance_cpu_usage_seconds_total",
		"Cumulative CPU time consumed by the Unikraft instance, in seconds.",
		[]string{"namespace", "pod", "container", "node", "runtime_node", "ukp_instance_uuid"},
		nil,
	)
	memoryWorkingSetDesc = prometheus.NewDesc(
		"datum_compute_instance_memory_working_set_bytes",
		"Resident set size reported by ukpd, emitted as the closest available working set approximation.",
		[]string{"namespace", "pod", "container", "node", "runtime_node", "ukp_instance_uuid"},
		nil,
	)
)

type Config struct {
	UKPClient       UKPClient
	PlatformDir     string
	RuntimeNodeName string
	VirtualNodeName string
	KubeClient      kubernetes.Interface
}

type Exporter struct {
	ukpClient       UKPClient
	platformDir     string
	runtimeNodeName string
	virtualNodeName string
	kubeClient      kubernetes.Interface
}

type UKPClient interface {
	ListInstances(ctx context.Context) ([]Instance, error)
	GetInstanceMetrics(ctx context.Context, uuid string) (InstanceMetrics, error)
}

type Instance struct {
	UUID      string
	Name      string
	PrivateIP string
}

type InstanceMetrics struct {
	RSSBytes  uint64
	CPUTimeMS uint64
}

func NewExporter(cfg Config) (*Exporter, error) {
	if cfg.UKPClient == nil {
		return nil, fmt.Errorf("ukp client is required")
	}
	if cfg.KubeClient == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}
	return &Exporter{
		ukpClient:       cfg.UKPClient,
		platformDir:     cfg.PlatformDir,
		runtimeNodeName: cfg.RuntimeNodeName,
		virtualNodeName: cfg.VirtualNodeName,
		kubeClient:      cfg.KubeClient,
	}, nil
}

func (e *Exporter) CollectSamples(ctx context.Context) ([]sample, error) {
	ukpInstances, err := e.ukpClient.ListInstances(ctx)
	if err != nil {
		return nil, err
	}

	listOptions := metav1.ListOptions{}
	if e.virtualNodeName != "" {
		listOptions.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", e.virtualNodeName).String()
	}
	pods, err := e.kubeClient.CoreV1().Pods(metav1.NamespaceAll).List(ctx, listOptions)
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	instanceIPs := map[string]string{}
	for _, instance := range ukpInstances {
		ip := instance.PrivateIP
		if ip == "" {
			ip = e.readWorkspaceIP(instance.UUID)
		}
		if ip == "" {
			continue
		}
		if instance.UUID != "" {
			instanceIPs[instance.UUID] = ip
		}
		if instance.Name != "" {
			instanceIPs[instance.Name] = ip
		}
	}

	podsByIP := podsByIP(pods.Items, e.virtualNodeName)
	samples := make([]sample, 0, len(ukpInstances)*2)
	for _, instance := range ukpInstances {
		if instance.UUID == "" {
			continue
		}
		ip := instanceIPs[instance.UUID]
		if ip == "" {
			ip = instanceIPs[instance.Name]
		}
		if ip == "" {
			continue
		}

		pod, ok := podsByIP[ip]
		if !ok {
			continue
		}

		metric, err := e.ukpClient.GetInstanceMetrics(ctx, instance.UUID)
		if err != nil {
			log.Printf("skipping ukpd instance %s: fetch metrics: %v", instance.UUID, err)
			continue
		}

		labels := sampleLabels{
			Namespace:       pod.Namespace,
			Pod:             pod.Name,
			Container:       containerName(pod),
			Node:            pod.Spec.NodeName,
			RuntimeNode:     e.runtimeNodeName,
			UKPInstanceUUID: instance.UUID,
		}
		samples = append(samples,
			sample{Name: "datum_compute_instance_cpu_usage_seconds_total", Labels: labels, Value: float64(metric.CPUTimeMS) / 1000},
			sample{Name: "datum_compute_instance_memory_working_set_bytes", Labels: labels, Value: float64(metric.RSSBytes)},
		)
	}

	return samples, nil
}

type Collector struct {
	exporter *Exporter
}

func NewCollector(exporter *Exporter) *Collector {
	return &Collector{exporter: exporter}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cpuUsageDesc
	ch <- memoryWorkingSetDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	samples, err := c.exporter.CollectSamples(context.Background())
	if err != nil {
		ch <- prometheus.NewInvalidMetric(cpuUsageDesc, err)
		ch <- prometheus.NewInvalidMetric(memoryWorkingSetDesc, err)
		return
	}
	for _, sample := range samples {
		valueType := prometheus.GaugeValue
		desc := memoryWorkingSetDesc
		if sample.Name == "datum_compute_instance_cpu_usage_seconds_total" {
			valueType = prometheus.CounterValue
			desc = cpuUsageDesc
		}
		ch <- prometheus.MustNewConstMetric(
			desc,
			valueType,
			sample.Value,
			sample.Labels.Namespace,
			sample.Labels.Pod,
			sample.Labels.Container,
			sample.Labels.Node,
			sample.Labels.RuntimeNode,
			sample.Labels.UKPInstanceUUID,
		)
	}
}

func (e *Exporter) readWorkspaceIP(uuid string) string {
	if e.platformDir == "" || uuid == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(e.platformDir, uuid, "vmm.json"))
	if err != nil {
		return ""
	}
	match := bootIPPattern.FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	return string(match[1])
}

type sampleLabels struct {
	Namespace       string
	Pod             string
	Container       string
	Node            string
	RuntimeNode     string
	UKPInstanceUUID string
}

type sample struct {
	Name   string
	Labels sampleLabels
	Value  float64
}

func podsByIP(pods []corev1.Pod, virtualNodeName string) map[string]corev1.Pod {
	byIP := map[string]corev1.Pod{}
	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}
		if virtualNodeName != "" && pod.Spec.NodeName != virtualNodeName {
			continue
		}
		byIP[pod.Status.PodIP] = pod
		for _, ip := range pod.Status.PodIPs {
			if ip.IP != "" {
				byIP[ip.IP] = pod
			}
		}
	}
	return byIP
}

func containerName(pod corev1.Pod) string {
	if len(pod.Spec.Containers) == 1 && pod.Spec.Containers[0].Name != "" {
		return pod.Spec.Containers[0].Name
	}
	return defaultContainerName
}
