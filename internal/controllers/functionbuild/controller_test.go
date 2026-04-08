package functionbuild_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
	"go.datum.net/ufo/internal/controllers/functionbuild"
)

func newBuildScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, functionsv1alpha1.AddToScheme(s))
	require.NoError(t, batchv1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func reconcileBuild(t *testing.T, r *functionbuild.Reconciler, namespace, name string) (reconcile.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
}

func newGitFunctionBuild(namespace, name string) *functionsv1alpha1.FunctionBuild {
	return &functionsv1alpha1.FunctionBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: functionsv1alpha1.FunctionBuildSpec{
			Source: functionsv1alpha1.FunctionSource{
				Git: &functionsv1alpha1.GitSource{
					URL: "https://github.com/example/my-function",
					Ref: functionsv1alpha1.GitRef{Branch: "main"},
				},
			},
			Language: "go",
		},
	}
}

func TestFunctionBuildController_GitSource_CreatesJob(t *testing.T) {
	s := newBuildScheme(t)
	build := newGitFunctionBuild("default", "mybuild")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(build).WithStatusSubresource(build).Build()
	r := &functionbuild.Reconciler{Client: c, Scheme: s, KraftImage: "ghcr.io/unikraft/kraftkit:latest"}

	_, err := reconcileBuild(t, r, "default", "mybuild")
	require.NoError(t, err)

	var job batchv1.Job
	jobName := fmt.Sprintf("build-%s", build.Name)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: jobName}, &job))

	// Verify initContainer has git clone.
	require.NotEmpty(t, job.Spec.Template.Spec.InitContainers)
	initContainer := job.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, "git-clone", initContainer.Name)
	assert.Contains(t, initContainer.Command, "git")
	assert.Contains(t, initContainer.Command, "https://github.com/example/my-function")

	// Verify main container has kraft command.
	require.NotEmpty(t, job.Spec.Template.Spec.Containers)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "kraft-build", container.Name)
	assert.Contains(t, container.Command, "kraft")

	// Verify KRAFTCLOUD_TOKEN env var is present.
	foundToken := false
	for _, env := range container.Env {
		if env.Name == "KRAFTCLOUD_TOKEN" {
			foundToken = true
			require.NotNil(t, env.ValueFrom)
			require.NotNil(t, env.ValueFrom.SecretKeyRef)
			assert.Equal(t, "kraftcloud-credentials", env.ValueFrom.SecretKeyRef.Name)
		}
	}
	assert.True(t, foundToken, "KRAFTCLOUD_TOKEN env var should be present")
}

func TestFunctionBuildController_GitSource_StatusSetToRunning(t *testing.T) {
	s := newBuildScheme(t)
	build := newGitFunctionBuild("default", "runbuild")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(build).WithStatusSubresource(build).Build()
	r := &functionbuild.Reconciler{Client: c, Scheme: s, KraftImage: "ghcr.io/unikraft/kraftkit:latest"}

	_, err := reconcileBuild(t, r, "default", "runbuild")
	require.NoError(t, err)

	var updated functionsv1alpha1.FunctionBuild
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "runbuild"}, &updated))
	assert.Equal(t, functionsv1alpha1.BuildPhaseRunning, updated.Status.Phase)
	assert.Equal(t, fmt.Sprintf("build-%s", build.Name), updated.Status.JobRef)
	assert.NotNil(t, updated.Status.StartTime, "StartTime should be set")
}

func TestFunctionBuildController_JobSucceeds_ConfigMapPresent_Succeeded(t *testing.T) {
	s := newBuildScheme(t)
	build := newGitFunctionBuild("default", "successbuild")
	build.Status.Phase = functionsv1alpha1.BuildPhaseRunning

	jobName := fmt.Sprintf("build-%s", build.Name)
	resultConfigMapName := fmt.Sprintf("build-result-%s", jobName)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	imageRef := "registry.unikraft.cloud/org/fn@sha256:built"
	resultData, err := json.Marshal(map[string]string{"imageRef": imageRef})
	require.NoError(t, err)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resultConfigMapName,
			Namespace: "default",
		},
		Data: map[string]string{
			"result": string(resultData),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(build, job, cm).
		WithStatusSubresource(build).
		Build()
	r := &functionbuild.Reconciler{Client: c, Scheme: s, KraftImage: "ghcr.io/unikraft/kraftkit:latest"}

	_, reconcileErr := reconcileBuild(t, r, "default", "successbuild")
	require.NoError(t, reconcileErr)

	var updated functionsv1alpha1.FunctionBuild
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "successbuild"}, &updated))
	assert.Equal(t, functionsv1alpha1.BuildPhaseSucceeded, updated.Status.Phase)
	assert.Equal(t, imageRef, updated.Status.ImageRef)
	assert.NotNil(t, updated.Status.CompletionTime, "CompletionTime should be set")
}

func TestFunctionBuildController_JobFails_PhaseFailed(t *testing.T) {
	s := newBuildScheme(t)
	build := newGitFunctionBuild("default", "failbuild")
	build.Status.Phase = functionsv1alpha1.BuildPhaseRunning

	jobName := fmt.Sprintf("build-%s", build.Name)
	failureMessage := "kraft cloud build exited with code 1"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Message: failureMessage,
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(build, job).
		WithStatusSubresource(build).
		Build()
	r := &functionbuild.Reconciler{Client: c, Scheme: s, KraftImage: "ghcr.io/unikraft/kraftkit:latest"}

	_, err := reconcileBuild(t, r, "default", "failbuild")
	require.NoError(t, err)

	var updated functionsv1alpha1.FunctionBuild
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "failbuild"}, &updated))
	assert.Equal(t, functionsv1alpha1.BuildPhaseFailed, updated.Status.Phase)
	assert.Equal(t, failureMessage, updated.Status.Message)
	assert.NotNil(t, updated.Status.CompletionTime, "CompletionTime should be set on failure")
}

func TestFunctionBuildController_AlreadyTerminal_NoJobCreated(t *testing.T) {
	s := newBuildScheme(t)

	// A build in Succeeded state should not create another Job.
	build := newGitFunctionBuild("default", "donebuild")
	build.Status.Phase = functionsv1alpha1.BuildPhaseSucceeded
	build.Status.ImageRef = "registry.unikraft.cloud/org/fn@sha256:done"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(build).WithStatusSubresource(build).Build()
	r := &functionbuild.Reconciler{Client: c, Scheme: s, KraftImage: "ghcr.io/unikraft/kraftkit:latest"}

	_, err := reconcileBuild(t, r, "default", "donebuild")
	require.NoError(t, err)

	var job batchv1.Job
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "build-donebuild"}, &job)
	assert.True(t, apierrors.IsNotFound(err), "no Job should be created for a terminal-phase build")
}

func TestFunctionBuildController_GitRef_BranchInInitContainer(t *testing.T) {
	s := newBuildScheme(t)
	build := &functionsv1alpha1.FunctionBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "branchbuild", Namespace: "default"},
		Spec: functionsv1alpha1.FunctionBuildSpec{
			Source: functionsv1alpha1.FunctionSource{
				Git: &functionsv1alpha1.GitSource{
					URL: "https://github.com/example/fn",
					Ref: functionsv1alpha1.GitRef{Branch: "feat/my-feature"},
				},
			},
			Language: "go",
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(build).WithStatusSubresource(build).Build()
	r := &functionbuild.Reconciler{Client: c, Scheme: s, KraftImage: "ghcr.io/unikraft/kraftkit:latest"}

	_, err := reconcileBuild(t, r, "default", "branchbuild")
	require.NoError(t, err)

	var job batchv1.Job
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "build-branchbuild"}, &job))
	initCmd := strings.Join(job.Spec.Template.Spec.InitContainers[0].Command, " ")
	assert.Contains(t, initCmd, "feat/my-feature", "branch name should appear in git clone command")
}

func TestFunctionBuildController_GitRef_CommitOverridesBranch(t *testing.T) {
	s := newBuildScheme(t)
	build := &functionsv1alpha1.FunctionBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "commitbuild", Namespace: "default"},
		Spec: functionsv1alpha1.FunctionBuildSpec{
			Source: functionsv1alpha1.FunctionSource{
				Git: &functionsv1alpha1.GitSource{
					URL: "https://github.com/example/fn",
					Ref: functionsv1alpha1.GitRef{
						Branch: "main",
						Commit: "abc123deadbeef",
					},
				},
			},
			Language: "go",
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(build).WithStatusSubresource(build).Build()
	r := &functionbuild.Reconciler{Client: c, Scheme: s, KraftImage: "ghcr.io/unikraft/kraftkit:latest"}

	_, err := reconcileBuild(t, r, "default", "commitbuild")
	require.NoError(t, err)

	var job batchv1.Job
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "build-commitbuild"}, &job))
	initCmd := strings.Join(job.Spec.Template.Spec.InitContainers[0].Command, " ")
	assert.Contains(t, initCmd, "abc123deadbeef", "commit SHA should take precedence over branch")
	assert.NotContains(t, initCmd, "main")
}
