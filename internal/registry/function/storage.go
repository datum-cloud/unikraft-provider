package function

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

// REST implements a RESTStorage for Function.
type REST struct {
	*genericregistry.Store
}

// StatusREST implements the REST endpoint for updating Function status.
type StatusREST struct {
	store *genericregistry.Store
}

// New creates a new Function object.
func (s *StatusREST) New() runtime.Object {
	return &functions.Function{}
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

// NewStorage creates a new REST storage for Function backed by etcd.
func NewStorage(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*REST, *StatusREST, error) {
	strategy := newStrategy(scheme)
	statusStrategy := newStatusStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &functions.Function{} },
		NewListFunc:               func() runtime.Object { return &functions.FunctionList{} },
		DefaultQualifiedResource:  v1alpha1.Resource("functions"),
		SingularQualifiedResource: v1alpha1.Resource("function"),

		CreateStrategy: strategy,
		UpdateStrategy: strategy,
		DeleteStrategy: strategy,

		TableConvertor: rest.NewDefaultTableConvertor(v1alpha1.Resource("functions")),
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

// functionStrategy implements create/update behavior for Function.
type functionStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func newStrategy(typer runtime.ObjectTyper) functionStrategy {
	return functionStrategy{
		ObjectTyper:   typer,
		NameGenerator: names.SimpleNameGenerator,
	}
}

func (s functionStrategy) NamespaceScoped() bool { return true }

func (s functionStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	fn := obj.(*functions.Function)
	fn.Status = functions.FunctionStatus{}
}

func (s functionStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newFn := obj.(*functions.Function)
	oldFn := old.(*functions.Function)
	newFn.Status = oldFn.Status
}

func (s functionStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	fn := obj.(*functions.Function)
	return validateFunction(fn)
}

func (s functionStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

func (s functionStrategy) AllowCreateOnUpdate() bool  { return false }
func (s functionStrategy) AllowUnconditionalUpdate() bool { return true }
func (s functionStrategy) Canonicalize(obj runtime.Object) {}

func (s functionStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	fn := obj.(*functions.Function)
	return validateFunction(fn)
}

func (s functionStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

// functionStatusStrategy implements status update behavior for Function.
type functionStatusStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func newStatusStrategy(typer runtime.ObjectTyper) functionStatusStrategy {
	return functionStatusStrategy{
		ObjectTyper:   typer,
		NameGenerator: names.SimpleNameGenerator,
	}
}

func (s functionStatusStrategy) NamespaceScoped() bool { return true }

func (s functionStatusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newFn := obj.(*functions.Function)
	oldFn := old.(*functions.Function)
	newFn.Spec = oldFn.Spec
}

func (s functionStatusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return nil
}

func (s functionStatusStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

func (s functionStatusStrategy) AllowCreateOnUpdate() bool     { return false }
func (s functionStatusStrategy) AllowUnconditionalUpdate() bool { return true }
func (s functionStatusStrategy) Canonicalize(obj runtime.Object) {}

func (s functionStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"functions.datumapis.com/v1alpha1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
	}
}

func validateFunction(fn *functions.Function) field.ErrorList {
	allErrs := field.ErrorList{}
	specPath := field.NewPath("spec")

	sourcePath := specPath.Child("source")
	if fn.Spec.Source.Git == nil && fn.Spec.Source.Image == nil {
		allErrs = append(allErrs, field.Required(sourcePath, "either git or image source must be specified"))
	}
	if fn.Spec.Source.Git != nil && fn.Spec.Source.Image != nil {
		allErrs = append(allErrs, field.Invalid(sourcePath, fn.Spec.Source, "only one of git or image source may be specified"))
	}
	if fn.Spec.Source.Git != nil && fn.Spec.Source.Git.URL == "" {
		allErrs = append(allErrs, field.Required(sourcePath.Child("git", "url"), "git URL is required"))
	}
	if fn.Spec.Source.Image != nil && fn.Spec.Source.Image.Ref == "" {
		allErrs = append(allErrs, field.Required(sourcePath.Child("image", "ref"), "image ref is required"))
	}

	if fn.Spec.Runtime.Language == "" {
		allErrs = append(allErrs, field.Required(specPath.Child("runtime", "language"), "runtime language is required"))
	}

	return allErrs
}

func getAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	fn, ok := obj.(*functions.Function)
	if !ok {
		return nil, nil, fmt.Errorf("given object is not a Function")
	}
	return fn.ObjectMeta.Labels, selectableFields(fn), nil
}

func selectableFields(fn *functions.Function) fields.Set {
	return generic.ObjectMetaFieldsSet(&fn.ObjectMeta, true)
}

func matchFunction(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label:    label,
		Field:    field,
		GetAttrs: getAttrs,
	}
}
