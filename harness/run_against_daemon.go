package harness

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
)

// HermeticRunOptions configures RunHermeticAgainstDaemon.
type HermeticRunOptions struct {
	// Binary is the absolute path of the executable to invoke.
	// Required.
	Binary string

	// Args are the command-line arguments passed after the binary.
	// Required.
	Args []string

	// HomeDir is the isolated HOME directory the subprocess should
	// read/write under. Used as cmd.Dir and propagated as HOME +
	// XDG_CONFIG_HOME via env. Required.
	HomeDir string

	// DaemonURLEnvVar is the environment variable name the binary reads
	// to discover the daemon's base URL (e.g. "RENSEI_DAEMON_URL" for
	// rensei, "AF_DAEMON_URL" for the af binary). Required.
	DaemonURLEnvVar string

	// DaemonURL is the daemon's base URL (e.g. http://127.0.0.1:7734)
	// the binary should target for /api/daemon/* requests. Required.
	DaemonURL string

	// ExtraEnv is appended after the default hermetic env (PATH, HOME,
	// XDG_CONFIG_HOME, the DaemonURL pair, NO_COLOR=1). Useful for
	// unsetting platform credentials with empty assignments
	// (WORKOS_TEST_EMAIL=, etc.) so the subprocess doesn't accidentally
	// pick them up from the operator's shell.
	ExtraEnv []string
}

// RunHermeticAgainstDaemon invokes the configured binary with a hermetic
// env that points it at a live daemon (HermeticRunOptions.DaemonURL) and
// returns the combined stdout+stderr (ANSI-stripped) plus the exit error.
//
// The hermetic env is:
//
//	PATH=/usr/bin:/bin:/usr/sbin:/sbin
//	HOME=<HomeDir>
//	XDG_CONFIG_HOME=<HomeDir>/.config
//	<DaemonURLEnvVar>=<DaemonURL>
//	NO_COLOR=1
//	<ExtraEnv...>
//
// The point of the hermetic shape: the subprocess can't accidentally pick
// up ambient credentials, can't write to the operator's real home, and
// can't render colour codes that would interfere with assertion-on-output
// patterns. The output is ANSI-stripped via StripANSI before return so
// callers don't have to do it themselves.
func RunHermeticAgainstDaemon(ctx context.Context, opts HermeticRunOptions) (string, error) {
	if opts.Binary == "" {
		return "", fmt.Errorf("RunHermeticAgainstDaemon: Binary is required")
	}
	if opts.HomeDir == "" {
		return "", fmt.Errorf("RunHermeticAgainstDaemon: HomeDir is required")
	}
	if opts.DaemonURLEnvVar == "" {
		return "", fmt.Errorf("RunHermeticAgainstDaemon: DaemonURLEnvVar is required")
	}
	if opts.DaemonURL == "" {
		return "", fmt.Errorf("RunHermeticAgainstDaemon: DaemonURL is required")
	}

	cmd := exec.CommandContext(ctx, opts.Binary, opts.Args...) //nolint:gosec
	cmd.Dir = opts.HomeDir
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + opts.HomeDir,
		"XDG_CONFIG_HOME=" + filepath.Join(opts.HomeDir, ".config"),
		opts.DaemonURLEnvVar + "=" + opts.DaemonURL,
		"NO_COLOR=1",
	}
	env = append(env, opts.ExtraEnv...)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	return StripANSI(string(out)), err
}
