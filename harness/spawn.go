package harness

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// SpawnOptions configures SpawnDaemon.
type SpawnOptions struct {
	// Binary is the absolute path of the daemon-bearing executable
	// (e.g. the result of BuildBinary on agentfactory-tui's ./cmd/af).
	// Required.
	Binary string

	// Args are the daemon-run arguments passed after the binary.
	// Required (typically []string{"daemon", "run", "--port", "<n>",
	// "--skip-wizard"}).
	Args []string

	// Env is the environment passed to the spawned process. If nil,
	// SpawnDaemon supplies a hermetic default scoped to HomeDir.
	// Callers wanting custom env (e.g. RENSEI_DAEMON_FORCE_STUB=1)
	// should build their own slice and pass it here.
	Env []string

	// HomeDir is the isolated HOME the daemon should read/write under.
	// Used when Env is nil to construct the default hermetic env. Also
	// used to scope XDG_CONFIG_HOME and the daemon's log directory.
	// Typical value is t.TempDir().
	HomeDir string

	// LogSink, when non-nil, receives the daemon's combined stdout+stderr.
	// Pass a *LogTail to retain the trailing N bytes for failure-mode
	// diagnostics. If nil, daemon output is discarded — useful when the
	// caller only cares about /healthz reaching 200.
	LogSink io.Writer

	// HealthzBaseURL is the base URL SpawnDaemon polls /healthz against.
	// Required (typically "http://127.0.0.1:<port>" using PickFreePort).
	HealthzBaseURL string

	// HealthzTimeout caps how long SpawnDaemon waits for /healthz to
	// return 200 before declaring the daemon dead. Zero defaults to 30s.
	HealthzTimeout time.Duration

	// StopGraceTimeout caps how long the returned LiveDaemon.Stop waits
	// after SIGTERM before sending SIGKILL. Zero defaults to 5s.
	StopGraceTimeout time.Duration
}

// LiveDaemon wraps a spawned daemon child process under harness control.
// Callers MUST call Stop (typically via defer) to clean up the subprocess.
type LiveDaemon struct {
	// Cmd is the exec.Cmd handle for the running daemon process.
	Cmd *exec.Cmd

	// URL is the base URL the daemon is bound to (matches
	// SpawnOptions.HealthzBaseURL).
	URL string

	// Stop sends SIGTERM and waits up to StopGraceTimeout for a graceful
	// exit, then SIGKILLs if the process is still running.
	Stop func()
}

// SpawnDaemon spawns a daemon child process per the supplied SpawnOptions
// and returns a LiveDaemon once /healthz returns 200.
//
// The default hermetic env (when SpawnOptions.Env is nil) sets:
//
//	PATH=/usr/bin:/bin:/usr/sbin:/sbin
//	HOME=<HomeDir>
//	XDG_CONFIG_HOME=<HomeDir>/.config
//	NO_COLOR=1
//
// Callers needing custom env (RENSEI_DAEMON_FORCE_STUB=1, RENSEI_LOG_DIR=…,
// or any other daemon-specific knob) must construct the slice themselves
// and pass it via Env.
//
// On healthz timeout, SpawnDaemon stops the spawned process and returns
// an error that includes the daemon's log tail if a *LogTail was supplied
// as LogSink.
func SpawnDaemon(ctx context.Context, opts SpawnOptions) (*LiveDaemon, error) {
	if opts.Binary == "" {
		return nil, fmt.Errorf("SpawnDaemon: Binary is required")
	}
	if len(opts.Args) == 0 {
		return nil, fmt.Errorf("SpawnDaemon: Args is required")
	}
	if opts.HealthzBaseURL == "" {
		return nil, fmt.Errorf("SpawnDaemon: HealthzBaseURL is required")
	}

	healthzTimeout := opts.HealthzTimeout
	if healthzTimeout <= 0 {
		healthzTimeout = 30 * time.Second
	}
	stopGrace := opts.StopGraceTimeout
	if stopGrace <= 0 {
		stopGrace = 5 * time.Second
	}

	// Use a plain exec.Command (not CommandContext) so SIGTERM goes
	// through the daemon's graceful drain path rather than a hard kill
	// from context cancellation. The healthz timeout below is the only
	// deadline applied to startup.
	cmd := exec.Command(opts.Binary, opts.Args...) //nolint:gosec

	if opts.Env != nil {
		cmd.Env = opts.Env
	} else if opts.HomeDir != "" {
		cmd.Env = []string{
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
			"HOME=" + opts.HomeDir,
			"XDG_CONFIG_HOME=" + filepath.Join(opts.HomeDir, ".config"),
			"NO_COLOR=1",
		}
	}

	if opts.LogSink != nil {
		cmd.Stdout = opts.LogSink
		cmd.Stderr = opts.LogSink
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daemon binary %s: %w", opts.Binary, err)
	}

	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			// graceful exit
		case <-time.After(stopGrace):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
	}

	healthCtx, healthCancel := context.WithTimeout(ctx, healthzTimeout)
	defer healthCancel()
	if err := WaitForDaemonHealthz(healthCtx, opts.HealthzBaseURL); err != nil {
		stop()
		return nil, fmt.Errorf("daemon /healthz never returned 200: %w", err)
	}

	return &LiveDaemon{
		Cmd:  cmd,
		URL:  opts.HealthzBaseURL,
		Stop: stop,
	}, nil
}
