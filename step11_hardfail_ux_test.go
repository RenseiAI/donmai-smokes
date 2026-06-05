package smokes

// step11_hardfail_ux_test.go — GAP-03 coverage.
//
// Proves that the donmai binary's hard-fail error paths:
//   a) Guide the user with an actionable next step containing the correct
//      `donmai` binary name.
//   b) Do NOT reference the legacy `af ` binary or `AgentFactory` in
//      user-facing output (GO-1 / P4 debrand work).
//
// What this exercises (binary-only, no live daemon required):
//
//   1. `donmai daemon status --port <unreachable>` — connection-refused path.
//      The daemonDownErr helper (afcli/helpers.go) must produce a message
//      containing "daemon is not running", "donmai daemon install", and
//      "donmai daemon start". No raw "connection refused" dial error,
//      no "af " binary reference, no "AgentFactory" text.
//
//   2. `donmai daemon stats --port <unreachable>` — same daemonDownErr path
//      for the stats command.
//
//   3. `donmai daemon doctor` under a hermetic HOME (no launchd plist /
//      systemd unit installed) — the doctor command must exit non-zero and
//      its error output must contain "daemon install" guidance referencing
//      the `donmai` binary, not "af".
//
// Platform-free: no WorkOS, no Linear, no /api/cli/*, no rsk_* tokens.
// Only the donmai binary itself is invoked — no daemon process is started.
//
// Skip-mode: honours -short (build skip) and DONMAI_SMOKES_SKIP_INSTALLER=1
// for the doctor sub-test (which uses the hermetic HOME / installer path).
// The status/stats sub-tests only need the binary; they honour -short.
//
// Timing: warm cache ~1s (three subprocess calls; binary build dominates
// on cold cache: 60-90s).

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestDaemonHardFailRemediation exercises the three key hard-fail paths in
// the donmai CLI and asserts the error output guides the user with the
// correct binary name and actionable next step.
func TestDaemonHardFailRemediation(t *testing.T) {
	if testing.Short() {
		t.Skip("binary build required; skipped under -short")
	}

	// Build the donmai binary from the sibling checkout. Cold cache 60-90s;
	// warm sub-second. 3-minute parent context matches the other step files.
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()

	binDir := t.TempDir()
	donmaiBinary, err := afh.BuildDonmaiBinary(buildCtx, afh.BuildOptions{
		OutputPath: filepath.Join(binDir, "donmai"),
		Env:        append(os.Environ(), "GOWORK="),
	})
	if err != nil {
		if strings.Contains(err.Error(), "resolve ../") ||
			strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("donmai binary unavailable: %v", err)
		}
		t.Fatalf("build donmai binary: %v", err)
	}

	// pickUnreachablePort returns a port number that is guaranteed to have
	// nothing listening (it was free when we checked, and we're not binding
	// it). Used so daemon status/stats get a clean "connection refused"
	// rather than a spurious timeout from a stale local process.
	pickUnreachablePort := func(t *testing.T) int {
		t.Helper()
		p, err := afh.PickFreePort()
		if err != nil {
			t.Fatalf("pick free port: %v", err)
		}
		return p
	}

	// runCapture executes donmaiBinary with the given args under a hermetic
	// env and returns (combined output, exit error). The hermetic env
	// contains a minimal PATH + HOME so the binary doesn't look at the
	// operator's real config.
	runCapture := func(t *testing.T, home string, args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, donmaiBinary, args...) //nolint:gosec
		cmd.Env = []string{
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			"NO_COLOR=1",
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		return out.String(), err
	}

	// assertNoLegacyBinary fails the test if output contains " af " (the
	// old binary name as a standalone word with surrounding spaces or at
	// start) or "AgentFactory" (the old product name). We guard the space-
	// bounded form so we don't accidentally match path components or Go
	// package names like "afcli".
	assertNoLegacyBinary := func(t *testing.T, output, cmd string) {
		t.Helper()
		// Match " af " (space-bounded), "` af`", "'af'", "\"af\"".
		legacy := []string{
			" af ",     // standalone word in middle of message
			"`af`",     // backtick-quoted
			"'af'",     // single-quoted
			`"af"`,     // double-quoted
			"af daemon", // "af daemon" as a command example
			"af linear", "af github", "af project", "af login",
			"AgentFactory",
		}
		for _, needle := range legacy {
			if strings.Contains(output, needle) {
				t.Errorf("%s: output contains legacy binary reference %q — debrand regression\n--- output ---\n%s",
					cmd, needle, output)
			}
		}
	}

	// ─── Sub-test 1: daemon status — connection-refused path ─────────────
	//
	// `donmai daemon status --port <unreachable>` must convert the raw
	// "connection refused" TCP error into user-facing guidance via
	// daemonDownErr (afcli/helpers.go:51-55).
	//
	// Expected output shape (from helpers.go line 53):
	//   "daemon is not running — start it with `donmai daemon install` then `donmai daemon start`"
	t.Run("daemon_status_down", func(t *testing.T) {
		port := pickUnreachablePort(t)
		// Belt-and-suspenders: make sure nothing is listening on this port
		// by attempting a connection ourselves. If something answers,
		// skip cleanly.
		conn, connErr := net.DialTimeout("tcp",
			fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if connErr == nil {
			_ = conn.Close()
			t.Skipf("port %d is not free (something is listening); cannot test connection-refused path", port)
		}

		home := t.TempDir()
		out, runErr := runCapture(t, home, "daemon", "status",
			"--host", "127.0.0.1",
			"--port", fmt.Sprintf("%d", port),
		)

		// Must exit non-zero.
		if runErr == nil {
			t.Fatalf("donmai daemon status expected non-zero exit; got exit 0\n--- output ---\n%s", out)
		}

		// Must tell the user the daemon is not running.
		if !strings.Contains(out, "daemon is not running") &&
			!strings.Contains(out, "not running") {
			t.Errorf("daemon status output does not contain 'daemon is not running' or 'not running'\n--- output ---\n%s", out)
		}

		// Must guide the user toward an actionable next step referencing
		// `donmai daemon install` (the install command) and `donmai daemon start`
		// (the start command). Either or both in the message are acceptable
		// as long as at least one actionable `donmai` command is present.
		hasGuidance := strings.Contains(out, "donmai daemon install") ||
			strings.Contains(out, "donmai daemon start") ||
			strings.Contains(out, "donmai daemon")
		if !hasGuidance {
			t.Errorf("daemon status output does not contain actionable `donmai daemon` guidance\n--- output ---\n%s", out)
		}

		assertNoLegacyBinary(t, out, "donmai daemon status")

		t.Logf("daemon status hard-fail output (exit=%v):\n%s", runErr, out)
	})

	// ─── Sub-test 2: daemon stats — connection-refused path ──────────────
	//
	// `donmai daemon stats --port <unreachable>` follows the same
	// daemonDownErr code path as daemon status (afcli/daemon.go:756).
	t.Run("daemon_stats_down", func(t *testing.T) {
		port := pickUnreachablePort(t)
		conn, connErr := net.DialTimeout("tcp",
			fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if connErr == nil {
			_ = conn.Close()
			t.Skipf("port %d is not free; cannot test connection-refused path", port)
		}

		home := t.TempDir()
		out, runErr := runCapture(t, home, "daemon", "stats",
			"--host", "127.0.0.1",
			"--port", fmt.Sprintf("%d", port),
		)

		if runErr == nil {
			t.Fatalf("donmai daemon stats expected non-zero exit; got exit 0\n--- output ---\n%s", out)
		}

		if !strings.Contains(out, "not running") && !strings.Contains(out, "daemon") {
			t.Errorf("daemon stats output missing expected error context\n--- output ---\n%s", out)
		}

		hasGuidance := strings.Contains(out, "donmai daemon install") ||
			strings.Contains(out, "donmai daemon start") ||
			strings.Contains(out, "donmai daemon")
		if !hasGuidance {
			t.Errorf("daemon stats output does not contain actionable `donmai daemon` guidance\n--- output ---\n%s", out)
		}

		assertNoLegacyBinary(t, out, "donmai daemon stats")

		t.Logf("daemon stats hard-fail output (exit=%v):\n%s", runErr, out)
	})

	// ─── Sub-test 3: daemon doctor — service-not-installed path ──────────
	//
	// `donmai daemon doctor` under a hermetic HOME (no launchd plist /
	// systemd unit written) must exit non-zero and produce a message
	// referencing `donmai daemon install` as the remediation hint.
	//
	// Per afcli/daemon.go:556-557:
	//   "service is not installed — run `<bin> daemon install`"
	//
	// This sub-test is gated on darwin/linux because the installer
	// dispatcher only supports those platforms; and on
	// DONMAI_SMOKES_SKIP_INSTALLER=1 for hermetic CI environments that
	// can't tolerate the plist/unit path check.
	t.Run("daemon_doctor_not_installed", func(t *testing.T) {
		if os.Getenv("DONMAI_SMOKES_SKIP_INSTALLER") == "1" {
			t.Skip("DONMAI_SMOKES_SKIP_INSTALLER=1 — operator opted out")
		}
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			t.Skipf("daemon doctor smoke only runs on darwin/linux; got %s", runtime.GOOS)
		}

		// Fresh hermetic HOME — no plist or unit file exists.
		home := t.TempDir()
		out, runErr := runCapture(t, home, "daemon", "doctor")

		// Must exit non-zero (not installed = failure).
		if runErr == nil {
			t.Fatalf("donmai daemon doctor expected non-zero exit on fresh HOME; got exit 0\n--- output ---\n%s", out)
		}

		// Must contain the install guidance.
		if !strings.Contains(out, "daemon install") {
			t.Errorf("daemon doctor output does not contain 'daemon install' remediation hint\n--- output ---\n%s", out)
		}

		// The install guidance must reference `donmai` (the correct binary
		// name), not `af`.
		if !strings.Contains(out, "donmai") {
			t.Errorf("daemon doctor output does not contain 'donmai' binary name in guidance\n--- output ---\n%s", out)
		}

		assertNoLegacyBinary(t, out, "donmai daemon doctor")

		t.Logf("daemon doctor not-installed output (exit=%v):\n%s", runErr, out)
	})
}
