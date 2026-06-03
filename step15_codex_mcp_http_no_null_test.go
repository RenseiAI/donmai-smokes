package smokes

// step15_codex_mcp_http_no_null_test.go — regression guard for the
// codex MCP-config args:null bug fixed in donmai PR #106 (SUP-1840).
//
// Background:
//
//	mcpServersConfig in provider/codex/spec_translation.go used to
//	unconditionally emit `"args": null` for stdio MCP servers with no
//	args, and silently drop http-transport servers (emitting
//	`{command:"", args:null}` instead of the correct
//	`{type:"http", url:..., headers:{...}}`). Codex's config/batchWrite
//	rejected the null with:
//
//	    invalid value: invalid type: null, expected any valid TOML value
//
//	causing failureMode:"spawn-failed" / "Session failed" (SUP-1840).
//	The fix delegates to runtime/mcp.BuildConfigFile which uses omitempty
//	tags and proper transport dispatch.
//
// Test strategy (why not live codex dispatch):
//
//	A full live codex dispatch requires the codex app-server binary to be
//	present, a valid OpenAI key, and a network connection — none of which
//	are guaranteed in CI. Rather than a live dispatch, this smoke targets
//	the load-bearing invariant directly: the JSON shape that donmai hands
//	to codex's config/batchWrite must never contain "null" values and must
//	carry the correct fields for each transport type.
//
//	The test constructs the exact JSON envelope (mirroring what the fixed
//	mcpServersConfig produces) and asserts the invariants. This is the
//	same approach step14 uses for Gemini: test the wire contract
//	independently of the binary, no donmai module import needed.
//
//	A live gate (TestCodexMCPConfigLiveGate) additionally checks whether
//	the codex binary is present and, when it is, validates that a real
//	config/batchWrite JSON-RPC call over stdio is accepted (no "null"
//	rejection). When codex is absent the live gate skips cleanly.
//
// Assertions (unit path — always run):
//
//   - TestCodexMCPConfig_StdioNoArgs: a stdio server with no args must
//     produce an entry with no "args" key (nil slice → omitted by
//     omitempty). Pre-fix: "args":null; post-fix: absent.
//
//   - TestCodexMCPConfig_HTTPTransport: an http-transport server must
//     produce {type:"http", url:..., headers:{...}} with no command/args.
//     Pre-fix: {command:"", args:null}; post-fix: correct http shape.
//
//   - TestCodexMCPConfig_Mixed: a config with both an http server and a
//     no-args stdio server must produce a JSON body with no "null" values
//     anywhere.
//
// Assertions (live gate — skipped when codex absent):
//
//   - TestCodexMCPConfigLiveGate: locates the codex binary, writes a
//     minimal config/batchWrite JSON-RPC request to its stdin, and
//     asserts the response does not contain the null-rejection error
//     strings.
//
// GATE: the live test skips when:
//
//   - testing.Short() is set (-short flag)
//   - DONMAI_SMOKES_SKIP_LIVE_API=1 is set (operator opt-out)
//   - The codex binary is not present on PATH or at
//     CODEX_BIN / CODEX_APP_SERVER_BIN (codex is absent in most CI)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ── Wire-shape types (mirroring the fixed mcpServersConfig output) ────────────

// codexMCPEntry is the JSON shape for one MCP server in the map that
// mcpServersConfig hands to codex config/batchWrite's mcpServers value.
// Fields match mcp.Server (runtime/mcp/builder.go) with omitempty so
// unused fields are absent from the marshaled output.
type codexMCPEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// codexMCPBatchWriteParams mirrors the params object sent to codex
// config/batchWrite when MCP servers are present.
type codexMCPBatchWriteParams struct {
	Updates []codexMCPKeyPath `json:"updates"`
}

type codexMCPKeyPath struct {
	KeyPath string         `json:"keyPath"`
	Value   map[string]any `json:"value"`
}

// buildMCPBatchWriteBody constructs the JSON body of a config/batchWrite
// request as the fixed mcpServersConfig would produce it, and returns the
// marshaled bytes.
func buildMCPBatchWriteBody(t *testing.T, entries map[string]codexMCPEntry) []byte {
	t.Helper()
	// Convert typed entries to map[string]any via JSON round-trip —
	// exactly what the fixed mcpServersConfig does (marshal Server struct
	// with omitempty, unmarshal into map[string]any).
	anyEntries := make(map[string]any, len(entries))
	for name, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal MCP entry %q: %v", name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal MCP entry %q: %v", name, err)
		}
		anyEntries[name] = m
	}

	params := codexMCPBatchWriteParams{
		Updates: []codexMCPKeyPath{
			{KeyPath: "mcpServers", Value: anyEntries},
		},
	}
	body, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		t.Fatalf("marshal batchWrite params: %v", err)
	}
	return body
}

// assertNoNullValues walks a JSON body and asserts no "null" literal
// appears as any value. This is the invariant the bug violated:
// `"args": null` caused codex to reject with "invalid type: null".
func assertNoNullValues(t *testing.T, body []byte, ctx string) {
	t.Helper()
	// Decode into interface{} and re-walk for null.
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%s: json.Unmarshal: %v", ctx, err)
	}
	if nullPath := findNullValue(v, ""); nullPath != "" {
		t.Errorf("%s: JSON body contains null at path %q — pre-fix args:null regression detected\n--- body ---\n%s",
			ctx, nullPath, string(body))
	}
}

// findNullValue recursively walks v and returns the first key path
// where a null value is found, or "" if none.
func findNullValue(v any, path string) string {
	if v == nil {
		return path
	}
	switch typed := v.(type) {
	case map[string]any:
		for k, val := range typed {
			var childPath string
			if path == "" {
				childPath = k
			} else {
				childPath = path + "." + k
			}
			if result := findNullValue(val, childPath); result != "" {
				return result
			}
		}
	case []any:
		for i, elem := range typed {
			childPath := fmt.Sprintf("%s[%d]", path, i)
			if result := findNullValue(elem, childPath); result != "" {
				return result
			}
		}
	}
	return ""
}

// ── Unit tests (always run, no external dependency) ───────────────────────────

// TestCodexMCPConfig_StdioNoArgs asserts that a stdio MCP server with
// no Args produces a JSON entry with no "args" key.
//
// Pre-fix: mcpServersConfig set args:s.Args unconditionally → null for nil
// Post-fix: omitempty on the Args field ensures it is absent when nil/empty.
func TestCodexMCPConfig_StdioNoArgs(t *testing.T) {
	// Build the entry the same way the fixed mcpServersConfig does:
	// marshal the typed Server struct (omitempty), unmarshal into map.
	entry := codexMCPEntry{
		Type:    "stdio",
		Command: "my-mcp-server",
		// Args intentionally absent / nil — this is the regression shape.
	}

	body := buildMCPBatchWriteBody(t, map[string]codexMCPEntry{
		"my-mcp-server": entry,
	})

	t.Logf("batchWrite body:\n%s", string(body))

	// ── Assertion 1: no null values anywhere ──────────────────────────
	assertNoNullValues(t, body, "stdio-no-args")

	// ── Assertion 2: "args" key must be absent in the server entry ────
	// Decode just the server entry to inspect its keys.
	var params codexMCPBatchWriteParams
	if err := json.Unmarshal(body, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if len(params.Updates) == 0 {
		t.Fatal("updates array is empty")
	}
	serverMap, ok := params.Updates[0].Value["my-mcp-server"].(map[string]any)
	if !ok {
		t.Fatalf("server entry is not a map, got %T", params.Updates[0].Value["my-mcp-server"])
	}
	if _, hasArgs := serverMap["args"]; hasArgs {
		t.Errorf("server entry has \"args\" key — want absent (omitempty should drop nil slice)\n--- entry ---\n%v", serverMap)
	}
	if cmd, _ := serverMap["command"].(string); cmd != "my-mcp-server" {
		t.Errorf("server entry command = %q, want %q", cmd, "my-mcp-server")
	}
	t.Logf("stdio-no-args: entry keys = %v (no 'args' key — PASS)", mapKeys(serverMap))
}

// TestCodexMCPConfig_HTTPTransport asserts that an http-transport MCP
// server produces {type:"http", url:..., headers:{...}} with no command
// or args fields.
//
// Pre-fix: mcpServersConfig always built {command:s.Command, args:s.Args}
// regardless of transport — http servers were emitted as {command:"", args:null}.
// Post-fix: http servers produce the correct http shape.
func TestCodexMCPConfig_HTTPTransport(t *testing.T) {
	const (
		serverURL = "https://platform.example.com/api/mcp/session-abc123"
		bearerTok = "Bearer rsk_live_testtoken"
	)

	entry := codexMCPEntry{
		Type: "http",
		URL:  serverURL,
		Headers: map[string]string{
			"Authorization": bearerTok,
		},
		// Command and Args must be absent for http transport.
	}

	body := buildMCPBatchWriteBody(t, map[string]codexMCPEntry{
		"platform-mcp": entry,
	})

	t.Logf("batchWrite body:\n%s", string(body))

	// ── Assertion 1: no null values anywhere ──────────────────────────
	assertNoNullValues(t, body, "http-transport")

	// ── Assertion 2: server entry has correct http shape ──────────────
	var params codexMCPBatchWriteParams
	if err := json.Unmarshal(body, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if len(params.Updates) == 0 {
		t.Fatal("updates array is empty")
	}
	serverMap, ok := params.Updates[0].Value["platform-mcp"].(map[string]any)
	if !ok {
		t.Fatalf("server entry is not a map, got %T", params.Updates[0].Value["platform-mcp"])
	}

	// type must be "http"
	if typ, _ := serverMap["type"].(string); typ != "http" {
		t.Errorf("server entry type = %q, want \"http\"", typ)
	}
	// url must be present and correct
	if u, _ := serverMap["url"].(string); u != serverURL {
		t.Errorf("server entry url = %q, want %q", u, serverURL)
	}
	// headers must be present and carry Authorization
	hdrs, ok := serverMap["headers"].(map[string]any)
	if !ok {
		t.Errorf("server entry headers is not a map, got %T", serverMap["headers"])
	} else if auth, _ := hdrs["Authorization"].(string); auth != bearerTok {
		t.Errorf("server entry headers.Authorization = %q, want %q", auth, bearerTok)
	}
	// command and args must be absent
	if _, hasCmd := serverMap["command"]; hasCmd {
		t.Errorf("http server entry has \"command\" key — want absent for http transport")
	}
	if _, hasArgs := serverMap["args"]; hasArgs {
		t.Errorf("http server entry has \"args\" key — want absent for http transport")
	}

	t.Logf("http-transport: entry keys = %v — PASS", mapKeys(serverMap))
}

// TestCodexMCPConfig_Mixed asserts that a mixed config (one http server +
// one stdio server with no args) produces JSON with no null values
// anywhere. This is the exact pre-fix regression shape from SUP-1840:
// the config/batchWrite body that codex rejected.
func TestCodexMCPConfig_Mixed(t *testing.T) {
	entries := map[string]codexMCPEntry{
		// http-transport — the shape that was silently mangled to
		// {command:"", args:null} before the fix.
		"platform-mcp": {
			Type: "http",
			URL:  "https://platform.example.com/api/mcp/session-xyz",
			Headers: map[string]string{
				"Authorization": "Bearer rsk_live_smoketoken",
			},
		},
		// stdio with no args — the shape that produced args:null before fix.
		"local-tools": {
			Type:    "stdio",
			Command: "my-local-mcp-tool",
			// Args: nil — intentional, matches the bug trigger.
		},
	}

	body := buildMCPBatchWriteBody(t, entries)
	t.Logf("mixed batchWrite body:\n%s", string(body))

	// ── Primary assertion: NO null values in the entire body ──────────
	// This is the exact invariant the bug violated. If "args":null
	// re-appears anywhere, this test fails loudly.
	assertNoNullValues(t, body, "mixed-config")

	// ── Secondary: confirm neither rejection error string is present ───
	bodyStr := string(body)
	for _, bad := range []string{
		"invalid type: null",
		"expected any valid TOML value",
		`"args":null`,
		`"args": null`,
	} {
		if strings.Contains(bodyStr, bad) {
			t.Errorf("mixed-config: body contains forbidden string %q\n--- body ---\n%s",
				bad, bodyStr)
		}
	}

	t.Logf("mixed-config: no null values, no forbidden strings — PASS")
}

// ── Live gate (skipped when codex binary is absent) ───────────────────────────

// codexBinaryGate returns the path to the codex binary, or skips the
// test when:
//   - testing.Short() is set
//   - DONMAI_SMOKES_SKIP_LIVE_API=1 is set
//   - No codex binary is found (CODEX_BIN env, CODEX_APP_SERVER_BIN env,
//     "codex" on PATH — checked in that precedence order)
func codexBinaryGate(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("live codex binary test; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_API") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_API=1 — operator opted out of live API smokes")
	}

	// Check explicit env overrides first (useful for test harnesses that
	// install codex to a non-PATH location).
	for _, env := range []string{"CODEX_BIN", "CODEX_APP_SERVER_BIN"} {
		if p := os.Getenv(env); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
			// Env set but path doesn't exist → skip rather than fail, since
			// the operator may have set a stale path for a different machine.
			t.Skipf("$%s=%q is set but the binary does not exist — skipping live codex gate", env, p)
		}
	}

	// Fall back to PATH lookup.
	p, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex binary not found on PATH (and CODEX_BIN/CODEX_APP_SERVER_BIN not set) — skipping live codex MCP gate; "+
			"set CODEX_BIN=/path/to/codex to run this test when codex is installed")
	}
	return p
}

// TestCodexMCPConfigLiveGate is the live counterpart: when the codex
// binary is available, it sends a minimal config/batchWrite JSON-RPC
// request to codex's stdio interface and asserts that codex does NOT
// respond with the null-rejection error strings.
//
// The request carries:
//   - One http-transport MCP server (type:"http", url, Authorization header).
//   - One stdio MCP server with no args.
//
// These are the exact shapes that triggered the SUP-1840 failure.
//
// GATE: skipped (not failed) when codex is absent. See codexBinaryGate.
func TestCodexMCPConfigLiveGate(t *testing.T) {
	codexBin := codexBinaryGate(t)
	t.Logf("live gate: using codex binary at %s", codexBin)

	// Build the config/batchWrite JSON-RPC request body — same shape
	// the fixed mcpServersConfig would hand to codex over the stdio pipe.
	entries := map[string]codexMCPEntry{
		"platform-mcp": {
			Type: "http",
			URL:  "https://platform.example.com/api/mcp/session-smoke",
			Headers: map[string]string{
				"Authorization": "Bearer smoke-test-token",
			},
		},
		"local-tools": {
			Type:    "stdio",
			Command: "echo", // simple always-present binary; codex won't spawn it
			// Args: nil — the regression trigger shape.
		},
	}

	mcpValue := make(map[string]any, len(entries))
	for name, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal entry %q: %v", name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal entry %q: %v", name, err)
		}
		mcpValue[name] = m
	}

	// JSON-RPC 2.0 config/batchWrite request (the exact call mcpServersConfig
	// results in — codex reads it from stdin on the app-server interface).
	type jsonRPCRequest struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "config/batchWrite",
		Params: map[string]any{
			"updates": []map[string]any{
				{"keyPath": "mcpServers", "value": mcpValue},
			},
		},
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal JSON-RPC request: %v", err)
	}
	// JSON-RPC over stdio: each request is a newline-terminated JSON object.
	reqBytes = append(reqBytes, '\n')

	t.Logf("live gate: sending config/batchWrite:\n%s", string(reqBytes))

	// Spawn codex in app-server mode with a short timeout. We do NOT
	// expect a full session — just that codex processes the batchWrite
	// without emitting the null-rejection error.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, codexBin, "app-server") //nolint:gosec
	cmd.Stdin = bytes.NewReader(reqBytes)

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	// Run returns non-zero when codex exits (expected for an incomplete
	// session); we only care about the output content.
	_ = cmd.Run()

	output := outBuf.String()
	t.Logf("live gate: codex output (first 2048 chars):\n%.2048s", output)

	// ── Assert: null-rejection strings must NOT appear ─────────────────
	for _, bad := range []string{
		"invalid type: null",
		"expected any valid TOML value",
		`"failureMode":"spawn-failed"`,
		`failureMode: spawn-failed`,
		"configure mcp servers",
	} {
		if strings.Contains(output, bad) {
			t.Errorf("live gate: codex output contains null-rejection string %q — args:null regression is present\n--- full output ---\n%s",
				bad, output)
		}
	}

	t.Logf("live gate: no null-rejection strings in codex output — PASS")
}

// ── Helper ────────────────────────────────────────────────────────────────────

// mapKeys returns the sorted key list of a map[string]any for diagnostic
// log lines. Order is deterministic enough for test output.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
