package install

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"go.datum.net/ufo/pkg/apis/functions"
	"go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

// Install registers the API group and adds types to a scheme.
// This registers both internal types (for server-side storage) and
// versioned types (for client-facing API).
func Install(scheme *runtime.Scheme) {
	utilruntime.Must(functions.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(scheme.SetVersionPriority(v1alpha1.SchemeGroupVersion))
}
