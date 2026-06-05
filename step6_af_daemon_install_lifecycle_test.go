package smokes

// step6_af_daemon_install_lifecycle_test.go — exercises the
// `af daemon install / uninstall` CLI surface with a hermetic HOME so
// the plist (macOS) / unit file (Linux) is written to a temp path
// instead of the operator's real ~/Library/LaunchAgents or
// ~/.config/systemd/user. The hidden `--skip-service-manager` flag
// (Wave 12 Phase 5d, agentfactory-tui afcli/daemon.go) prevents the
// launchctl / systemctl invocation that would otherwise need real
// elevated state.
//
// Per WAVE12_PLAN.md § "Phase 5d" + WAVE12_PHASE2_AUDIT.md § 3.4:
// Wave-10 Q11 carryover. `TestAfDaemonLifecycle` (step1) covers the
// foreground `af daemon run` cycle; this smoke covers the install-side
// CLI surface so a regression that drops a verb, renames the unit
// file, or breaks plist generation fires here.
//
// Hermetic-by-HOME is feasible because both installers resolve the
// service path via `os.UserHomeDir()` (which honours HOME):
//
//   - launchd:  <home>/Library/LaunchAgents/dev.donmai.daemon.plist
//   - systemd:  <home>/.config/systemd/user/rensei-daemon.service
//
// Audit-noted nit: the audit prompt enumerated the Linux unit
// filename as `dev.rensei.daemon.service`, but the actual systemd
// installer's `UnitFilename` (installer/systemd/installer.go:46-49)
// is `rensei-daemon.service`. The smoke uses the actual filename.
//
// Skip-mode: honours `RENSEI_SMOKES_SKIP_INSTALLER=1` + `-short`,
// matching step1-step5's pattern. Also skipped on non-darwin/linux
// since the installer dispatcher only supports those.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestAfDaemonInstallLifecycle exercises `af daemon install` +
// `af daemon uninstall` under a hermetic HOME with the hidden
// `--skip-service-manager` flag set, asserting the unit file is
// written on install and removed on uninstall.
func TestAfDaemonInstallLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("install lifecycle smoke; skipped under -short")
	}
	if os.Getenv("RENSEI_SMOKES_SKIP_INSTALLER") == "1" {
		t.Skip("RENSEI_SMOKES_SKIP_INSTALLER=1 — operator opted out of the install lifecycle smoke")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("install lifecycle smoke only runs on darwin/linux; got %s", runtime.GOOS)
	}

	// Build donmai from the sibling donmai checkout. Cold cache
	// 60-90s; warm sub-second. 3-minute parent context is generous.
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

	// Hermetic HOME — both installers resolve the service file path via
	// os.UserHomeDir() (HOME is checked first), so this redirects the
	// plist / unit-file write into the temp tree.
	fakeHome := t.TempDir()

	// Resolve the OS-specific service file path the installer will
	// write under the hermetic HOME. The constants below are anchored
	// to the installer packages (launchd.LaunchdLabel,
	// systemd.UnitFilename) so a rename surfaces here, not silently.
	var servicePath string
	switch runtime.GOOS {
	case "darwin":
		// installer/launchd/installer.go: LaunchdLabel = "dev.rensei.daemon"
		// PlistPath = $HOME/Library/LaunchAgents/<label>.plist
		servicePath = filepath.Join(fakeHome, "Library", "LaunchAgents", "dev.donmai.daemon.plist")
	case "linux":
		// installer/systemd/installer.go: UnitFilename = "rensei-daemon.service"
		// User-scope dir = $HOME/.config/systemd/user (XDG_CONFIG_HOME unused
		// by the installer — it builds the path from os.UserHomeDir() +
		// ".config/systemd/user" directly per UserUnitDir(), so the env var
		// is set defensively but not load-bearing).
		servicePath = filepath.Join(fakeHome, ".config", "systemd", "user", "rensei-daemon.service")
	}

	// Hermetic env shared by install + uninstall invocations. PATH is
	// minimal; HOME redirects the unit-file write; XDG_CONFIG_HOME is
	// belt-and-suspenders (not consumed by the systemd installer
	// today, but kept aligned with LiveDaemonWithConfig's shape so
	// future changes don't trip the test).
	hermeticEnv := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + fakeHome,
		"XDG_CONFIG_HOME=" + filepath.Join(fakeHome, ".config"),
		"NO_COLOR=1",
	}

	// ─── Install ────────────────────────────────────────────────────────
	{
		runCtx, runCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer runCancel()

		args := []string{"daemon", "install", "--skip-service-manager"}
		// On Linux --user is the systemd installer's default scope;
		// pass it explicitly so the assertion path matches regardless of
		// any future default-flip.
		if runtime.GOOS == "linux" {
			args = append(args, "--user")
		}

		installCmd := exec.CommandContext(runCtx, donmaiBinary, args...) //nolint:gosec // hermetic test
		installCmd.Env = hermeticEnv

		installOut, err := installCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("af daemon install --skip-service-manager failed: %v\n--- output ---\n%s",
				err, installOut)
		}
		t.Logf("install output:\n%s", installOut)

		// Assert the unit file was written at the expected path.
		info, err := os.Stat(servicePath)
		if err != nil {
			t.Fatalf("service file not written at %s: %v\n--- install output ---\n%s",
				servicePath, err, installOut)
		}
		if info.IsDir() {
			t.Fatalf("expected file at %s, got directory", servicePath)
		}

		// Assert the install output references the unit file path so the
		// CLI's success message stays load-bearing (operators expect to
		// see the path).
		if !strings.Contains(string(installOut), servicePath) {
			t.Errorf("install output does not reference service path %q\n--- output ---\n%s",
				servicePath, installOut)
		}

		// Read the unit file content; assert it references the af
		// binary path + the `daemon run` subcommand. This is the
		// REN-1406 contract — the registered service must invoke
		// `<host-binary> daemon run`, not a separate rensei-daemon.
		content, err := os.ReadFile(servicePath) //nolint:gosec // hermetic test path
		if err != nil {
			t.Fatalf("read service file: %v", err)
		}
		body := string(content)
		if !strings.Contains(body, donmaiBinary) {
			t.Errorf("service file does not reference af binary path %q\n--- content ---\n%s",
				donmaiBinary, body)
		}
		// Both plist and unit-file formats reference the literal
		// substring `daemon run` (plist splits to two <string>
		// elements: `<string>daemon</string>\n    <string>run</string>`;
		// systemd writes `ExecStart=<bin> daemon run`). Both contain
		// the substring "daemon" + "run" — assert each individually
		// because the plist's whitespace-and-XML-tag separator means
		// the literal "daemon run" substring is not present.
		if !strings.Contains(body, "daemon") {
			t.Errorf("service file does not reference 'daemon' subcommand\n--- content ---\n%s",
				body)
		}
		if !strings.Contains(body, "run") {
			t.Errorf("service file does not reference 'run' subcommand\n--- content ---\n%s",
				body)
		}
		t.Logf("service file at %s (%d bytes) — references af binary + daemon/run",
			servicePath, info.Size())
	}

	// ─── Uninstall ──────────────────────────────────────────────────────
	{
		runCtx, runCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer runCancel()

		args := []string{"daemon", "uninstall", "--skip-service-manager"}
		if runtime.GOOS == "linux" {
			args = append(args, "--user")
		}

		uninstallCmd := exec.CommandContext(runCtx, donmaiBinary, args...) //nolint:gosec // hermetic test
		uninstallCmd.Env = hermeticEnv

		uninstallOut, err := uninstallCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("af daemon uninstall --skip-service-manager failed: %v\n--- output ---\n%s",
				err, uninstallOut)
		}
		t.Logf("uninstall output:\n%s", uninstallOut)

		// Assert the unit file is removed.
		if _, err := os.Stat(servicePath); !os.IsNotExist(err) {
			t.Errorf("service file still exists at %s after uninstall: %v",
				servicePath, err)
		}

		// Assert the uninstall output references the unit file path
		// (operators rely on this to know where the file lived).
		if !strings.Contains(string(uninstallOut), servicePath) {
			t.Errorf("uninstall output does not reference service path %q\n--- output ---\n%s",
				servicePath, uninstallOut)
		}
	}
}
