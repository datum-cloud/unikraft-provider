package activator

import (
	"context"
	"log/slog"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

// functionIndex maps Function.status.hostname to its NamespacedName.
// It is populated by watching Function resources across all namespaces via a
// controller-runtime cache.
type functionIndex struct {
	mu    sync.RWMutex
	byKey map[string]types.NamespacedName // key = "<namespace>/<name>"
}

func newFunctionIndex() *functionIndex {
	return &functionIndex{
		byKey: make(map[string]types.NamespacedName),
	}
}

// LookupByHeaders returns the NamespacedName for the function identified by
// the given namespace and name (extracted from request headers). The second
// return value is false when no entry is found.
func (idx *functionIndex) LookupByNamespacedName(namespace, name string) (types.NamespacedName, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	nn := types.NamespacedName{Namespace: namespace, Name: name}
	// Verify we have an entry for this function.
	_, ok := idx.byKey[nn.String()]
	return nn, ok
}

// set records that the given function exists.
func (idx *functionIndex) set(fn *functionsv1alpha1.Function) {
	nn := types.NamespacedName{Namespace: fn.Namespace, Name: fn.Name}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.byKey[nn.String()] = nn
}

// delete removes a function from the index.
func (idx *functionIndex) delete(fn *functionsv1alpha1.Function) {
	nn := types.NamespacedName{Namespace: fn.Namespace, Name: fn.Name}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.byKey, nn.String())
}

// syncFromCache performs an initial full sync of all Function resources from
// the cache into the index.
func (idx *functionIndex) syncFromCache(ctx context.Context, c cache.Cache) error {
	var list functionsv1alpha1.FunctionList
	if err := c.List(ctx, &list, &client.ListOptions{}); err != nil {
		return err
	}
	for i := range list.Items {
		idx.set(&list.Items[i])
	}
	slog.Info("function index initialised", "count", len(list.Items))
	return nil
}
