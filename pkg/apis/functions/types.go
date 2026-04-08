package functions

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FunctionAccess controls whether a function is reachable from the public internet.
type FunctionAccess string

const (
	// FunctionAccessPublic makes the function reachable from the public internet.
	FunctionAccessPublic FunctionAccess = "Public"
	// FunctionAccessPrivate makes the function reachable only within the cluster.
	FunctionAccessPrivate FunctionAccess = "Private"
)

// RevisionPhase describes the lifecycle phase of a FunctionRevision.
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

// BuildPhase describes the lifecycle phase of a FunctionBuild.
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

// Condition type constants for Function.
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

// Condition type constants for FunctionRevision.
const (
	// FunctionRevisionConditionReady indicates the revision is ready to serve traffic.
	FunctionRevisionConditionReady = "Ready"
	// FunctionRevisionConditionWorkloadReady indicates the underlying Workload is ready.
	FunctionRevisionConditionWorkloadReady = "WorkloadReady"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Function is the primary user-facing resource for serverless functions.
type Function struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   FunctionSpec
	Status FunctionStatus
}

// FunctionSpec defines the desired state of a Function.
type FunctionSpec struct {
	Source               FunctionSource
	Runtime              FunctionRuntime
	Scaling              ScalingConfig
	Access               FunctionAccess
	Env                  []corev1.EnvVar
	RevisionHistoryLimit *int32
}

// FunctionSource defines where to obtain the function code or image.
type FunctionSource struct {
	Git   *GitSource
	Image *ImageSource
}

// GitSource describes a git repository to build from.
type GitSource struct {
	URL        string
	Ref        GitRef
	ContextDir string
}

// GitRef selects a specific git revision.
type GitRef struct {
	Branch string
	Tag    string
	Commit string
}

// ImageSource describes a pre-built Unikraft image.
type ImageSource struct {
	Ref string
}

// FunctionRuntime defines the execution environment for a function.
type FunctionRuntime struct {
	Language  string
	Port      *int32
	Resources ResourceRequirements
	Command   []string
	Env       []corev1.EnvVar
}

// ResourceRequirements describes the compute resource requirements.
type ResourceRequirements struct {
	Memory string
}

// ScalingConfig defines scale-to-zero behaviour for a function.
type ScalingConfig struct {
	MinReplicas     *int32
	MaxReplicas     *int32
	CooldownSeconds *int32
}

// FunctionStatus defines the observed state of a Function.
type FunctionStatus struct {
	ActiveRevision     string
	ReadyRevision      string
	ObservedGeneration int64
	Hostname           string
	Conditions         []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FunctionList is a list of Function objects.
type FunctionList struct {
	metav1.TypeMeta
	metav1.ListMeta

	Items []Function
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FunctionRevision is an immutable snapshot of a deployed function version.
type FunctionRevision struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   FunctionRevisionSpec
	Status FunctionRevisionStatus
}

// FunctionRevisionSpec defines the desired state of a FunctionRevision.
type FunctionRevisionSpec struct {
	FunctionSpec FunctionSpec
	ImageRef     string
	BuildRef     string
}

// FunctionRevisionStatus defines the observed state of a FunctionRevision.
type FunctionRevisionStatus struct {
	Phase       RevisionPhase
	WorkloadRef string
	Conditions  []metav1.Condition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FunctionRevisionList is a list of FunctionRevision objects.
type FunctionRevisionList struct {
	metav1.TypeMeta
	metav1.ListMeta

	Items []FunctionRevision
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FunctionBuild tracks a single build job for a function revision.
type FunctionBuild struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   FunctionBuildSpec
	Status FunctionBuildStatus
}

// FunctionBuildSpec defines the desired state of a FunctionBuild.
type FunctionBuildSpec struct {
	Source            FunctionSource
	Language          string
	KraftfileTemplate string
}

// FunctionBuildStatus defines the observed state of a FunctionBuild.
type FunctionBuildStatus struct {
	Phase          BuildPhase
	JobRef         string
	ImageRef       string
	StartTime      *metav1.Time
	CompletionTime *metav1.Time
	Message        string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FunctionBuildList is a list of FunctionBuild objects.
type FunctionBuildList struct {
	metav1.TypeMeta
	metav1.ListMeta

	Items []FunctionBuild
}
