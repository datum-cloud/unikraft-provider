// This file is the core state machine: it turns each vm.state_change event
// into an open/close window transition, resolves attribution, and drives
// the periodic flush that reports long-running instances incrementally.

package stateprojector

import (
	"context"
	"log"
	"time"
)

// resolver maps an instance uuid to its identity/resources. Satisfied by
// *podIndex; kept as an interface so processor doesn't depend on how
// attribution is actually looked up (k8s-backed today, a fake in tests).
type resolver interface {
	resolve(uuid string) (*info, string)
}

// recordWriter appends usage records and owns output-file rotation.
// Satisfied by *outputWriter.
type recordWriter interface {
	append(rec record) error
	rotateIfNeeded()
	cleanupOldRotations()
}

// processor turns vm.state_change events into windowed, attributed usage
// records. It implements eventHandler, so any eventSource (socket today, a
// tailed file later) can drive it without either side depending on the other.
type processor struct {
	windows  *windowStore
	resolve  resolver
	out      recordWriter
	stats    *stats
	debug    bool
	interval time.Duration
}

func newProcessor(resolve resolver, out recordWriter, stats *stats, debug bool, flushInterval time.Duration) *processor {
	return &processor{
		windows:  newWindowStore(),
		resolve:  resolve,
		out:      out,
		stats:    stats,
		debug:    debug,
		interval: flushInterval,
	}
}

func (p *processor) debugf(format string, args ...any) {
	if p.debug {
		log.Printf(format, args...)
	}
}

func (p *processor) openWindows() int {
	p.windows.Lock()
	defer p.windows.Unlock()
	return p.windows.len()
}

// HandleEvent implements eventHandler.
func (p *processor) HandleEvent(ev stateChange) {
	if ev.Type != "" && ev.Type != "vm.state_change" {
		p.stats.eventsWrongTyp.Add(1)
		p.debugf("event ignored reason=wrong_type type=%q", ev.Type)
		return
	}
	ts := parseTime(ev.Timestamp)
	if ts.IsZero() {
		ts = time.Now().UTC()
		p.debugf("event warn=unparseable_timestamp raw_timestamp=%q using=now", ev.Timestamp)
	}
	uuid, oldState, newState := extractTransition(ev.Data)
	if uuid == "" {
		// Logged loudly with the payload: if the vendor renames its uuid
		// field this is the only signal, and it silently stops all billing.
		p.stats.noUUID.Add(1)
		log.Printf("event dropped reason=no_uuid raw=%s", truncate(ev.raw(), 512))
		return
	}
	if newState == "" {
		// An unparseable new state tells us nothing about whether the
		// instance is consuming, so leave any open window alone.
		p.stats.noState.Add(1)
		log.Printf("event dropped reason=no_new_state uuid=%s raw=%s", uuid, truncate(ev.raw(), 512))
		return
	}
	enteredOn := newState == onState

	p.windows.Lock()
	defer p.windows.Unlock()

	if enteredOn {
		w, created := p.windows.getOrOpen(uuid, ts)
		if !created {
			p.debugf("event noop reason=already_running uuid=%s prev=%q new=%q running_since=%s",
				uuid, oldState, newState, w.runningSince.Format(time.RFC3339))
			return
		}
		p.stats.windowsOpened.Add(1)
		log.Printf("window opened uuid=%s prev=%q new=%q at=%s open_windows=%d",
			uuid, oldState, newState, ts.Format(time.RFC3339), p.windows.len())
		// A window anchored well behind wall clock (a replayed/buffered
		// event) will be flushed against time.Now(), so the first flush
		// bills the whole gap.
		if skew := time.Since(ts); skew > maxEventSkew {
			p.stats.staleEvents.Add(1)
			log.Printf("window WARN=stale_open uuid=%s event_time=%s skew=%s "+
				"(first flush will bill the gap to wall clock)", uuid, ts.Format(time.RFC3339), skew.Round(time.Second))
		}
		// Resolve now, while the VM is guaranteed to be running — see
		// resolveWindow's comment for why this can't wait until close/flush.
		p.resolveWindow(w)
		return
	}

	w, ok := p.windows.get(uuid)
	if !ok {
		p.debugf("event noop reason=not_running uuid=%s prev=%q new=%q", uuid, oldState, newState)
		return
	}
	// Any non-running state closes the window. The open window — not the
	// event's old-state field — is the authority on whether we were
	// running, so a missing/renamed "prev" cannot strand a window open.
	p.emitWindow(w, ts, "close")
	p.windows.delete(uuid)
	p.stats.windowsClosed.Add(1)
	log.Printf("window closed uuid=%s prev=%q new=%q running_since=%s total_s=%.1f records=%d open_windows=%d",
		uuid, oldState, newState, w.runningSince.Format(time.RFC3339), ts.Sub(w.runningSince).Seconds(), w.records, p.windows.len())
}

// periodicFlush incrementally reports open running windows so a long-running
// instance bills continuously and a crash loses at most one flush interval.
//
// Time bases: a window's start comes from the event's own timestamp, while
// the flush end comes from wall clock. These normally agree (ukpd shares
// this node's clock), but a replayed event anchors a window in the past and
// the next flush then bills start -> now in one record — see the
// "stale_open"/"overbilled" logs for both ends of that hazard.
func (p *processor) periodicFlush(ctx context.Context) {
	tick := time.NewTicker(p.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now := time.Now().UTC()
			p.windows.Lock()
			var flushed, skipped int
			for _, w := range p.windows.all() {
				if now.Sub(w.reportedUntil) < p.interval {
					skipped++
					continue
				}
				p.emitWindow(w, now, "flush")
				flushed++
			}
			open := p.windows.len()
			p.windows.Unlock()
			if open > 0 {
				p.debugf("window flush_cycle open=%d flushed=%d skipped=%d", open, flushed, skipped)
			}
			p.out.cleanupOldRotations()
		}
	}
}

// emitWindow writes a windowed usage record covering [w.reportedUntil, end]
// and advances the watermark. Caller holds the windows lock.
func (p *processor) emitWindow(w *window, end time.Time, cause string) {
	if end.Before(w.reportedUntil) {
		// The watermark is already past this event's time: a wall-clock
		// flush billed a span that event time says had not happened yet —
		// i.e. already over-billed, and the true close is about to be
		// clamped to nothing. Loud, because it is a money bug.
		log.Printf("window ALERT=overbilled uuid=%s cause=%s overbilled_s=%.1f event_end=%s watermark=%s",
			w.uuid, cause, w.reportedUntil.Sub(end).Seconds(), end.Format(time.RFC3339), w.reportedUntil.Format(time.RFC3339))
		p.stats.overbilled.Add(1)
		end = w.reportedUntil
	}
	if end.Equal(w.reportedUntil) {
		p.debugf("window skip reason=zero_duration uuid=%s cause=%s at=%s", w.uuid, cause, end.Format(time.RFC3339))
		return
	}
	rec := p.recordFor(w, end)
	if err := p.out.append(rec); err != nil {
		// The window is not advanced, so the next flush retries this span.
		p.stats.writeErrors.Add(1)
		log.Printf("record write_error uuid=%s cause=%s err=%v", w.uuid, cause, err)
		return
	}
	w.reportedUntil = end
	w.records++
	p.stats.recordsWritten.Add(1)
	// The emitted record is the billing artifact, so log it in full: this
	// is the line to compare against what Vector shipped.
	log.Printf("record written cause=%s id=%s uuid=%s project=%s instance=%s vcpu=%d memory_bytes=%d start=%s end=%s duration_s=%.1f",
		cause, rec.ID, rec.UUID, rec.Project, rec.Instance, rec.VCPU, rec.MemoryBytes, rec.Start, rec.End, rec.DurationS)
	p.out.rotateIfNeeded()
}

// resolveWindow attempts to resolve a window's attribution once, caching the
// result on w.resolved. Called both when a window opens — while the VM is
// guaranteed to be running — and again lazily from recordFor as a
// fallback/retry for anything still unresolved (e.g. the pod watch hadn't
// synced yet when the window opened).
//
// Resolving only at close/flush time (the original design) loses a real
// race: a short-lived instance can fully stop before this ever gets a
// chance to look it up. Resolving at open time wins that race.
func (p *processor) resolveWindow(w *window) {
	if w.resolved != nil {
		return
	}
	rec, reason := p.resolve.resolve(w.uuid)
	if rec != nil {
		w.resolved = rec
		log.Printf("resolve ok uuid=%s %s", w.uuid, rec)
		return
	}
	if reason != w.lastResolveLog {
		// Attribution retries every flush; log each distinct reason once
		// per window so a persistent failure does not repeat indefinitely.
		p.stats.unresolved.Add(1)
		log.Printf("resolve failed uuid=%s reason=%s (record emitted with project=\"-\")", w.uuid, reason)
		w.lastResolveLog = reason
	}
}

// recordFor resolves the instance's identity/resources and builds the record.
func (p *processor) recordFor(w *window, end time.Time) record {
	p.resolveWindow(w)
	rec := w.resolved
	start := w.reportedUntil
	dur := end.Sub(start).Seconds()
	if dur < 0 {
		dur = 0
	}
	out := record{
		ID:        windowID(w.uuid, start, end),
		Project:   "-",
		Instance:  "-",
		UUID:      w.uuid,
		Start:     start.UTC().Format(time.RFC3339),
		End:       end.UTC().Format(time.RFC3339),
		DurationS: dur,
	}
	if rec != nil {
		// An empty project (namespace unresolved or genuinely unlabeled)
		// stays "-" rather than becoming an empty string in the record.
		if rec.project != "" {
			out.Project = rec.project
		}
		out.Instance = rec.instance
		out.VCPU = coresFromMilli(rec.vcpuMilli)
		out.MemoryBytes = rec.memoryBytes
	}
	return out
}
