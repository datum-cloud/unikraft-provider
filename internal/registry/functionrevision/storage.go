package functionrevision

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.datum.net/ufo/pkg/apis/functions"
	"go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

// REST implements a RESTStorage for FunctionRevision.
type REST struct {
	*genericregistry.Store
}

// StatusREST implements the REST endpoint for updating FunctionRevision status.
type StatusREST struct {
	store *genericregistry.Store
}

// New creates a new FunctionRevision object.
func (s *StatusREST) New() runtime.Object {
	return &functions.FunctionRevision{}
}

// Destroy cleans up resources on shutdown.
func (s *StatusREST) Destroy() {}

// Get retrieves the object from the storage.
func (s *StatusREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, options)
}

// Update alters the status subset of an object.
func (s *StatusREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
}

// NewStorage creates a new REST storage for FunctionRevision backed by etcd.
func NewStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*REST, *StatusREST, error) {
	strategy := newStrategy(scheme)
	statusStrategy := newStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &functions.FunctionRevision{} },
		NewListFunc:               func() runtime.Object { return &functions.FunctionRevisionList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("functionrevisions"),
		SingularQualifiedResource: v1alpha1.Resource("functionrevision"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("functionrevisions")),
	}

	options := &generic.StoreOptions{
		RESTOptions: optsGetter,
		AttrFunc:    getAttrs,
	}

	if err := store.CompleteWithOptions(options); err != nil {
		return nil, nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = statusStrategy
	statusStore.ResetFieldsStrategy = statusStrategy

	return &REST{store}, &StatusREST{store: &statusStore}, nil
}

// functionRevisionStrategy implements create/update behavior for FunctionRevision.
type functionRevisionStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func newStrategy(typer runtime.ObjectTyper) functionRevisionStrategy {
	return functionRevisionStrategy{
		ObjectTyper:   typer,
		NameGenerator: names.SimpleNameGenerator,
	}
}

func (s functionRevisionStrategy) NamespaceScoped() bool { return true }

func (s functionRevisionStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	rev := obj.(*functions.FunctionRevision)
	rev.Status = functions.FunctionRevisionStatus{}
}

func (s functionRevisionStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newRev := obj.(*functions.FunctionRevision)
	oldRev := old.(*functions.FunctionRevision)
	newRev.Status = oldRev.Status
}

func (s functionRevisionStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return nil
}

func (s functionRevisionStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

func (s functionRevisionStrategy) AllowCreateOnUpdate() bool     { return false }
func (s functionRevisionStrategy) AllowUnconditionalUpdate() bool { return true }
func (s functionRevisionStrategy) Canonicalize(obj runtime.Object) {}

func (s functionRevisionStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return nil
}

func (s functionRevisionStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

// functionRevisionStatusStrategy implements status update behavior for FunctionRevision.
type functionRevisionStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func newStatusStrategy(typer runtime.ObjectTyper) functionRevisionStatusStrategy {
	return functionRevisionStatusStrategy{
		ObjectTyper:   typer,
		NameGenerator: names.SimpleNameGenerator,
	}
}

func (s functionRevisionStatusStrategy) NamespaceScoped() bool { return true }

func (s functionRevisionStatusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newRev := obj.(*functions.FunctionRevision)
	oldRev := old.(*functions.FunctionRevision)
	newRev.Spec = oldRev.Spec
}

func (s functionRevisionStatusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return nil
}

func (s functionRevisionStatusStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

func (s functionRevisionStatusStrategy) AllowCreateOnUpdate() bool     { return false }
func (s functionRevisionStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (s functionRevisionStatusStrategy) Canonicalize(obj runtime.Object) {}

func (s functionRevisionStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"functions.datumapis.com/v1alpha1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
	}
}

func getAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	rev, ok := obj.(*functions.FunctionRevision)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not a FunctionRevision")
	}
	return rev.ObjectMeta.Labels, selectableFields(rev), nil
}

func selectableFields(rev *functions.FunctionRevision) fields.Set {
	return generic.ObjectMetaFieldsSet(&rev.ObjectMeta, true)
}

func matchFunctionRevision(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label:    label,
		Field:    field,
		GetAttrs: getAttrs,
	}
}
