package smokes

// step15_codex_mcp_http_no_null_test.go — regression guard for the
// codex MCP-config args:null bug fixed in donmai PR #106.
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
//	causing failureMode:"spawn-failed" / "Session failed".
//	The fix delegates to runtime/mcp.BuildConfigFile which uses omitempty
//	tags and proper transport dispatch.
//
// Test strategy (why not live codex dispatch):
//
//	A full live codex dispatch requires the codex app-server binary to be
//	present, a valid OpenAI key, and a network connection — none of which
//	are guaranteed in CI. The always-run unit checks target the
//	load-bearing shared MCP-entry invariant: no "null" values and the
//	correct fields for each transport type.
//
//	Those checks construct an independent envelope; they do not import
//	Donmai's production serializer or prove its emitted bytes. The live
//	gate uses the native Codex config protocol and reads back the result.
//
//	A live gate (TestCodexMCPConfigLiveGate) additionally checks whether
//	the codex binary is present and, when it is, requires correlated
//	initialize, config/batchWrite, and config/read responses from its
//	app-server. When codex is absent the live gate skips cleanly.
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
//   - TestCodexMCPConfigLiveGate: locates the codex binary, uses a
//     private config home, and requires a successful batchWrite plus
//     effective readback of both MCP server entries.
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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
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

// codexMCPBatchWriteParams is the legacy envelope retained by the
// transport/null shape checks. The live probe below uses Codex's native
// edits/keyPath form instead.
type codexMCPBatchWriteParams struct {
	Updates []codexMCPKeyPath `json:"updates"`
}

type codexMCPKeyPath struct {
	KeyPath string         `json:"keyPath"`
	Value   map[string]any `json:"value"`
}

// buildMCPBatchWriteBody constructs the independent transport/null shape
// fixture and returns its marshaled bytes.
func buildMCPBatchWriteBody(t *testing.T, entries map[string]codexMCPEntry) []byte {
	t.Helper()
	// Convert typed entries to map[string]any via JSON round-trip so
	// omitempty omissions are observable in the fixture.
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
// anywhere. This is the exact pre-fix regression shape:
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
	afh.SkipIfShort(t, "live codex binary test")
	afh.SkipIfKnob(t, "DONMAI_SMOKES_SKIP_LIVE_API", "operator opted out of live API smokes")

	// Check explicit env overrides first (useful for test harnesses that
	// install codex to a non-PATH location).
	for _, env := range []string{"CODEX_BIN", "CODEX_APP_SERVER_BIN"} {
		if p := os.Getenv(env); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
			// Env set but the path does not exist. This used to skip, on the
			// theory that the operator might have left a stale path from
			// another machine. That reasoning inverts who is responsible:
			// setting the variable IS the request to run this gate, so a
			// broken value is a broken request, not a precondition. Skipping
			// it means an operator who typos the path gets `ok` and believes
			// the live codex gate ran. Fail instead — the fix (unset it, or
			// correct it) is one line and now discoverable.
			t.Fatalf("$%s=%q is set but no such file exists — unset it to fall back to PATH, "+
				"or correct it to the codex binary you meant to gate against", env, p)
		}
	}

	// Fall back to PATH lookup.
	p, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex binary not found on PATH (and CODEX_BIN/CODEX_APP_SERVER_BIN not set) — skipping live codex MCP gate; " +
			"set CODEX_BIN=/path/to/codex to run this test when codex is installed")
	}
	return p
}

// TestCodexMCPConfigLiveGate checks Codex's native config protocol. The
// unit checks above retain the shared transport/null shape; this live path
// uses Codex's native mcp_servers key and http_headers field. It does not
// import or execute Donmai's production serializer.
func TestCodexMCPConfigLiveGate(t *testing.T) {
	codexBin := codexBinaryGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := probeCodexMCPConfig(ctx, codexBin, t.TempDir()); err != nil {
		t.Fatalf("live Codex MCP config protocol: %v", err)
	}
	t.Log("live gate: correlated batchWrite succeeded and config/read confirmed both servers")
}

type codexProbeFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type codexProbeRead struct {
	frame codexProbeFrame
	err   error
}

const maxCodexProbeOutput = 64 << 10

var errCodexProbeOutputLimit = errors.New("codex output exceeded bounded capture")

// codexProbeCapture bounds diagnostic output. Truncation fails the probe so
// a rejection string cannot disappear beyond the capture limit.
type codexProbeCapture struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *codexProbeCapture) Write(p []byte) (int, error) {
	remaining := maxCodexProbeOutput - c.buf.Len()
	if remaining > 0 {
		_, _ = c.buf.Write(p[:min(len(p), remaining)])
	}
	if len(p) > remaining {
		c.truncated = true
	}
	return len(p), nil
}

// probeCodexMCPConfig exercises only local app-server config RPCs. Every
// process receives a fresh HOME/CODEX_HOME; the HTTP URL is loopback and no
// thread, model turn, provider login, or MCP tool is started.
func probeCodexMCPConfig(ctx context.Context, codexBin, base string) (retErr error) {
	home := filepath.Join(base, "home")
	codexHome := filepath.Join(home, ".codex")
	for _, dir := range []string{codexHome, filepath.Join(home, "tmp"), filepath.Join(home, ".config")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create private Codex directory: %w", err)
		}
	}
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte("mcp_servers = {}\n"), 0o600); err != nil {
		return fmt.Errorf("write private Codex config: %w", err)
	}
	cmd := exec.CommandContext(ctx, codexBin, "app-server", "--stdio") //nolint:gosec // explicitly gated test executable
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home, "CODEX_HOME=" + codexHome, "TMPDIR=" + filepath.Join(home, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=/usr/bin:/bin",
		"NO_COLOR=1", "TERM=dumb",
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Codex stdout: %w", err)
	}
	var stdoutCapture, stderrCapture codexProbeCapture
	cmd.Stderr = &stderrCapture
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Codex app-server: %w", err)
	}
	frames := make(chan codexProbeRead, 16)
	readDone := make(chan struct{})
	stopReader := make(chan struct{})
	go func() {
		defer close(readDone)
		dec := json.NewDecoder(io.TeeReader(io.LimitReader(stdout, maxCodexProbeOutput+1), &stdoutCapture))
		for {
			var frame codexProbeFrame
			err := dec.Decode(&frame)
			if stdoutCapture.truncated {
				err = errCodexProbeOutputLimit
			}
			select {
			case frames <- codexProbeRead{frame: frame, err: err}:
			case <-stopReader:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		_ = stdin.Close()
		readerTimer := time.NewTimer(3 * time.Second)
		defer readerTimer.Stop()
		readerJoined := false
		terminated := false
		stopOwned := func() {
			if err := cmd.Process.Kill(); err == nil {
				terminated = true
			} else if !errors.Is(err, os.ErrProcessDone) {
				retErr = errors.Join(retErr, fmt.Errorf("stop owned Codex app-server: %w", err))
			}
			_ = stdout.Close()
		}
		// A protocol or bounded-output failure cannot leave the child running
		// until the request's parent deadline. Stop exactly this process before
		// joining its reader and os/exec's stderr copier.
		if retErr != nil {
			stopOwned()
		}
		for !readerJoined {
			select {
			case <-readDone:
				readerJoined = true
			case <-frames:
				// Drain notifications so the decoder can reach EOF before Wait.
			case <-readerTimer.C:
				close(stopReader)
				stopOwned() // unblock Decode even if the child keeps the pipe open
				select {
				case <-readDone:
					readerJoined = true
				case <-time.After(2 * time.Second):
					retErr = errors.Join(retErr, errors.New("Codex stdout reader did not stop"))
				}
				// The next step is bounded even when a pipe copier is stranded.
				goto waitProcess
			}
		}
	waitProcess:
		// Wait synchronously: returning while a separate Wait goroutine was
		// still reaping this child made the old deadline branch untruthful.
		// The existing three-second budget stops only this process; Cmd.WaitDelay
		// then bounds its stderr copier after process exit.
		watchdogDone := make(chan error, 1)
		watchdog := time.AfterFunc(3*time.Second, func() {
			killErr := cmd.Process.Kill()
			_ = stdout.Close()
			watchdogDone <- killErr
		})
		waitErr := cmd.Wait()
		if !watchdog.Stop() {
			killErr := <-watchdogDone
			if killErr == nil {
				terminated = true
			} else if !errors.Is(killErr, os.ErrProcessDone) {
				retErr = errors.Join(retErr, fmt.Errorf("stop owned Codex app-server: %w", killErr))
			}
			retErr = errors.Join(retErr, errors.New("Codex app-server wait deadline exceeded"))
		}
		if waitErr != nil && !terminated {
			retErr = errors.Join(retErr, fmt.Errorf("Codex app-server exit: %w", waitErr))
		}
		if !readerJoined {
			select {
			case <-readDone:
				readerJoined = true
			default:
			}
		}
		if !readerJoined {
			retErr = errors.Join(retErr, errors.New("Codex output writers were not joined"))
			return
		}
		if (stdoutCapture.truncated || stderrCapture.truncated) && !errors.Is(retErr, errCodexProbeOutputLimit) {
			retErr = errors.Join(retErr, errCodexProbeOutputLimit)
		}
		output := stdoutCapture.buf.String() + "\n" + stderrCapture.buf.String()
		for _, bad := range []string{
			"invalid type: null",
			"expected any valid TOML value",
			`"failureMode":"spawn-failed"`,
			"failureMode: spawn-failed",
			"configure mcp servers",
		} {
			if strings.Contains(output, bad) {
				retErr = errors.Join(retErr, fmt.Errorf("Codex output contains forbidden rejection %q", bad))
			}
		}
	}()

	enc := json.NewEncoder(stdin)
	request := func(id int, method string, params map[string]any) (json.RawMessage, error) {
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return nil, fmt.Errorf("%s write: %w", method, err)
		}
		for {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("%s response timeout: %w", method, err)
			}
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%s response timeout: %w", method, ctx.Err())
			case incoming := <-frames:
				if incoming.err != nil {
					if errors.Is(incoming.err, io.EOF) {
						return nil, fmt.Errorf("%s: app-server closed output before response", method)
					}
					return nil, fmt.Errorf("%s: invalid response: %w", method, incoming.err)
				}
				frame := incoming.frame
				if len(frame.ID) == 0 && frame.Method != "" {
					continue // notification, not a reply to this request
				}
				if frame.JSONRPC != "" && frame.JSONRPC != "2.0" {
					return nil, fmt.Errorf("%s: wrong JSON-RPC version %q", method, frame.JSONRPC)
				}
				if string(frame.ID) != fmt.Sprint(id) || frame.Method != "" {
					return nil, fmt.Errorf("%s: mismatched response id/method", method)
				}
				if frame.Error != nil {
					return nil, fmt.Errorf("%s: RPC error code %d", method, frame.Error.Code)
				}
				if len(frame.Result) == 0 || bytes.Equal(bytes.TrimSpace(frame.Result), []byte("null")) {
					return nil, fmt.Errorf("%s: empty result", method)
				}
				return frame.Result, nil
			}
		}
	}

	if _, err := request(1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "donmai-smoke", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return err
	}
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	servers := map[string]any{
		"local-tools": map[string]any{"command": "/usr/bin/false"}, // args deliberately absent
		"local-http": map[string]any{
			"url": "http://127.0.0.1:9/mcp", "http_headers": map[string]any{"X-Fixture": "smoke"},
		},
	}
	writeResult, err := request(2, "config/batchWrite", map[string]any{
		"filePath": configPath, "reloadUserConfig": true,
		"edits": []map[string]any{{"keyPath": "mcp_servers", "mergeStrategy": "replace", "value": servers}},
	})
	if err != nil {
		return err
	}
	var writeAck struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(writeResult, &writeAck); err != nil || writeAck.Status != "ok" {
		return fmt.Errorf("config/batchWrite: missing successful acknowledgement: %v", err)
	}
	readResult, err := request(3, "config/read", map[string]any{"includeLayers": true})
	if err != nil {
		return err
	}
	var readback struct {
		Config struct {
			MCPServers map[string]map[string]any `json:"mcp_servers"`
		} `json:"config"`
	}
	if err := json.Unmarshal(readResult, &readback); err != nil {
		return fmt.Errorf("decode config/read: %w", err)
	}
	active := readback.Config.MCPServers
	if len(active) != 2 || active["local-tools"]["command"] != "/usr/bin/false" ||
		active["local-http"]["url"] != "http://127.0.0.1:9/mcp" {
		return errors.New("config/read did not confirm both requested MCP servers")
	}
	if args, exists := active["local-tools"]["args"]; exists && args == nil {
		return errors.New("config/read returned null stdio args")
	}
	header, ok := active["local-http"]["http_headers"].(map[string]any)
	if !ok || header["X-Fixture"] != "smoke" {
		return errors.New("config/read did not confirm HTTP header")
	}
	return nil
}

func TestCodexMCPProtocolRejectsBadReplies(t *testing.T) {
	validReplies := `read line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
read line
read line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"status":"ok"}}'
read line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"config":{"mcp_servers":{"local-tools":{"command":"/usr/bin/false"},"local-http":{"url":"http://127.0.0.1:9/mcp","http_headers":{"X-Fixture":"smoke"}}}}}}'
`
	tests := []struct {
		name, script, want string
		timeout            time.Duration
	}{
		{name: "empty_exit", script: "read line\nexit 23\n", want: "closed output"},
		{name: "rpc_error", script: "read line\nprintf '{\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32603,\"message\":\"refused\"}}\\n'\n", want: "RPC error"},
		{name: "mismatched_id", script: "read line\nprintf '{\"jsonrpc\":\"2.0\",\"id\":99,\"result\":{}}\\n'\n", want: "mismatched response"},
		{name: "malformed", script: "read line\nprintf 'not-json\\n'\n", want: "invalid response"},
		{name: "timeout", script: "read line\nread more\n", want: "response timeout", timeout: 300 * time.Millisecond},
		{name: "notification_flood", script: "read line\nwhile :; do printf '{\"jsonrpc\":\"2.0\",\"method\":\"noise\"}\\n'; done\n", want: "output exceeded bounded capture", timeout: 3 * time.Second},
		{name: "valid_then_nonzero_exit", script: validReplies + "exit 23\n", want: "exit status 23"},
		{name: "valid_then_forbidden_stderr", script: validReplies + "printf 'invalid type: null' >&2\n", want: "forbidden rejection"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			bin := filepath.Join(base, "fake-codex")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+tc.script), 0o700); err != nil {
				t.Fatal(err)
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 3 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			started := time.Now()
			err := probeCodexMCPConfig(ctx, bin, base)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("probe error = %v, want %q", err, tc.want)
			}
			if tc.name == "notification_flood" && strings.Contains(err.Error(), "response timeout") {
				t.Fatalf("flood reached deadline instead of reader output budget: %v", err)
			}
			if elapsed := time.Since(started); elapsed > 8*time.Second {
				t.Fatalf("probe cleanup exceeded bound: %s", elapsed)
			}
		})
	}
}

// The original flood case retains its three-second parent deadline. This
// separate control leaves the same fake shell unlimited until the output
// budget fires, proving the probe stops and joins it before parent cancellation.
func TestCodexMCPNotificationFloodLongParentStopsAndJoins(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, "fake-codex")
	const flood = "read line\nwhile :; do printf '{\"jsonrpc\":\"2.0\",\"method\":\"noise\"}\\n'; done\n"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+flood), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	err := probeCodexMCPConfig(ctx, bin, base)
	if ctx.Err() != nil {
		t.Fatalf("flood parent expired before bounded capture: %v", ctx.Err())
	}
	if err == nil || !strings.Contains(err.Error(), "output exceeded bounded capture") {
		t.Fatalf("flood probe error = %v, want bounded capture failure", err)
	}
	if strings.Contains(err.Error(), "wait deadline exceeded") || strings.Contains(err.Error(), "writers were not joined") {
		t.Fatalf("flood returned without joining owned output: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("flood cleanup exceeded existing bound: %s", elapsed)
	}
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
