// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"context"
	"fmt"
	mcproviders "go.miloapis.com/milo/pkg/multicluster-runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// UnikraftProvider defines the configuration for the Unikraft infrastructure provider
type UnikraftProvider struct {
	metav1.TypeMeta

	MetricsServer MetricsServerConfig `json:"metricsServer"`

	WebhookServer WebhookServerConfig `json:"webhookServer"`

	Discovery DiscoveryConfig `json:"discovery"`

	DownstreamResourceManagement DownstreamResourceManagementConfig `json:"downstreamResourceManagement"`

	// LocationClassName configures the operator to only consider resources
	// attached to locations with the specified location class.
	// +default="self-managed"
	LocationClassName string `json:"locationClassName"`
}

// +k8s:deepcopy-gen=true

// WebhookServerConfig configures the webhook server
type WebhookServerConfig struct {
	// Host is the address that the server will listen on.
	// Defaults to "" - all addresses.
	Host string `json:"host"`

	// Port is the port number that the server will serve.
	// +default=9443
	Port int `json:"port"`

	// CertDir is the directory that contains the server key and certificate.
	CertDir string `json:"certDir"`

	// CertName is the server certificate name. Defaults to tls.crt.
	CertName string `json:"certName"`

	// KeyName is the server key name. Defaults to tls.key.
	KeyName string `json:"keyName"`
}

func (w *WebhookServerConfig) Options(_ context.Context, _ client.Client) webhook.Options {
	return webhook.Options{
		Host:     w.Host,
		Port:     w.Port,
		CertDir:  w.CertDir,
		CertName: w.CertName,
		KeyName:  w.KeyName,
	}
}

// +k8s:deepcopy-gen=true

// MetricsServerConfig configures the metrics server
type MetricsServerConfig struct {
	// BindAddress is the TCP address that the server should bind to.
	// +default="0"
	BindAddress string `json:"bindAddress"`

	// SecureServing configures the secure serving options.
	SecureServing bool `json:"secureServing"`

	// CertDir is the directory that contains the server key and certificate.
	CertDir string `json:"certDir"`

	// CertName is the server certificate name. Defaults to tls.crt.
	CertName string `json:"certName"`

	// KeyName is the server key name. Defaults to tls.key.
	KeyName string `json:"keyName"`
}

func (m *MetricsServerConfig) Options(ctx context.Context, c client.Client) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   m.BindAddress,
		SecureServing: m.SecureServing,
		CertDir:       m.CertDir,
		CertName:      m.CertName,
		KeyName:       m.KeyName,
	}

	if m.SecureServing {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	return opts
}

// +k8s:deepcopy-gen=true

// DownstreamResourceManagementConfig configures downstream resource management
type DownstreamResourceManagementConfig struct {
	// KubeconfigPath is the path to the kubeconfig for the downstream cluster.
	KubeconfigPath string `json:"kubeconfigPath"`

	// ProviderConfigStrategy defines how to select the ProviderConfig for resources.
	ProviderConfigStrategy ProviderConfigStrategy `json:"providerConfigStrategy"`

	// ManagedResourceLabels are labels applied to downstream resources for filtering.
	ManagedResourceLabels map[string]string `json:"managedResourceLabels,omitempty"`
}

func (d *DownstreamResourceManagementConfig) RestConfig() (*rest.Config, error) {
	if d.KubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", d.KubeconfigPath)
}

// +k8s:deepcopy-gen=true

// ProviderConfigStrategy defines how to select ProviderConfig
type ProviderConfigStrategy struct {
	// Single specifies a single ProviderConfig to use for all resources.
	Single *SingleProviderConfigStrategy `json:"single,omitempty"`
}

// +k8s:deepcopy-gen=true

// SingleProviderConfigStrategy uses a single named ProviderConfig
type SingleProviderConfigStrategy struct {
	Name string `json:"name"`
}

// +k8s:deepcopy-gen=true

type DiscoveryConfig struct {
	// Mode is the mode that the operator should use to discover clusters.
	//
	// +default="single"
	Mode mcproviders.Provider `json:"mode"`

	// InternalServiceDiscovery will result in the operator to connect to internal
	// service addresses for projects.
	InternalServiceDiscovery bool `json:"internalServiceDiscovery"`

	// DiscoveryKubeconfigPath is the path to the kubeconfig file to use for
	// project discovery. When not provided, the operator will use the in-cluster
	// config.
	DiscoveryKubeconfigPath string `json:"discoveryKubeconfigPath"`

	// DiscoveryContext is the context to use for discovery. When not provided,
	// the operator will use the current-context in the kubeconfig file..
	DiscoveryContext string `json:"discoveryContext"`

	// ProjectKubeconfigPath is the path to the kubeconfig file to use as a
	// template when connecting to project control planes. When not provided,
	// the operator will use the in-cluster config.
	ProjectKubeconfigPath string `json:"projectKubeconfigPath"`

	// LabelSelector is an optional selector to filter projects based on labels.
	// When provided, only projects matching this selector will be reconciled.
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

func (d *DiscoveryConfig) DiscoveryRestConfig() (*rest.Config, error) {
	if d.DiscoveryKubeconfigPath == "" {
		return nil, fmt.Errorf("discovery kubeconfig path is required")
	}
	return clientcmd.BuildConfigFromFlags("", d.DiscoveryKubeconfigPath)
}

func (d *DiscoveryConfig) ProjectRestConfig() (*rest.Config, error) {
	if d.ProjectKubeconfigPath == "" {
		return nil, fmt.Errorf("project kubeconfig path is required")
	}
	return clientcmd.BuildConfigFromFlags("", d.ProjectKubeconfigPath)
}

// DiscoveryMode defines the cluster discovery mode
type DiscoveryMode string

const (
	DiscoveryModeSingle DiscoveryMode = "single"
	DiscoveryModeMilo   DiscoveryMode = "milo"
)
