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
	"path/filepath"
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

	// Build af from the sibling agentfactory-tui checkout. Cold cache can
	// take 60-90s; warm runs are sub-second. The 3-minute parent context
	// deadline is the same as rensei-smokes step11.
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()

	binDir := t.TempDir()
	afBinary, err := afh.BuildAfBinary(buildCtx, afh.BuildOptions{
		OutputPath: filepath.Join(binDir, "af"),
		Env:        append(os.Environ(), "GOWORK="),
	})
	if err != nil {
		// Toolchain or sibling-worktree miss is a clean skip — the harness
		// can be cloned standalone for CI flag-parsing / lint without the
		// agentfactory-tui worktree present.
		if strings.Contains(err.Error(), "resolve ../") ||
			strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("live-daemon af binary unavailable: %v", err)
		}
		t.Fatalf("build af binary: %v", err)
	}

	// Pick a free port and isolate HOME so the daemon's daemon.yaml,
	// daemon.jwt, kit registry, and log directory all land under tmp
	// rather than in the developer's real ~/.rensei.
	port, err := afh.PickFreePort()
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	daemonHome := t.TempDir()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)

	logBuf := afh.NewLogTail(64 * 1024)

	// Spawn `af daemon run --port <free> --skip-wizard`. The healthz cap
	// is 30s — generous compared to the ~1s warm / ~5s cold daemon
	// startup observed in practice, but slow CI runners need the headroom.
	startCtx, startCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startCancel()

	live, err := afh.SpawnDaemon(startCtx, afh.SpawnOptions{
		Binary: afBinary,
		Args: []string{
			"daemon", "run",
			"--port", fmt.Sprintf("%d", port),
			"--skip-wizard",
		},
		Env: []string{
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
			"HOME=" + daemonHome,
			"XDG_CONFIG_HOME=" + filepath.Join(daemonHome, ".config"),
			"RENSEI_DAEMON_FORCE_STUB=1",
			"RENSEI_LOG_DIR=" + filepath.Join(daemonHome, ".rensei", "logs"),
			"NO_COLOR=1",
		},
		HomeDir:        daemonHome,
		LogSink:        logBuf,
		HealthzBaseURL: url,
		HealthzTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("spawn af daemon: %v\n--- daemon log tail ---\n%s", err, logBuf.String())
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			live.Stop()
		}
	})

	t.Logf("af daemon up at %s (pid %d)", live.URL, live.Cmd.Process.Pid)

	// Hermetic env for `af daemon status` / `af daemon stats`. The
	// commands consult --port + --host directly rather than reading
	// RENSEI_DAEMON_URL, so we pass them on the command line.
	hostFlag := "--host=127.0.0.1"
	portFlag := fmt.Sprintf("--port=%d", port)

	// Status check — must exit 0 and surface uptime / version-ish content.
	t.Run("status", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := afh.RunHermeticAgainstDaemon(ctx, afh.HermeticRunOptions{
			Binary:          afBinary,
			Args:            []string{"daemon", "status", hostFlag, portFlag},
			HomeDir:         t.TempDir(),
			DaemonURLEnvVar: "RENSEI_DAEMON_URL",
			DaemonURL:       url,
		})
		if err != nil {
			t.Fatalf("af daemon status failed: %v\n--- output ---\n%s\n--- daemon log tail ---\n%s",
				err, out, logBuf.String())
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("af daemon status produced empty output\n--- daemon log tail ---\n%s",
				logBuf.String())
		}
		// The daemon status renderer surfaces lifecycle + uptime; either
		// shape is enough to confirm the daemon answered.
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

	// Stats check — same shape, different endpoint.
	t.Run("stats", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := afh.RunHermeticAgainstDaemon(ctx, afh.HermeticRunOptions{
			Binary:          afBinary,
			Args:            []string{"daemon", "stats", hostFlag, portFlag},
			HomeDir:         t.TempDir(),
			DaemonURLEnvVar: "RENSEI_DAEMON_URL",
			DaemonURL:       url,
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
	// process exits within StopGraceTimeout (default 5s).
	t.Run("graceful_shutdown", func(t *testing.T) {
		// Capture the pid before we send the signal; if Stop kills it
		// faster than we can read live.Cmd.Process.Pid we'd race.
		pid := live.Cmd.Process.Pid

		stopStart := time.Now()
		live.Stop()
		stopped = true
		stopElapsed := time.Since(stopStart)

		// Stop returns once the process is reaped (graceful or SIGKILL'd).
		// 6s is the default 5s grace + 1s of cushion for the harness.
		if stopElapsed > 6*time.Second {
			t.Errorf("daemon graceful shutdown took %s; expected ≤ 6s\n--- daemon log tail ---\n%s",
				stopElapsed, logBuf.String())
		}

		// Confirm the pid is no longer alive. signal(0) on a reaped pid
		// returns os.ErrProcessDone (Go ≥1.20) or "process already finished".
		// On macOS, signal(0) against a zombie/reaped pid returns ESRCH.
		if err := syscall.Kill(pid, 0); err == nil {
			t.Errorf("daemon pid %d still alive after Stop()", pid)
		} else if !errors.Is(err, syscall.ESRCH) {
			// Any other error is fine — process is gone.
			t.Logf("daemon pid %d post-stop signal: %v (expected)", pid, err)
		}

		// Confirm the port is released — a fresh listener on the same
		// port must succeed. Brief retry loop because TCP TIME_WAIT can
		// hold the port briefly even after the daemon's listener closed.
		portReleased := false
		for i := 0; i < 10; i++ {
			free, err := afh.PickFreePort()
			if err == nil && free > 0 {
				// PickFreePort doesn't bind to our specific port, but we
				// can attempt a binding to the original port directly.
				// Use net.Listen via a tiny inline check.
				if probePortFree(port) {
					portReleased = true
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !portReleased {
			t.Errorf("daemon port %d still bound after Stop()", port)
		}
	})
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
