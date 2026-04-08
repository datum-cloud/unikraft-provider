package activator

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/tools/cache"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

const (
	// headerFunctionName is injected by the HTTPProxy Envoy filter.
	headerFunctionName = "X-Datum-Function-Name"
	// headerFunctionNamespace is injected by the HTTPProxy Envoy filter.
	headerFunctionNamespace = "X-Datum-Function-Namespace"

	// requestTimeout is the maximum time the handler will wait for a cold-start
	// before returning 504 to the caller.
	requestTimeout = 10 * time.Second
)

// ServeHTTP implements http.Handler. It is the entry point for all inbound
// function traffic while a function is scaled to zero.
func (a *Activator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fnName := r.Header.Get(headerFunctionName)
	fnNamespace := r.Header.Get(headerFunctionNamespace)

	if fnName == "" || fnNamespace == "" {
		slog.Warn("request missing function identity headers",
			"remote_addr", r.RemoteAddr,
			"host", r.Host,
		)
		http.Error(w, `{"error":"missing function identity headers"}`, http.StatusBadRequest)
		return
	}

	nn, ok := a.index.LookupByNamespacedName(fnNamespace, fnName)
	if !ok {
		slog.Warn("function not found in index",
			"namespace", fnNamespace,
			"name", fnName,
		)
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	a.handleRequest(ctx, nn, w, r)
}

// functionEventHandler updates the function index in response to informer events.
type functionEventHandler struct {
	index *functionIndex
}

func (h functionEventHandler) OnAdd(obj interface{}, isInInitialList bool) {
	fn, ok := obj.(*functionsv1alpha1.Function)
	if !ok {
		return
	}
	h.index.set(fn)
	if !isInInitialList {
		slog.Debug("function added to index",
			"namespace", fn.Namespace,
			"name", fn.Name,
		)
	}
}

func (h functionEventHandler) OnUpdate(_, newObj interface{}) {
	fn, ok := newObj.(*functionsv1alpha1.Function)
	if !ok {
		return
	}
	h.index.set(fn)
}

func (h functionEventHandler) OnDelete(obj interface{}) {
	fn, ok := obj.(*functionsv1alpha1.Function)
	if !ok {
		// Handle tombstone objects returned by the informer on delete.
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		fn, ok = tombstone.Obj.(*functionsv1alpha1.Function)
		if !ok {
			return
		}
	}
	h.index.delete(fn)
	slog.Debug("function removed from index",
		"namespace", fn.Namespace,
		"name", fn.Name,
	)
}

// Ensure functionEventHandler satisfies the informer ResourceEventHandler interface.
var _ cache.ResourceEventHandler = functionEventHandler{}
