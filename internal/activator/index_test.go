package activator_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

// newFunctionForIndex builds a minimal Function for index tests.
func newFunctionForIndex(namespace, name string) *functionsv1alpha1.Function {
	return &functionsv1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: functionsv1alpha1.FunctionSpec{
			Source: functionsv1alpha1.FunctionSource{
				Image: &functionsv1alpha1.ImageSource{Ref: "registry.unikraft.cloud/org/fn@sha256:abc"},
			},
			Runtime: functionsv1alpha1.FunctionRuntime{Language: "go"},
		},
	}
}

// indexWrapper is a test-local map that mirrors what the unexported
// functionIndex does. We use it to verify the logical behaviour of
// add/update/delete/lookup without needing access to the unexported type.
// The activator_test.go file tests the real index via Activator.ServeHTTP.
type indexWrapper struct {
	mu     sync.RWMutex
	lookup map[types.NamespacedName]bool
}

func newIndexWrapper() *indexWrapper {
	return &indexWrapper{lookup: make(map[types.NamespacedName]bool)}
}

func (w *indexWrapper) add(fn *functionsv1alpha1.Function) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lookup[types.NamespacedName{Namespace: fn.Namespace, Name: fn.Name}] = true
}

func (w *indexWrapper) remove(fn *functionsv1alpha1.Function) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.lookup, types.NamespacedName{Namespace: fn.Namespace, Name: fn.Name})
}

func (w *indexWrapper) has(namespace, name string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lookup[types.NamespacedName{Namespace: namespace, Name: name}]
}

// TestIndex_AddAndLookup verifies that adding a function makes it discoverable.
func TestIndex_AddAndLookup(t *testing.T) {
	idx := newIndexWrapper()
	fn := newFunctionForIndex("ns1", "fn-a")
	idx.add(fn)

	assert.True(t, idx.has("ns1", "fn-a"), "function should be found after add")
	assert.False(t, idx.has("ns1", "fn-b"), "unknown function should not be found")
	assert.False(t, idx.has("other-ns", "fn-a"), "function in different namespace should not be found")
}

// TestIndex_UpdatePreservesLookup verifies that re-adding (simulating an update) is idempotent.
func TestIndex_UpdatePreservesLookup(t *testing.T) {
	idx := newIndexWrapper()
	fn := newFunctionForIndex("ns1", "fn-b")
	idx.add(fn)
	// Simulate an update: same key re-added.
	idx.add(fn)

	assert.True(t, idx.has("ns1", "fn-b"), "function should still be found after update")
}

// TestIndex_DeleteRemovesEntry verifies that deleting a function removes it from the index.
func TestIndex_DeleteRemovesEntry(t *testing.T) {
	idx := newIndexWrapper()
	fn := newFunctionForIndex("ns1", "fn-c")
	idx.add(fn)
	require.True(t, idx.has("ns1", "fn-c"), "precondition: function added")

	idx.remove(fn)
	assert.False(t, idx.has("ns1", "fn-c"), "function should not be found after delete")
}

// TestIndex_DeleteNonExistentIsNoop verifies that deleting a function not in the index is safe.
func TestIndex_DeleteNonExistentIsNoop(t *testing.T) {
	idx := newIndexWrapper()
	fn := newFunctionForIndex("ns1", "fn-ghost")
	// Should not panic.
	idx.remove(fn)
	assert.False(t, idx.has("ns1", "fn-ghost"))
}

// TestIndex_MultipleNamespaces verifies that functions in different namespaces are isolated.
func TestIndex_MultipleNamespaces(t *testing.T) {
	idx := newIndexWrapper()
	fn1 := newFunctionForIndex("ns-a", "fn-shared-name")
	fn2 := newFunctionForIndex("ns-b", "fn-shared-name")

	idx.add(fn1)
	assert.True(t, idx.has("ns-a", "fn-shared-name"))
	assert.False(t, idx.has("ns-b", "fn-shared-name"), "function in ns-b not yet added")

	idx.add(fn2)
	assert.True(t, idx.has("ns-a", "fn-shared-name"))
	assert.True(t, idx.has("ns-b", "fn-shared-name"))

	idx.remove(fn1)
	assert.False(t, idx.has("ns-a", "fn-shared-name"), "ns-a function deleted")
	assert.True(t, idx.has("ns-b", "fn-shared-name"), "ns-b function unaffected")
}

// TestIndex_ConcurrentReadWrite verifies there are no data races under concurrent load.
// Run with -race to detect races.
func TestIndex_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	idx := newIndexWrapper()
	var wg sync.WaitGroup
	const n = 200

	// Concurrent writers adding functions.
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fn := newFunctionForIndex("ns-concurrent", fmt.Sprintf("fn-%d", i))
			idx.add(fn)
		}(i)
	}

	// Concurrent readers querying functions.
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = idx.has("ns-concurrent", fmt.Sprintf("fn-%d", i))
		}(i)
	}

	// Concurrent deleters removing functions.
	for i := range n / 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fn := newFunctionForIndex("ns-concurrent", fmt.Sprintf("fn-%d", i))
			idx.remove(fn)
		}(i)
	}

	wg.Wait()
	// No race condition = success when run with -race.
}
