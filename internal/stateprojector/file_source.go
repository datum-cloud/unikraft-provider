// This file is the event transport: it tails a plain file ukpd's `type:
// file` log sink appends vm.state_change events to, decoding each complete
// line and handing it to an eventHandler. It replaced a Unix-socket
// transport (ukpd pushing events over a live connection) because that
// requires an active listener at the exact moment an event fires — a
// redeploy or brief crash of this component could miss an event a socket
// would have delivered. A file sink has no such requirement: ukpd keeps
// appending regardless of whether anything is reading, and this tailer
// resumes from a persisted byte offset across restarts.
//
// Vendor-confirmed (2026-08-28): `--log-rotation` only applies to the main
// `--log-path` controller log, not sink files; `SIGHUP` does reopen sink
// files the same way it reopens that log; and there is no built-in cap on a
// sink file's size. That means nothing here will ever rotate this file for
// us — an external logrotate-style rotation (rename + SIGHUP to ukpd) is an
// operational requirement, not something this tailer does. What this tailer
// does handle is the file being replaced out from under it (rotated or
// truncated by anything, including that external process): it detects the
// change via file identity and reopens from the start.

package stateprojector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// eventHandler processes a decoded vm.state_change event. Implemented by
// *processor; kept as an interface so an event source doesn't depend on how
// events get turned into billing records.
type eventHandler interface {
	HandleEvent(ev stateChange)
}

// fileSource tails a plain file, decoding one JSON event per line.
type fileSource struct {
	path         string
	offsetPath   string
	handler      eventHandler
	stats        *stats
	pollInterval time.Duration
}

func newFileSource(path string, handler eventHandler, stats *stats) *fileSource {
	return &fileSource{
		path:         path,
		offsetPath:   path + ".offset",
		handler:      handler,
		stats:        stats,
		pollInterval: 500 * time.Millisecond,
	}
}

// Run implements eventSource: it owns the events directory's setup and then
// polls the file for new content until ctx is done.
func (f *fileSource) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return fmt.Errorf("mkdir events dir: %w", err)
	}

	offset := f.loadOffset()
	lastSaved := offset
	log.Printf("conn tailing path=%s offset=%d", f.path, offset)

	var (
		file     *os.File
		fileInfo os.FileInfo
		pending  []byte
	)
	defer func() {
		if file != nil {
			file.Close()
		}
		f.saveOffset(offset)
	}()

	tick := time.NewTicker(f.pollInterval)
	defer tick.Stop()

	for {
		if file == nil {
			opened, fi, corrected, err := f.open(offset)
			if err != nil {
				log.Printf("conn open_error path=%s err=%v", f.path, err)
			}
			file, fileInfo, offset = opened, fi, corrected
			if file == nil {
				select {
				case <-ctx.Done():
					return nil
				case <-tick.C:
					continue
				}
			}
		}

		if fi, err := os.Stat(f.path); err == nil {
			if !os.SameFile(fi, fileInfo) || fi.Size() < offset {
				log.Printf("conn rotated path=%s (reopening from start)", f.path)
				file.Close()
				file = nil
				offset = 0
				pending = nil
				continue
			}
		}

		buf := make([]byte, 64*1024)
		for {
			n, err := file.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				offset += int64(n)
				pending = f.consumeLines(pending)
			}
			if err != nil {
				break // EOF (or a real read error) — either way, wait for the next tick
			}
		}
		if offset != lastSaved {
			f.saveOffset(offset)
			lastSaved = offset
		}

		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// open seeks to offset, treating a persisted offset beyond the file's
// current size as evidence the file was rotated/truncated since the last
// checkpoint (start over from 0 rather than erroring or blocking forever) —
// the corrected offset is returned so the caller's bookkeeping stays in
// sync with where the file descriptor actually is. Returns a nil file (and
// no error) if the file does not exist yet — ukpd may not have created it
// at boot.
func (f *fileSource) open(offset int64) (*os.File, os.FileInfo, int64, error) {
	file, err := os.Open(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, offset, nil
		}
		return nil, nil, offset, err
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, offset, err
	}
	if offset > fi.Size() {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return nil, nil, offset, err
	}
	return file, fi, offset, nil
}

// consumeLines decodes every complete line in buf, returning whatever
// incomplete line remains at the end (to be prefixed onto the next read).
func (f *fileSource) consumeLines(buf []byte) []byte {
	for {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			return buf
		}
		line := buf[:idx]
		buf = buf[idx+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			f.decodeAndHandle(line)
		}
	}
}

func (f *fileSource) decodeAndHandle(line []byte) {
	var ev stateChange
	if err := json.Unmarshal(line, &ev); err != nil {
		f.stats.decodeErrors.Add(1)
		log.Printf("conn unmarshal_error err=%v raw=%s", err, truncate(line, 512))
		return
	}
	ev.Raw = append([]byte(nil), line...)
	f.stats.eventsReceived.Add(1)
	f.handler.HandleEvent(ev)
}

func (f *fileSource) loadOffset() int64 {
	b, err := os.ReadFile(f.offsetPath)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func (f *fileSource) saveOffset(offset int64) {
	if err := os.WriteFile(f.offsetPath, []byte(strconv.FormatInt(offset, 10)), 0o644); err != nil {
		log.Printf("conn offset_save_error path=%s err=%v", f.offsetPath, err)
	}
}
