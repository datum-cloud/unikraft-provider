// This file is today's event transport: a Unix-socket listener ukpd's
// vm.state_change sink connects to, decoding each event and handing it to
// an eventHandler. Kept behind that interface so a file-tailing source can
// replace it later without processor.go changing at all.

package stateprojector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
)

// eventHandler processes a decoded vm.state_change event. Implemented by
// *processor; kept as an interface so an event source doesn't depend on how
// events get turned into billing records.
type eventHandler interface {
	HandleEvent(ev stateChange)
}

// socketSource accepts vm.state_change streams from ukpd over a Unix domain
// socket. It is today's eventSource; a file-tailing source (planned, see
// docs/enhancements/usage-metering) implements the same eventSource
// interface, so swapping it in is a change to service.go's wiring only —
// nothing here or in processor.go needs to change.
type socketSource struct {
	path    string
	handler eventHandler
	stats   *stats
}

func newSocketSource(path string, handler eventHandler, stats *stats) *socketSource {
	return &socketSource{path: path, handler: handler, stats: stats}
}

// Run implements eventSource: it owns all of this transport's setup (here,
// the socket directory and a stale socket left by a prior run) and then
// blocks accepting connections until ctx is done.
func (s *socketSource) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// Remove a stale socket left by a prior run.
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.path, err)
	}
	defer ln.Close()
	log.Printf("conn listening socket=%s", s.path)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("conn accept_error err=%v", err)
			continue
		}
		id := s.stats.connections.Add(1)
		log.Printf("conn accepted id=%d", id)
		go func(c net.Conn, id int64) {
			defer c.Close()
			s.handleConnection(c, id)
		}(conn, id)
	}
}

func (s *socketSource) handleConnection(conn net.Conn, id int64) {
	start := time.Now()
	var events int64
	dec := json.NewDecoder(bufio.NewReader(conn))
	for {
		// Decode to raw bytes first so a payload we cannot interpret can be
		// logged exactly as ukpd sent it.
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				log.Printf("conn closed id=%d events=%d duration=%s", id, events, time.Since(start).Round(time.Millisecond))
				return
			}
			s.stats.decodeErrors.Add(1)
			log.Printf("conn decode_error id=%d events=%d err=%v", id, events, err)
			return
		}
		var ev stateChange
		if err := json.Unmarshal(raw, &ev); err != nil {
			s.stats.decodeErrors.Add(1)
			log.Printf("conn unmarshal_error id=%d err=%v raw=%s", id, err, truncate(raw, 512))
			return
		}
		ev.Raw = raw
		events++
		s.stats.eventsReceived.Add(1)
		s.handler.HandleEvent(ev)
	}
}
