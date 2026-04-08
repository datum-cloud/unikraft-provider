package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildPhase describes the lifecycle phase of a FunctionBuild.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type BuildPhase string

const (
	// BuildPhasePending means the build has been created but not yet started.
	BuildPhasePending BuildPhase = "Pending"
	// BuildPhaseRunning means the build Job is running.
	BuildPhaseRunning BuildPhase = "Running"
	// BuildPhaseSucceeded means the build completed successfully.
	BuildPhaseSucceeded BuildPhase = "Succeeded"
	// BuildPhaseFailed means the build failed.
	BuildPhaseFailed BuildPhase = "Failed"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fnbuild
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.imageRef`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FunctionBuild tracks a single build job for a function revision.
type FunctionBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   FunctionBuildSpec   `json:"spec"`
	Status FunctionBuildStatus `json:"status,omitempty"`
}

// FunctionBuildSpec defines the desired state of a FunctionBuild.
type FunctionBuildSpec struct {
	// Source is the function source to build.
	// +kubebuilder:validation:Required
	Source FunctionSource `json:"source"`

	// Language is the programming language of the function.
	// +kubebuilder:validation:Required
	Language string `json:"language"`

	// KraftfileTemplate is the rendered Kraftfile content for this build.
	// +kubebuilder:validation:Optional
	KraftfileTemplate string `json:"kraftfileTemplate,omitempty"`
}

// FunctionBuildStatus defines the observed state of a FunctionBuild.
type FunctionBuildStatus struct {
	// Phase is the current lifecycle phase of the build.
	// +kubebuilder:validation:Optional
	Phase BuildPhase `json:"phase,omitempty"`

	// JobRef is the name of the Kubernetes Job running the build.
	// +kubebuilder:validation:Optional
	JobRef string `json:"jobRef,omitempty"`

	// ImageRef is the full image reference with digest set on successful completion.
	// +kubebuilder:validation:Optional
	ImageRef string `json:"imageRef,omitempty"`

	// StartTime is when the build started.
	// +kubebuilder:validation:Optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the build completed.
	// +kubebuilder:validation:Optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides a human-readable status or error message.
	// +kubebuilder:validation:Optional
	Message string `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// FunctionBuildList is a list of FunctionBuild objects.
type FunctionBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FunctionBuild `json:"items"`
}
