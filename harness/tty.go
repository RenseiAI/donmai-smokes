package harness

import "os"

// StdinIsTTY returns true when the process's stdin is connected to a
// terminal device. In CI (GitHub Actions, etc.) stdin is typically a
// pipe or /dev/null, so this returns false.
//
// Indirected through a package-level var so tests can simulate the CI
// non-TTY condition without manipulating the test process's actual
// stdin.
var StdinIsTTY = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// IsNonInteractive returns true when the harness is running in a context
// where interactive auth (browser flow, device-code grant, password
// prompt) would block waiting for a TTY.
//
// Returns true when:
//   - the CI environment variable is set (any value), OR
//   - StdinIsTTY returns false (stdin is a pipe / /dev/null).
//
// The CI check is the deciding signal on GitHub Actions runners, where
// stdin can be wired to /dev/null (a character device) and would
// otherwise look like a TTY despite being effectively non-interactive.
func IsNonInteractive() bool {
	if os.Getenv("CI") != "" {
		return true
	}
	return !StdinIsTTY()
}
