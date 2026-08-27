// This file resolves a ukpd instance uuid to the identity/resources of the
// provider Pod running it, keyed by the Pod's container status containerID
// (see containerIDs) and kept up to date by watch.go's Pod informer.

package stateprojector

import (
	"fmt"
	"log"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// upstreamInstanceLabel is stamped on the provider Pod by the Instance
// controller; it names the compute Instance the pod backs.
const upstreamInstanceLabel = "upstream.instance"

// Reasons resolution can fail, logged distinctly since each has a different fix.
const (
	reasonOK            = "ok"
	reasonPodNotIndexed = "pod_not_indexed" // no provider Pod's containerID matches this uuid
)

// projectResolver decodes a namespace name to its Milo project id. Satisfied
// by *namespaceIndex; kept as an interface so podIndex doesn't depend on the
// namespace watch's concrete implementation.
type projectResolver interface {
	project(ns string) (string, bool)
}

// info is the identity + requested resources of the provider Pod for an
// instance, indexed by the ukpd instance uuid (see containerIDs).
type info struct {
	project     string
	instance    string
	vcpuMilli   int64 // millicores; 0 if unset
	memoryBytes int64
}

func (i *info) equal(other *info) bool {
	if i == nil || other == nil {
		return i == other
	}
	return *i == *other
}

func (i *info) String() string {
	if i == nil {
		return "<nil>"
	}
	return fmt.Sprintf("project=%s instance=%s vcpu_milli=%d memory_bytes=%d",
		i.project, i.instance, i.vcpuMilli, i.memoryBytes)
}

// podIndex maps a ukpd instance uuid to the identity/resources of the
// provider Pod running it.
type podIndex struct {
	projects projectResolver
	stats    *stats
	debug    bool

	mu   sync.RWMutex
	pods map[string]*info
}

func newPodIndex(projects projectResolver, stats *stats, debug bool) *podIndex {
	return &podIndex{projects: projects, stats: stats, debug: debug, pods: make(map[string]*info)}
}

func (p *podIndex) debugf(format string, args ...any) {
	if p.debug {
		log.Printf(format, args...)
	}
}

// containerIDs returns the ukpd instance uuid(s) backing this Pod's
// containers. Kraftlet (the virtual-kubelet provider driving these Pods)
// sets each container's status.containerID to the ukpd instance uuid
// itself
func containerIDs(pod *corev1.Pod) []string {
	var ids []string
	for _, cs := range pod.Status.ContainerStatuses {
		id := cs.ContainerID
		if id == "" {
			continue
		}
		if _, rest, ok := strings.Cut(id, "://"); ok {
			id = rest
		}
		ids = append(ids, id)
	}
	return ids
}

func (p *podIndex) upsert(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	ids := containerIDs(pod)
	if len(ids) == 0 {
		// Normal before the container has actually started; not a misconfiguration.
		p.debugf("podindex skip reason=no_container_id pod=%s/%s", pod.Namespace, pod.Name)
		return
	}
	instance := pod.Labels[upstreamInstanceLabel]
	// project is "" when unresolved (namespace not yet indexed, or genuinely
	// missing the label) — never falls back to pod.Namespace, which is a
	// synthetic edge-local id, not the project.
	project, projectOK := p.projects.project(pod.Namespace)
	rec := &info{project: project, instance: instance}
	for _, c := range pod.Spec.Containers {
		rec.vcpuMilli = max64(rec.vcpuMilli, milliCPU(c.Resources.Limits[corev1.ResourceCPU]))
		if mem, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			rec.memoryBytes = max64(rec.memoryBytes, mem.Value())
		}
		rec.vcpuMilli = max64(rec.vcpuMilli, milliCPU(c.Resources.Requests[corev1.ResourceCPU]))
		if mem, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			rec.memoryBytes = max64(rec.memoryBytes, mem.Value())
		}
	}

	p.mu.Lock()
	added, changed := false, false
	for _, id := range ids {
		prev, existed := p.pods[id]
		p.pods[id] = rec
		if !existed {
			added = true
		} else if !prev.equal(rec) {
			changed = true
		}
	}
	total := len(p.pods)
	p.mu.Unlock()

	// The informer's resync replays every pod in the cluster, so log only on
	// an actual change — otherwise this floods.
	if added {
		p.stats.podIndexed.Add(1)
		p.debugf("podindex added uuids=%v %s pod=%s/%s total=%d", ids, rec, pod.Namespace, pod.Name, total)
	} else if changed {
		p.debugf("podindex updated uuids=%v %s pod=%s/%s", ids, rec, pod.Namespace, pod.Name)
	}
	if instance == "" {
		p.debugf("podindex warn=missing_instance_label uuids=%v pod=%s/%s label=%s", ids, pod.Namespace, pod.Name, upstreamInstanceLabel)
	} else if !projectOK {
		// A real provider Pod (carries upstream.instance) whose namespace has no
		// project label — compute's own controller treats this as misconfiguration,
		// not a transient state, so it's unconditional (not debug-gated).
		p.stats.projectLabelMissing.Add(1)
		log.Printf("podindex ALERT=missing_project_label ns=%s pod=%s/%s label=%s (record emitted with project=\"-\")",
			pod.Namespace, pod.Namespace, pod.Name, upstreamClusterNameLabel)
	}
}

func (p *podIndex) delete(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	ids := containerIDs(pod)
	if len(ids) == 0 {
		return
	}
	p.mu.Lock()
	for _, id := range ids {
		delete(p.pods, id)
	}
	total := len(p.pods)
	p.mu.Unlock()
	p.debugf("podindex removed uuids=%v pod=%s/%s total=%d", ids, pod.Namespace, pod.Name, total)
}

func (p *podIndex) lookup(uuid string) (*info, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	rec, ok := p.pods[uuid]
	return rec, ok
}

func (p *podIndex) len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.pods)
}

// resolve maps an instance uuid directly to identity/resources via the pod
// index: Kraftlet sets each provider Pod container's status.containerID to
// the ukpd instance uuid itself (see containerIDs), so the Pod carrying that
// containerID is the Pod for this instance — a plain map lookup, no IP or
// on-disk file involved.
//
// This replaced an earlier guest-IP-based join (uuid -> vmm.json's netdev.ip
// -> pod.Status.PodIP), retired after a CNI change on the runtime stopped
// populating netdev.ip (confirmed 2026-08-26) and, separately, Kraftlet was
// observed leaving status.podIP unset on fully-Running Pods. The containerID
// join depends on neither.
func (p *podIndex) resolve(uuid string) (*info, string) {
	rec, ok := p.lookup(uuid)
	if !ok {
		p.debugf("resolve detail uuid=%s stage=pod_index indexed_instances=%d", uuid, p.len())
		return nil, reasonPodNotIndexed
	}
	p.debugf("resolve detail uuid=%s %s", uuid, rec)
	return rec, reasonOK
}

func milliCPU(q resource.Quantity) int64 {
	if q.IsZero() {
		return 0
	}
	return q.MilliValue()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func coresFromMilli(milli int64) int64 {
	return milli / 1000
}
