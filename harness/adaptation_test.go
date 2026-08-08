package harness

import (
	"strings"
	"testing"
)

// realDenialStderr is the exact stderr line a `donmai agent run` emits when the
// pre-spawn tool/lifecycle compiler denies the session. Copied verbatim from a
// local reproduction so the recognizer is pinned to the real wire text, not to
// a paraphrase of it.
const realDenialStderr = `time=2026-08-08T02:11:43.672-04:00 level=DEBUG msg="worktree provisioned" sessionId=smoke-gateway-completedturn
Error: runner.Run: agent: spawn failed: tool/lifecycle adaptation denied (delivery_unsupported, channel=mcp_server): exact harness profile "pi/headless/tool-lifecycle-v1" cannot apply required entry "mcp-servers"
`

// realDenialResultJSON is the same denial as it appears in the agent-run Result
// JSON on stdout, where every inner quote is backslash-escaped. Both forms must
// parse — a lane concatenates stdout and stderr before asking.
const realDenialResultJSON = `{
  "status": "failed",
  "providerName": "pi",
  "failureMode": "spawn-failed",
  "error": "agent: spawn failed: tool/lifecycle adaptation denied (delivery_unsupported, channel=mcp_server): exact harness profile \"pi/headless/tool-lifecycle-v1\" cannot apply required entry \"mcp-servers\"",
  "sessionId": "smoke-pi-harness-handshake"
}`

func TestFindAdaptationDenial(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		wantFound   bool
		wantCode    string
		wantChannel string
		wantProfile string
		wantEntry   string
	}{
		{
			name:        "real stderr form",
			output:      realDenialStderr,
			wantFound:   true,
			wantCode:    "delivery_unsupported",
			wantChannel: "mcp_server",
			wantProfile: "pi/headless/tool-lifecycle-v1",
			wantEntry:   "mcp-servers",
		},
		{
			name:        "json-escaped result form",
			output:      realDenialResultJSON,
			wantFound:   true,
			wantCode:    "delivery_unsupported",
			wantChannel: "mcp_server",
			wantProfile: "pi/headless/tool-lifecycle-v1",
			wantEntry:   "mcp-servers",
		},
		{
			name:        "stdout and stderr concatenated",
			output:      realDenialResultJSON + "\n" + realDenialStderr,
			wantFound:   true,
			wantCode:    "delivery_unsupported",
			wantChannel: "mcp_server",
			wantProfile: "pi/headless/tool-lifecycle-v1",
			wantEntry:   "mcp-servers",
		},
		{
			name:        "another channel is still recognized",
			output:      `Error: agent: spawn failed: tool/lifecycle adaptation denied (delivery_unsupported, channel=allowed_tools): exact harness profile "ollama/ollama-chat/tool-lifecycle-v1" cannot apply required entry "allowed-tools"`,
			wantFound:   true,
			wantCode:    "delivery_unsupported",
			wantChannel: "allowed_tools",
			wantProfile: "ollama/ollama-chat/tool-lifecycle-v1",
			wantEntry:   "allowed-tools",
		},
		{
			name:        "denial with no channel or profile in scope",
			output:      `Error: tool/lifecycle adaptation denied (delivery_unsupported, channel=): manifest has no tool/lifecycle profile for requested session mode`,
			wantFound:   true,
			wantCode:    "delivery_unsupported",
			wantChannel: "",
			wantProfile: "",
			wantEntry:   "",
		},
		{
			name:      "unrelated spawn failure is not a denial",
			output:    `Error: runner.Run: agent: spawn failed: policy extension failed to load: event stream closed before handshake`,
			wantFound: false,
		},
		{
			name:      "empty output",
			output:    "",
			wantFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, found := FindAdaptationDenial(tc.output)
			if found != tc.wantFound {
				t.Fatalf("FindAdaptationDenial found = %v, want %v (parsed %+v)", found, tc.wantFound, got)
			}
			if !tc.wantFound {
				return
			}
			if got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			if got.Channel != tc.wantChannel {
				t.Errorf("Channel = %q, want %q", got.Channel, tc.wantChannel)
			}
			if got.Profile != tc.wantProfile {
				t.Errorf("Profile = %q, want %q", got.Profile, tc.wantProfile)
			}
			if got.Entry != tc.wantEntry {
				t.Errorf("Entry = %q, want %q", got.Entry, tc.wantEntry)
			}
			if got.Raw == "" {
				t.Error("Raw is empty; the matched denial text must be preserved for the lane's diagnostic")
			}
		})
	}
}

func TestExplainAdaptationDenial(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		wantEmpty   bool
		wantSubstrs []string
		denySubstrs []string
	}{
		{
			name:   "runner-injected mcp gate explains the injection",
			output: realDenialStderr,
			wantSubstrs: []string{
				"PRE-SPAWN ADAPTATION DENIAL",
				"never executed",
				"delivery_unsupported",
				"mcp_server",
				"pi/headless/tool-lifecycle-v1",
				"mcp-servers",
				"defaultMCPServersForProvider",
				"belongs upstream in donmai",
			},
		},
		{
			name:   "other channels get the header without the mcp-injection paragraph",
			output: `tool/lifecycle adaptation denied (delivery_unsupported, channel=allowed_tools): exact harness profile "ollama/ollama-chat/tool-lifecycle-v1" cannot apply required entry "allowed-tools"`,
			wantSubstrs: []string{
				"PRE-SPAWN ADAPTATION DENIAL",
				"allowed_tools",
				"allowed-tools",
			},
			denySubstrs: []string{"defaultMCPServersForProvider"},
		},
		{
			name:        "unparsed profile is reported as unparsed, never as empty",
			output:      `tool/lifecycle adaptation denied (unsupported_contract_version, channel=): got "donmai.tool-lifecycle/v0"`,
			wantSubstrs: []string{"unsupported_contract_version", "(not reported)"},
		},
		{
			name:      "no denial yields no diagnostic",
			output:    `Error: runner.Run: agent: spawn failed: policy extension failed to load`,
			wantEmpty: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExplainAdaptationDenial(tc.output)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("ExplainAdaptationDenial = %q, want an empty string for non-denial output", got)
				}
				return
			}
			if got == "" {
				t.Fatal("ExplainAdaptationDenial returned an empty string for a real denial")
			}
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostic is missing %q; got:\n%s", want, got)
				}
			}
			for _, unwanted := range tc.denySubstrs {
				if strings.Contains(got, unwanted) {
					t.Errorf("diagnostic wrongly contains %q for this channel; got:\n%s", unwanted, got)
				}
			}
		})
	}
}
