package harness

// live_daemon.go — `LiveDaemonWithConfig` is the canonical helper for
// af-binary smoke tests that need a live `af daemon run` process, with
// the option of pre-writing a `daemon.yaml` under the daemon's isolated
// HOME before spawn.
//
// Wave 12 / Phase 5b (C2 cleanup carryover from Wave 11): step4 +
// step5 each duplicated ~80 lines of build + pickPort + write
// daemon.yaml + spawn + healthz. This helper dedupes that pattern.
// step1 / step2's in-package `setupLiveDaemon` is now a thin wrapper
// over `LiveDaemonWithConfig(t, "")`.
//
// The daemon-yaml path runs through the daemon's regular
// LoadConfig path (the file is read BEFORE the wizard fallback in
// daemon.Start), so callers configure trust mode / kit scan paths /
// project allowlists / orchestrator stubs etc. via daemon.yaml rather
// than via env vars. Keeping the env-var surface small is intentional
// per the Phase 2 audit § 3.2 recommendation.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// LiveDaemonWithConfig builds the donmai binary, optionally writes
// daemon.yaml under <home>/.donmai/daemon.yaml with the given content,
// picks a free port, spawns `donmai daemon run --port <p> --skip-wizard`
// foreground with isolated HOME + DONMAI_DAEMON_FORCE_STUB=1, waits
// for /healthz, and registers t.Cleanup(live.Stop). Returns the
// LiveDaemon, the absolute donmai binary path, the log tail buffer
// (caller should attach String() to assertion failures for
// daemon-side context), and the daemon's isolated HOME directory.
//
// daemonYAML is the YAML body to write at <home>/.donmai/daemon.yaml.
// Empty string skips the daemon.yaml write — equivalent to step1's
// in-package `setupLiveDaemon` shape (the daemon falls through to its
// default-config setup-wizard path, suppressed by --skip-wizard).
//
// extraEnv is appended to the hermetic default env. Pass nothing for
// the default; pass extra `KEY=VALUE` strings for additional vars.
// Most caller-specific configuration should land via the daemonYAML
// body to keep the env-var surface small (per Phase 2 audit § 3.2).
//
// Skips the test cleanly when the donmai sibling worktree or
// the Go toolchain isn't available, so the harness can run standalone
// for CI flag-parsing checks.
func LiveDaemonWithConfig(t *testing.T, daemonYAML string, extraEnv ...string) (
	live *LiveDaemon, donmaiBinary string, logBuf *LogTail, home string,
) {
	t.Helper()

	// Build donmai from the sibling donmai checkout. Cold cache
	// 60-90s; warm sub-second. 3-minute parent context is generous.
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()

	binDir := t.TempDir()
	donmaiBinary, err := BuildDonmaiBinary(buildCtx, BuildOptions{
		OutputPath: filepath.Join(binDir, "donmai"),
		Env:        append(os.Environ(), "GOWORK="),
	})
	if err != nil {
		if strings.Contains(err.Error(), "resolve ../") ||
			strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("live-daemon donmai binary unavailable: %v", err)
		}
		t.Fatalf("build donmai binary: %v", err)
	}

	port, err := PickFreePort()
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}

	home = t.TempDir()

	// Pre-write daemon.yaml when the caller supplied a non-empty body.
	// LoadConfig reads <home>/.donmai/daemon.yaml BEFORE the wizard
	// fallback in daemon.Start. Empty string short-circuits the write
	// so the daemon takes its default-config path (matching step1's
	// shape); --skip-wizard suppresses the interactive prompt.
	if daemonYAML != "" {
		daemonYAMLDir := filepath.Join(home, ".donmai")
		if err := os.MkdirAll(daemonYAMLDir, 0o700); err != nil {
			t.Fatalf("mkdir daemon yaml dir: %v", err)
		}
		daemonYAMLPath := filepath.Join(daemonYAMLDir, "daemon.yaml")
		if err := os.WriteFile(daemonYAMLPath, []byte(daemonYAML), 0o600); err != nil {
			t.Fatalf("write daemon.yaml: %v", err)
		}
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	logBuf = NewLogTail(64 * 1024)

	startCtx, startCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startCancel()

	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"DONMAI_DAEMON_FORCE_STUB=1",
		"DONMAI_STATE_HOME=" + home,
		"NO_COLOR=1",
	}
	if len(extraEnv) > 0 {
		env = append(env, extraEnv...)
	}

	live, err = SpawnDaemon(startCtx, SpawnOptions{
		Binary: donmaiBinary,
		Args: []string{
			"daemon", "run",
			"--port", fmt.Sprintf("%d", port),
			"--skip-wizard",
		},
		Env:            env,
		HomeDir:        home,
		LogSink:        logBuf,
		HealthzBaseURL: url,
		HealthzTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("spawn donmai daemon: %v\n--- daemon log tail ---\n%s", err, logBuf.String())
	}

	// LiveDaemon.Stop is idempotent (Wave 11 Phase 7a — sync.Once-guarded
	// inside the harness package). Tests that exercise the graceful-
	// shutdown path explicitly call live.Stop themselves; this Cleanup
	// can call it again without double-invoking Wait on the same exec.Cmd.
	t.Cleanup(live.Stop)
	t.Logf("donmai daemon up at %s (pid %d, port %d)", live.URL, live.Cmd.Process.Pid, live.Port())

	return live, donmaiBinary, logBuf, home
}
