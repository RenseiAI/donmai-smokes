package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// JSONUnmarshal is a thin wrapper around encoding/json.Unmarshal.
//
// Wrapping the std-lib call keeps imports out of step files (which would
// otherwise need to depend on encoding/json for one parse site each)
// and gives a single place to swap in tolerant decoding later if any
// shape grows more permissive.
func JSONUnmarshal(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	// DryRun, when true, prints the command that would be executed
	// instead of running it. Run helpers return ("", nil) without
	// invoking the binary.
	DryRun bool

	// Verbose, when true, tees subprocess stdout/stderr to the parent
	// process's stdout/stderr and prints an "[exec] <cmd>" line before
	// each invocation.
	Verbose bool

	// Timeout caps the duration of each Run / RunWithInput /
	// RunCaptureBoth invocation. Zero defaults to 60 seconds.
	Timeout time.Duration

	// BinaryOverride, when non-empty, substitutes the configured path
	// for invocations whose program name matches OverrideTarget.
	// Centralising the substitution here is the single-seam approach:
	// every run helper passes argv[0] through resolveBinary, so we only
	// need to add the override to one place to flip the entire harness
	// onto a different binary. Other binaries (gh, git, codesign, …)
	// are intentionally left alone.
	BinaryOverride string

	// BinaryOverrideSource records the provenance of BinaryOverride for
	// log lines. Free-form (e.g. "flag", "env", "build-local").
	BinaryOverrideSource string

	// OverrideTarget is the program name (e.g. "rensei", "af") whose
	// invocations get rewritten to BinaryOverride. Required when
	// BinaryOverride is non-empty.
	OverrideTarget string

	// GlobalFlags, when non-empty AND argv[0] matches OverrideTarget,
	// are inserted between the binary name and the rest of the args.
	// Example: GlobalFlags = []string{"--url", "http://127.0.0.1:3010"}
	// turns Runner.Run("rensei","org","whoami") into
	// "rensei --url http://… org whoami".
	GlobalFlags []string
}

// Runner is a subprocess executor with dry-run, verbose, timeout, and
// binary-override support.
//
// Construct via NewRunner. Use Run / RunWithInput / RunCaptureBoth for
// subprocess invocation. Use WaitFor / Assert / AssertContains for the
// loop-and-check / contract-check patterns.
type Runner struct {
	cfg RunnerConfig
}

// NewRunner returns a Runner configured with cfg.
func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	return &Runner{cfg: cfg}
}

// Config returns a copy of the Runner's config. Useful for callers
// embedding a Runner who need to read e.g. Verbose.
func (r *Runner) Config() RunnerConfig { return r.cfg }

// resolveBinary returns the program name to invoke for a subprocess call.
//
// When the caller asks for OverrideTarget and BinaryOverride is non-empty,
// the override path is returned. For any other binary name the input is
// returned unchanged.
func (r *Runner) resolveBinary(name string) string {
	if r.cfg.OverrideTarget != "" && name == r.cfg.OverrideTarget && r.cfg.BinaryOverride != "" {
		return r.cfg.BinaryOverride
	}
	return name
}

// injectGlobalFlags returns args with global flags injected when the
// leading binary is OverrideTarget and GlobalFlags is non-empty.
func (r *Runner) injectGlobalFlags(args []string) []string {
	if len(args) < 1 || r.cfg.OverrideTarget == "" || args[0] != r.cfg.OverrideTarget {
		return args
	}
	if len(r.cfg.GlobalFlags) == 0 {
		return args
	}
	out := make([]string, 0, len(args)+len(r.cfg.GlobalFlags))
	out = append(out, args[0])
	out = append(out, r.cfg.GlobalFlags...)
	out = append(out, args[1:]...)
	return out
}

// Run executes a subprocess command.
//
// Honours cfg.DryRun (prints the command instead of running it),
// cfg.Verbose (tees subprocess output to os.Stdout/Stderr), and
// cfg.Timeout (context deadline per invocation).
//
// On success the trimmed stdout is returned. On failure a formatted
// error that includes the exit code and last 512 bytes of combined
// output is returned.
func (r *Runner) Run(args ...string) (string, error) {
	return r.RunWithInput(nil, args...)
}

// RunWithInput is like Run but supplies stdin to the subprocess.
func (r *Runner) RunWithInput(stdin io.Reader, args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("Run: no arguments supplied")
	}

	label := strings.Join(args, " ")

	if r.cfg.DryRun {
		_, _ = fmt.Fprintf(os.Stdout, "  [dry-run] would exec: %s\n", label)
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.Timeout)
	defer cancel()

	args = r.injectGlobalFlags(args)
	cmd := exec.CommandContext(ctx, r.resolveBinary(args[0]), args[1:]...) //nolint:gosec
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if r.cfg.Verbose {
		cmd.Stdout = io.MultiWriter(&stdoutBuf, os.Stdout)
		cmd.Stderr = io.MultiWriter(&stderrBuf, os.Stderr)
	} else {
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
	}

	if r.cfg.Verbose {
		_, _ = fmt.Fprintf(os.Stdout, "  [exec] %s\n", label)
	}

	if err := cmd.Run(); err != nil {
		combined := stdoutBuf.String() + stderrBuf.String()
		if len(combined) > 512 {
			combined = "..." + combined[len(combined)-512:]
		}
		return "", fmt.Errorf("exec %q: %w\noutput: %s", label, err, combined)
	}

	return strings.TrimRight(stdoutBuf.String(), "\n"), nil
}

// RunCaptureBoth is like Run but returns combined stdout+stderr output
// alongside the exit error. Both are populated simultaneously — the
// caller gets full output for inspection even when the command exited
// non-zero, and can independently check err for the exit status.
//
// Used by steps that intentionally invoke commands expected to exit
// non-zero (e.g., a doctor command on a fresh machine) and need both
// signals.
func (r *Runner) RunCaptureBoth(args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("RunCaptureBoth: no arguments supplied")
	}

	if r.cfg.DryRun {
		_, _ = fmt.Fprintf(os.Stdout, "  [dry-run] would exec: %s\n", strings.Join(args, " "))
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.Timeout)
	defer cancel()

	args = r.injectGlobalFlags(args)
	cmd := exec.CommandContext(ctx, r.resolveBinary(args[0]), args[1:]...) //nolint:gosec
	var combined bytes.Buffer
	if r.cfg.Verbose {
		cmd.Stdout = io.MultiWriter(&combined, os.Stdout)
		cmd.Stderr = io.MultiWriter(&combined, os.Stderr)
		_, _ = fmt.Fprintf(os.Stdout, "  [exec] %s\n", strings.Join(args, " "))
	} else {
		cmd.Stdout = &combined
		cmd.Stderr = &combined
	}

	err := cmd.Run()
	return strings.TrimRight(combined.String(), "\n"), err
}

// Assert verifies that condition is true, returning a formatted error
// otherwise. Free function (not method) since it carries no Runner
// state.
func Assert(condition bool, format string, args ...any) error {
	if condition {
		return nil
	}
	return fmt.Errorf("assertion failed: "+format, args...)
}

// AssertContains verifies that haystack contains needle. Returns a
// descriptive error including the captured haystack when the assertion
// fails. Free function (not method) since it carries no Runner state.
func AssertContains(haystack, needle, ctx string) error {
	if strings.Contains(haystack, needle) {
		return nil
	}
	return fmt.Errorf("%s: expected output to contain %q\ngot: %s", ctx, needle, haystack)
}

// WaitFor polls fn until it returns nil or until deadline.
//
// Logs each retry when r.cfg.Verbose is enabled. The minimum deadline
// is 1 second; the minimum interval is 200ms (defensive against
// runaway-loop typos in callers).
func (r *Runner) WaitFor(deadline time.Duration, interval time.Duration, fn func() error) error {
	if deadline < time.Second {
		deadline = time.Second
	}
	if interval < 200*time.Millisecond {
		interval = 200 * time.Millisecond
	}

	end := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(end) {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if r.cfg.Verbose {
			_, _ = fmt.Fprintf(os.Stdout, "  [wait] %v; retrying in %s\n", lastErr, interval)
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("timed out after %s: %w", deadline, lastErr)
}

// Logf prints a message when verbose mode is enabled, prefixed with
// "[verbose]". Free function-shaped on Runner so platform-side wrappers
// can re-use it consistently.
func (r *Runner) Logf(format string, args ...any) {
	if r.cfg.Verbose {
		_, _ = fmt.Fprintf(os.Stdout, "  [verbose] "+format+"\n", args...)
	}
}
