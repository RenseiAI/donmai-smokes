package harness

// opencode_install.go — resolves (installing if necessary) the pinned
// `opencode` CLI binary for CI smoke coverage.
//
// The opencode harness lane (step18_opencode_harness_test.go) needs a
// real `opencode` binary matching donmai's version pin
// (donmai/provider/harness/opencode.PinnedVersion,
// runs/2026-07-21-open-harness-strategy/07-design-opencode-spawn.md §8)
// so the smoke exercises the actual CLI surface D-1..D-4 harden, not a
// fake stand-in. donmai-smokes never imports donmai as a Go library (it
// only builds and execs the donmai binary), so it has no import path to
// that constant — OpenCodePinnedVersion below is this harness's own copy,
// kept in lockstep by the pin-bump protocol (07 §8's "Upgrade protocol":
// bumping the pin is a PR that updates BOTH the donmai-side constant and
// this one, then re-runs this lane in CI against the new pin).
//
// Resolution order in EnsureOpenCodeBinary:
//
//  1. $DONMAI_SMOKES_OPENCODE_BIN, if set, is used verbatim (operator/CI
//     escape hatch — a runner image that pre-bakes its own copy).
//  2. An `opencode` already on $PATH reporting the pinned version (or
//     newer) is reused, avoiding a redundant install on a developer
//     machine or pre-provisioned runner.
//  3. Otherwise `npm install -g --prefix <isolated dir> opencode-ai@<pin>`
//     installs an isolated copy — NEVER the operator's/runner's real
//     global npm prefix — and returns its binary path.
//
// Skips the calling test cleanly (t.Skipf) when neither an existing
// binary nor npm is available, so the lane degrades gracefully on a
// runner without Node.js instead of failing the whole suite.

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
	// EnvOpenCodeBin points directly at a pre-installed opencode binary,
	// bypassing resolution/installation entirely.
	EnvOpenCodeBin = "DONMAI_SMOKES_OPENCODE_BIN"

	// EnvOpenCodePin overrides the version EnsureOpenCodeBinary installs
	// or accepts from $PATH. Empty (the default) uses OpenCodePinnedVersion.
	EnvOpenCodePin = "DONMAI_SMOKES_OPENCODE_PIN"
)

// OpenCodePinnedVersion mirrors donmai's
// provider/harness/opencode.PinnedVersion as of the last pin-bump PR.
// See the package doc above for how the two stay in lockstep.
const OpenCodePinnedVersion = "1.17.18"

// npmInstallTimeout bounds the install; opencode-ai is a small package but
// a cold npm registry fetch on a loaded CI runner can take a while.
const npmInstallTimeout = 3 * time.Minute

// versionProbeTimeout bounds a single "<binary> --version" invocation.
const versionProbeTimeout = 10 * time.Second

// openCodeVersionRe extracts a dotted X.Y.Z version from free-form
// "--version" output (mirrors donmai's own
// provider/harness/opencode/probe.go extraction, kept independently here
// since donmai-smokes has no import path to that package).
var openCodeVersionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// EnsureOpenCodeBinary resolves a usable opencode CLI binary per the
// resolution order documented above, or calls t.Skipf when none can be
// obtained.
func EnsureOpenCodeBinary(t *testing.T) string {
	t.Helper()

	pin := strings.TrimSpace(os.Getenv(EnvOpenCodePin))
	if pin == "" {
		pin = OpenCodePinnedVersion
	}

	if override := strings.TrimSpace(os.Getenv(EnvOpenCodeBin)); override != "" {
		return override
	}

	if path, err := exec.LookPath("opencode"); err == nil {
		if v, ok := probeOpenCodeVersion(path); ok && compareDottedVersions(v, pin) >= 0 {
			return path
		}
	}

	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("opencode not on PATH (matching or exceeding the pin) and npm not available " +
			"— skipping opencode harness smoke")
	}

	prefix := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), npmInstallTimeout)
	defer cancel()
	// nolint:gosec // G204: npmPath is resolved via exec.LookPath; args are
	// a closed set (fixed flags + a version string this file owns).
	cmd := exec.CommandContext(
		ctx, npmPath,
		"install", "-g", "--no-audit", "--no-fund",
		"--prefix", prefix,
		fmt.Sprintf("opencode-ai@%s", pin),
	)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm install -g --prefix %s opencode-ai@%s: %v\n%s", prefix, pin, err, out)
	}

	installed := filepath.Join(prefix, "bin", "opencode")
	if _, statErr := os.Stat(installed); statErr != nil {
		t.Fatalf("opencode-ai@%s installed but binary not found at %s: %v\n--- npm output ---\n%s",
			pin, installed, statErr, out)
	}
	if v, ok := probeOpenCodeVersion(installed); !ok || v != pin {
		t.Fatalf("installed opencode reports version %q (ok=%v), want exactly %q", v, ok, pin)
	}
	return installed
}

// probeOpenCodeVersion runs "<binary> --version" and extracts the dotted
// version. Returns ok=false on any execution or parse failure.
func probeOpenCodeVersion(binary string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	// nolint:gosec // G204: binary is either the operator-supplied
	// override, a PATH-resolved `opencode`, or the path this file just
	// installed — never externally-controlled input.
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "", false
	}
	m := openCodeVersionRe.FindString(strings.TrimSpace(string(out)))
	return m, m != ""
}

// compareDottedVersions compares two dotted-integer version strings
// (e.g. "1.17.18") component-wise. Returns -1/0/1. Lenient: missing or
// non-numeric components compare as 0 — this is an advisory comparison
// for "is $PATH's opencode good enough to skip installing", not a strict
// semver library.
func compareDottedVersions(a, b string) int {
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
