// Tests for file_source.go's tailing, offset persistence, and rotation
// handling.

package stateprojector

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// recordingHandler collects every event HandleEvent was called with. events
// is guarded by mu — the channel alone only synchronizes the wake-up, not
// the initial (pre-receive) length check in waitForEvents' loop condition.
type recordingHandler struct {
	mu     sync.Mutex
	events []stateChange
	signal chan struct{} // buffered; one send per HandleEvent, for waitForEvents to block on
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{signal: make(chan struct{}, 1000)}
}

func (h *recordingHandler) HandleEvent(ev stateChange) {
	h.mu.Lock()
	h.events = append(h.events, ev)
	h.mu.Unlock()
	h.signal <- struct{}{}
}

func (h *recordingHandler) all() []stateChange {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]stateChange(nil), h.events...)
}

// waitForEvents blocks until at least n events have been recorded, or fails
// the test after a short timeout — the tailer polls on its own schedule, so
// tests can't just check synchronously after a write.
func (h *recordingHandler) waitForEvents(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for len(h.all()) < n {
		select {
		case <-h.signal:
		case <-deadline:
			t.Fatalf("timed out waiting for %d events, got %d", n, len(h.all()))
		}
	}
}

func newTestFileSource(path string, handler eventHandler, stats *stats) *fileSource {
	f := newFileSource(path, handler, stats)
	f.pollInterval = 20 * time.Millisecond // fast polling for tests
	return f
}

func TestFileSourceTailsAppendedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm-state.events")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newRecordingHandler()
	src := newTestFileSource(path, handler, &stats{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	appendLine(t, path, `{"type":"vm.state_change","timestamp":"1970-01-01T00:00:01Z","data":{"vm":"u1","prev":"stopped","new":"starting"}}`)
	handler.waitForEvents(t, 1)

	appendLine(t, path, `{"type":"vm.state_change","timestamp":"1970-01-01T00:00:02Z","data":{"vm":"u1","prev":"starting","new":"running"}}`)
	handler.waitForEvents(t, 2)

	if handler.all()[0].Data["new"] != "starting" || handler.all()[1].Data["new"] != "running" {
		t.Errorf("events out of order or wrong: %+v", handler.all())
	}
}

func TestFileSourceIgnoresPartialTrailingLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm-state.events")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newRecordingHandler()
	src := newTestFileSource(path, handler, &stats{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	// Write a complete line followed by a partial one (no trailing newline).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"vm.state_change","timestamp":"1970-01-01T00:00:01Z","data":{"vm":"u1","prev":"stopped","new":"starting"}}` + "\n" +
		`{"type":"vm.state_change","timestamp":"1970-01-01T00:00:02Z","data":{"vm":"u1"`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	handler.waitForEvents(t, 1)
	time.Sleep(100 * time.Millisecond) // give the tailer time to (wrongly) parse a partial line if it were going to
	if len(handler.all()) != 1 {
		t.Fatalf("expected exactly 1 event (partial line held back), got %d: %+v", len(handler.all()), handler.all())
	}

	// Completing the line should deliver the second event.
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`,"prev":"starting","new":"running"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	handler.waitForEvents(t, 2)
}

func TestFileSourceResumesFromPersistedOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm-state.events")
	line1 := `{"type":"vm.state_change","timestamp":"1970-01-01T00:00:01Z","data":{"vm":"u1","prev":"stopped","new":"starting"}}` + "\n"
	line2 := `{"type":"vm.state_change","timestamp":"1970-01-01T00:00:02Z","data":{"vm":"u1","prev":"starting","new":"running"}}` + "\n"
	if err := os.WriteFile(path, []byte(line1+line2), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-seed the offset file as if a prior run already consumed line1.
	if err := os.WriteFile(path+".offset", []byte(strconv.Itoa(len(line1))), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newRecordingHandler()
	src := newTestFileSource(path, handler, &stats{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	handler.waitForEvents(t, 1)
	time.Sleep(100 * time.Millisecond)
	if len(handler.all()) != 1 {
		t.Fatalf("expected only the event after the persisted offset, got %d: %+v", len(handler.all()), handler.all())
	}
	if handler.all()[0].Data["new"] != "running" {
		t.Errorf("resumed at the wrong event: %+v", handler.all()[0])
	}
}

func TestFileSourceHandlesRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm-state.events")
	if err := os.WriteFile(path, []byte(`{"type":"vm.state_change","timestamp":"1970-01-01T00:00:01Z","data":{"vm":"u1","prev":"stopped","new":"starting"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newRecordingHandler()
	src := newTestFileSource(path, handler, &stats{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)
	handler.waitForEvents(t, 1)

	// Simulate an external rotation: rename the current file away, then have
	// ukpd (per the vendor, via SIGHUP) create a brand new file at the same
	// path starting from scratch.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"vm.state_change","timestamp":"1970-01-01T00:00:03Z","data":{"vm":"u2","prev":"stopped","new":"starting"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler.waitForEvents(t, 2)
	if handler.all()[1].Data["vm"] != "u2" {
		t.Errorf("did not pick up the rotated-in file's event: %+v", handler.all()[1])
	}
}

func TestFileSourceToleratesMissingFileAtBoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm-state.events")
	// Do NOT create the file — ukpd may not have written it yet.

	handler := newRecordingHandler()
	src := newTestFileSource(path, handler, &stats{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	time.Sleep(50 * time.Millisecond) // must not panic/error while the file is absent

	if err := os.WriteFile(path, []byte(`{"type":"vm.state_change","timestamp":"1970-01-01T00:00:01Z","data":{"vm":"u1","prev":"stopped","new":"starting"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler.waitForEvents(t, 1)
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}
