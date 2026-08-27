// This file resolves a Kubernetes namespace name to its decoded Milo
// project id, kept up to date by watch.go's Namespace informer.

package stateprojector

import (
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// upstreamClusterNameLabel is stamped on the edge Namespace (never the Pod)
// by Karmada federation before any Instance can exist in it. Its value
// decodes to the real Milo project id. The namespace's own name (ns-<uuid>)
// is a synthetic, edge-local identifier and must never be used as the
// project — see go.datum.net/compute's clustername.go.
const upstreamClusterNameLabel = "meta.datumapis.com/upstream-cluster-name"

// decodeProjectID reverses go.datum.net/compute's EncodeClusterName
// ("cluster-" + strings.ReplaceAll(name, "/", "_")).
func decodeProjectID(encoded string) string {
	return strings.ReplaceAll(strings.TrimPrefix(encoded, "cluster-"), "_", "/")
}

// namespaceIndex resolves a namespace name to its decoded Milo project id.
type namespaceIndex struct {
	mu       sync.RWMutex
	projects map[string]string
}

func newNamespaceIndex() *namespaceIndex {
	return &namespaceIndex{projects: make(map[string]string)}
}

func (n *namespaceIndex) upsert(ns *corev1.Namespace) {
	if ns == nil {
		return
	}
	encoded, ok := ns.Labels[upstreamClusterNameLabel]
	n.mu.Lock()
	defer n.mu.Unlock()
	if ok {
		n.projects[ns.Name] = decodeProjectID(encoded)
	} else {
		delete(n.projects, ns.Name)
	}
}

func (n *namespaceIndex) delete(ns *corev1.Namespace) {
	if ns == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.projects, ns.Name)
}

// project returns the decoded project id for a namespace, and whether it
// resolved. false means either the namespace hasn't been indexed yet, or it
// genuinely carries no upstreamClusterNameLabel.
func (n *namespaceIndex) project(ns string) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	p, ok := n.projects[ns]
	return p, ok
}

func (n *namespaceIndex) len() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.projects)
}
