// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"context"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// +default=":8080"
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

// DownstreamResourceManagementConfig configures downstream resource management.
//
// The kraftlet cluster is always the same cluster as the cell cluster (kraftlet
// runs as a virtual-kubelet node in the cell). ConfigMap and Secret volumes are
// referenced by name in the Pod spec; kraftlet resolves and mounts them using
// its own node/kubelet identity at runtime. The provider never reads or writes
// ConfigMap/Secret data.
type DownstreamResourceManagementConfig struct {
	// NodeSelector overrides the node selector applied to every Instance Pod.
	// When unset, the provider defaults to {"unikraft.com/virtual-kubelet": "true"},
	// which places guests on any per-host kraftlet virtual-kubelet node (kraftlet-<host>).
	// Set this field to select a different node or to use a different label-based
	// selector (e.g. {"node-role": "kraftlet"}) in multi-node deployments.
	//
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations overrides the tolerations applied to every Instance Pod.
	// When unset, the provider applies a single default toleration for the
	// virtual-kubelet.io/provider=ukc:NoSchedule taint that kraftlet nodes carry.
	// Set this field to add or replace tolerations (e.g. for custom taint keys
	// or to support nodes without the ukc taint in lab/testing environments).
	//
	// +optional
	Tolerations []core.Toleration `json:"tolerations,omitempty"`

	// EnableCNI turns on platform-managed instance networking: the provider
	// marks every Instance Pod for kraftlet's remote-CNI integration, which
	// hands network setup to the co-located ukp-remote-cni service instead of
	// leaving it entirely internal to the runtime. This is a platform-wide
	// setting, not something a tenant can opt in or out of per Instance.
	// Defaults to enabled; set to false only in environments that don't
	// deploy ukp-remote-cni (e.g. the kind e2e overlay).
	//
	// +optional
	// +default=true
	EnableCNI bool `json:"enableCNI,omitempty"`

	// EnableVPCNetworking attaches every Instance to the tenant network its
	// interfaces belong to: the provider waits for the networking stack to
	// publish the annotations that deliver each interface, and carries them on
	// the Instance Pod. Like EnableCNI this is a platform-wide setting rather
	// than a per-Instance one.
	// Defaults to disabled; set to true only in cells that run the VPC controller.
	//
	// +optional
	// +default=false
	EnableVPCNetworking bool `json:"enableVPCNetworking,omitempty"`
}
