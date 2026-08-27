// This file holds the per-instance running-window bookkeeping (open, close,
// flush watermark) that processor.go operates on.

package stateprojector

import (
	"sync"
	"time"
)

// window is the per-instance running-window bookkeeping.
type window struct {
	uuid          string
	runningSince  time.Time // when the current running stretch began (window identity)
	reportedUntil time.Time // watermark of the last emitted record for this window
	resolved      *info

	records        int    // records emitted for this window, for the close log
	lastResolveLog string // last attribution failure logged, to avoid repeating it every flush
}

// windowStore holds every currently-open window, keyed by instance uuid.
//
// Locking is coarse-grained and explicit (Lock/Unlock, not per-call): a
// caller locks once for a whole logical transition — e.g. "open or no-op,
// then resolve" or "iterate every window and flush each" — mirroring how a
// single mutex protected both the map and every window's fields in the
// original single-file implementation. This package is the only consumer,
// so the manual discipline is acceptable; do not export this type as-is.
type windowStore struct {
	mu      sync.Mutex
	windows map[string]*window
}

func newWindowStore() *windowStore {
	return &windowStore{windows: make(map[string]*window)}
}

func (s *windowStore) Lock()   { s.mu.Lock() }
func (s *windowStore) Unlock() { s.mu.Unlock() }

// getOrOpen returns the existing window for uuid, or creates and stores a new
// one anchored at `at`. The bool reports whether it was newly created. Caller
// must hold the lock.
func (s *windowStore) getOrOpen(uuid string, at time.Time) (*window, bool) {
	if w, ok := s.windows[uuid]; ok {
		return w, false
	}
	w := &window{uuid: uuid, runningSince: at, reportedUntil: at}
	s.windows[uuid] = w
	return w, true
}

// get requires the lock held.
func (s *windowStore) get(uuid string) (*window, bool) {
	w, ok := s.windows[uuid]
	return w, ok
}

// delete requires the lock held.
func (s *windowStore) delete(uuid string) {
	delete(s.windows, uuid)
}

// len requires the lock held.
func (s *windowStore) len() int {
	return len(s.windows)
}

// all requires the lock held; the caller (periodicFlush) keeps the lock for
// the whole iteration, same as the original design.
func (s *windowStore) all() []*window {
	out := make([]*window, 0, len(s.windows))
	for _, w := range s.windows {
		out = append(out, w)
	}
	return out
}
