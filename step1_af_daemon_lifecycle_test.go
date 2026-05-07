package smokes

// step1_af_daemon_lifecycle_test.go — foreground daemon lifecycle smoke
// for the `af daemon run` entry point.
//
// Per Wave 10 Phase 10 dispatch + Q6 resolution, this smoke covers ONLY
// the foreground spawn path:
//
//   build af → spawn `af daemon run` foreground → wait for /healthz →
//   exercise daemon status / daemon stats → SIGTERM → wait for clean exit
//
// Service-unit install (`af daemon install` → launchd / systemd path) is
// deferred to a follow-up wave. The af binary today (v0.7.0) advertises
// install/uninstall verbs but the smoke harness has no privileged-test
// runner to actually load the unit and verify lifecycle through it.
//
// `af daemon --help` (v0.7.0) advertises the following verbs (Step 10.0
// discovery, 2026-05-07):
//
//   doctor, drain, evict, install, logs, pause, resume, run, set, setup,
//   stats, status, stop, uninstall, update
//
// This test exercises run/status/stats and asserts SIGTERM produces a
// graceful exit. drain/pause/stop verbs are NOT exercised here — they
// require a running orchestrator to be meaningful (drain blocks on
// in-flight work, pause toggles polling state). Adding them to this
// smoke is a follow-up once the OSS daemon stub-mode supports a
// no-orchestrator drain path.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	afh "github.com/RenseiAI/agentfactory-smokes/harness"
)

// TestAfDaemonLifecycle exercises the foreground daemon spawn / status /
// stats / SIGTERM cycle against a freshly-built af binary.
//
// Skipped under -short and when RENSEI_SMOKES_SKIP_LIVE_DAEMON=1 is set
// (matching the rensei-smokes step11 pattern).
func TestAfDaemonLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end live-daemon test; skipped under -short")
	}
	if os.Getenv("RENSEI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("RENSEI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of the live-daemon smoke")
	}

	live, afBinary, logBuf := setupLiveDaemon(t)

	// The daemon's bind port is encoded in live.URL as
	// http://127.0.0.1:<port>. We need it both as a flag and to verify
	// release after SIGTERM.
	port := portFromURL(t, live.URL)
	hostFlag := "--host=127.0.0.1"
	portFlag := fmt.Sprintf("--port=%d", port)

	// Status check — must exit 0 and surface uptime / lifecycle content.
	t.Run("status", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := afh.RunHermeticAgainstDaemon(ctx, afh.HermeticRunOptions{
			Binary:          afBinary,
			Args:            []string{"daemon", "status", hostFlag, portFlag},
			HomeDir:         t.TempDir(),
			DaemonURLEnvVar: "RENSEI_DAEMON_URL",
			DaemonURL:       live.URL,
		})
		if err != nil {
			t.Fatalf("af daemon status failed: %v\n--- output ---\n%s\n--- daemon log tail ---\n%s",
				err, out, logBuf.String())
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("af daemon status produced empty output\n--- daemon log tail ---\n%s",
				logBuf.String())
		}
		afh.AssertOutputContainsAny(t, out, []string{
			"uptime",
			"Uptime",
			"running",
			"Running",
			"lifecycle",
			"Lifecycle",
			"version",
			"Version",
		})
		afh.AssertOutputDoesNotContain(t, out, []string{
			"connection refused",
			"404 not found",
			"/v1/",
		})
	})

	t.Run("stats", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := afh.RunHermeticAgainstDaemon(ctx, afh.HermeticRunOptions{
			Binary:          afBinary,
			Args:            []string{"daemon", "stats", hostFlag, portFlag},
			HomeDir:         t.TempDir(),
			DaemonURLEnvVar: "RENSEI_DAEMON_URL",
			DaemonURL:       live.URL,
		})
		if err != nil {
			t.Fatalf("af daemon stats failed: %v\n--- output ---\n%s\n--- daemon log tail ---\n%s",
				err, out, logBuf.String())
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("af daemon stats produced empty output\n--- daemon log tail ---\n%s",
				logBuf.String())
		}
		afh.AssertOutputContainsAny(t, out, []string{
			"capacity",
			"Capacity",
			"sessions",
			"Sessions",
			"queue",
			"Queue",
			"machines",
			"Machines",
		})
		afh.AssertOutputDoesNotContain(t, out, []string{
			"connection refused",
			"404 not found",
			"/v1/",
		})
	})

	// Graceful shutdown — SIGTERM the daemon directly and confirm the
	// process exits within StopGraceTimeout (default 5s) and the bind
	// port is released. Runs as the final subtest because it tears
	// down the daemon; subsequent subtests would have nothing to talk
	// to.
	t.Run("graceful_shutdown", func(t *testing.T) {
		// Capture the pid before we send the signal; if Stop kills it
		// faster than we can read live.Cmd.Process.Pid we'd race.
		pid := live.Cmd.Process.Pid

		stopStart := time.Now()
		live.Stop()
		stopElapsed := time.Since(stopStart)

		// Stop returns once the process is reaped (graceful or SIGKILL'd).
		// 6s is the default 5s grace + 1s of cushion for the harness.
		if stopElapsed > 6*time.Second {
			t.Errorf("daemon graceful shutdown took %s; expected <= 6s\n--- daemon log tail ---\n%s",
				stopElapsed, logBuf.String())
		}

		// Confirm the pid is no longer alive. signal(0) on a reaped pid
		// returns ESRCH on macOS / Linux.
		if err := syscall.Kill(pid, 0); err == nil {
			t.Errorf("daemon pid %d still alive after Stop()", pid)
		} else if !errors.Is(err, syscall.ESRCH) {
			t.Logf("daemon pid %d post-stop signal: %v (expected non-nil)", pid, err)
		}

		// Confirm the bind port is released — a fresh listener on the
		// same port must succeed. Brief retry loop because TCP TIME_WAIT
		// can hold the port briefly even after the daemon's listener
		// closed.
		portReleased := false
		for i := 0; i < 10; i++ {
			if probePortFree(port) {
				portReleased = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !portReleased {
			t.Errorf("daemon port %d still bound after Stop()", port)
		}
	})
}

// portFromURL parses the port out of a base URL of the form
// "http://host:port". Used to pluck the daemon's bind port out of the
// LiveDaemon.URL string for --port= flag construction and post-shutdown
// release verification.
func portFromURL(t *testing.T, url string) int {
	t.Helper()
	host := strings.TrimPrefix(url, "http://")
	host = strings.TrimPrefix(host, "https://")
	idx := strings.LastIndex(host, ":")
	if idx < 0 {
		t.Fatalf("portFromURL: no port in %q", url)
	}
	portStr := host[idx+1:]
	// Strip any trailing path component the URL might carry.
	if slash := strings.Index(portStr, "/"); slash >= 0 {
		portStr = portStr[:slash]
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 {
		t.Fatalf("portFromURL: parse %q: %v", portStr, err)
	}
	return port
}

// probePortFree attempts a brief Listen/Close on 127.0.0.1:<port> to
// confirm the port is free. Used by the graceful_shutdown subtest to
// verify the daemon released its bind on exit.
func probePortFree(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
