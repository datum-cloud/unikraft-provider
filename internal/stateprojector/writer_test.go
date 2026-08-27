package stateprojector

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotateIfNeeded(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "vm-state.usage")
	w := newOutputWriter(out, 10, 0, &stats{})

	if err := os.WriteFile(out, []byte("0123456789extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.rotateIfNeeded()

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
	if w.stats.rotations.Load() != 1 {
		t.Errorf("rotations counter = %d, want 1", w.stats.rotations.Load())
	}

	// Below threshold: no rotation.
	if err := os.WriteFile(out, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.rotateIfNeeded()
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("small file should not have been rotated away: %v", err)
	}

	// rotateSizeBytes <= 0 disables rotation entirely.
	w2 := newOutputWriter(out, 0, 0, &stats{})
	if err := os.WriteFile(out, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	w2.rotateIfNeeded()
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("rotateSizeBytes=0 must disable rotation: %v", err)
	}
}

func TestCleanupOldRotations(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "vm-state.usage")
	w := newOutputWriter(out, 0, time.Hour, &stats{})

	old := fmt.Sprintf("%s.%d", out, time.Now().Add(-2*time.Hour).Unix())
	recent := fmt.Sprintf("%s.%d", out, time.Now().Add(-10*time.Minute).Unix())
	notOurs := out + ".garbage"
	for _, f := range []string{old, recent, notOurs} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w.cleanupOldRotations()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expected old rotated file to be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent rotated file should survive: %v", err)
	}
	if _, err := os.Stat(notOurs); err != nil {
		t.Errorf("non-numeric-suffixed file must not be touched: %v", err)
	}
	if w.stats.rotationDeletes.Load() != 1 {
		t.Errorf("rotationDeletes counter = %d, want 1", w.stats.rotationDeletes.Load())
	}

	// rotateMaxAge <= 0 disables the backstop entirely.
	w2 := newOutputWriter(out, 0, 0, &stats{})
	veryOld := fmt.Sprintf("%s.%d", out, time.Now().Add(-1000*time.Hour).Unix())
	if err := os.WriteFile(veryOld, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w2.cleanupOldRotations()
	if _, err := os.Stat(veryOld); err != nil {
		t.Errorf("rotateMaxAge=0 must disable the backstop: %v", err)
	}
}
