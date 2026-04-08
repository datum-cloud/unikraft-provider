package functionrevisiongc_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
	"go.datum.net/ufo/internal/controllers/functionrevisiongc"
)

func newGCScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, functionsv1alpha1.AddToScheme(s))
	return s
}

func reconcileGC(t *testing.T, r *functionrevisiongc.Reconciler, namespace, name string) (reconcile.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
}

func ptr[T any](v T) *T { return &v }

func newFunction(namespace, name string, limit *int32, activeRevision string) *functionsv1alpha1.Function {
	fn := &functionsv1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: functionsv1alpha1.FunctionSpec{
			Source: functionsv1alpha1.FunctionSource{
				Image: &functionsv1alpha1.ImageSource{Ref: "registry.unikraft.cloud/org/fn@sha256:abc"},
			},
			Runtime: functionsv1alpha1.FunctionRuntime{Language: "go"},
			RevisionHistoryLimit: limit,
		},
		Status: functionsv1alpha1.FunctionStatus{
			ActiveRevision: activeRevision,
		},
	}
	return fn
}

// makeRetiredRevisions creates n FunctionRevisions in Retired phase labelled to the given function.
// Revisions are named <fnName>-<i> for i in [1..n].
// The Generation field on ObjectMeta is set to int64(i) so sorting works.
func makeRetiredRevisions(namespace, fnName string, count int) []*functionsv1alpha1.FunctionRevision {
	revs := make([]*functionsv1alpha1.FunctionRevision, count)
	for i := 1; i <= count; i++ {
		revs[i-1] = &functionsv1alpha1.FunctionRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name:       fmt.Sprintf("%s-%d", fnName, i),
				Namespace:  namespace,
				Generation: int64(i),
				Labels: map[string]string{
					"functions.datumapis.com/function-name": fnName,
				},
			},
			Status: functionsv1alpha1.FunctionRevisionStatus{
				Phase: functionsv1alpha1.RevisionPhaseRetired,
			},
		}
	}
	return revs
}

func buildFakeClient(t *testing.T, s *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()
	statusObjs := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		statusObjs = append(statusObjs, o)
	}
	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(statusObjs...).
		Build()
}

func TestFunctionRevisionGC_FewerThanLimit_NoDeletes(t *testing.T) {
	s := newGCScheme(t)
	fn := newFunction("default", "fn", ptr(int32(10)), "fn-3")
	revs := makeRetiredRevisions("default", "fn", 3) // well under limit of 10

	objs := []client.Object{fn}
	for _, r := range revs {
		objs = append(objs, r)
	}
	c := buildFakeClient(t, s, objs...)
	r := &functionrevisiongc.Reconciler{Client: c, Scheme: s}

	_, err := reconcileGC(t, r, "default", "fn")
	require.NoError(t, err)

	// All 3 revisions should still exist.
	for _, rev := range revs {
		var got functionsv1alpha1.FunctionRevision
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: rev.Name}, &got),
			"revision %s should not have been deleted", rev.Name)
	}
}

func TestFunctionRevisionGC_AtLimit_NoDeletes(t *testing.T) {
	s := newGCScheme(t)
	limit := int32(5)
	fn := newFunction("default", "fn2", &limit, "fn2-5")
	revs := makeRetiredRevisions("default", "fn2", 5) // exactly at limit

	objs := []client.Object{fn}
	for _, r := range revs {
		objs = append(objs, r)
	}
	c := buildFakeClient(t, s, objs...)
	r := &functionrevisiongc.Reconciler{Client: c, Scheme: s}

	_, err := reconcileGC(t, r, "default", "fn2")
	require.NoError(t, err)

	for _, rev := range revs {
		var got functionsv1alpha1.FunctionRevision
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: rev.Name}, &got))
	}
}

func TestFunctionRevisionGC_ExceedsLimit_DeletesOldestRetired(t *testing.T) {
	s := newGCScheme(t)
	limit := int32(3)
	fn := newFunction("default", "fn3", &limit, "") // no active revision
	// Create 5 retired revisions; oldest are fn3-1 and fn3-2.
	revs := makeRetiredRevisions("default", "fn3", 5)

	objs := []client.Object{fn}
	for _, r := range revs {
		objs = append(objs, r)
	}
	c := buildFakeClient(t, s, objs...)
	r := &functionrevisiongc.Reconciler{Client: c, Scheme: s}

	_, err := reconcileGC(t, r, "default", "fn3")
	require.NoError(t, err)

	// fn3-1 and fn3-2 (lowest generation = oldest) should be deleted.
	for _, name := range []string{"fn3-1", "fn3-2"} {
		var got functionsv1alpha1.FunctionRevision
		err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got)
		assert.True(t, apierrors.IsNotFound(err), "revision %s should have been GC'd", name)
	}

	// fn3-3, fn3-4, fn3-5 should remain.
	for _, name := range []string{"fn3-3", "fn3-4", "fn3-5"} {
		var got functionsv1alpha1.FunctionRevision
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got),
			"revision %s should not have been deleted", name)
	}
}

func TestFunctionRevisionGC_ActiveRevisionNeverDeleted(t *testing.T) {
	s := newGCScheme(t)
	limit := int32(2)
	// Active revision is fn4-1 (oldest). Even over limit it must not be deleted.
	fn := newFunction("default", "fn4", &limit, "fn4-1")
	revs := makeRetiredRevisions("default", "fn4", 4) // 4 revisions, limit=2

	// Override fn4-1's phase to something non-Retired so it's a candidate but should be
	// protected by the active-revision check.
	revs[0].Status.Phase = functionsv1alpha1.RevisionPhaseReady // fn4-1 is active

	objs := []client.Object{fn}
	for _, r := range revs {
		objs = append(objs, r)
	}
	c := buildFakeClient(t, s, objs...)
	r := &functionrevisiongc.Reconciler{Client: c, Scheme: s}

	_, err := reconcileGC(t, r, "default", "fn4")
	require.NoError(t, err)

	// Active revision fn4-1 must always survive.
	var active functionsv1alpha1.FunctionRevision
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fn4-1"}, &active),
		"active revision should never be deleted")
}

func TestFunctionRevisionGC_CustomLimit_Respected(t *testing.T) {
	s := newGCScheme(t)
	customLimit := int32(1)
	fn := newFunction("default", "fn5", &customLimit, "")
	revs := makeRetiredRevisions("default", "fn5", 4)

	objs := []client.Object{fn}
	for _, r := range revs {
		objs = append(objs, r)
	}
	c := buildFakeClient(t, s, objs...)
	r := &functionrevisiongc.Reconciler{Client: c, Scheme: s}

	_, err := reconcileGC(t, r, "default", "fn5")
	require.NoError(t, err)

	// With limit=1, only fn5-4 (newest, highest generation) should remain.
	var surviving functionsv1alpha1.FunctionRevision
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fn5-4"}, &surviving))

	// fn5-1, fn5-2, fn5-3 should be deleted.
	for _, name := range []string{"fn5-1", "fn5-2", "fn5-3"} {
		var got functionsv1alpha1.FunctionRevision
		err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got)
		assert.True(t, apierrors.IsNotFound(err), "revision %s should have been GC'd with limit=1", name)
	}
}

func TestFunctionRevisionGC_NonRetiredRevisions_NotDeleted(t *testing.T) {
	s := newGCScheme(t)
	limit := int32(1)
	fn := newFunction("default", "fn6", &limit, "")
	// Mix retired and non-retired revisions.
	revs := []*functionsv1alpha1.FunctionRevision{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "fn6-1", Namespace: "default", Generation: 1,
				Labels: map[string]string{"functions.datumapis.com/function-name": "fn6"},
			},
			Status: functionsv1alpha1.FunctionRevisionStatus{Phase: functionsv1alpha1.RevisionPhaseRetired},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "fn6-2", Namespace: "default", Generation: 2,
				Labels: map[string]string{"functions.datumapis.com/function-name": "fn6"},
			},
			Status: functionsv1alpha1.FunctionRevisionStatus{Phase: functionsv1alpha1.RevisionPhaseReady},
		},
	}

	objs := []client.Object{fn}
	for _, r := range revs {
		objs = append(objs, r)
	}
	c := buildFakeClient(t, s, objs...)
	r := &functionrevisiongc.Reconciler{Client: c, Scheme: s}

	_, err := reconcileGC(t, r, "default", "fn6")
	require.NoError(t, err)

	// fn6-2 (Ready, not Retired) should NOT be deleted even though limit=1 and it's beyond limit.
	var ready functionsv1alpha1.FunctionRevision
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fn6-2"}, &ready),
		"non-retired revision should never be deleted by GC controller")
}
