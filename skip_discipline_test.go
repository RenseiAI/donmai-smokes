package smokes

// skip_discipline_test.go — a source-level guard that keeps new skips honest.
//
// The liveness gate (liveness_test.go) can only weigh skips that reported
// themselves. A future smoke that calls t.Skip directly is invisible to it,
// and the suite drifts back toward the state this lane found it in: a skip
// path reachable from an ERROR, wearing the clothes of a PRECONDITION.
//
// So the live suite's skips must go through the harness helpers, which record
// as they skip:
//
//	afh.SkipIfShort         -short
//	afh.SkipIfKnob          a documented DONMAI_SMOKES_SKIP_* operator knob
//	afh.SkipIfToolMissing   a required external tool is not installed
//	afh.DeclineLive         the located SUT lacks the surface under test
//
// A raw t.Skip is allowed only with its reason written down in
// allowedRawSkips below. That list is the point of this test: it is cheap to
// extend and impossible to extend by accident, so "why is it legitimate for
// this smoke to report ok without running?" gets answered in review rather
// than discovered months later from a suspiciously fast green run.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// allowedRawSkips maps a substring of the skip's message to the reason that
// raw skip is legitimate. A skip is permitted when its source line contains
// one of these keys.
//
// The bar for an entry: the condition must be a genuine, non-error
// precondition that cannot produce evidence and cannot be retried into
// success — the machine or the platform simply is not one this smoke applies
// to. "The thing I need is missing BECAUSE something went wrong" never
// qualifies; that is a failure.
var allowedRawSkips = map[string]string{
	"only runs on darwin/linux": "the daemon service installer targets launchd and systemd; " +
		"there is no Windows service path to assert against, so no run on Windows can produce evidence.",
	"codex binary not found on PATH": "codex is a third-party CLI this repo deliberately does not " +
		"install (AGENTS.md: no live external API calls by default). Its absence is the documented " +
		"default state, not a fault — and CODEX_BIN set to a bad path FAILS rather than skipping.",
	"subprocess SIGTERM semantics are POSIX-specific": "signal-number assertions have no Windows analogue.",
	"/usr/bin/true unavailable": "a fixed absolute path to a POSIX utility; its absence means the " +
		"platform is not one this spawn test describes.",
	"git not on PATH": "harness-internal fixture-repo helper test; git is the fixture format itself, " +
		"so without it there is no fixture to make. Live smokes use afh.SkipIfToolMissing instead.",
	"and npm not available": "the pinned pi / opencode CLIs are installed via npm on demand; with " +
		"neither the binary nor npm present there is no way to obtain the SUT's harness peer. " +
		"CI installs both explicitly (see .github/workflows/test.yml).",
	"pinned donmai has no viewertest fixture package": "the interactive suite drives a fixture TUI " +
		"that ships inside the pinned donmai module; a pin predating it has no surface to drive. " +
		"A fixture package that IS present but fails to build is a t.Fatal, checked separately.",
}

// skipScanDirs are the packages whose skips must be accounted for: the live
// suite and the harness that gates it. live_gate.go is exempt because it IS
// the recording implementation — its t.Skip calls are the ones every other
// skip is routed through.
var skipScanDirs = []string{".", "harness"}

const skipGateImpl = "live_gate.go"

// skipCallRE matches a t.Skip / t.Skipf / t.SkipNow call.
var skipCallRE = regexp.MustCompile(`\bt\.Skip(f|Now)?\(`)

// TestSkipDisciplineInLiveSuite fails when a test file in this package calls
// t.Skip directly without an entry in allowedRawSkips.
func TestSkipDisciplineInLiveSuite(t *testing.T) {
	var offenders []string
	scanned := 0
	for _, dir := range skipScanDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") {
				continue
			}
			// skip_discipline_test.go names the banned construct in its own
			// allowlist and regex; live_gate.go implements the recording
			// helpers every other skip routes through.
			if name == "skip_discipline_test.go" || name == skipGateImpl {
				continue
			}
			path := filepath.Join(dir, name)
			body, err := os.ReadFile(path) //nolint:gosec // G304: in-repo sources.
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			scanned++
			for i, line := range strings.Split(string(body), "\n") {
				if !skipCallRE.MatchString(line) || isCommentLine(line) {
					continue
				}
				if allowedSkipLine(line) {
					continue
				}
				offenders = append(offenders, formatOffender(path, i+1, line))
			}
		}
	}

	// Guard the guard: a scan that walked no files would pass vacuously —
	// the very failure mode this file exists to prevent.
	if scanned == 0 {
		t.Fatal("skip-discipline scan found no *_test.go files to read; the guard would pass vacuously")
	}

	if len(offenders) > 0 {
		t.Errorf("raw t.Skip in the live suite (%d):\n%s\n\n"+
			"Route it through a harness helper so the skip records itself:\n"+
			"  afh.SkipIfShort / afh.SkipIfKnob   an operator asked for it\n"+
			"  afh.SkipIfToolMissing              a required tool is not installed\n"+
			"  afh.DeclineLive                    the located SUT lacks this surface\n"+
			"If the skip is a genuine precondition none of those cover, add it to\n"+
			"allowedRawSkips in %s with the reason.",
			len(offenders), strings.Join(offenders, "\n"), "skip_discipline_test.go")
	}
	t.Logf("skip discipline: scanned %d test files, %d allowlisted raw-skip reasons", scanned, len(allowedRawSkips))
}

func allowedSkipLine(line string) bool {
	for key := range allowedRawSkips {
		if strings.Contains(line, key) {
			return true
		}
	}
	return false
}

func formatOffender(file string, line int, src string) string {
	return "  " + filepath.Join(".", file) + ":" + itoa(line) + ": " + strings.TrimSpace(src)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestNoSkipDecidedByErrorText bans the exact mechanism that produced the
// silent-skip run: reading an error's prose to decide between t.Skip and
// t.Fatal.
//
//	if strings.Contains(err.Error(), "no such file") { t.Skipf(...) }
//
// It shipped in nine copies across this suite. It mistook a MISSING checkout
// for a legitimate precondition — and would equally have mistaken a real
// donmai compile error for one, since "no such file" appears in plenty of
// genuine build failures. Errors are classified by type and by ORDERING now
// (locate the SUT, then probe it, then build it), never by reading them.
//
// The check is deliberately narrow: matching on an error message is fine and
// common in assertions (`if !strings.Contains(err.Error(), "does not exist")`
// is how harness/build_test.go pins BuildBinary's contract). What is banned is
// a match that GUARDS a skip — so this looks for err.Error() and a t.Skip
// close enough together to be one decision.
func TestNoSkipDecidedByErrorText(t *testing.T) {
	// Widest gap observed in the removed copies was 4 lines (a three-clause
	// condition plus the skip). 6 gives headroom without reaching across
	// unrelated code.
	const window = 6

	var offenders []string
	scanned := 0
	for _, dir := range []string{".", "harness", "interactive", "released"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") {
				continue
			}
			path := filepath.Join(dir, name)
			if path == filepath.Join(".", "skip_discipline_test.go") {
				continue // names the construct in order to forbid it
			}
			body, err := os.ReadFile(path) //nolint:gosec // G304: in-repo sources.
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			scanned++
			lines := strings.Split(string(body), "\n")
			for i, line := range lines {
				if !strings.Contains(line, "err.Error()") || isCommentLine(line) {
					continue
				}
				for j := i; j < len(lines) && j <= i+window; j++ {
					if skipCallRE.MatchString(lines[j]) && !isCommentLine(lines[j]) {
						offenders = append(offenders, formatOffender(path, i+1, line))
						break
					}
				}
			}
		}
	}

	if scanned == 0 {
		t.Fatal("error-text scan read no .go files; the guard would pass vacuously")
	}
	if len(offenders) > 0 {
		t.Errorf("skip decided by error text (%d) — an error is not a precondition:\n%s\n\n"+
			"Order the checks instead: locate the thing, then probe its capability, then use it.\n"+
			"Each step then has one meaning and can fail or decline on its own terms.",
			len(offenders), strings.Join(offenders, "\n"))
	}
	t.Logf("skip-by-error-text guard: scanned %d Go files, 0 offenders", scanned)
}

func isCommentLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "//")
}
