package stateprojector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeResolver satisfies resolver without any Kubernetes dependency,
// demonstrating the decoupling: processor never knows whether attribution
// comes from a real cluster watch or a test fixture.
type fakeResolver struct {
	infos map[string]*info
}

func (f *fakeResolver) resolve(uuid string) (*info, string) {
	if rec, ok := f.infos[uuid]; ok {
		return rec, reasonOK
	}
	return nil, reasonPodNotIndexed
}

// newTestProcessor wires a processor to a fake resolver and a real
// outputWriter backed by a temp file, returning the processor and that
// file's path.
func newTestProcessor(t *testing.T, uuid string, vcpuMilli, memBytes int64) (*processor, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "vm-state.usage")
	resolver := &fakeResolver{infos: map[string]*info{
		uuid: {project: "my-project", instance: "web-1", vcpuMilli: vcpuMilli, memoryBytes: memBytes},
	}}
	st := &stats{}
	w := newOutputWriter(out, 0, 0, st)
	p := newProcessor(resolver, w, st, false, 5*time.Minute)
	return p, out
}

func (p *processor) window(uuid string) *window {
	p.windows.Lock()
	defer p.windows.Unlock()
	w, _ := p.windows.get(uuid)
	return w
}

func TestWindowOnOffSingleRecord(t *testing.T) {
	const uuid = "51020cdc-eee4-440a-8b0b-57c4690ae3d0"
	p, out := newTestProcessor(t, uuid, 1000, 2*1024*1024*1024)

	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:01:40Z", Type: "vm.state_change",
		Data: map[string]any{"uuid": uuid, "prev": "starting", "new": "running"}})
	if p.openWindows() != 1 {
		t.Fatalf("expected one open window after on, got %d", p.openWindows())
	}
	// re-entering running is a no-op (idempotent)
	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:01:50Z", Type: "vm.state_change",
		Data: map[string]any{"uuid": uuid, "prev": "running", "new": "running"}})
	if p.openWindows() != 1 {
		t.Fatalf("running->running must not restart window")
	}
	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:02:40Z", Type: "vm.state_change",
		Data: map[string]any{"uuid": uuid, "prev": "running", "new": "standby"}})
	if p.openWindows() != 0 {
		t.Fatalf("window should be closed on standby, got %d open", p.openWindows())
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

// TestVendorPayloadDrivesWindowing is the regression guard for the real
// ukpd payload: the vm/prev/new shape must open and close a window end to
// end, not just parse. Reading the states off the wrong keys leaves them
// empty, which silently opens no window and emits no record at all.
func TestVendorPayloadDrivesWindowing(t *testing.T) {
	const uuid = "dda7fe99-387a-4d81-80f2-35e2b51ee5c5"
	p, out := newTestProcessor(t, uuid, 2000, 1024*1024*1024)

	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:01:40Z", Type: "vm.state_change",
		Data: map[string]any{"vm": uuid, "prev": "starting", "new": "running"}})
	if p.openWindows() != 1 {
		t.Fatalf("vendor running event did not open a window; got %d open", p.openWindows())
	}

	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:02:40Z", Type: "vm.state_change",
		Data: map[string]any{"vm": uuid, "prev": "running", "new": "stopping"}})
	if p.openWindows() != 0 {
		t.Fatalf("vendor stopping event did not close the window; got %d open", p.openWindows())
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

// A window must close on any non-running state even when the event carries
// no old-state field, so a renamed/missing "prev" cannot strand it open and
// stop billing a stopped instance.
func TestWindowClosesWithoutOldState(t *testing.T) {
	const uuid = "81020cdc-eee4-440a-8b0b-57c4690ae3d0"
	p, out := newTestProcessor(t, uuid, 1000, 1024)

	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:00:10Z", Type: "vm.state_change",
		Data: map[string]any{"vm": uuid, "new": "running"}})
	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:00:40Z", Type: "vm.state_change",
		Data: map[string]any{"vm": uuid, "new": "stopped"}})

	if p.openWindows() != 0 {
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
	p, _ := newTestProcessor(t, uuid, 1000, 1024)

	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:00:10Z", Type: "vm.state_change",
		Data: map[string]any{"vm": uuid, "new": "running"}})
	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:00:40Z", Type: "vm.state_change",
		Data: map[string]any{"vm": uuid, "some_unknown_key": "stopped"}})

	if p.openWindows() != 1 {
		t.Fatalf("unparseable state must not close the window; got %d open", p.openWindows())
	}
}

func TestIncrementalFlushAndDeterministicID(t *testing.T) {
	const uuid = "61020cdc-eee4-440a-8b0b-57c4690ae3d0"
	p, out := newTestProcessor(t, uuid, 2000, 1024*1024*1024)

	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:10:00Z", Type: "vm.state_change",
		Data: map[string]any{"uuid": uuid, "new": "running"}})

	// Window opened at 600s (00:10:00). The first flush at 600 is a no-op;
	// each later flush covers the elapsed since the last watermark.
	p.emitWindow(p.window(uuid), time.Unix(1200, 0).UTC(), "flush")
	p.emitWindow(p.window(uuid), time.Unix(1800, 0).UTC(), "flush")

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
	if recs[0].ID != windowID(uuid, time.Unix(600, 0).UTC(), time.Unix(1200, 0).UTC()) {
		t.Errorf("id not deterministic: %s", recs[0].ID)
	}
}

func TestUnresolvedAttribution(t *testing.T) {
	const uuid = "71020cdc-eee4-440a-8b0b-57c4690ae3d0"
	out := filepath.Join(t.TempDir(), "vm-state.usage")
	st := &stats{}
	// No pod indexed for this uuid -> attribution falls back to "-" but the
	// window is still emitted (fail toward not-dropping).
	p := newProcessor(&fakeResolver{infos: map[string]*info{}}, newOutputWriter(out, 0, 0, st), st, false, 5*time.Minute)

	p.HandleEvent(stateChange{Timestamp: "1970-01-01T00:00:10Z", Type: "vm.state_change",
		Data: map[string]any{"uuid": uuid, "new": "running"}})
	p.emitWindow(p.window(uuid), time.Unix(40, 0).UTC(), "flush")
	recs := readRecords(t, out)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record even when unresolved, got %d", len(recs))
	}
	if recs[0].Project != "-" || recs[0].DurationS != 30 {
		t.Errorf("unresolved record = %+v", recs[0])
	}
}

func readRecords(t *testing.T, path string) []record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []record
	for _, line := range splitLines(string(b)) {
		if line == "" {
			continue
		}
		var r record
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
