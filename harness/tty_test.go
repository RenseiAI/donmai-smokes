package harness

import "testing"

// TestStdinIsTTY_Overridable verifies the StdinIsTTY package-level var
// is overridable so tests can simulate the CI non-TTY condition
// deterministically.
func TestStdinIsTTY_Overridable(t *testing.T) {
	orig := StdinIsTTY
	t.Cleanup(func() { StdinIsTTY = orig })

	StdinIsTTY = func() bool { return false }
	if StdinIsTTY() {
		t.Error("StdinIsTTY override (false) did not take effect")
	}
	StdinIsTTY = func() bool { return true }
	if !StdinIsTTY() {
		t.Error("StdinIsTTY override (true) did not take effect")
	}
}

// TestIsNonInteractive_CIEnv verifies IsNonInteractive returns true
// when CI=true.
func TestIsNonInteractive_CIEnv(t *testing.T) {
	t.Setenv("CI", "true")
	if !IsNonInteractive() {
		t.Error("IsNonInteractive should return true when CI=true")
	}
}

// TestIsNonInteractive_NoTTYNoCI verifies IsNonInteractive returns true
// when CI is unset but StdinIsTTY returns false.
func TestIsNonInteractive_NoTTYNoCI(t *testing.T) {
	t.Setenv("CI", "")
	orig := StdinIsTTY
	StdinIsTTY = func() bool { return false }
	t.Cleanup(func() { StdinIsTTY = orig })

	if !IsNonInteractive() {
		t.Error("IsNonInteractive should return true when stdin is not a TTY")
	}
}

// TestIsNonInteractive_TTYNoCI verifies IsNonInteractive returns false
// when CI is unset and StdinIsTTY returns true.
func TestIsNonInteractive_TTYNoCI(t *testing.T) {
	t.Setenv("CI", "")
	orig := StdinIsTTY
	StdinIsTTY = func() bool { return true }
	t.Cleanup(func() { StdinIsTTY = orig })

	if IsNonInteractive() {
		t.Error("IsNonInteractive should return false when CI is unset and stdin is a TTY")
	}
}
