package smokes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

func TestA2AV1CLIEndToEnd(t *testing.T) {
	afh.SkipIfShort(t, "build-and-run formal A2A CLI smoke")

	const extension = "https://example.test/a2a/smoke/v1"
	var rpcCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/card":
			if request.Header.Get("A2A-Version") != "1.0" {
				t.Errorf("card version = %q", request.Header.Get("A2A-Version"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "Smoke Agent", "description": "Local formal A2A fixture", "version": "1.0.0",
				"supportedInterfaces": []any{map[string]any{
					"url": server.URL + "/rpc", "protocolBinding": "JSONRPC",
					"protocolVersion": "1.0.2", "tenant": "smoke-seat",
				}},
				"capabilities":      map[string]any{"extensions": []any{map[string]any{"uri": extension, "required": true}}},
				"defaultInputModes": []string{"text/plain"}, "defaultOutputModes": []string{"text/plain"}, "skills": []any{},
			})
		case "/rpc":
			rpcCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer smoke-token" || request.Header.Get("A2A-Version") != "1.0" || request.Header.Get("A2A-Extensions") != extension {
				t.Errorf("RPC headers auth=%q version=%q extensions=%q", request.Header.Get("Authorization"), request.Header.Get("A2A-Version"), request.Header.Get("A2A-Extensions"))
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode RPC: %v", err)
			}
			params, _ := body["params"].(map[string]any)
			if body["method"] != "SendMessage" || params["tenant"] != "smoke-seat" {
				t.Errorf("RPC body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": body["id"],
				"result": map[string]any{"task": map[string]any{
					"id": "smoke-task", "contextId": "smoke-context",
					"status": map[string]any{"state": "TASK_STATE_COMPLETED"},
				}},
			})
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)

	requestedSource := inFlightSourceDir()
	binary, sourceDir := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir: requestedSource,
		Timeout:   3 * time.Minute,
	})
	if requestedSource != "" {
		requestedAbsolute, err := filepath.Abs(requestedSource)
		if err != nil {
			t.Fatalf("resolve requested in-flight source: %v", err)
		}
		if sourceDir != requestedAbsolute {
			t.Fatalf("built source = %q, want exact in-flight source %q", sourceDir, requestedAbsolute)
		}
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("smoke-token\n"), 0o600); err != nil {
		t.Fatalf("write token fixture: %v", err)
	}
	runner := afh.NewRunner(afh.RunnerConfig{
		Timeout:        20 * time.Second,
		BinaryOverride: binary,
		OverrideTarget: "donmai",
	})
	output, err := runner.Run(
		"donmai", "a2a", "send",
		"--card", server.URL+"/card",
		"--bearer-token-file", tokenFile,
		"--extension", extension,
		"--message-id", "smoke-message",
		"--message", "hello from the smoke",
		"--json",
	)
	if err != nil {
		t.Fatalf("formal A2A send: %v", err)
	}
	var response struct {
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("decode output %q: %v", output, err)
	}
	if response.Task == nil || response.Task.ID != "smoke-task" {
		t.Fatalf("response = %+v", response)
	}

	negativeOutput, negativeErr := runner.RunCaptureBoth(
		"donmai", "a2a", "send",
		"--card", server.URL+"/card",
		"--bearer-token-file", tokenFile,
		"--message", "missing extension",
	)
	if negativeErr == nil || !strings.Contains(negativeOutput, "required extension") {
		t.Fatalf("missing-extension result = (%q, %v), want refusal", negativeOutput, negativeErr)
	}
	if rpcCalls.Load() != 1 {
		t.Fatalf("RPC calls = %d, want one positive call and no negative call", rpcCalls.Load())
	}
}
