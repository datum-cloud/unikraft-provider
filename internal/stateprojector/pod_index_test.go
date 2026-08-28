package stateprojector

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestContainerIDsStripsSchemePrefix covers containerIDs, which replaced the
// old vmm.json-based guest-IP join: Kraftlet sets each provider Pod
// container's status.containerID to the ukpd instance uuid itself (confirmed
// against a live deployment, 2026-08-26 — a Pod's containerID matched
// exactly one of ukpd's own <platform-dir>/<uuid>/ directories, with no
// scheme prefix). A "docker://"/"containerd://"-style prefix is stripped
// defensively in case a future Kraftlet version adds one.
func TestContainerIDsStripsSchemePrefix(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "5ae0061b-d18e-4246-9c81-2f693f0faa6c"},
				{ContainerID: "docker://10243eed-e706-4b70-b24b-32dba403ab60"},
				{ContainerID: ""},
			},
		},
	}
	got := containerIDs(pod)
	want := []string{"5ae0061b-d18e-4246-9c81-2f693f0faa6c", "10243eed-e706-4b70-b24b-32dba403ab60"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("containerIDs() = %v, want %v", got, want)
	}
}

// TestUpsertPodResolvesProjectFromNamespaceLabel is the regression guard for
// the reported bug: state-projector used to set project=pod.Namespace
// directly, which is a synthetic Karmada-assigned edge identifier
// (ns-<uuid>), never the real project. The real project is a label on the
// Namespace object, not the Pod.
func TestUpsertPodResolvesProjectFromNamespaceLabel(t *testing.T) {
	ns := newNamespaceIndex()
	pods := newPodIndex(ns, &stats{}, false)

	ns.upsert(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ns-7c30e6d4-b337-4d46-a425-196116dfd5d3",
			Labels: map[string]string{upstreamClusterNameLabel: "cluster-project-htxrg"},
		},
	})
	pods.upsert(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "joseszycho-default-dfw-0",
			Namespace: "ns-7c30e6d4-b337-4d46-a425-196116dfd5d3",
			Labels:    map[string]string{upstreamInstanceLabel: "joseszycho"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{ContainerID: "5ae0061b-d18e-4246-9c81-2f693f0faa6c"}},
		},
	})

	rec, ok := pods.lookup("5ae0061b-d18e-4246-9c81-2f693f0faa6c")
	if !ok {
		t.Fatal("pod was not indexed")
	}
	if rec.project != "project-htxrg" {
		t.Errorf("project = %q, want %q (the namespace's own name must never be used)", rec.project, "project-htxrg")
	}
	if rec.project == "ns-7c30e6d4-b337-4d46-a425-196116dfd5d3" {
		t.Fatal("project resolved to the raw namespace name — the exact bug this fixes")
	}
}

// A provider Pod (carries upstream.instance) in a namespace with no project
// label must not silently attribute to the namespace's own name — it falls
// back to unresolved (recordFor emits "-"), and the misconfiguration is
// counted.
func TestUpsertPodMissingProjectLabel(t *testing.T) {
	ns := newNamespaceIndex()
	st := &stats{}
	pods := newPodIndex(ns, st, false)

	pods.upsert(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-instance",
			Namespace: "ns-unlabeled",
			Labels:    map[string]string{upstreamInstanceLabel: "some-instance"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{ContainerID: "22eb85cf-661f-4b51-8d77-1f051c73a7e3"}},
		},
	})

	rec, ok := pods.lookup("22eb85cf-661f-4b51-8d77-1f051c73a7e3")
	if !ok {
		t.Fatal("pod was not indexed")
	}
	if rec.project != "" {
		t.Errorf("project = %q, want empty (unresolved, not the namespace name)", rec.project)
	}
	if st.projectLabelMissing.Load() != 1 {
		t.Errorf("projectLabelMissing = %d, want 1", st.projectLabelMissing.Load())
	}

	// A non-provider pod (no upstream.instance) in the same unlabeled
	// namespace is routine — most of the cluster has no project label at
	// all — and must not count as a misconfiguration.
	pods.upsert(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns-x", Namespace: "ns-unlabeled"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{ContainerID: "af25bc66-2fd7-4a86-b587-2bfc62ab5c19"}},
		},
	})
	if st.projectLabelMissing.Load() != 1 {
		t.Errorf("projectLabelMissing = %d after a non-provider pod, want still 1", st.projectLabelMissing.Load())
	}
}
