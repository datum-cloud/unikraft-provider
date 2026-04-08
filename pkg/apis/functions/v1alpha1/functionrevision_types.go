package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RevisionPhase describes the lifecycle phase of a FunctionRevision.
// +kubebuilder:validation:Enum=Pending;Building;Ready;Failed;Retired
type RevisionPhase string

const (
	// RevisionPhasePending means the revision has been created but no action taken.
	RevisionPhasePending RevisionPhase = "Pending"
	// RevisionPhaseBuilding means a FunctionBuild is running for this revision.
	RevisionPhaseBuilding RevisionPhase = "Building"
	// RevisionPhaseReady means the revision is deployed and serving traffic.
	RevisionPhaseReady RevisionPhase = "Ready"
	// RevisionPhaseFailed means the build or deployment failed.
	RevisionPhaseFailed RevisionPhase = "Failed"
	// RevisionPhaseRetired means the revision is no longer serving traffic.
	RevisionPhaseRetired RevisionPhase = "Retired"
)

const (
	// FunctionRevisionConditionReady indicates the revision is ready to serve traffic.
	FunctionRevisionConditionReady = "Ready"
	// FunctionRevisionConditionWorkloadReady indicates the underlying Workload is ready.
	FunctionRevisionConditionWorkloadReady = "WorkloadReady"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fnrev
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.imageRef`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FunctionRevision is an immutable snapshot of a deployed function version.
type FunctionRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   FunctionRevisionSpec   `json:"spec"`
	Status FunctionRevisionStatus `json:"status,omitempty"`
}

// FunctionRevisionSpec defines the desired state of a FunctionRevision.
// This is immutable after creation.
type FunctionRevisionSpec struct {
	// FunctionSpec is a full copy of the Function.spec at the time this revision was created.
	// +kubebuilder:validation:Required
	FunctionSpec FunctionSpec `json:"functionSpec"`

	// ImageRef is the Unikraft image reference with digest.
	// +kubebuilder:validation:Optional
	ImageRef string `json:"imageRef,omitempty"`

	// BuildRef is the name of the FunctionBuild that produced this image.
	// +kubebuilder:validation:Optional
	BuildRef string `json:"buildRef,omitempty"`
}

// FunctionRevisionStatus defines the observed state of a FunctionRevision.
type FunctionRevisionStatus struct {
	// Phase is the current lifecycle phase of the revision.
	// +kubebuilder:validation:Optional
	Phase RevisionPhase `json:"phase,omitempty"`

	// WorkloadRef is the name of the Workload resource for this revision.
	// +kubebuilder:validation:Optional
	WorkloadRef string `json:"workloadRef,omitempty"`

	// Conditions represent the current state of the revision.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// FunctionRevisionList is a list of FunctionRevision objects.
type FunctionRevisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FunctionRevision `json:"items"`
}
