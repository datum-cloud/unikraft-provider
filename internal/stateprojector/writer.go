// This file appends usage records to the output JSONL file and owns its
// size-based rotation and age-based rotated-file cleanup.

package stateprojector

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// record is one windowed billing record written to the output JSONL stream.
type record struct {
	ID          string  `json:"id"`
	Project     string  `json:"project"`
	Instance    string  `json:"instance"`
	UUID        string  `json:"uuid"`
	VCPU        int64   `json:"vcpu"`
	MemoryBytes int64   `json:"memory_bytes"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
	DurationS   float64 `json:"duration_s"`
}

// windowID is a deterministic id for a (uuid, start, end) window, for
// downstream dedup on replay.
func windowID(uuid string, start, end time.Time) string {
	sum := md5.Sum(fmt.Appendf(nil, "%s|%s|%s", uuid, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])
}

// outputWriter appends records to a JSONL file and owns its rotation.
type outputWriter struct {
	path            string
	rotateSizeBytes int64
	rotateMaxAge    time.Duration
	stats           *stats
}

func newOutputWriter(path string, rotateSizeBytes int64, rotateMaxAge time.Duration, stats *stats) *outputWriter {
	return &outputWriter{path: path, rotateSizeBytes: rotateSizeBytes, rotateMaxAge: rotateMaxAge, stats: stats}
}

func (w *outputWriter) append(rec record) error {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// rotateIfNeeded renames the output file once it reaches rotateSizeBytes, so
// a reader with the file already open keeps growing under its original name
// while a rotated-away one stops changing and gets a final EOF. The next
// append recreates the path fresh (O_CREATE). Renaming — never truncating in
// place — is what makes this safe: a rename only changes a directory entry,
// so an existing open handle keeps reading the same underlying data to
// whatever became its final content.
func (w *outputWriter) rotateIfNeeded() {
	if w.rotateSizeBytes <= 0 {
		return
	}
	info, err := os.Stat(w.path)
	if err != nil {
		return // nothing to rotate yet
	}
	if info.Size() < w.rotateSizeBytes {
		return
	}
	rotated := fmt.Sprintf("%s.%d", w.path, time.Now().Unix())
	if err := os.Rename(w.path, rotated); err != nil {
		log.Printf("rotate error=%v path=%s size_bytes=%d", err, w.path, info.Size())
		return
	}
	w.stats.rotations.Add(1)
	log.Printf("rotate done from=%s to=%s size_bytes=%d threshold_bytes=%d", w.path, rotated, info.Size(), w.rotateSizeBytes)
}

// cleanupOldRotations deletes rotated files older than rotateMaxAge. This is
// a disk-safety backstop, not the primary cleanup path — the correct owner
// of deletion is Vector, which alone knows a file was fully shipped
// downstream. Age is read from the rotation timestamp embedded in the
// filename by rotateIfNeeded, not the file's mtime (which rename does not
// update).
func (w *outputWriter) cleanupOldRotations() {
	if w.rotateMaxAge <= 0 {
		return
	}
	matches, err := filepath.Glob(w.path + ".*")
	if err != nil {
		log.Printf("rotate cleanup_error err=%v", err)
		return
	}
	now := time.Now()
	for _, f := range matches {
		idx := strings.LastIndex(f, ".")
		if idx < 0 {
			continue
		}
		ts, err := strconv.ParseInt(f[idx+1:], 10, 64)
		if err != nil {
			// Not one of ours (or malformed) — skip rather than guess.
			continue
		}
		age := now.Sub(time.Unix(ts, 0))
		if age < w.rotateMaxAge {
			continue
		}
		if err := os.Remove(f); err != nil {
			log.Printf("rotate delete_error path=%s age=%s err=%v", f, age.Round(time.Second), err)
			continue
		}
		w.stats.rotationDeletes.Add(1)
		log.Printf("rotate deleted reason=age_backstop path=%s age=%s max_age=%s", f, age.Round(time.Second), w.rotateMaxAge)
	}
}
