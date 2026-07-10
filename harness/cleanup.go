package harness

import (
	"fmt"
	"strings"
)

// CleanupHook is one teardown step in a sequence run by RunCleanups.
//
// Implementations should be idempotent: a CleanupHook may be invoked
// multiple times across re-runs (--cleanup-only mode, post-failure
// recovery), and resources may already be absent. Return nil rather
// than an error when the resource is already gone.
type CleanupHook interface {
	// Name returns a human-readable label for the hook (used in
	// aggregated error messages).
	Name() string

	// Run executes the teardown step.
	Run() error
}

// FuncCleanupHook adapts a (name, func() error) pair into a CleanupHook.
//
// Useful for wrapping ad-hoc closures without defining a named type.
type FuncCleanupHook struct {
	HookName string
	Fn       func() error
}

// Name implements CleanupHook.
func (h FuncCleanupHook) Name() string { return h.HookName }

// Run implements CleanupHook.
func (h FuncCleanupHook) Run() error { return h.Fn() }

// RunCleanups runs each hook in order, aggregating non-nil errors into
// a single returned error rather than short-circuiting at the first
// failure. The aggregated error includes the count and each hook's
// error message prefixed with the hook's Name.
//
// This is the canonical pattern for harness teardown: every cleanup
// hook should run regardless of whether earlier hooks failed, so a
// resource leak in one path doesn't hide leaks in others.
func RunCleanups(hooks []CleanupHook) error {
	var errs []string
	for _, h := range hooks {
		if h == nil {
			continue
		}
		if err := h.Run(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cleanup encountered %d error(s):\n  %s",
			len(errs), strings.Join(errs, "\n  "))
	}
	return nil
}

// StopAndUninstallDaemon attempts to stop and uninstall a daemon
// gracefully via the supplied Runner. Errors are logged via Runner.Logf
// rather than propagated — the daemon may already be stopped or
// uninstalled, and we want cleanup hooks to be idempotent.
//
// Mode-aware: when DaemonModeBinary is supplied (legacy standalone
// binary), uninstall routes through legacyUninstallCmd. Otherwise the
// subcommand-based hostUninstallCmd is used.
//
// hostStopCmd:      the argv that issues the stop request, e.g.
//
//	["donmai", "daemon", "stop"].
//
// hostUninstallCmd: the argv for the in-process uninstall, e.g.
//
//	["donmai", "daemon", "uninstall"].
//
// legacyUninstallCmd: the argv for the standalone-binary uninstall, e.g.
//
//	["legacy-daemon", "uninstall"]. May be empty if no
//	legacy binary exists for the daemon under test.
func StopAndUninstallDaemon(r *Runner, mode DaemonMode, hostStopCmd, hostUninstallCmd, legacyUninstallCmd []string) error {
	if r == nil {
		return fmt.Errorf("StopAndUninstallDaemon: Runner is required")
	}

	// Try to stop; ignore error (daemon may already be stopped).
	if len(hostStopCmd) > 0 {
		if _, err := r.Run(hostStopCmd...); err != nil {
			r.Logf("cleanup: daemon stop returned error (may already be stopped): %v", err)
		}
	}

	uninstallCmd := hostUninstallCmd
	if mode == DaemonModeBinary && len(legacyUninstallCmd) > 0 {
		uninstallCmd = legacyUninstallCmd
	}
	if len(uninstallCmd) > 0 {
		if _, err := r.Run(uninstallCmd...); err != nil {
			r.Logf("cleanup: %s returned error (may not be installed): %v",
				strings.Join(uninstallCmd, " "), err)
		}
	}

	return nil
}
