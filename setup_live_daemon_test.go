package smokes

// setup_live_daemon_test.go — shared test helper for af-binary smokes
// that need a live `af daemon run` process under harness control.
//
// Two callers today:
//   - TestAfDaemonLifecycle (step1) — exercises status / stats and asserts
//     graceful shutdown.
//   - TestAfDaemonCommandSurface (step2) — exercises the four migrated
//     command surfaces (provider/kit/workarea/routing) against the
//     /api/daemon/* HTTP control API.
//
// The helper builds the af binary, picks a free port, spawns the daemon
// with isolated HOME + RENSEI_DAEMON_FORCE_STUB=1 (so the daemon does
// NOT dial the platform on startup), waits for /healthz, and registers
// a t.Cleanup that calls live.Stop on test exit. Returns the LiveDaemon,
// the absolute af binary path, and the log tail buffer so callers can
// attach trailing logs to assertion failures.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/agentfactory-smokes/harness"
)

// setupLiveDaemon builds the af binary, spawns `af daemon run`
// foreground on a free port with isolated HOME, and returns once
// /healthz returns 200.
//
// Skips the test cleanly when the agentfactory-tui sibling worktree or
// Go toolchain isn't available (so the harness can run standalone for
// CI flag-parsing checks).
//
// The returned afBinary path is absolute. The returned logBuf retains
// the last 64 KiB of daemon stdout+stderr — callers should attach its
// String() to any assertion failure that needs daemon-side context.
func setupLiveDaemon(t *testing.T) (live *afh.LiveDaemon, afBinary string, logBuf *afh.LogTail) {
	t.Helper()

	// Build af from the sibling agentfactory-tui checkout. Cold cache
	// 60-90s; warm sub-second. 3-minute parent context is generous.
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()

	binDir := t.TempDir()
	afBinary, err := afh.BuildAfBinary(buildCtx, afh.BuildOptions{
		OutputPath: filepath.Join(binDir, "af"),
		Env:        append(os.Environ(), "GOWORK="),
	})
	if err != nil {
		if strings.Contains(err.Error(), "resolve ../") ||
			strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("live-daemon af binary unavailable: %v", err)
		}
		t.Fatalf("build af binary: %v", err)
	}

	port, err := afh.PickFreePort()
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}

	daemonHome := t.TempDir()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	logBuf = afh.NewLogTail(64 * 1024)

	startCtx, startCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startCancel()

	live, err = afh.SpawnDaemon(startCtx, afh.SpawnOptions{
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

	// Idempotent stop wrapper. Tests that exercise the graceful-shutdown
	// path explicitly (e.g. TestAfDaemonLifecycle's graceful_shutdown
	// subtest) call live.Stop themselves; this Cleanup must not double-
	// invoke Wait on the same exec.Cmd. The boolean guard is plain test-
	// goroutine sequential — no concurrent stop callers expected.
	stopped := false
	originalStop := live.Stop
	live.Stop = func() {
		if stopped {
			return
		}
		stopped = true
		originalStop()
	}
	t.Cleanup(live.Stop)
	t.Logf("af daemon up at %s (pid %d)", live.URL, live.Cmd.Process.Pid)

	return live, afBinary, logBuf
}
