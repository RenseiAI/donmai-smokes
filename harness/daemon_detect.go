package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DaemonMode describes how a daemon runtime is reachable from the harness.
type DaemonMode string

const (
	// DaemonModeSubcommand indicates that the configured Binary's
	// subcommand probe (`<binary> <subcommand> --help`) exits 0 AND
	// advertises the subcommand by name in its Usage line — the
	// canonical "binary present" shape for the OSS single-binary UX
	// where the daemon is a subcommand of the top-level CLI.
	DaemonModeSubcommand DaemonMode = "subcommand"

	// DaemonModeBinary indicates that a separate legacy binary
	// (DaemonProbeOptions.LegacyBinary) is on PATH — the older shape
	// where the daemon ships as its own executable rather than a
	// subcommand of the main CLI.
	DaemonModeBinary DaemonMode = "binary"

	// DaemonModeAbsent indicates neither the subcommand nor the legacy
	// binary is reachable. Detect-mode steps log and pass.
	DaemonModeAbsent DaemonMode = "absent"
)

// DaemonProbeTimeout caps how long DaemonAvailable spends probing for the
// daemon subcommand. Short on purpose: a probe that takes more than a few
// seconds likely indicates a hang, in which case we should fall through to
// the LookPath check rather than block the whole smoke run.
const DaemonProbeTimeout = 5 * time.Second

// DaemonProbeOptions parameterises DaemonAvailable so the same probe shape
// applies to any daemon-bearing CLI: rensei (rensei host run + legacy
// rensei-daemon), donmai (donmai daemon run + no legacy binary), or any
// future fork.
//
// The probe semantics:
//
//  1. If Binary is on PATH and `Binary <SubcommandPath...> --help` exits 0
//     AND prints UsageMarker in its output, the subcommand mode is used.
//  2. If LegacyBinary is non-empty AND on PATH, the binary mode is used.
//  3. Otherwise the daemon is absent.
type DaemonProbeOptions struct {
	// Binary is the top-level CLI to probe (e.g. "rensei", "donmai").
	// Empty values are treated as "no binary" — the probe falls through.
	Binary string

	// SubcommandPath is the chain of subcommands that exposes the daemon.
	// For rensei this is []string{"host", "run"}; for af it is
	// []string{"daemon", "run"}.
	SubcommandPath []string

	// UsageMarker is the substring DaemonAvailable looks for in the
	// `<binary> <subcommand...> --help` output to confirm the subcommand
	// is present. For rensei this is "rensei host run [" — the trailing
	// "[" disambiguates the Usage line ("Usage:\n  rensei host run [flags]")
	// from a help blurb that happens to mention the command. For af it is
	// "donmai daemon run [".
	//
	// Older CLI versions where the subcommand is absent fall through to
	// the parent help (Usage: "<binary> <parent> [command]") which does
	// not match the marker, so we cleanly detect non-presence.
	UsageMarker string

	// LegacyBinary is the standalone executable name to probe via
	// exec.LookPath when the subcommand probe fails. Empty disables the
	// legacy fallback (e.g. af has no legacy daemon binary, only the
	// subcommand).
	LegacyBinary string
}

// DaemonAvailable reports whether a daemon runtime is reachable, and which
// shape it is — the subcommand-of-CLI form or the legacy standalone-binary
// form. See DaemonProbeOptions for the parameter semantics.
//
// Detection order:
//
//  1. `<Binary> <SubcommandPath...> --help` — if it exits 0 AND advertises
//     the subcommand by UsageMarker, subcommand mode is used.
//  2. exec.LookPath(LegacyBinary) — if non-empty, fall back to the legacy
//     standalone binary check.
//  3. Otherwise — absent.
//
// The returned DaemonMode is suitable for inclusion in detect-mode log
// lines (see DaemonModeLog).
func DaemonAvailable(opts DaemonProbeOptions) (bool, DaemonMode) {
	if subcommandAvailable(opts) {
		return true, DaemonModeSubcommand
	}
	if opts.LegacyBinary != "" {
		if _, err := exec.LookPath(opts.LegacyBinary); err == nil {
			return true, DaemonModeBinary
		}
	}
	return false, DaemonModeAbsent
}

// subcommandAvailable returns true iff `<Binary> <SubcommandPath...> --help`
// exits 0 and the output contains opts.UsageMarker.
//
// We deliberately do not error on non-zero exit codes (older builds may
// fail with "unknown command" rather than print parent help on some Cobra
// versions); those cases also return false and let the caller fall back to
// the LookPath check on LegacyBinary.
func subcommandAvailable(opts DaemonProbeOptions) bool {
	if opts.Binary == "" || opts.UsageMarker == "" {
		return false
	}
	if _, err := exec.LookPath(opts.Binary); err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), DaemonProbeTimeout)
	defer cancel()

	args := append([]string{}, opts.SubcommandPath...)
	args = append(args, "--help")

	out, err := exec.CommandContext(ctx, opts.Binary, args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return false
	}
	return strings.Contains(string(out), opts.UsageMarker)
}

// DaemonModeLog returns a one-line summary suitable for the detect-mode
// log header in each step. The phrase varies by mode:
//
//	subcommand → "daemon: subcommand (<binary> <subcommand> available)"
//	binary     → "daemon: binary (<legacyBinary> on PATH)"
//	absent     → "daemon: absent (no <binary> <subcommand>, no <legacyBinary>)"
//
// Pass the same DaemonProbeOptions used for DaemonAvailable so the log
// labels match the actual probe shape.
func DaemonModeLog(m DaemonMode, opts DaemonProbeOptions) string {
	subcommandLabel := strings.TrimSpace(opts.Binary + " " + strings.Join(opts.SubcommandPath, " "))
	switch m {
	case DaemonModeSubcommand:
		return fmt.Sprintf("daemon: subcommand (%s available)", subcommandLabel)
	case DaemonModeBinary:
		return fmt.Sprintf("daemon: binary (%s on PATH)", opts.LegacyBinary)
	default:
		legacyLabel := opts.LegacyBinary
		if legacyLabel == "" {
			legacyLabel = "no legacy binary"
		}
		return fmt.Sprintf("daemon: absent (no %s, no %s)", subcommandLabel, legacyLabel)
	}
}
