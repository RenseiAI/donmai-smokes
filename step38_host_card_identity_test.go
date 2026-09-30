package smokes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	vt "github.com/charmbracelet/x/vt"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// cardIdentityScreen uses the existing terminal dependency to interpret the
// current screen, including scoped scroll operations. Old transcript text must
// not satisfy a current-card assertion after an index update.
func cardIdentityScreen(raw []byte, columns int) []string {
	terminal := vt.NewEmulator(columns, watchRows)
	readerDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, terminal)
		close(readerDone)
	}()
	_, _ = terminal.Write(raw)
	lines := make([]string, terminal.Height())
	for row := range lines {
		var line strings.Builder
		for column := 0; column < terminal.Width(); column++ {
			cell := terminal.CellAt(column, row)
			if cell == nil {
				line.WriteByte(' ')
			} else if cell.Width > 0 {
				line.WriteString(cell.Content)
			}
		}
		lines[row] = strings.TrimRight(line.String(), " ")
	}
	// Read and Close in this dependency share an unsynchronized closed flag.
	// Stop the pipe reader first, then close the now-single-owner emulator.
	_ = terminal.InputPipe().(io.Closer).Close()
	<-readerDone
	_ = terminal.Close()
	return lines
}

func TestHostCardTerminalReference(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, operations string
		want             []string
	}{
		{name: "reverse_index", operations: "\x1b[2;3r\x1b[2;1H\x1bM\x1b[2;1HNEW", want: []string{"HOST", "NEW", "ALPHA", "FOOTER"}},
		{name: "regional_line_feed", operations: "\x1b[2;3r\x1b[3;1H\nNEW", want: []string{"HOST", "BETA", "NEW", "FOOTER"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := cardIdentityScreen([]byte("\x1b[HHOST\r\nALPHA\r\nBETA\r\nFOOTER"+tc.operations), 30)
			for row, want := range tc.want {
				if lines[row] != want {
					t.Fatalf("terminal row %d = %q, want %q", row, lines[row], want)
				}
			}
		})
	}
}

func cardIdentityContext(lines []string, marker string) string {
	column := -1
	for _, line := range lines {
		if strings.Contains(line, marker) {
			column = len([]rune(line[:strings.Index(line, marker)]))
			break
		}
	}
	if column < 0 {
		return ""
	}
	// A card is 44 cells wide and its header starts four cells inside it.
	start := max(0, column-4)
	var body strings.Builder
	for _, line := range lines {
		if strings.Contains(line, "session stream") {
			break
		}
		runes := []rune(line)
		if start < len(runes) {
			body.WriteString(string(runes[start:min(start+44, len(runes))]))
		}
		body.WriteByte('\n')
	}
	return body.String()
}

func waitCardIdentity(t *testing.T, capture *watchCapture, marker string, fields ...string) string {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		body := cardIdentityContext(cardIdentityScreen(capture.snapshot(), capture.columns), marker)
		matches := body != ""
		for _, field := range fields {
			matches = matches && strings.Contains(body, field)
		}
		if matches {
			return body
		}
		select {
		case <-capture.notify:
		case <-deadline.C:
			t.Fatalf("current card %q missing %q:\n%s", marker, fields, body)
		}
	}
}

// TestHostWatchCardAndResponseIdentity drives the compiled OSS watcher through
// local read-only daemon responses and private runner state/event files.
func TestHostWatchCardAndResponseIdentity(t *testing.T) {
	afh.SkipIfShort(t, "compiled host-watch card and native response identity smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	if _, err := os.Stat(filepath.Join(source, "afcli", "host_watch.go")); err != nil {
		if os.IsNotExist(err) {
			afh.DeclineLive(t, "located source predates the host-watch command")
		}
		t.Fatalf("probe host-watch source: %v", err)
	}
	binary, source := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: source, Timeout: 8 * time.Minute})
	t.Logf("compiled host-watch identity SUT from %s", source)
	root := t.TempDir()
	home, cwd := filepath.Join(root, "home"), filepath.Join(root, "cwd")
	for _, path := range []string{home, cwd} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON := func(path string, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".next", body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".next", path); err != nil {
			t.Fatal(err)
		}
	}
	makeWorktree := func(name string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(path, ".agent"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".agent", "events.jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	appendEvent := func(worktree string, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(filepath.Join(worktree, ".agent", "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write(append(body, '\n'))
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("append fixture event: write=%v close=%v", writeErr, closeErr)
		}
	}
	rich, legacy := makeWorktree("rich"), makeWorktree("legacy")
	started := time.Now().Add(-time.Minute).UnixMilli()
	writeState := func(start int64) {
		writeJSON(filepath.Join(rich, ".agent", "state.json"), map[string]any{
			"sessionId": "rich-identity", "startedAt": start, "issueIdentifier": "CARD-1",
			"agentCardId": "stale-card", "agentCardName": "Stale", "harness": "stale-harness",
			"model": "stale-model", "providerName": "stale-provider", "workType": "stale-work",
		})
	}
	writeState(started)
	richHandle := map[string]any{
		"sessionId": "rich-identity", "state": "running", "worktreePath": rich,
		"agentCardId": "card-review", "agentCardName": "Reviewer", "harness": "pi",
		"model": "request-alias", "modelProvider": "configured", "workType": "development", "projectName": "alpha",
	}
	legacyHandle := map[string]any{"sessionId": "legacy-identity", "state": "running", "worktreePath": legacy, "projectName": "beta"}
	var mu sync.Mutex
	handles := []map[string]any{richHandle}
	polls, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("watcher attempted mutation %s %s", request.Method, request.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		gets++
		var body any
		switch request.URL.Path {
		case "/api/daemon/sessions":
			polls++
			body = handles
		case "/api/daemon/status":
			body = map[string]any{"activeSessions": len(handles), "maxSessions": 8, "status": "ready"}
		case "/api/daemon/stats":
			body = map[string]any{"queueDepth": 0}
		default:
			t.Errorf("unexpected local daemon endpoint %s", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode daemon fixture: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	waitPolls := func(extra int) {
		t.Helper()
		mu.Lock()
		want := polls + extra
		mu.Unlock()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			current := polls
			mu.Unlock()
			if current >= want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("daemon did not receive %d additional actual session polls", extra)
	}
	_, capture, quit := startWatchPTY(t, binary, cwd, home, server.URL, 180, "--all")
	waitCardIdentity(t, capture, "CARD-1", "Agent card Reviewer", "Card ID card-review", "Model identity unknown", "Actual provider unknown", "Model version unknown", "harness pi", "model request-alias", "provider configured", "state running", "tools not reported", "project alpha")
	// Requested/configured model fields alone are not a native response identity.
	appendEvent(rich, map[string]any{"kind": "llm_call", "model": "request-alias", "system": "configured", "inputTokens": 1, "usageSource": "provider"})
	appendEvent(rich, map[string]any{"kind": "tool_use", "toolName": "Read", "input": map[string]any{}})
	waitCardIdentity(t, capture, "CARD-1", "tools 1", "Model identity unknown", "Actual provider unknown", "Model version unknown")
	appendEvent(rich, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"model": "served-id", "provider": "actual-vendor", "version": "snapshot-v2"}})
	waitCardIdentity(t, capture, "CARD-1", "Model identity served-id", "Actual provider actual-vendor", "Model version snapshot-v2", "model request-alias", "provider configured")
	appendEvent(rich, map[string]any{"kind": "tool_use", "toolName": "Read", "input": map[string]any{"path": "fixture.txt"}})
	waitCardIdentity(t, capture, "CARD-1", "tools 2", "Model identity served-id")
	mu.Lock()
	handles = append(handles, legacyHandle)
	mu.Unlock()
	waitCardIdentity(t, capture, "legacy-i", "Agent card unknown", "Card ID unknown", "Model identity unknown", "Actual provider unknown", "Model version unknown", "harness unknown", "model unknown", "provider unknown", "project beta")
	writeJSON(filepath.Join(legacy, ".agent", "state.json"), map[string]any{"sessionId": "legacy-identity", "startedAt": started, "issueIdentifier": "CARD-2", "agentCardId": "local-card", "agentCardName": "Local"})
	waitCardIdentity(t, capture, "CARD-2", "Agent card Local", "Card ID local-card", "Model identity unknown", "Actual provider unknown", "Model version unknown")
	writeJSON(filepath.Join(legacy, ".agent", "state.json"), map[string]any{"sessionId": "different-session", "agentCardId": "foreign-card", "agentCardName": "Foreign"})
	legacyBody := waitCardIdentity(t, capture, "legacy-i", "Agent card unknown", "Card ID unknown", "Model identity unknown")
	if strings.Contains(legacyBody, "Foreign") || strings.Contains(legacyBody, "foreign-card") {
		t.Fatalf("foreign session state became identity: %s", legacyBody)
	}
	waitPolls(2)
	waitCardIdentity(t, capture, "CARD-1", "tools 2", "Model identity served-id", "Actual provider actual-vendor", "Model version snapshot-v2", "harness pi", "state running")
	mu.Lock()
	handles = []map[string]any{legacyHandle, richHandle}
	mu.Unlock()
	waitPolls(1)
	waitCardIdentity(t, capture, "CARD-1", "tools 2", "Model identity served-id", "Actual provider actual-vendor", "Model version snapshot-v2")
	appendEvent(rich, map[string]any{"kind": "llm_call", "model": "request-alias", "system": "configured", "responseModel": "fallback-id", "inputTokens": 1, "usageSource": "provider"})
	waitCardIdentity(t, capture, "CARD-1", "Model identity fallback-id", "Actual provider unknown", "Model version unknown", "tools 2")
	writeState(time.Now().UnixMilli())
	waitCardIdentity(t, capture, "CARD-1", "Agent card Reviewer", "Model identity unknown", "Actual provider unknown", "Model version unknown", "tools not reported")
	waitPolls(2)
	waitCardIdentity(t, capture, "CARD-1", "Model identity unknown", "Actual provider unknown", "Model version unknown", "tools not reported")
	// Existing response rows still on disk cannot be replayed into a replaced
	// run during this watcher's lifetime; a new native observation may proceed.
	appendEvent(rich, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"provider": "provider-only"}})
	waitCardIdentity(t, capture, "CARD-1", "Model identity unknown", "Actual provider provider-only", "Model version unknown")
	appendEvent(rich, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"version": "version-only"}})
	waitCardIdentity(t, capture, "CARD-1", "Model identity unknown", "Actual provider unknown", "Model version version-only")
	// Identity values are data: CSI erase/cursor and OSC hyperlink instructions
	// must not execute or ride unchanged into the user's terminal.
	cardName := "Review\x1b[2Jer\x1b]8;;https://example.invalid/card\aName\x1b]8;;\a"
	mu.Lock()
	richHandle["agentCardName"] = cardName
	richHandle["agentCardId"] = "card\x1b[H-review"
	mu.Unlock()
	appendEvent(rich, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"model": "served\x1b[H-id", "provider": "actual\x1b]0;BAD-TITLE\a-vendor", "version": "snap\x1b[2J-v3"}})
	waitCardIdentity(t, capture, "CARD-1", "Agent card ReviewerName", "Card ID card-review", "Model identity served-id", "Actual provider actual-vendor", "Model version snap-v3", "harness pi", "state running")
	for _, forbidden := range []string{cardName, "BAD-TITLE", "https://example.invalid/card"} {
		if strings.Contains(string(capture.snapshot()), forbidden) {
			t.Fatalf("identity annotation escaped as terminal instructions: %q", forbidden)
		}
	}
	quit()
	// Replay uses a distinct matching session/run and must fold identity/counts
	// without promoting historical work/output to fresh observations.
	replay := makeWorktree("replay")
	writeJSON(filepath.Join(replay, ".agent", "state.json"), map[string]any{"sessionId": "replay-identity", "issueIdentifier": "CARD-3", "startedAt": started})
	appendEvent(replay, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"model": "history-id", "provider": "history-vendor", "version": "history-v1"}})
	appendEvent(replay, map[string]any{"kind": "tool_use", "toolName": "Read", "input": map[string]any{}})
	appendEvent(replay, map[string]any{"kind": "result", "success": true, "cost": map[string]any{"totalCostUsd": 1.25, "numTurns": 4}})
	mu.Lock()
	handles = []map[string]any{{"sessionId": "replay-identity", "state": "completed", "worktreePath": replay, "model": "history-alias", "modelProvider": "configured"}}
	mu.Unlock()
	_, history, quitHistory := startWatchPTY(t, binary, cwd, home, server.URL, 180, "--all", "--replay")
	waitCardIdentity(t, history, "CARD-3", "Model identity history-id", "Actual provider history-vendor", "Model version history-v1", "model history-alias", "tools 1", "cost $1.25", "turns 4", "work never", "output never")
	quitHistory()
	mu.Lock()
	t.Logf("compiled identity smoke completed: session polls=%d GET requests=%d", polls, gets)
	mu.Unlock()
}
