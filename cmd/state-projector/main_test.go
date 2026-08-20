package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// makeProjector returns a projector wired to a temp vmm.json for the given uuid,
// with a pre-populated guest-IP -> pod index.
func makeProjector(t *testing.T, uuid, ip string, vcpuMilli, memBytes int64) (*projector, string) {
	t.Helper()
	dir := t.TempDir()
	// Write a vmm.json whose boot args carry the guest netdev IP.
	vmPath := filepath.Join(dir, uuid)
	if err := os.MkdirAll(vmPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// The real format (captured from a running ukpd): boot_args wraps
	// netdev.ip in escaped quotes as a compound "<ip>/<prefix>:<gw>:<gw>::<host>:internal"
	// field, not a bare IP — see the netdevIPRe comment in main.go.
	vmmJSON := `{"boot-source":{"boot_args":"unikraft netdev.ip=\"` + ip + `/30:172.16.0.6:172.16.0.6::somehost:internal\" -- /server"}}`
	if err := os.WriteFile(filepath.Join(vmPath, "vmm.json"), []byte(vmmJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "vm-state.usage")
	p := &projector{
		platformDir: dir,
		socketPath:  "/tmp/test.sock",
		outputPath:  out,
		pods: map[string]*podInfo{
			ip: {project: "my-project", instance: "web-1", vcpuMilli: vcpuMilli, memoryBytes: memBytes},
		},
		windows: make(map[string]*windowState),
		winMu:   sync.Mutex{},
	}
	return p, out
}

func TestDecodeProjectID(t *testing.T) {
	cases := []struct{ encoded, want string }{
		// Real value captured from a live deployment (2026-08-19): namespace
		// "ns-7c30e6d4-..." carries this label and the actual project is
		// "project-htxrg" — nothing like the namespace's own opaque name.
		{"cluster-project-htxrg", "project-htxrg"},
		// EncodeClusterName replaces "/" with "_" for names containing a path.
		{"cluster-org_team", "org/team"},
		{"cluster-", ""},
	}
	for _, c := range cases {
		if got := decodeProjectID(c.encoded); got != c.want {
			t.Errorf("decodeProjectID(%q) = %q, want %q", c.encoded, got, c.want)
		}
	}
}

// TestUpsertPodResolvesProjectFromNamespaceLabel is the regression guard for
// the reported bug: state-projector used to set project=pod.Namespace
// directly, which is a synthetic Karmada-assigned edge identifier
// (ns-<uuid>), never the real project. The real project is a label on the
// Namespace object, not the Pod.
func TestUpsertPodResolvesProjectFromNamespaceLabel(t *testing.T) {
	p := &projector{
		pods:     make(map[string]*podInfo),
		projects: make(map[string]string),
	}
	p.upsertNamespace(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ns-7c30e6d4-b337-4d46-a425-196116dfd5d3",
			Labels: map[string]string{upstreamClusterNameLabel: "cluster-project-htxrg"},
		},
	})
	p.upsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "joseszycho-default-dfw-0",
			Namespace: "ns-7c30e6d4-b337-4d46-a425-196116dfd5d3",
			Labels:    map[string]string{upstreamInstanceLabel: "joseszycho"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.9"},
	})

	info := p.pods["10.0.0.9"]
	if info == nil {
		t.Fatal("pod was not indexed")
	}
	if info.project != "project-htxrg" {
		t.Errorf("project = %q, want %q (the namespace's own name must never be used)", info.project, "project-htxrg")
	}
	if info.project == "ns-7c30e6d4-b337-4d46-a425-196116dfd5d3" {
		t.Fatal("project resolved to the raw namespace name — the exact bug this fixes")
	}
}

// A provider Pod (carries upstream.instance) in a namespace with no project
// label must not silently attribute to the namespace's own name — it falls
// back to unresolved (recordFor emits "-"), and the misconfiguration is
// counted.
func TestUpsertPodMissingProjectLabel(t *testing.T) {
	p := &projector{
		pods:     make(map[string]*podInfo),
		projects: make(map[string]string),
	}
	p.upsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-instance",
			Namespace: "ns-unlabeled",
			Labels:    map[string]string{upstreamInstanceLabel: "some-instance"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.10"},
	})

	info := p.pods["10.0.0.10"]
	if info == nil {
		t.Fatal("pod was not indexed")
	}
	if info.project != "" {
		t.Errorf("project = %q, want empty (unresolved, not the namespace name)", info.project)
	}
	if p.stats.projectLabelMissing.Load() != 1 {
		t.Errorf("projectLabelMissing = %d, want 1", p.stats.projectLabelMissing.Load())
	}

	// A non-provider pod (no upstream.instance) in the same unlabeled namespace
	// is routine — most of the cluster has no project label at all — and must
	// not count as a misconfiguration.
	p.upsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns-x", Namespace: "ns-unlabeled"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.11"},
	})
	if p.stats.projectLabelMissing.Load() != 1 {
		t.Errorf("projectLabelMissing = %d after a non-provider pod, want still 1", p.stats.projectLabelMissing.Load())
	}
}

func TestRotateIfNeeded(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "vm-state.usage")
	p := &projector{outputPath: out, rotateSizeBytes: 10}

	if err := os.WriteFile(out, []byte("0123456789extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.rotateIfNeeded()

	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be renamed away, stat err = %v", out, err)
	}
	matches, err := filepath.Glob(out + ".*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one rotated file, got %v", matches)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123456789extra" {
		t.Errorf("rotated file content = %q, want original content preserved", got)
	}
	if p.stats.rotations.Load() != 1 {
		t.Errorf("rotations counter = %d, want 1", p.stats.rotations.Load())
	}

	// Below threshold: no rotation.
	if err := os.WriteFile(out, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.rotateIfNeeded()
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("small file should not have been rotated away: %v", err)
	}

	// rotateSizeBytes <= 0 disables rotation entirely (the zero value, so
	// existing callers that don't set it get today's unbounded-file behavior
	// rather than being silently switched on).
	p2 := &projector{outputPath: out}
	if err := os.WriteFile(out, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	p2.rotateIfNeeded()
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("rotateSizeBytes=0 must disable rotation: %v", err)
	}
}

func TestCleanupOldRotations(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "vm-state.usage")
	p := &projector{outputPath: out, rotateMaxAge: time.Hour}

	old := fmt.Sprintf("%s.%d", out, time.Now().Add(-2*time.Hour).Unix())
	recent := fmt.Sprintf("%s.%d", out, time.Now().Add(-10*time.Minute).Unix())
	notOurs := out + ".garbage"
	for _, f := range []string{old, recent, notOurs} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p.cleanupOldRotations()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expected old rotated file to be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent rotated file should survive: %v", err)
	}
	if _, err := os.Stat(notOurs); err != nil {
		t.Errorf("non-numeric-suffixed file must not be touched: %v", err)
	}
	if p.stats.rotationDeletes.Load() != 1 {
		t.Errorf("rotationDeletes counter = %d, want 1", p.stats.rotationDeletes.Load())
	}

	// rotateMaxAge <= 0 disables the backstop entirely.
	p2 := &projector{outputPath: out, rotateMaxAge: 0}
	veryOld := fmt.Sprintf("%s.%d", out, time.Now().Add(-1000*time.Hour).Unix())
	if err := os.WriteFile(veryOld, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2.cleanupOldRotations()
	if _, err := os.Stat(veryOld); err != nil {
		t.Errorf("rotateMaxAge=0 must disable the backstop: %v", err)
	}
}

// TestGuestIPRealVmmJSON is a golden regression test using the exact bytes
// captured from a real ukpd's vmm.json (chainsaw e2e run, 2026-08-19). The
// original regex required digits immediately after "netdev.ip=" and never
// matched in production — vmm.json is itself JSON, and boot_args's string
// value wraps the netdev arg in escaped quotes, so the real bytes read
// `netdev.ip=\"172.16.0.5/30:...`, not `netdev.ip=172.16.0.5`.
func TestGuestIPRealVmmJSON(t *testing.T) {
	const raw = `{"boot-source":{"kernel_image_path":"/var/lib/ukp/images/x","initrd_path":"/var/lib/ukp/images/y","boot_args":"unikraft netdev.ip=\"172.16.0.5/30:172.16.0.6:172.16.0.6::yu1uwkqertaqmlxdkoncxki7bdyzbvdrsdhif7qfabo:internal\" env.vars=PATH=/usr/local/sbin -- /server"},"machine-config":{"vcpu_count":1,"mem_size_mib":256,"track_dirty_pages":true},"vsock":{"guest_cid":3,"uds_path":"v.sock"},"drives":[],"fs":[],"network-interfaces":[{"iface_id":"eth0","guest_mac":"12:b0:ac:10:00:05","host_dev_name":"ukp1.vif1"}],"consoles":[{"id":"console0","ports":[{"console":true,"name":"default","tx":{"type":"stdout"}}]}]}`

	dir := t.TempDir()
	const uuid = "cc20e657-7db8-49bb-b34c-5882c18676f1"
	vmPath := filepath.Join(dir, uuid)
	if err := os.MkdirAll(vmPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmPath, "vmm.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	ip, err := guestIP(dir, uuid)
	if err != nil {
		t.Fatalf("guestIP() error = %v", err)
	}
	if ip != "172.16.0.5" {
		t.Errorf("guestIP() = %q, want 172.16.0.5", ip)
	}
}

func TestExtractTransitionKeys(t *testing.T) {
	cases := []struct {
		data           map[string]interface{}
		wantUUID       string
		wantOld, wantN string
	}{
		// The payload ukpd actually emits: vm / prev / new.
		{map[string]interface{}{"vm": "dda7fe99-387a-4d81-80f2-35e2b51ee5c5", "prev": "running", "new": "stopping"},
			"dda7fe99-387a-4d81-80f2-35e2b51ee5c5", "running", "stopping"},
		{map[string]interface{}{"vm": "dda7fe99-387a-4d81-80f2-35e2b51ee5c5", "prev": "starting", "new": "running"},
			"dda7fe99-387a-4d81-80f2-35e2b51ee5c5", "starting", "running"},
		{map[string]interface{}{"uuid": "u1", "old_state": "starting", "state": "running"}, "u1", "starting", "running"},
		{map[string]interface{}{"uuid": "u1", "previous_state": "running", "state": "standby"}, "u1", "running", "standby"},
		{map[string]interface{}{"instance_uuid": "u2", "from": "standby", "to": "running"}, "u2", "standby", "running"},
		{map[string]interface{}{"id": "u3", "state_to": "stopped"}, "u3", "", "stopped"},
	}
	for _, c := range cases {
		u, old, n := extractTransition(c.data)
		if u != c.wantUUID || old != c.wantOld || n != c.wantN {
			t.Errorf("extractTransition(%v) = (%q,%q,%q), want (%q,%q,%q)", c.data, u, old, n, c.wantUUID, c.wantOld, c.wantN)
		}
	}
	// uuid fallback scans an appended uuid-shaped token.
	u, _, _ := extractTransition(map[string]interface{}{"instance": "31020cdc-eee4-440a-8b0b-57c4690ae3d0"})
	if u != "31020cdc-eee4-440a-8b0b-57c4690ae3d0" {
		t.Errorf("uuid fallback = %q", u)
	}
}

func TestWindowOnOffSingleRecord(t *testing.T) {
	p, out := makeProjector(t, "51020cdc-eee4-440a-8b0b-57c4690ae3d0", "10.0.0.5", 1000, 2*1024*1024*1024)

	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:01:40Z", Type: "vm.state_change",
		Data: map[string]interface{}{"uuid": "51020cdc-eee4-440a-8b0b-57c4690ae3d0", "old_state": "starting", "state": "running"}})
	if len(p.windows) != 1 {
		t.Fatalf("expected one open window after on, got %d", len(p.windows))
	}
	// re-entering running is a no-op (idempotent)
	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:01:50Z", Type: "vm.state_change",
		Data: map[string]interface{}{"uuid": "51020cdc-eee4-440a-8b0b-57c4690ae3d0", "old_state": "running", "state": "running"}})
	if len(p.windows) != 1 {
		t.Fatalf("running->running must not restart window")
	}
	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:02:40Z", Type: "vm.state_change",
		Data: map[string]interface{}{"uuid": "51020cdc-eee4-440a-8b0b-57c4690ae3d0", "old_state": "running", "state": "standby"}})
	if len(p.windows) != 0 {
		t.Fatalf("window should be closed on standby, got %d open", len(p.windows))
	}

	recs := readRecords(t, out)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d: %v", len(recs), recs)
	}
	r := recs[0]
	if r.Project != "my-project" || r.Instance != "web-1" {
		t.Errorf("attribution wrong: %+v", r)
	}
	if r.VCPU != 1 { // 1000 millicores
		t.Errorf("vcpu = %d, want 1", r.VCPU)
	}
	if r.MemoryBytes != 2*1024*1024*1024 {
		t.Errorf("memory_bytes = %d", r.MemoryBytes)
	}
	// Window is from the "on" timestamp (100s) to the "off" timestamp (160s).
	if r.DurationS != 60 {
		t.Errorf("duration_s = %v, want 60", r.DurationS)
	}
}

// TestVendorPayloadDrivesWindowing is the regression guard for the real ukpd
// payload: the vm/prev/new shape must open and close a window end-to-end, not
// just parse. Reading the states off the wrong keys leaves them empty, which
// silently opens no window and emits no record at all.
func TestVendorPayloadDrivesWindowing(t *testing.T) {
	const uuid = "dda7fe99-387a-4d81-80f2-35e2b51ee5c5"
	p, out := makeProjector(t, uuid, "10.0.0.7", 2000, 1024*1024*1024)

	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:01:40Z", Type: "vm.state_change",
		Data: map[string]interface{}{"vm": uuid, "prev": "starting", "new": "running"}})
	if len(p.windows) != 1 {
		t.Fatalf("vendor running event did not open a window; got %d open", len(p.windows))
	}

	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:02:40Z", Type: "vm.state_change",
		Data: map[string]interface{}{"vm": uuid, "prev": "running", "new": "stopping"}})
	if len(p.windows) != 0 {
		t.Fatalf("vendor stopping event did not close the window; got %d open", len(p.windows))
	}

	recs := readRecords(t, out)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record from the vendor payload, got %d", len(recs))
	}
	if recs[0].UUID != uuid || recs[0].Project != "my-project" || recs[0].Instance != "web-1" {
		t.Errorf("vendor record attribution wrong: %+v", recs[0])
	}
	if recs[0].DurationS != 60 {
		t.Errorf("duration_s = %v, want 60", recs[0].DurationS)
	}
}

// A window must close on any non-running state even when the event carries no
// old-state field, so a renamed/missing "prev" cannot strand it open and stop
// billing a stopped instance.
func TestWindowClosesWithoutOldState(t *testing.T) {
	const uuid = "81020cdc-eee4-440a-8b0b-57c4690ae3d0"
	p, out := makeProjector(t, uuid, "10.0.0.8", 1000, 1024)

	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:00:10Z", Type: "vm.state_change",
		Data: map[string]interface{}{"vm": uuid, "new": "running"}})
	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:00:40Z", Type: "vm.state_change",
		Data: map[string]interface{}{"vm": uuid, "new": "stopped"}})

	if len(p.windows) != 0 {
		t.Fatalf("window stranded open without an old-state field")
	}
	recs := readRecords(t, out)
	if len(recs) != 1 || recs[0].DurationS != 30 {
		t.Fatalf("expected one 30s record, got %+v", recs)
	}
}

// An unparseable new state must leave an open window untouched rather than
// guessing the instance stopped.
func TestUnknownStateLeavesWindowOpen(t *testing.T) {
	const uuid = "91020cdc-eee4-440a-8b0b-57c4690ae3d0"
	p, _ := makeProjector(t, uuid, "10.0.0.10", 1000, 1024)

	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:00:10Z", Type: "vm.state_change",
		Data: map[string]interface{}{"vm": uuid, "new": "running"}})
	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:00:40Z", Type: "vm.state_change",
		Data: map[string]interface{}{"vm": uuid, "some_unknown_key": "stopped"}})

	if len(p.windows) != 1 {
		t.Fatalf("unparseable state must not close the window; got %d open", len(p.windows))
	}
}

func TestIncrementalFlushAndDeterministicID(t *testing.T) {
	p, out := makeProjector(t, "61020cdc-eee4-440a-8b0b-57c4690ae3d0", "10.0.0.9", 2000, 1024*1024*1024)
	p.flushInterval = 5 * time.Minute

	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:10:00Z", Type: "vm.state_change",
		Data: map[string]interface{}{"uuid": "61020cdc-eee4-440a-8b0b-57c4690ae3d0", "state": "running"}})

	// Window opened at 600s (00:10:00). The first flush at 600 is a no-op; each
	// later flush covers the elapsed since the last watermark.
	p.emitWindow(p.windows["61020cdc-eee4-440a-8b0b-57c4690ae3d0"], time.Unix(1200, 0).UTC(), "flush")
	p.emitWindow(p.windows["61020cdc-eee4-440a-8b0b-57c4690ae3d0"], time.Unix(1800, 0).UTC(), "flush")

	recs := readRecords(t, out)
	if len(recs) != 2 {
		t.Fatalf("expected 2 flush records, got %d", len(recs))
	}
	if recs[0].DurationS != 600 || recs[1].DurationS != 600 {
		t.Errorf("flush durations = %v, %v; want 600, 600", recs[0].DurationS, recs[1].DurationS)
	}
	if recs[0].Start != "1970-01-01T00:10:00Z" || recs[1].Start != "1970-01-01T00:20:00Z" {
		t.Errorf("flush starts = %q, %q", recs[0].Start, recs[1].Start)
	}
	// Deterministic ids: same (uuid,start,end) yields the same id.
	if recs[0].ID == recs[1].ID {
		t.Errorf("distinct windows must have distinct ids")
	}
	if recs[0].ID != windowID("61020cdc-eee4-440a-8b0b-57c4690ae3d0", time.Unix(600, 0).UTC(), time.Unix(1200, 0).UTC()) {
		t.Errorf("id not deterministic: %s", recs[0].ID)
	}
}

func TestUnresolvedAttribution(t *testing.T) {
	p, out := makeProjector(t, "71020cdc-eee4-440a-8b0b-57c4690ae3d0", "10.0.0.99", 1000, 1024)
	// No pod indexed for 10.0.0.99 -> attribution falls back to "-" but the
	// window is still emitted (fail toward not-dropping).
	p.pods = nil
	p.handleEvent(stateChange{Timestamp: "1970-01-01T00:00:10Z", Type: "vm.state_change",
		Data: map[string]interface{}{"uuid": "71020cdc-eee4-440a-8b0b-57c4690ae3d0", "state": "running"}})
	p.emitWindow(p.windows["71020cdc-eee4-440a-8b0b-57c4690ae3d0"], time.Unix(40, 0).UTC(), "flush")
	recs := readRecords(t, out)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record even when unresolved, got %d", len(recs))
	}
	if recs[0].Project != "-" || recs[0].DurationS != 30 {
		t.Errorf("unresolved record = %+v", recs[0])
	}
}

func readRecords(t *testing.T, path string) []usageRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []usageRecord
	for _, line := range splitLines(string(b)) {
		if line == "" {
			continue
		}
		var r usageRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad record line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
