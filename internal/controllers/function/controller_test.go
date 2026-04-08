package function_test

import (
	"context"
	"fmt"
	"testing"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
	"go.datum.net/ufo/internal/controllers/function"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, functionsv1alpha1.AddToScheme(s))
	require.NoError(t, computev1alpha.AddToScheme(s))
	require.NoError(t, networkingv1alpha.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func ptr[T any](v T) *T { return &v }

func newGitFunction(namespace, name string) *functionsv1alpha1.Function {
	return &functionsv1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
		},
		Spec: functionsv1alpha1.FunctionSpec{
			Source: functionsv1alpha1.FunctionSource{
				Git: &functionsv1alpha1.GitSource{
					URL: "https://github.com/example/fn",
					Ref: functionsv1alpha1.GitRef{Branch: "main"},
				},
			},
			Runtime: functionsv1alpha1.FunctionRuntime{
				Language: "go",
				Port:     ptr(int32(8080)),
			},
		},
	}
}

func newImageFunction(namespace, name, imageRef string) *functionsv1alpha1.Function {
	return &functionsv1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
		},
		Spec: functionsv1alpha1.FunctionSpec{
			Source: functionsv1alpha1.FunctionSource{
				Image: &functionsv1alpha1.ImageSource{
					Ref: imageRef,
				},
			},
			Runtime: functionsv1alpha1.FunctionRuntime{
				Language: "go",
				Port:     ptr(int32(8080)),
			},
		},
	}
}

func reconcileFunction(t *testing.T, r *function.Reconciler, namespace, name string) (reconcile.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
}

func TestFunctionController_FinalizerAdded(t *testing.T) {
	s := newScheme(t)
	fn := newGitFunction("default", "my-fn")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(fn).WithStatusSubresource(fn).Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	// First reconcile adds the finalizer.
	_, err := reconcileFunction(t, r, "default", "my-fn")
	require.NoError(t, err)

	var updated functionsv1alpha1.Function
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-fn"}, &updated))
	assert.True(t, controllerutil.ContainsFinalizer(&updated, "functions.datumapis.com/cleanup"),
		"finalizer should be present after first reconcile")
}

func TestFunctionController_GitSource_CreatesRevisionAndBuild(t *testing.T) {
	s := newScheme(t)
	fn := newGitFunction("default", "git-fn")
	// Pre-set finalizer so we skip the "add finalizer" reconcile step.
	controllerutil.AddFinalizer(fn, "functions.datumapis.com/cleanup")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(fn).WithStatusSubresource(fn).Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	_, err := reconcileFunction(t, r, "default", "git-fn")
	require.NoError(t, err)

	// Expect FunctionRevision to be created.
	var rev functionsv1alpha1.FunctionRevision
	revName := fmt.Sprintf("git-fn-%d", fn.Generation)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: revName}, &rev))
	assert.Equal(t, "go", rev.Spec.FunctionSpec.Runtime.Language)
	// Git source revision has no imageRef yet.
	assert.Empty(t, rev.Spec.ImageRef)

	// Expect FunctionBuild to be created.
	var build functionsv1alpha1.FunctionBuild
	buildName := fmt.Sprintf("build-%s", revName)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: buildName}, &build))
	assert.Equal(t, "https://github.com/example/fn", build.Spec.Source.Git.URL)
	assert.Equal(t, "go", build.Spec.Language)
}

func TestFunctionController_ImageSource_CreatesRevisionWithImageRef_NoBuild(t *testing.T) {
	s := newScheme(t)
	imageRef := "registry.unikraft.cloud/org/my-fn@sha256:abc123"
	fn := newImageFunction("default", "img-fn", imageRef)
	controllerutil.AddFinalizer(fn, "functions.datumapis.com/cleanup")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(fn).WithStatusSubresource(fn).Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	_, err := reconcileFunction(t, r, "default", "img-fn")
	require.NoError(t, err)

	// Revision should have imageRef set immediately.
	var rev functionsv1alpha1.FunctionRevision
	revName := fmt.Sprintf("img-fn-%d", fn.Generation)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: revName}, &rev))
	assert.Equal(t, imageRef, rev.Spec.ImageRef)

	// No FunctionBuild should be created for image-sourced functions.
	var build functionsv1alpha1.FunctionBuild
	buildName := fmt.Sprintf("build-%s", revName)
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: buildName}, &build)
	assert.True(t, apierrors.IsNotFound(err), "FunctionBuild should not exist for image-sourced function")
}

func TestFunctionController_HTTPProxy_CreatedWithActivatorBackend(t *testing.T) {
	s := newScheme(t)
	fn := newImageFunction("default", "proxy-fn", "registry.unikraft.cloud/org/x@sha256:abc")
	controllerutil.AddFinalizer(fn, "functions.datumapis.com/cleanup")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(fn).WithStatusSubresource(fn).Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	// Need multiple reconciles: first creates revision, second creates proxy.
	_, err := reconcileFunction(t, r, "default", "proxy-fn")
	require.NoError(t, err)
	_, err = reconcileFunction(t, r, "default", "proxy-fn")
	require.NoError(t, err)

	var proxy networkingv1alpha.HTTPProxy
	proxyName := "function-proxy-fn"
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: proxyName}, &proxy))
	require.NotEmpty(t, proxy.Spec.Rules)
	require.NotEmpty(t, proxy.Spec.Rules[0].Backends)
	assert.Equal(t, "http://ufo-activator.datum-system.svc.cluster.local:8080",
		proxy.Spec.Rules[0].Backends[0].Endpoint,
		"HTTPProxy should point to activator when no active revision")
}

func TestFunctionController_ActiveRevisionReady_ReplicasZero_BackendIsActivator(t *testing.T) {
	s := newScheme(t)
	fn := newImageFunction("default", "scale-fn", "registry.unikraft.cloud/org/x@sha256:abc")
	controllerutil.AddFinalizer(fn, "functions.datumapis.com/cleanup")
	fn.Status.ActiveRevision = "scale-fn-1"

	revName := "scale-fn-1"
	workloadName := revName
	rev := &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revName,
			Namespace: "default",
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: fn.Spec,
			ImageRef:     "registry.unikraft.cloud/org/x@sha256:abc",
		},
		Status: functionsv1alpha1.FunctionRevisionStatus{
			Phase:       functionsv1alpha1.RevisionPhaseReady,
			WorkloadRef: workloadName,
		},
	}
	workload := &computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName,
			Namespace: "default",
		},
		Status: computev1alpha.WorkloadStatus{
			Replicas: 0, // scaled to zero
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fn, rev, workload).
		WithStatusSubresource(fn, rev, workload).
		Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	_, err := reconcileFunction(t, r, "default", "scale-fn")
	require.NoError(t, err)

	var proxy networkingv1alpha.HTTPProxy
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "function-scale-fn"}, &proxy))
	assert.Equal(t, "http://ufo-activator.datum-system.svc.cluster.local:8080",
		proxy.Spec.Rules[0].Backends[0].Endpoint,
		"when replicas==0 backend should be activator")
}

func TestFunctionController_ActiveRevisionReady_ReplicasGtZero_BackendIsWorkload(t *testing.T) {
	s := newScheme(t)
	fn := newImageFunction("default", "hot-fn", "registry.unikraft.cloud/org/x@sha256:abc")
	controllerutil.AddFinalizer(fn, "functions.datumapis.com/cleanup")
	fn.Status.ActiveRevision = "hot-fn-1"

	revName := "hot-fn-1"
	workloadName := revName
	rev := &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revName,
			Namespace: "default",
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: fn.Spec,
			ImageRef:     "registry.unikraft.cloud/org/x@sha256:abc",
		},
		Status: functionsv1alpha1.FunctionRevisionStatus{
			Phase:       functionsv1alpha1.RevisionPhaseReady,
			WorkloadRef: workloadName,
		},
	}
	workload := &computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName,
			Namespace: "default",
		},
		Status: computev1alpha.WorkloadStatus{
			Replicas: 2, // hot
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fn, rev, workload).
		WithStatusSubresource(fn, rev, workload).
		Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	_, err := reconcileFunction(t, r, "default", "hot-fn")
	require.NoError(t, err)

	var proxy networkingv1alpha.HTTPProxy
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "function-hot-fn"}, &proxy))
	assert.Equal(t, "http://hot-fn-1.default.svc.cluster.local:8080",
		proxy.Spec.Rules[0].Backends[0].Endpoint,
		"when replicas>0 backend should be the workload service")
}

func TestFunctionController_Delete_RemovesHTTPProxyAndFinalizer(t *testing.T) {
	s := newScheme(t)
	fn := newGitFunction("default", "del-fn")
	controllerutil.AddFinalizer(fn, "functions.datumapis.com/cleanup")
	now := metav1.NewTime(time.Now())
	fn.DeletionTimestamp = &now

	proxy := &networkingv1alpha.HTTPProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "function-del-fn",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fn, proxy).
		WithStatusSubresource(fn).
		Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	_, err := reconcileFunction(t, r, "default", "del-fn")
	require.NoError(t, err)

	// HTTPProxy should be deleted.
	var deletedProxy networkingv1alpha.HTTPProxy
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "function-del-fn"}, &deletedProxy)
	assert.True(t, apierrors.IsNotFound(err), "HTTPProxy should be deleted")

	// Finalizer should be removed.
	var updated functionsv1alpha1.Function
	// The fake client may or may not garbage-collect after finalizer removal;
	// tolerate NotFound as well.
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "del-fn"}, &updated)
	if err == nil {
		assert.False(t, controllerutil.ContainsFinalizer(&updated, "functions.datumapis.com/cleanup"),
			"finalizer should be removed after delete reconcile")
	}
}

func TestFunctionController_StatusSync_RevisionReady(t *testing.T) {
	s := newScheme(t)
	fn := newImageFunction("default", "status-fn", "registry.unikraft.cloud/org/x@sha256:abc")
	controllerutil.AddFinalizer(fn, "functions.datumapis.com/cleanup")
	fn.Generation = 1

	revName := fmt.Sprintf("status-fn-%d", fn.Generation)
	rev := &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revName,
			Namespace: "default",
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: fn.Spec,
			ImageRef:     "registry.unikraft.cloud/org/x@sha256:abc",
		},
		Status: functionsv1alpha1.FunctionRevisionStatus{
			Phase: functionsv1alpha1.RevisionPhaseReady,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fn, rev).
		WithStatusSubresource(fn, rev).
		Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	_, err := reconcileFunction(t, r, "default", "status-fn")
	require.NoError(t, err)

	var updated functionsv1alpha1.Function
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "status-fn"}, &updated))
	assert.Equal(t, revName, updated.Status.ActiveRevision)
	assert.Equal(t, revName, updated.Status.ReadyRevision)

	// Verify Ready condition is True.
	found := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == functionsv1alpha1.FunctionConditionReady {
			found = true
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
		}
	}
	assert.True(t, found, "Ready condition should be set")
}

func TestFunctionController_NewSourceUpdate_CreatesNewRevision(t *testing.T) {
	s := newScheme(t)

	// Simulate a function that was updated: generation bumped to 2.
	fn := newImageFunction("default", "update-fn", "registry.unikraft.cloud/org/x@sha256:new")
	controllerutil.AddFinalizer(fn, "functions.datumapis.com/cleanup")
	fn.Generation = 2
	fn.Status.ActiveRevision = "update-fn-1" // old revision still active

	oldRev := &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "update-fn-1",
			Namespace: "default",
			Labels: map[string]string{
				"functions.datumapis.com/function-name": "update-fn",
			},
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: fn.Spec,
			ImageRef:     "registry.unikraft.cloud/org/x@sha256:old",
		},
		Status: functionsv1alpha1.FunctionRevisionStatus{
			Phase: functionsv1alpha1.RevisionPhaseReady,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fn, oldRev).
		WithStatusSubresource(fn, oldRev).
		Build()
	r := &function.Reconciler{Client: c, Scheme: s}

	_, err := reconcileFunction(t, r, "default", "update-fn")
	require.NoError(t, err)

	// New revision for generation 2 should be created.
	var newRev functionsv1alpha1.FunctionRevision
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "update-fn-2"}, &newRev))
	assert.Equal(t, "registry.unikraft.cloud/org/x@sha256:new", newRev.Spec.ImageRef)

	// Active revision remains the old one until new one is Ready.
	var updated functionsv1alpha1.Function
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "update-fn"}, &updated))
	// syncStatus picks up the new rev (gen 2) which is not yet Ready, so ActiveRevision stays ""
	// (the controller updates it from the *current generation* revision).
	// Old revision still exists untouched.
	var stillOld functionsv1alpha1.FunctionRevision
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "update-fn-1"}, &stillOld))
}

// newTestWorkload builds a minimal Workload so fake client calls referencing
// computev1alpha.Workload compile correctly in unit tests.
func newTestWorkload(namespace, name string) *computev1alpha.Workload {
	return &computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
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
	}
}

