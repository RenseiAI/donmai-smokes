package harness

import "strings"

// ExtractTokenByPrefix scans output line-by-line, returns the first
// whitespace-separated token starting with prefix and longer than
// minLength. Returns "" when no match. Used by smoke tests that need
// to extract a token from CLI output (e.g., rsk_live_* worker tokens
// from `rensei org api-keys create`).
//
// The length comparison is strict (len(field) > minLength), so a token
// whose length equals minLength does NOT match. Pass minLength as the
// length of the prefix itself when callers only care that a non-empty
// suffix follows the prefix.
func ExtractTokenByPrefix(output, prefix string, minLength int) string {
	for _, line := range strings.Split(output, "\n") {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, prefix) && len(field) > minLength {
				return field
			}
		}
	}
	return ""
}
