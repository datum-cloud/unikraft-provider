package functionbuild

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

// REST implements a RESTStorage for FunctionBuild.
type REST struct {
	*genericregistry.Store
}

// StatusREST implements the REST endpoint for updating FunctionBuild status.
type StatusREST struct {
	store *genericregistry.Store
}

// New creates a new FunctionBuild object.
func (s *StatusREST) New() runtime.Object {
	return &functions.FunctionBuild{}
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

// NewStorage creates a new REST storage for FunctionBuild backed by etcd.
func NewStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*REST, *StatusREST, error) {
	strategy := newStrategy(scheme)
	statusStrategy := newStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &functions.FunctionBuild{} },
		NewListFunc:               func() runtime.Object { return &functions.FunctionBuildList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("functionbuilds"),
		SingularQualifiedResource: v1alpha1.Resource("functionbuild"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("functionbuilds")),
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

// functionBuildStrategy implements create/update behavior for FunctionBuild.
type functionBuildStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func newStrategy(typer runtime.ObjectTyper) functionBuildStrategy {
	return functionBuildStrategy{
		ObjectTyper:   typer,
		NameGenerator: names.SimpleNameGenerator,
	}
}

func (s functionBuildStrategy) NamespaceScoped() bool { return true }

func (s functionBuildStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	build := obj.(*functions.FunctionBuild)
	build.Status = functions.FunctionBuildStatus{}
}

func (s functionBuildStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newBuild := obj.(*functions.FunctionBuild)
	oldBuild := old.(*functions.FunctionBuild)
	newBuild.Status = oldBuild.Status
}

func (s functionBuildStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	build := obj.(*functions.FunctionBuild)
	return validateFunctionBuild(build)
}

func (s functionBuildStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

func (s functionBuildStrategy) AllowCreateOnUpdate() bool     { return false }
func (s functionBuildStrategy) AllowUnconditionalUpdate() bool { return true }
func (s functionBuildStrategy) Canonicalize(obj runtime.Object) {}

func (s functionBuildStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	build := obj.(*functions.FunctionBuild)
	return validateFunctionBuild(build)
}

func (s functionBuildStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

// functionBuildStatusStrategy implements status update behavior for FunctionBuild.
type functionBuildStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func newStatusStrategy(typer runtime.ObjectTyper) functionBuildStatusStrategy {
	return functionBuildStatusStrategy{
		ObjectTyper:   typer,
		NameGenerator: names.SimpleNameGenerator,
	}
}

func (s functionBuildStatusStrategy) NamespaceScoped() bool { return true }

func (s functionBuildStatusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newBuild := obj.(*functions.FunctionBuild)
	oldBuild := old.(*functions.FunctionBuild)
	newBuild.Spec = oldBuild.Spec
}

func (s functionBuildStatusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return nil
}

func (s functionBuildStatusStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

func (s functionBuildStatusStrategy) AllowCreateOnUpdate() bool     { return false }
func (s functionBuildStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (s functionBuildStatusStrategy) Canonicalize(obj runtime.Object) {}

func (s functionBuildStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"functions.datumapis.com/v1alpha1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
	}
}

func validateFunctionBuild(build *functions.FunctionBuild) field.ErrorList {
	allErrs := field.ErrorList{}
	specPath := field.NewPath("spec")

	if build.Spec.Language == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("language"), "language is required"))
	}

	sourcePath := specPath.Child("source")
	if build.Spec.Source.Git == nil && build.Spec.Source.Image == nil {
		allErrs = append(allErrs, field.Required(sourcePath, "either git or image source must be specified"))
	}

	return allErrs
}

func getAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	build, ok := obj.(*functions.FunctionBuild)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not a FunctionBuild")
	}
	return build.ObjectMeta.Labels, selectableFields(build), nil
}

func selectableFields(build *functions.FunctionBuild) fields.Set {
	return generic.ObjectMetaFieldsSet(&build.ObjectMeta, true)
}

func matchFunctionBuild(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label:    label,
		Field:    field,
		GetAttrs: getAttrs,
	}
}
