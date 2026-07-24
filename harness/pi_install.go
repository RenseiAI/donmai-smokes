package harness

// pi_install.go — resolves (installing if necessary) the pinned `pi` CLI
// binary (@earendil-works/pi-coding-agent) for CI smoke coverage.
//
// Mirrors opencode_install.go's pattern exactly (09-design-pi-adapter.md §8
// / 12-work-breakdown.md W2b's own follow-up note): donmai-smokes never
// imports donmai as a Go library, so it has no import path to donmai's
// provider/harness/pi.PinnedVersion — PiPinnedVersion below is this
// harness's own copy, kept in lockstep by the same pin-bump protocol
// (07 §8's "Upgrade protocol", applied identically to pi per 09 §8's
// version-churn note).
//
// Resolution order in EnsurePiBinary:
//
//  1. $DONMAI_SMOKES_PI_BIN, if set, is used verbatim (operator/CI escape
//     hatch — a runner image that pre-bakes its own copy).
//  2. A `pi` already on $PATH reporting the pinned version (or newer) is
//     reused, avoiding a redundant install on a developer machine or
//     pre-provisioned runner.
//  3. Otherwise `npm install -g --prefix <isolated dir>
//     @earendil-works/pi-coding-agent@<pin>` installs an isolated copy —
//     NEVER the operator's/runner's real global npm prefix — and returns
//     its binary path.
//
// Skips the calling test cleanly (t.Skipf) when neither an existing binary
// nor npm is available, so the lane degrades gracefully on a runner
// without Node.js instead of failing the whole suite.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// EnvPiBin points directly at a pre-installed pi binary, bypassing
	// resolution/installation entirely.
	EnvPiBin = "DONMAI_SMOKES_PI_BIN"

	// EnvPiPin overrides the version EnsurePiBinary installs or accepts
	// from $PATH. Empty (the default) uses PiPinnedVersion.
	EnvPiPin = "DONMAI_SMOKES_PI_PIN"
)

// PiPinnedVersion mirrors donmai's provider/harness/pi.PinnedVersion as of
// the last pin-bump PR (09-design-pi-adapter.md §8 research-time pin,
// carried forward — see the package doc above for how the two stay in
// lockstep).
const PiPinnedVersion = "0.80.10"

// piNpmPackage is the pinned npm package name.
const piNpmPackage = "@earendil-works/pi-coding-agent"

// piNpmInstallTimeout bounds the install; a cold npm registry fetch on a
// loaded CI runner can take a while.
const piNpmInstallTimeout = 3 * time.Minute

// piVersionProbeTimeout bounds a single "<binary> --version" invocation.
const piVersionProbeTimeout = 10 * time.Second

// piVersionRe extracts a dotted X.Y.Z version from free-form "--version"
// output (mirrors donmai's own provider/harness/pi/probe.go extraction,
// kept independently here since donmai-smokes has no import path to that
// package).
var piVersionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// EnsurePiBinary resolves a usable pi CLI binary per the resolution order
// documented above, or calls t.Skipf when none can be obtained.
func EnsurePiBinary(t *testing.T) string {
	t.Helper()

	pin := strings.TrimSpace(os.Getenv(EnvPiPin))
	if pin == "" {
		pin = PiPinnedVersion
	}

	if override := strings.TrimSpace(os.Getenv(EnvPiBin)); override != "" {
		return override
	}

	if path, err := exec.LookPath("pi"); err == nil {
		if v, ok := probePiVersion(path); ok && comparePiDottedVersions(v, pin) >= 0 {
			return path
		}
	}

	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("pi not on PATH (matching or exceeding the pin) and npm not available " +
			"— skipping pi harness smoke")
	}

	prefix := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), piNpmInstallTimeout)
	defer cancel()
	// nolint:gosec // G204: npmPath is resolved via exec.LookPath; args are a
	// closed set (fixed flags + a version string this file owns).
	cmd := exec.CommandContext(
		ctx, npmPath,
		"install", "-g", "--no-audit", "--no-fund",
		"--prefix", prefix,
		fmt.Sprintf("%s@%s", piNpmPackage, pin),
	)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm install -g --prefix %s %s@%s: %v\n%s", prefix, piNpmPackage, pin, err, out)
	}

	installed := filepath.Join(prefix, "bin", "pi")
	if _, statErr := os.Stat(installed); statErr != nil {
		t.Fatalf("%s@%s installed but binary not found at %s: %v\n--- npm output ---\n%s",
			piNpmPackage, pin, installed, statErr, out)
	}
	if v, ok := probePiVersion(installed); !ok || v != pin {
		t.Fatalf("installed pi reports version %q (ok=%v), want exactly %q", v, ok, pin)
	}
	return installed
}

// probePiVersion runs "<binary> --version" and extracts the dotted
// version. Returns ok=false on any execution or parse failure.
func probePiVersion(binary string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), piVersionProbeTimeout)
	defer cancel()
	// nolint:gosec // G204: binary is either the operator-supplied override,
	// a PATH-resolved `pi`, or the path this file just installed — never
	// externally-controlled input.
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "", false
	}
	m := piVersionRe.FindString(strings.TrimSpace(string(out)))
	return m, m != ""
}

// comparePiDottedVersions compares two dotted-integer version strings
// (e.g. "0.80.10") component-wise. Returns -1/0/1. Lenient: missing or
// non-numeric components compare as 0 — this is an advisory comparison for
// "is $PATH's pi good enough to skip installing", not a strict semver
// library.
func comparePiDottedVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}
