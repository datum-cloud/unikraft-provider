package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FunctionAccess controls whether a function is reachable from the public internet.
// +kubebuilder:validation:Enum=Public;Private
type FunctionAccess string

const (
	// FunctionAccessPublic makes the function reachable from the public internet.
	FunctionAccessPublic FunctionAccess = "Public"
	// FunctionAccessPrivate makes the function reachable only within the cluster.
	FunctionAccessPrivate FunctionAccess = "Private"
)

const (
	// FunctionConditionReady indicates the function is serving traffic.
	FunctionConditionReady = "Ready"
	// FunctionConditionBuildSucceeded indicates the most recent build succeeded.
	FunctionConditionBuildSucceeded = "BuildSucceeded"
	// FunctionConditionRevisionReady indicates the active revision is ready.
	FunctionConditionRevisionReady = "RevisionReady"
	// FunctionConditionRouteConfigured indicates the HTTPProxy is configured.
	FunctionConditionRouteConfigured = "RouteConfigured"
	// FunctionConditionScaleToZeroReady indicates scale-to-zero is configured.
	FunctionConditionScaleToZeroReady = "ScaleToZeroReady"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fn
// +kubebuilder:printcolumn:name="Active Revision",type=string,JSONPath=`.status.activeRevision`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.status.hostname`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Function is the primary user-facing resource for serverless HTTP functions running as Unikraft unikernel VMs.
type Function struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   FunctionSpec   `json:"spec"`
	Status FunctionStatus `json:"status,omitempty"`
}

// FunctionSpec defines the desired state of a Function.
type FunctionSpec struct {
	// Source defines where to obtain the function code or image.
	// +kubebuilder:validation:Required
	Source FunctionSource `json:"source"`

	// Runtime defines the execution environment for the function.
	// +kubebuilder:validation:Required
	Runtime FunctionRuntime `json:"runtime"`

	// Scaling defines scale-to-zero behaviour.
	// +kubebuilder:validation:Optional
	Scaling ScalingConfig `json:"scaling,omitempty"`

	// Access controls whether the function is publicly reachable.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=Public
	Access FunctionAccess `json:"access,omitempty"`

	// Env defines environment variables available to all revisions.
	// +kubebuilder:validation:Optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// RevisionHistoryLimit controls how many old FunctionRevisions are retained.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=10
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
}

// FunctionSource defines where to obtain the function code or image.
// Exactly one of git or image must be set.
type FunctionSource struct {
	// Git describes a git repository to build from.
	// +kubebuilder:validation:Optional
	Git *GitSource `json:"git,omitempty"`

	// Image describes a pre-built Unikraft image.
	// +kubebuilder:validation:Optional
	Image *ImageSource `json:"image,omitempty"`
}

// GitSource describes a git repository to build from.
type GitSource struct {
	// URL is the HTTPS git URL of the repository.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Ref selects a specific git revision.
	// +kubebuilder:validation:Optional
	Ref GitRef `json:"ref,omitempty"`

	// ContextDir is the subdirectory within the repository to use as the build context.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="/"
	ContextDir string `json:"contextDir,omitempty"`
}

// GitRef selects a specific git revision.
type GitRef struct {
	// Branch is the git branch to use.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=main
	Branch string `json:"branch,omitempty"`

	// Tag is the git tag to use.
	// +kubebuilder:validation:Optional
	Tag string `json:"tag,omitempty"`

	// Commit is the full SHA of a specific commit; takes precedence over branch and tag.
	// +kubebuilder:validation:Optional
	Commit string `json:"commit,omitempty"`
}

// ImageSource describes a pre-built Unikraft image.
type ImageSource struct {
	// Ref is the full image reference including digest.
	// +kubebuilder:validation:Required
	Ref string `json:"ref"`
}

// FunctionRuntime defines the execution environment for a function.
type FunctionRuntime struct {
	// Language is the programming language of the function.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=go;nodejs;python;rust
	Language string `json:"language"`

	// Port is the port the function listens on.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=8080
	Port *int32 `json:"port,omitempty"`

	// Resources describes the compute resource requirements.
	// +kubebuilder:validation:Optional
	Resources ResourceRequirements `json:"resources,omitempty"`

	// Command overrides the default CMD for the function container.
	// +kubebuilder:validation:Optional
	Command []string `json:"command,omitempty"`

	// Env defines environment variables merged with Function.spec.env.
	// +kubebuilder:validation:Optional
	Env []corev1.EnvVar `json:"env,omitempty"`
}

// ResourceRequirements describes the compute resource requirements.
type ResourceRequirements struct {
	// Memory is the memory limit for the function, e.g. "512Mi".
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="256Mi"
	Memory string `json:"memory,omitempty"`
}

// ScalingConfig defines scale-to-zero behaviour for a function.
type ScalingConfig struct {
	// MinReplicas is the minimum number of replicas; set to 0 for scale-to-zero.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=0
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of replicas.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=10
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// CooldownSeconds is the idle time before scaling to zero.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=60
	CooldownSeconds *int32 `json:"cooldownSeconds,omitempty"`
}

// FunctionStatus defines the observed state of a Function.
type FunctionStatus struct {
	// ActiveRevision is the name of the FunctionRevision currently serving traffic.
	// +kubebuilder:validation:Optional
	ActiveRevision string `json:"activeRevision,omitempty"`

	// ReadyRevision is the name of the last revision that became ready.
	// +kubebuilder:validation:Optional
	ReadyRevision string `json:"readyRevision,omitempty"`

	// ObservedGeneration is the generation last processed by the controller.
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Hostname is the assigned FQDN for the function.
	// +kubebuilder:validation:Optional
	Hostname string `json:"hostname,omitempty"`

	// Conditions represent the current state of the function.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// FunctionList is a list of Function objects.
type FunctionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Function `json:"items"`
}
