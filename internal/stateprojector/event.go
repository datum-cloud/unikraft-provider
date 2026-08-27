// This file decodes ukpd's vm.state_change wire payload and extracts the
// uuid + old/new state transition from it.

package stateprojector

import (
	"encoding/json"
	"regexp"
	"strconv"
	"time"
)

// onState is the single runtime state that counts as "running/consuming".
// Every other state (standby, stopped, stopping, draining, terminated) is
// "off" and bills nothing.
const onState = "running"

// maxEventSkew is how far an event's own timestamp may sit behind wall clock
// before it is called out. A real skew means a replayed/buffered event,
// which matters because flushes are computed against wall clock.
const maxEventSkew = time.Minute

// uuidRe scans for a uuid-shaped token when an event carries no recognized
// uuid field.
var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// stateChange is the subset of a vm.state_change event (JSON encoding) this
// package cares about. json.Decoder tolerates unknown fields, so the full
// vendor payload is dropped; only these are read.
type stateChange struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Data      map[string]any `json:"data"`

	// Raw is the exact wire bytes, kept only so a payload we fail to
	// interpret can be logged verbatim.
	Raw json.RawMessage `json:"-"`
}

func (ev stateChange) raw() []byte {
	if len(ev.Raw) > 0 {
		return ev.Raw
	}
	b, _ := json.Marshal(ev)
	return b
}

// extractTransition pulls uuid + old/new state out of the event data
// object. ukpd's real payload only ever carries "vm"/"prev"/"new"; if the
// uuid field is ever renamed, the regex fallback below still catches it by
// shape rather than needing another speculative key added here.
func extractTransition(data map[string]any) (uuid, oldState, newState string) {
	if data == nil {
		return "", "", ""
	}
	uuid = stringField(data, "vm")
	if uuid == "" {
		raw, _ := json.Marshal(data)
		if m := uuidRe.Find(raw); m != nil {
			uuid = string(m)
		}
	}
	newState = stringField(data, "new")
	oldState = stringField(data, "prev")
	return uuid, oldState, newState
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// parseTime accepts RFC3339 or Unix-epoch seconds (string or number).
func parseTime(v any) time.Time {
	switch t := v.(type) {
	case string:
		if tt, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return tt
		}
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return time.Unix(n, 0).UTC()
		}
	case float64:
		return time.Unix(int64(t), 0).UTC()
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Time{}
}

// truncate bounds a payload logged verbatim so a pathological event cannot
// flood the log.
func truncate(b []byte, limit int) string {
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + " ...(" + strconv.Itoa(len(b)-limit) + " bytes truncated)"
}
