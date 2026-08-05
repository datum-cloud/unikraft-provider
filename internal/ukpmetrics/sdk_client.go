package ukpmetrics

import (
	"context"
	"fmt"
	"time"

	"unikraft.com/cloud/sdk/platform"
)

const defaultSDKTimeout = 5 * time.Second

type SDKClient struct {
	client platform.Client
}

func NewSDKClient(endpoint, token string) *SDKClient {
	return &SDKClient{client: platform.NewClient(
		platform.WithDefaultEndpoint(endpoint),
		platform.WithToken(token),
		platform.WithAllowInsecure(true),
		platform.WithUserAgent("ukp-resource-metrics-exporter"),
	)}
}

func (c *SDKClient) ListInstances(ctx context.Context) ([]Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultSDKTimeout)
	defer cancel()

	resp, err := c.client.GetInstances(ctx, nil, platform.GetInstancesOpts{})
	if err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("ukpd returned %q: %s", resp.Status, resp.Message)
	}
	if resp.Data == nil {
		return nil, nil
	}

	instances := make([]Instance, 0, len(resp.Data.Instances))
	for _, item := range resp.Data.Instances {
		instance := Instance{
			UUID: stringValue(item.Uuid),
			Name: stringValue(item.Name),
		}
		for _, iface := range item.NetworkInterfaces {
			if iface.PrivateIp != nil && *iface.PrivateIp != "" {
				instance.PrivateIP = *iface.PrivateIp
				break
			}
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func (c *SDKClient) GetInstanceMetrics(ctx context.Context, uuid string) (InstanceMetrics, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultSDKTimeout)
	defer cancel()

	resp, err := c.client.GetInstanceMetricsByUUID(ctx, uuid)
	if err != nil {
		return InstanceMetrics{}, err
	}
	if resp.Status != "success" {
		return InstanceMetrics{}, fmt.Errorf("ukpd returned %q for instance %s: %s", resp.Status, uuid, resp.Message)
	}
	if resp.Data == nil || len(resp.Data.Instances) == 0 {
		return InstanceMetrics{}, nil
	}
	metric := resp.Data.Instances[0]
	return InstanceMetrics{
		RSSBytes:  uint64Value(metric.RssBytes),
		CPUTimeMS: uint64Value(metric.CpuTimeMs),
	}, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func uint64Value(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}
