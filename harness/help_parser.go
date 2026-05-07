package harness

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// ParseHelpSubcommands runs `<binary> <args...> --help` with a hermetic
// env and returns the parsed (subcommand, short-description) map from
// the "Available Commands" section, the full captured help output, and
// any exec error.
//
// Returns an empty map when no Available Commands section is present —
// useful for leaf commands that have no children.
//
// The hermetic env is intentionally minimal — PATH stripped to system,
// HOME pointed at os.TempDir, NO_COLOR=1, plus two empty WORKOS_TEST_*
// assignments to suppress godotenv noise on the rensei side. Add custom
// env via a wrapper if a binary needs platform-specific assignments.
//
// Used by:
//   - rensei-smokes' help-mirror regression guard
//     (TestRenseiHelpMirrorsAfForMigratedSurfaces) which compares the
//     parsed maps from both rensei and af binaries.
//   - agentfactory-smokes' (Phase 10) TestAfHelpDeprecationGuard which
//     pins the af binary's help surface against a hard-coded baseline.
func ParseHelpSubcommands(ctx context.Context, binary string, args ...string) (map[string]string, string, error) {
	helpArgs := append([]string{}, args...)
	helpArgs = append(helpArgs, "--help")

	cmd := exec.CommandContext(ctx, binary, helpArgs...) //nolint:gosec
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + os.TempDir(),
		"NO_COLOR=1",
		// Suppress godotenv noise / preserve hermeticity. These are
		// no-ops for binaries that don't read them.
		"WORKOS_TEST_EMAIL=",
		"WORKOS_TEST_PASSWORD=",
	}
	rawOut, err := cmd.CombinedOutput()
	out := StripANSI(string(rawOut))
	if err != nil {
		return nil, out, fmt.Errorf("run %s %s --help: %w\n%s", binary, strings.Join(args, " "), err, out)
	}

	subs := map[string]string{}
	inSection := false
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if !inSection {
			if trimmed == "Available Commands:" {
				inSection = true
			}
			continue
		}
		// Section ends at the first blank line OR the "Flags:" line.
		if trimmed == "" || strings.HasPrefix(trimmed, "Flags:") || strings.HasPrefix(trimmed, "Global Flags:") {
			break
		}
		// Subcommand rows are "  <name>      <description>" — two-space
		// leading indent. Skip rows that don't fit (defensive).
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		row := strings.TrimLeft(line, " ")
		// Cobra's help formatter pads name and description with multiple
		// spaces. Use Fields then re-join all but the first as the
		// description so multi-word descriptions survive.
		fields := strings.Fields(row)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		desc := ""
		if len(fields) > 1 {
			desc = strings.Join(fields[1:], " ")
		}
		subs[name] = desc
	}
	return subs, out, nil
}

// SortedKeys returns a sorted slice of map keys. Used by help-output
// diff sites for deterministic failing-test messages.
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
