// This file holds the cumulative counters every other file increments, and
// the heartbeat loop that logs them on an interval.

package stateprojector

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

// stats are cumulative counters reported by the heartbeat, so a single log
// line answers "is anything arriving, and is anything coming out" without
// correlating a whole log stream.
type stats struct {
	decodeErrors        atomic.Int64
	eventsReceived      atomic.Int64
	eventsWrongTyp      atomic.Int64
	noUUID              atomic.Int64
	noState             atomic.Int64
	windowsOpened       atomic.Int64
	windowsClosed       atomic.Int64
	recordsWritten      atomic.Int64
	writeErrors         atomic.Int64
	unresolved          atomic.Int64
	podIndexed          atomic.Int64
	watchErrors         atomic.Int64
	staleEvents         atomic.Int64
	overbilled          atomic.Int64
	rotations           atomic.Int64
	rotationDeletes     atomic.Int64
	projectLabelMissing atomic.Int64
}

// statsSource supplies the point-in-time gauges the heartbeat can't track as
// simple counters (current index/window sizes).
type statsSource struct {
	indexedInstances func() int
	openWindows      func() int
}

// logHeartbeat runs unconditionally (not only under -debug) and is the first
// line to read when diagnosing: if events_received stays 0 nothing has
// arrived in the events file yet, and if records_written stays 0 while
// windows open, attribution or the output path is at fault.
func logHeartbeat(ctx context.Context, s *stats, src statsSource, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			log.Printf("stats uptime=%s events_received=%d events_wrong_type=%d "+
				"dropped_no_uuid=%d dropped_no_state=%d decode_errors=%d "+
				"windows_open=%d windows_opened=%d windows_closed=%d "+
				"records_written=%d write_errors=%d unresolved=%d indexed_instances=%d watch_errors=%d "+
				"stale_events=%d overbilled=%d rotations=%d rotation_deletes=%d project_label_missing=%d",
				time.Since(start).Round(time.Second),
				s.eventsReceived.Load(), s.eventsWrongTyp.Load(),
				s.noUUID.Load(), s.noState.Load(), s.decodeErrors.Load(),
				src.openWindows(), s.windowsOpened.Load(), s.windowsClosed.Load(),
				s.recordsWritten.Load(), s.writeErrors.Load(),
				s.unresolved.Load(), src.indexedInstances(), s.watchErrors.Load(),
				s.staleEvents.Load(), s.overbilled.Load(),
				s.rotations.Load(), s.rotationDeletes.Load(),
				s.projectLabelMissing.Load())
		}
	}
}
