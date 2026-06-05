package smokes

// setup_live_daemon_test.go — shared test helper for donmai-binary smokes
// that need a live `donmai daemon run` process under harness control.
//
// Two callers today:
//   - TestAfDaemonLifecycle (step1) — exercises status / stats and asserts
//     graceful shutdown.
//   - TestAfDaemonCommandSurface (step2) — exercises the four migrated
//     command surfaces (provider/kit/workarea/routing) against the
//     /api/daemon/* HTTP control API.
//
// The helper is a thin in-package wrapper over the canonical
// `harness.LiveDaemonWithConfig` helper. It exists for ergonomic
// reasons — step1/step2 don't need a daemon.yaml (they exercise the
// default-config path) and don't need the daemon's HOME directory back,
// so this wrapper drops both arguments and matches the original Wave
// 10 shape. Tests that need to pre-write daemon.yaml (step4, step5,
// the Wave 12 acceptance smoke) call `harness.LiveDaemonWithConfig`
// directly.

import (
	"testing"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// setupLiveDaemon builds the donmai binary, spawns `donmai daemon run`
// foreground on a free port with isolated HOME (no daemon.yaml), and
// returns once /healthz returns 200.
//
// Skips the test cleanly when the donmai sibling worktree or
// Go toolchain isn't available (so the harness can run standalone for
// CI flag-parsing checks).
//
// The returned donmaiBinary path is absolute. The returned logBuf retains
// the last 64 KiB of daemon stdout+stderr — callers should attach its
// String() to any assertion failure that needs daemon-side context.
//
// For tests that need to pre-write daemon.yaml (project allowlists,
// kit scan paths, trust-mode overrides, etc.), call
// `harness.LiveDaemonWithConfig` directly — this in-package wrapper
// only covers the default-config callers.
func setupLiveDaemon(t *testing.T) (live *afh.LiveDaemon, donmaiBinary string, logBuf *afh.LogTail) {
	t.Helper()
	live, donmaiBinary, logBuf, _ = afh.LiveDaemonWithConfig(t, "")
	return live, donmaiBinary, logBuf
}
