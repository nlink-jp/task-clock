package logrotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotationKeepsGenerationsAndBoundsSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, err := New(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for range 10 {                         // 400 bytes → several rotations
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 100 {
		t.Errorf("active log %d bytes, want <= 100", info.Size())
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("generation .1 missing: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Errorf("generation .2 missing: %v", err)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("generation .3 exists — keep=2 must drop it")
	}
}

func TestOversizeSingleWriteStillLands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, err := New(path, 50, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	big := strings.Repeat("y", 200)
	if _, err := w.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if len(data) != 200 {
		t.Errorf("oversize write must not be dropped, got %d bytes", len(data))
	}
}

func TestAppendsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, _ := New(path, 1000, 2)
	w.Write([]byte("first\n"))
	w.Close()
	w2, _ := New(path, 1000, 2)
	w2.Write([]byte("second\n"))
	w2.Close()
	data, _ := os.ReadFile(path)
	if string(data) != "first\nsecond\n" {
		t.Errorf("reopen must append, got %q", string(data))
	}
}
