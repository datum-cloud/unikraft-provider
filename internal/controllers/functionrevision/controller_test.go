package functionrevision_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
	"go.datum.net/ufo/internal/controllers/functionrevision"
)

func newRevScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, functionsv1alpha1.AddToScheme(s))
	require.NoError(t, computev1alpha.AddToScheme(s))
	require.NoError(t, networkingv1alpha.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func ptr[T any](v T) *T { return &v }

func reconcileRevision(t *testing.T, r *functionrevision.Reconciler, namespace, name string) (reconcile.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
}

func newRevision(namespace, name, functionName, imageRef string) *functionsv1alpha1.FunctionRevision {
	rev := &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"functions.datumapis.com/function-name": functionName,
				"functions.datumapis.com/generation":    "1",
			},
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: functionsv1alpha1.FunctionSpec{
				Source: functionsv1alpha1.FunctionSource{
					Image: &functionsv1alpha1.ImageSource{Ref: imageRef},
				},
				Runtime: functionsv1alpha1.FunctionRuntime{
					Language: "go",
					Port:     ptr(int32(8080)),
				},
			},
			ImageRef: imageRef,
		},
	}
	return rev
}

func TestFunctionRevisionController_EmptyImageRef_NoWorkload(t *testing.T) {
	s := newRevScheme(t)
	rev := &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fn-1",
			Namespace: "default",
			Labels: map[string]string{
				"functions.datumapis.com/function-name": "fn",
			},
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: functionsv1alpha1.FunctionSpec{
				Runtime: functionsv1alpha1.FunctionRuntime{Language: "go"},
			},
			ImageRef: "", // no image yet
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(rev).WithStatusSubresource(rev).Build()
	r := &functionrevision.Reconciler{Client: c, Scheme: s}

	_, err := reconcileRevision(t, r, "default", "fn-1")
	require.NoError(t, err)

	// No Workload should have been created.
	var wl computev1alpha.Workload
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fn-1"}, &wl)
	assert.True(t, apierrors.IsNotFound(err), "Workload should not exist when imageRef is empty")
}

func TestFunctionRevisionController_EmptyImageRef_NoBuild_PhasePending(t *testing.T) {
	s := newRevScheme(t)
	rev := &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fn-pending",
			Namespace: "default",
			Labels: map[string]string{
				"functions.datumapis.com/function-name": "fn",
			},
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: functionsv1alpha1.FunctionSpec{
				Runtime: functionsv1alpha1.FunctionRuntime{Language: "go"},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(rev).WithStatusSubresource(rev).Build()
	r := &functionrevision.Reconciler{Client: c, Scheme: s}

	_, err := reconcileRevision(t, r, "default", "fn-pending")
	require.NoError(t, err)

	var updated functionsv1alpha1.FunctionRevision
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fn-pending"}, &updated))
	assert.Equal(t, functionsv1alpha1.RevisionPhasePending, updated.Status.Phase)
}

func TestFunctionRevisionController_ImageRefSet_CreatesWorkload(t *testing.T) {
	s := newRevScheme(t)
	imageRef := "registry.unikraft.cloud/org/fn@sha256:abc"
	rev := newRevision("default", "fn-1", "fn", imageRef)

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(rev).WithStatusSubresource(rev).Build()
	r := &functionrevision.Reconciler{Client: c, Scheme: s}

	_, err := reconcileRevision(t, r, "default", "fn-1")
	require.NoError(t, err)

	var wl computev1alpha.Workload
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fn-1"}, &wl))
	// Verify VirtualMachine runtime with image reference in disk populator.
	require.NotNil(t, wl.Spec.Template.Spec.Runtime.VirtualMachine)
	require.NotEmpty(t, wl.Spec.Template.Spec.Volumes)
	bootVol := wl.Spec.Template.Spec.Volumes[0]
	require.NotNil(t, bootVol.VolumeSource.Disk)
	require.NotNil(t, bootVol.VolumeSource.Disk.Template)
	require.NotNil(t, bootVol.VolumeSource.Disk.Template.Spec.Populator)
	require.NotNil(t, bootVol.VolumeSource.Disk.Template.Spec.Populator.Image)
	assert.Equal(t, imageRef, bootVol.VolumeSource.Disk.Template.Spec.Populator.Image.Name)
}

func TestFunctionRevisionController_WorkloadAvailable_RevisionReady(t *testing.T) {
	s := newRevScheme(t)
	imageRef := "registry.unikraft.cloud/org/fn@sha256:abc"
	rev := newRevision("default", "fn-rev-ready", "fn", imageRef)

	// Pre-create an Available workload.
	wl := &computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fn-rev-ready",
			Namespace: "default",
			Labels: map[string]string{
				"functions.datumapis.com/function-name": "fn",
			},
		},
		Spec: computev1alpha.WorkloadSpec{
			Template: computev1alpha.InstanceTemplateSpec{
				Spec: computev1alpha.InstanceSpec{
					Runtime: computev1alpha.InstanceRuntimeSpec{
						Resources: computev1alpha.InstanceRuntimeResources{
							InstanceType: "datumcloud/d1-standard-2",
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						VirtualMachine: &computev1alpha.VirtualMachineRuntime{},
					},
				},
			},
		},
		Status: computev1alpha.WorkloadStatus{
			Replicas: 1,
			Conditions: []metav1.Condition{
				{
					Type:               computev1alpha.WorkloadAvailable,
					Status:             metav1.ConditionTrue,
					Reason:             "Available",
					Message:            "Workload is available",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(rev, wl).
		WithStatusSubresource(rev, wl).
		Build()
	r := &functionrevision.Reconciler{Client: c, Scheme: s}

	_, err := reconcileRevision(t, r, "default", "fn-rev-ready")
	require.NoError(t, err)

	var updated functionsv1alpha1.FunctionRevision
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fn-rev-ready"}, &updated))
	assert.Equal(t, functionsv1alpha1.RevisionPhaseReady, updated.Status.Phase)
	assert.Equal(t, "fn-rev-ready", updated.Status.WorkloadRef)

	// Verify Ready condition.
	found := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == functionsv1alpha1.FunctionRevisionConditionReady {
			found = true
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
		}
	}
	assert.True(t, found, "Ready condition should be set to true")
}

func TestFunctionRevisionController_FunctionBuildSucceeded_CopiesImageRef(t *testing.T) {
	s := newRevScheme(t)

	// Revision with no imageRef, waiting for build.
	rev := &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fn-build-done",
			Namespace: "default",
			Labels: map[string]string{
				"functions.datumapis.com/function-name": "fn",
			},
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: functionsv1alpha1.FunctionSpec{
				Source: functionsv1alpha1.FunctionSource{
					Git: &functionsv1alpha1.GitSource{URL: "https://github.com/example/fn"},
				},
				Runtime: functionsv1alpha1.FunctionRuntime{Language: "go"},
			},
		},
	}

	imageRef := "registry.unikraft.cloud/org/fn@sha256:built"
	build := &functionsv1alpha1.FunctionBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("build-%s", rev.Name),
			Namespace: "default",
		},
		Spec: functionsv1alpha1.FunctionBuildSpec{
			Source:   rev.Spec.FunctionSpec.Source,
			Language: "go",
		},
		Status: functionsv1alpha1.FunctionBuildStatus{
			Phase:    functionsv1alpha1.BuildPhaseSucceeded,
			ImageRef: imageRef,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(rev, build).
		WithStatusSubresource(rev, build).
		Build()
	r := &functionrevision.Reconciler{Client: c, Scheme: s}

	_, err := reconcileRevision(t, r, "default", "fn-build-done")
	require.NoError(t, err)

	// Revision should now have the imageRef patched in.
	var updated functionsv1alpha1.FunctionRevision
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fn-build-done"}, &updated))
	assert.Equal(t, imageRef, updated.Spec.ImageRef)
}
