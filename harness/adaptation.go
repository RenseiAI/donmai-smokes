package harness

// adaptation.go — recognizes donmai's PRE-SPAWN tool/lifecycle adaptation
// denial in captured `donmai agent run` output.
//
// # Why this exists
//
// donmai compiles an exact tool/MCP/lifecycle contract against the selected
// harness's manifest profile BEFORE any provider process starts. When a
// required entry has no delivery boundary on that profile, the run fails with:
//
//	agent: spawn failed: tool/lifecycle adaptation denied (delivery_unsupported,
//	channel=mcp_server): exact harness profile "pi/headless/tool-lifecycle-v1"
//	cannot apply required entry "mcp-servers"
//
// The harness binary is never executed on that path. Every lane in this suite
// that drives a real harness child, though, carries protocol/handshake/version
// asserts written for the case where the child DID run — so a pre-spawn denial
// surfaces through them as "the handshake did not complete" or "the upstream
// served zero requests", which names the wrong subsystem and sends the reader
// hunting a protocol regression that is not there.
//
// These helpers are the shared, unit-tested recognizer both lanes consult
// first, so a denial reports itself as a denial. They assert nothing and
// weaken nothing: a lane still fails, it just fails naming the real cause.
//
// Kept in the harness package (this suite's sanctioned sharing point) rather
// than duplicated per step, because the denial format is a single donmai
// contract and a second copy would drift.

import (
	"fmt"
	"regexp"
	"strings"
)

// AdaptationDenial is one parsed pre-spawn tool/lifecycle denial.
//
// Code/Channel/Profile/Entry mirror donmai's ToolAdaptationError taxonomy.
// Profile and Entry are empty when the denial fired before a profile was
// selected (for example an unsupported contract version), which is still a
// pre-spawn denial and still worth reporting as one.
type AdaptationDenial struct {
	// Code is the typed denial code, e.g. "delivery_unsupported".
	Code string
	// Channel is the operational input axis, e.g. "mcp_server". Empty for
	// denials raised before a channel is in scope.
	Channel string
	// Profile is the exact harness profile id the denial cites.
	Profile string
	// Entry is the required entry the profile could not apply.
	Entry string
	// Raw is the matched denial text, verbatim.
	Raw string
}

var (
	// adaptationDenialRe matches donmai's ToolAdaptationError.Error() prefix.
	adaptationDenialRe = regexp.MustCompile(`tool/lifecycle adaptation denied \(([^,)]*)(?:, channel=([^)]*))?\): ([^\n"]*(?:"[^"]*"[^\n"]*)*)`)
	// adaptationEntryRe matches the detail the exact-profile denial carries.
	adaptationEntryRe = regexp.MustCompile(`exact harness profile \\?"([^"\\]*)\\?" cannot apply required entry \\?"([^"\\]*)\\?"`)
)

// FindAdaptationDenial reports the first pre-spawn tool/lifecycle denial in
// captured agent-run output (stdout, stderr, or both concatenated). The
// Result JSON escapes its quotes, so both the raw stderr form and the
// JSON-escaped form are recognized.
func FindAdaptationDenial(output string) (AdaptationDenial, bool) {
	m := adaptationDenialRe.FindStringSubmatch(output)
	if m == nil {
		return AdaptationDenial{}, false
	}
	d := AdaptationDenial{
		Code:    strings.TrimSpace(m[1]),
		Channel: strings.TrimSpace(m[2]),
		Raw:     strings.TrimSpace(m[0]),
	}
	if detail := adaptationEntryRe.FindStringSubmatch(m[3]); detail != nil {
		d.Profile = detail[1]
		d.Entry = detail[2]
	}
	return d, true
}

// ExplainAdaptationDenial returns a lane-ready diagnostic for the first
// pre-spawn denial in the captured output, or "" when there is none.
//
// Call it before a lane's protocol/handshake/turn asserts so a session that
// died in the pre-spawn compiler is reported as exactly that. It is a pure
// string function: it never decides pass/fail.
func ExplainAdaptationDenial(output string) string {
	d, ok := FindAdaptationDenial(output)
	if !ok {
		return ""
	}

	var b strings.Builder
	b.WriteString("PRE-SPAWN ADAPTATION DENIAL — donmai rejected the compiled agent.Spec before any\n")
	b.WriteString("harness process started. The harness binary was never executed, so this is NOT a\n")
	b.WriteString("harness protocol, handshake, model, or binary-version failure.\n\n")
	fmt.Fprintf(&b, "  denial code : %s\n", orUnparsed(d.Code))
	fmt.Fprintf(&b, "  channel     : %s\n", orUnparsed(d.Channel))
	fmt.Fprintf(&b, "  profile     : %s\n", orUnparsed(d.Profile))
	fmt.Fprintf(&b, "  entry       : %s\n", orUnparsed(d.Entry))

	if d.Channel == "mcp_server" && d.Entry == "mcp-servers" {
		b.WriteString("\nThis lane requests NO MCP servers of its own. The runner injects the per-session\n")
		b.WriteString("platform MCP gate itself (runner/loop.go defaultMCPServersForProvider) whenever the\n")
		b.WriteString("session detail carries platformUrl + authToken. The exact harness profile above\n")
		b.WriteString("declares its MCP delivery unsupported, so that runner-owned default is compiled as a\n")
		b.WriteString("REQUIRED entry with no delivery boundary and denied. Net effect: a harness that\n")
		b.WriteString("cannot deliver MCP can no longer spawn in a platform-connected session at all.\n\n")
		b.WriteString("The fix belongs upstream in donmai, not in this suite: the runner must not inject its\n")
		b.WriteString("own MCP default into a harness whose profile declares no MCP delivery. An MCP server\n")
		b.WriteString("the caller actually asked for must still deny, loudly, exactly as it does now.\n")
	}

	return b.String()
}

// orUnparsed renders an absent capture without pretending it was empty.
func orUnparsed(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(not reported)"
	}
	return value
}
