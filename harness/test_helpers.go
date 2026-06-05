package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FakeBinarySubcommandFixture configures WriteFakeBinaryAdvertisingSubcommand.
//
// The generated shell script is purely defensive POSIX (printf, no
// /bin/cat dep) so it runs from a restricted PATH that contains only
// the script's parent directory.
type FakeBinarySubcommandFixture struct {
	// BinaryName is the basename of the script written to Dir
	// (e.g. "rensei", "donmai"). The fixture writes the file at
	// filepath.Join(Dir, BinaryName) with mode 0o755.
	BinaryName string

	// SubcommandPath is the chain of subcommand args the fixture will
	// respond to with the help-output below (e.g. ["host","run"] for
	// rensei or ["daemon","run"] for donmai). When the script is invoked
	// with these args followed by --help, it prints the configured
	// help body and exits 0.
	SubcommandPath []string

	// SubcommandPresent toggles which help-output shape the script
	// emits when probed. When true, the script emits a Usage line
	// containing UsageMarker (the canonical "subcommand present" shape).
	// When false, the script emits the parent-help fallback (older
	// builds where the subcommand isn't yet wired).
	SubcommandPresent bool

	// UsageMarker is the substring DaemonAvailable looks for in the
	// help output to confirm the subcommand is present. The fixture
	// embeds this in the generated script's Usage line when
	// SubcommandPresent is true (e.g. "rensei host run [", "donmai daemon run [").
	UsageMarker string
}

// AssertOutputContainsAny fails the test if out does not contain at
// least one of the supplied fragments.
//
// Used as the OR-style assertion for daemon-targeted commands where
// one of several output shapes is acceptable (e.g. "agent-runtime"
// section header OR "claude" provider name OR the empty-state
// "No providers registered" line).
func AssertOutputContainsAny(t *testing.T, out string, fragments []string) {
	t.Helper()
	for _, frag := range fragments {
		if strings.Contains(out, frag) {
			return
		}
	}
	t.Errorf("output did not contain any of %v\n--- output ---\n%s", fragments, out)
}

// AssertOutputDoesNotContain fails the test if out contains any of the
// supplied forbidden fragments.
//
// Used as the negative-list regression guard for daemon-targeted
// commands (the rendered output must not look like the pre-Wave-9
// mis-routing symptoms — "404 not found", "session auth required",
// "/v1/" path references).
func AssertOutputDoesNotContain(t *testing.T, out string, forbidden []string) {
	t.Helper()
	lc := strings.ToLower(out)
	for _, frag := range forbidden {
		if strings.Contains(lc, strings.ToLower(frag)) {
			t.Errorf("output contains forbidden fragment %q\n--- output ---\n%s", frag, out)
		}
	}
}

// WriteFakeBinaryAdvertisingSubcommand writes a shell script at
// dir/<fixture.BinaryName> that responds to the configured subcommand
// probe with either the "subcommand present" or "subcommand absent"
// help-output shape. Used by daemon-detection tests to fixture a
// realistic CLI without depending on the actual binary being installed.
//
// The script always exits 1 for any invocation other than the configured
// subcommand probe — so a test that accidentally exercises a different
// args path against this fixture sees a clear non-zero exit rather than
// a silent zero.
//
// The script uses pure POSIX builtins (printf, no /bin/cat dependency)
// so it runs from a PATH stripped to the script's parent dir.
func WriteFakeBinaryAdvertisingSubcommand(t *testing.T, dir string, fixture FakeBinarySubcommandFixture) {
	t.Helper()
	if fixture.BinaryName == "" {
		t.Fatalf("WriteFakeBinaryAdvertisingSubcommand: BinaryName is required")
	}
	if len(fixture.SubcommandPath) == 0 {
		t.Fatalf("WriteFakeBinaryAdvertisingSubcommand: SubcommandPath is required")
	}

	var body string
	if fixture.SubcommandPresent {
		// Subcommand-present shape: Usage line contains the marker.
		body = "Start the long-running process.\\n" +
			"\\n" +
			"Usage:\\n" +
			"  " + fixture.UsageMarker + "flags]\\n" +
			"\\n" +
			"Flags:\\n" +
			"  -h, --help   help for run\\n"
	} else {
		// Subcommand-absent shape: cobra falls through to parent help.
		// We use a Usage line that does NOT contain the marker.
		parent := strings.Join(fixture.SubcommandPath[:len(fixture.SubcommandPath)-1], " ")
		body = "Manage the local daemon process.\\n" +
			"\\n" +
			"Usage:\\n" +
			"  " + fixture.BinaryName + " " + parent + " [command]\\n" +
			"\\n" +
			"Available Commands:\\n" +
			"  doctor      Run health checks\\n"
	}

	// Build the if-clause that matches the subcommand path. We compare
	// $1, $2, ... against the path elements followed by --help.
	var matchClause strings.Builder
	matchClause.WriteString("[ ")
	for i, p := range fixture.SubcommandPath {
		if i > 0 {
			matchClause.WriteString(" ] && [ ")
		}
		matchClause.WriteString(`"$` + intToStr(i+1) + `" = "` + p + `"`)
	}
	matchClause.WriteString(` ] && [ "$` + intToStr(len(fixture.SubcommandPath)+1) + `" = "--help" ]`)

	script := "#!/bin/sh\n" +
		"if " + matchClause.String() + "; then\n" +
		`  printf '` + body + `'` + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"

	path := filepath.Join(dir, fixture.BinaryName)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", fixture.BinaryName, err)
	}
}

// intToStr converts a small non-negative int to its decimal string.
// Used by WriteFakeBinaryAdvertisingSubcommand to build $1, $2, …
// shell variable references; pulled out as a tiny helper to avoid
// pulling in strconv just for this.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
