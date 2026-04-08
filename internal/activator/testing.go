package activator

import (
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

// NewWithFakeIndex creates an Activator with a pre-populated index and no
// cache. This is intended for use in unit tests that do not start a
// controller-runtime cache.
//
// The provided functions are loaded directly into the index so that
// ServeHTTP can route requests without a live Kubernetes API server.
func NewWithFakeIndex(c client.Client, fns ...*functionsv1alpha1.Function) *Activator {
	idx := newFunctionIndex()
	for _, fn := range fns {
		idx.set(fn)
	}
	return &Activator{
		client:     c,
		cache:      nil, // not used in unit tests
		index:      idx,
		httpClient: &http.Client{Timeout: 35 * time.Second},
		queues:     make(map[string]*funcQueue),
	}
}
