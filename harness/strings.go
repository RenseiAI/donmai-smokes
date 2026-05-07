package harness

import (
	"regexp"
	"strings"
)

// ansiSeq matches the SGR / cursor-control escape sequences a TUI binary
// emits when its output target looks TTY-shaped. The smokes harness
// asserts on plain-text fragments, so every test path that captures
// child-process stdout strips these before comparison.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// StripANSI removes SGR / cursor-control escape sequences from s.
//
// Use before asserting on any captured TUI binary output that may have
// been rendered with colour or cursor-move escape sequences. The smokes
// harness sets NO_COLOR=1 in spawned env (see SpawnOptions / Runner) but
// some libraries emit escape sequences regardless, so this is a defensive
// post-processing step.
func StripANSI(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// ContainsAny returns true if s contains any of the supplied non-empty
// substrings.
//
// Empty needles are ignored (an empty needle would otherwise match
// vacuously). The cost is O(len(s) * len(needles) * max(needle)); for
// the small smoke-test use case this is negligible.
func ContainsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// SafePrefix returns the first n bytes of s, suffixed with the U+2026
// "horizontal ellipsis" character when truncation occurred. If s is no
// longer than n, s is returned unchanged.
//
// Used to log token prefixes (token-format authoring) without leaking
// the full credential. Not constant-time and not suitable for
// security-sensitive comparisons.
func SafePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
