package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// ── FileExists ────────────────────────────────────────────────────────────────

func TestFileExists(t *testing.T) {
	tmp := t.TempDir()

	// Non-existent file.
	if FileExists(filepath.Join(tmp, "ghost.txt")) {
		t.Error("FileExists: non-existent file should return false")
	}

	// Existing file.
	path := filepath.Join(tmp, "real.txt")
	if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !FileExists(path) {
		t.Error("FileExists: existing file should return true")
	}

	// Directory should return false.
	if FileExists(tmp) {
		t.Error("FileExists: directory should return false")
	}
}

// ── Getuid ────────────────────────────────────────────────────────────────────

func TestGetuid(t *testing.T) {
	// We can't easily fixture the OS UID, but we can verify the helper
	// returns something within the int range and matches os.Getuid()
	// directly (the helper is purely a stdlib-wrapping seam).
	if got, want := Getuid(), os.Getuid(); got != want {
		t.Errorf("Getuid() = %d, want %d", got, want)
	}
}
