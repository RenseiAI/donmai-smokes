package harness

// sut.go — locating the system under test.
//
// This harness exists to drive one binary: `donmai`, built from a sibling
// donmai checkout. Every live smoke starts by finding that checkout, so how
// it is found decides whether the suite tests anything at all.
//
// The historical resolution was the literal relative path "../donmai",
// hard-coded in BuildDonmaiBinary and copied into five per-step
// *SourceDir() helpers. That path is correct from a primary checkout
// (<root>/donmai-smokes) and WRONG from a linked worktree
// (<root>/donmai-smokes.wt/<name>), where the sibling actually sits at
// ../../donmai. The build then failed with "source dir ... does not exist",
// and the failure was string-matched back into t.Skipf — so the whole live
// suite reported `ok` in ~1.2s while executing nothing. Two red controls
// "passed" against that before anyone noticed the timing tell (a cold donmai
// build alone is 60-90s).
//
// LocateDonmaiSource replaces the fixed relative path with an upward walk
// that positively IDENTIFIES the checkout (module path + cmd/donmai) rather
// than trusting a name, so it resolves from a primary checkout, a linked
// worktree, or hosted CI's two-repo layout without per-call-site special
// cases. When it cannot find the checkout it returns a typed *SUTNotFoundError
// listing everywhere it looked — and callers FAIL on that error instead of
// skipping (see RequireDonmaiSource in liveness.go).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DonmaiSourceDirEnv is the explicit override for the donmai checkout under
// test. Set it to an absolute (or cwd-relative) path when the checkout does
// not sit next to this repo — a container mount, a CI layout that nests the
// two repos differently, or a bisect worktree.
//
// The override is authoritative: when it is set and does not name a donmai
// checkout, resolution FAILS rather than falling back to the walk. An
// operator who names a path has stated an intent; silently testing some other
// checkout because theirs was a typo is the same laundering this file exists
// to remove.
const DonmaiSourceDirEnv = "DONMAI_SMOKES_DONMAI_DIR"

// donmaiModulePath is the module path in the go.mod of a genuine donmai
// checkout. Matching on it (rather than on the directory being named
// "donmai") is what makes the upward walk safe: an empty or stale directory
// with the right name does not satisfy it.
const donmaiModulePath = "github.com/RenseiAI/donmai"

// maxSourceWalkUp bounds the upward search. Three levels covers every layout
// we ship against — primary checkout (<root>/donmai-smokes, 1 level), linked
// worktree (<root>/donmai-smokes.wt/<name>, 2 levels), and hosted CI's
// two-repo checkout (1 level) — with one to spare. Bounding it keeps a
// failure message short and stops the walk wandering to / on a misconfigured
// machine.
const maxSourceWalkUp = 4

// SUTNotFoundError reports that the donmai checkout under test could not be
// located. It carries every candidate that was rejected, and why, so the
// message is actionable without a second run.
type SUTNotFoundError struct {
	// FromEnv is set when DonmaiSourceDirEnv named the (rejected) path.
	FromEnv bool
	// Candidates are the paths considered, in order, each with the reason
	// it was rejected.
	Candidates []SUTCandidate
}

// SUTCandidate is one rejected location plus the reason it did not qualify.
type SUTCandidate struct {
	Path   string
	Reason string
}

func (e *SUTNotFoundError) Error() string {
	var b strings.Builder
	if e.FromEnv {
		b.WriteString("donmai checkout under test not found at $" + DonmaiSourceDirEnv + ":\n")
	} else {
		b.WriteString("donmai checkout under test not found; this suite has nothing to test.\n")
	}
	for _, c := range e.Candidates {
		fmt.Fprintf(&b, "  - %s: %s\n", c.Path, c.Reason)
	}
	b.WriteString("A donmai checkout is a directory whose go.mod declares module " +
		donmaiModulePath + " and which contains cmd/donmai.\n")
	b.WriteString("Fix it with one of:\n")
	b.WriteString("  - clone the sibling:  gh repo clone RenseiAI/donmai <parent-of-this-repo>/donmai\n")
	b.WriteString("  - point at a checkout: " + DonmaiSourceDirEnv + "=/path/to/donmai\n")
	b.WriteString("  - opt out explicitly:  " + SkipLiveDaemonEnv + "=1 (or `go test -short`)\n")
	return b.String()
}

// LocateDonmaiSource returns the absolute path of the donmai checkout under
// test, or a *SUTNotFoundError naming everywhere it looked.
//
// Resolution order:
//
//  1. $DONMAI_SMOKES_DONMAI_DIR, if set. Authoritative — a set-but-invalid
//     value is an error, never a fallback.
//  2. A "donmai" sibling found by walking up from the working directory,
//     validated as a real donmai checkout at each level. This resolves
//     <root>/donmai from <root>/donmai-smokes AND from
//     <root>/donmai-smokes.wt/<worktree>, which the old fixed "../donmai"
//     could not.
func LocateDonmaiSource() (string, error) {
	if v := strings.TrimSpace(os.Getenv(DonmaiSourceDirEnv)); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", &SUTNotFoundError{
				FromEnv:    true,
				Candidates: []SUTCandidate{{Path: v, Reason: "cannot resolve to an absolute path: " + err.Error()}},
			}
		}
		if reason := describeNonDonmaiCheckout(abs); reason != "" {
			return "", &SUTNotFoundError{
				FromEnv:    true,
				Candidates: []SUTCandidate{{Path: abs, Reason: reason}},
			}
		}
		return abs, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory while locating the donmai checkout: %w", err)
	}

	var candidates []SUTCandidate
	dir := cwd
	for i := 0; i < maxSourceWalkUp; i++ {
		candidate := filepath.Join(dir, "donmai")
		if reason := describeNonDonmaiCheckout(candidate); reason == "" {
			return candidate, nil
		} else {
			candidates = append(candidates, SUTCandidate{Path: candidate, Reason: reason})
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", &SUTNotFoundError{Candidates: candidates}
}

// describeNonDonmaiCheckout returns "" when dir is a usable donmai checkout,
// or a short human reason why it is not. Positive identification — the
// directory must declare the donmai module path AND ship cmd/donmai — is what
// lets the upward walk trust its answer; a directory merely NAMED "donmai"
// (an empty placeholder, an unrelated worktree parent like donmai.wt) does
// not qualify.
func describeNonDonmaiCheckout(dir string) string {
	info, err := os.Stat(dir) // Stat, not Lstat: a symlinked sibling is fine.
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "does not exist"
	case err != nil:
		return "not readable: " + err.Error()
	case !info.IsDir():
		return "not a directory"
	}

	goMod := filepath.Join(dir, "go.mod")
	body, err := os.ReadFile(goMod) //nolint:gosec // G304: path derived from the walk above, not from untrusted input.
	if err != nil {
		return "no readable go.mod (" + err.Error() + ")"
	}
	if got := modulePathOf(string(body)); got != donmaiModulePath {
		if got == "" {
			return "go.mod declares no module path"
		}
		return "go.mod declares module " + got + ", want " + donmaiModulePath
	}

	entry := filepath.Join(dir, "cmd", "donmai")
	if info, err := os.Stat(entry); err != nil || !info.IsDir() {
		return "no cmd/donmai package (not a buildable donmai checkout)"
	}
	return ""
}

// modulePathOf extracts the module path from go.mod source, or "" when there
// is no module directive. Deliberately a few lines of parsing rather than a
// golang.org/x/mod dependency: this harness pins its dependency surface to
// the donmai module plus godotenv, and one directive does not justify
// widening it.
func modulePathOf(goMod string) string {
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(line)
		after, ok := strings.CutPrefix(line, "module")
		if !ok {
			continue
		}
		// "module" must be a whole word: "modulepath example.com/x" declares
		// nothing. Requiring the following byte to be space is what separates
		// the directive from any identifier that merely starts with it.
		if after == "" || !isSpaceByte(after[0]) {
			continue
		}
		rest := strings.TrimSpace(after)
		if rest == "" {
			continue
		}
		if i := strings.Index(rest, "//"); i >= 0 {
			rest = strings.TrimSpace(rest[:i])
		}
		return strings.Trim(rest, `"`)
	}
	return ""
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r'
}
