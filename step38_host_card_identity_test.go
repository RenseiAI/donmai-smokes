package smokes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

func TestHostCardTerminalReference(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, operations string
		want             []string
	}{
		{name: "reverse_index", operations: "\x1b[2;3r\x1b[2;1H\x1bM\x1b[2;1HNEW", want: []string{"HOST", "NEW", "ALPHA", "FOOTER"}},
		{name: "regional_line_feed", operations: "\x1b[2;3r\x1b[3;1H\nNEW", want: []string{"HOST", "BETA", "NEW", "FOOTER"}},
		// The grid repaints a changed pane with explicit scroll commands inside
		// a scroll region; the rows outside the region must stay put.
		{name: "region_scroll_down", operations: "\x1b[2;3r\x1b[2;1H\x1b[1T", want: []string{"HOST", "", "ALPHA", "FOOTER"}},
		{name: "region_scroll_up", operations: "\x1b[2;3r\x1b[2;1H\x1b[1S", want: []string{"HOST", "BETA", "", "FOOTER"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := watchScreen([]byte("\x1b[HHOST\r\nALPHA\r\nBETA\r\nFOOTER"+tc.operations), 30)
			for row, want := range tc.want {
				if lines[row] != want {
					t.Fatalf("terminal row %d = %q, want %q", row, lines[row], want)
				}
			}
		})
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
		journal, err := os.Stat(filepath.Join(rich, ".agent", "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		writeJSON(filepath.Join(rich, ".agent", "state.json"), map[string]any{
			"sessionId": "rich-identity", "startedAt": start, "eventLogStartOffset": journal.Size(), "issueIdentifier": "CARD-1", "issueTitle": "Review fixture title",
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
	terminal, capture, quit := startWatchPTY(t, binary, cwd, home, server.URL, 180, "--all")
	screen := func() []string { return watchScreen(capture.snapshot(), capture.columns) }
	press := func(keys string) {
		t.Helper()
		if _, err := terminal.Write([]byte(keys)); err != nil {
			t.Fatal(err)
		}
	}
	// A card carries the model and harness; every other identity axis is read
	// in the detail pane of the selected session. Enter opens it and j/k move
	// between sessions while it stays open. Cards sort by issue id, then
	// session id, so the rich session is first and the legacy one second.
	cursor := 0
	selectCard := func(index int) {
		t.Helper()
		for ; cursor < index; cursor++ {
			press("j")
		}
		for ; cursor > index; cursor-- {
			press("k")
		}
	}
	detail := func(id string, want ...watchField) []string {
		t.Helper()
		return waitWatchDetail(t, capture, screen, id, want...)
	}
	waitWatchCard(t, capture, "CARD-1")
	press("\r")
	detail("CARD-1", field("Agent card", "Reviewer"), field("Card ID", "card-review"), field("Model identity", "unknown"), field("Actual provider", "unknown"), field("Model version", "unknown"),
		field("Harness", "pi"), field("Model", "request-alias"), field("Endpoint surface", "configured"), field("State", "running"), field("Tools", "not reported"), field("Project", "alpha"),
		field("Title", "Review fixture title"))
	// The first row is the animated status dot, the issue id, then the title.
	waitCardRow(t, capture, "CARD-1", 0, `"CARD-1 Review fixture title" after the status dot`, func(row string) bool {
		fields := strings.Fields(row)
		return len(fields) > 1 && strings.Join(fields[1:], " ") == "CARD-1 Review fixture title"
	})
	cardRowIs(t, capture, "CARD-1", 1, "alpha · development")
	cardRowIs(t, capture, "CARD-1", 2, "request-alias · pi")
	// Requested/configured model fields alone are not a native response identity.
	appendEvent(rich, map[string]any{"kind": "llm_call", "model": "request-alias", "system": "configured", "inputTokens": 1, "usageSource": "provider"})
	appendEvent(rich, map[string]any{"kind": "tool_use", "toolName": "Read", "input": map[string]any{}})
	detail("CARD-1", field("Tools", "1"), field("Model identity", "unknown"), field("Actual provider", "unknown"), field("Model version", "unknown"))
	cardRowIs(t, capture, "CARD-1", 2, "request-alias · pi")
	appendEvent(rich, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"model": "served-id", "provider": "actual-vendor", "version": "snapshot-v2"}})
	detail("CARD-1", field("Model identity", "served-id"), field("Actual provider", "actual-vendor"), field("Model version", "snapshot-v2"), field("Model", "request-alias"), field("Endpoint surface", "configured"))
	cardRowIs(t, capture, "CARD-1", 2, "request-alias → served-id · pi")
	appendEvent(rich, map[string]any{"kind": "tool_use", "toolName": "Read", "input": map[string]any{"path": "fixture.txt"}})
	detail("CARD-1", field("Tools", "2"), field("Model identity", "served-id"))
	mu.Lock()
	handles = append(handles, legacyHandle)
	mu.Unlock()
	waitWatchCard(t, capture, "legacy-i")
	selectCard(1)
	detail("legacy-i", field("Agent card", "unknown"), field("Card ID", "unknown"), field("Model identity", "unknown"), field("Actual provider", "unknown"), field("Model version", "unknown"),
		field("Harness", "unknown"), field("Model", "unknown"), field("Endpoint surface", "unknown"), field("Project", "beta"))
	// A bare value stands for itself; a label appears only where it is unknown.
	cardRowIs(t, capture, "legacy-i", 1, "beta · work type unknown")
	cardRowIs(t, capture, "legacy-i", 2, "model unknown · harness unknown")
	writeJSON(filepath.Join(legacy, ".agent", "state.json"), map[string]any{"sessionId": "legacy-identity", "startedAt": started, "issueIdentifier": "CARD-2", "agentCardId": "local-card", "agentCardName": "Local"})
	detail("CARD-2", field("Agent card", "Local"), field("Card ID", "local-card"), field("Model identity", "unknown"), field("Actual provider", "unknown"), field("Model version", "unknown"))
	writeJSON(filepath.Join(legacy, ".agent", "state.json"), map[string]any{"sessionId": "different-session", "agentCardId": "foreign-card", "agentCardName": "Foreign"})
	legacyScreen := strings.Join(detail("legacy-i", field("Agent card", "unknown"), field("Card ID", "unknown"), field("Model identity", "unknown")), "\n")
	if strings.Contains(legacyScreen, "Foreign") || strings.Contains(legacyScreen, "foreign-card") {
		t.Fatalf("foreign session state became identity: %s", legacyScreen)
	}
	waitPolls(2)
	selectCard(0)
	detail("CARD-1", field("Tools", "2"), field("Model identity", "served-id"), field("Actual provider", "actual-vendor"), field("Model version", "snapshot-v2"), field("Harness", "pi"), field("State", "running"))
	// The daemon's order does not decide which card is which.
	mu.Lock()
	handles = []map[string]any{legacyHandle, richHandle}
	mu.Unlock()
	waitPolls(1)
	detail("CARD-1", field("Tools", "2"), field("Model identity", "served-id"), field("Actual provider", "actual-vendor"), field("Model version", "snapshot-v2"))
	appendEvent(rich, map[string]any{"kind": "llm_call", "model": "request-alias", "system": "configured", "responseModel": "fallback-id", "inputTokens": 1, "usageSource": "provider"})
	detail("CARD-1", field("Model identity", "fallback-id"), field("Actual provider", "unknown"), field("Model version", "unknown"), field("Tools", "2"))
	cardRowIs(t, capture, "CARD-1", 2, "request-alias → fallback-id · pi")
	writeState(time.Now().UnixMilli())
	detail("CARD-1", field("Agent card", "Reviewer"), field("Model identity", "unknown"), field("Actual provider", "unknown"), field("Model version", "unknown"), field("Tools", "not reported"))
	cardRowIs(t, capture, "CARD-1", 2, "request-alias · pi")
	waitPolls(2)
	detail("CARD-1", field("Model identity", "unknown"), field("Actual provider", "unknown"), field("Model version", "unknown"), field("Tools", "not reported"))
	// Existing response rows still on disk cannot be replayed into a replaced
	// run during this watcher's lifetime; a new native observation may proceed.
	appendEvent(rich, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"provider": "provider-only"}})
	detail("CARD-1", field("Model identity", "unknown"), field("Actual provider", "provider-only"), field("Model version", "unknown"))
	appendEvent(rich, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"version": "version-only"}})
	detail("CARD-1", field("Model identity", "unknown"), field("Actual provider", "unknown"), field("Model version", "version-only"))
	// Identity values are data: CSI erase/cursor and OSC hyperlink instructions
	// must not execute or ride unchanged into the user's terminal.
	cardName := "Review\x1b[2Jer\x1b]8;;https://example.invalid/card\aName\x1b]8;;\a"
	mu.Lock()
	richHandle["agentCardName"] = cardName
	richHandle["agentCardId"] = "card\x1b[H-review"
	mu.Unlock()
	appendEvent(rich, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"model": "served\x1b[H-id", "provider": "actual\x1b]0;BAD-TITLE\a-vendor", "version": "snap\x1b[2J-v3"}})
	detail("CARD-1", field("Agent card", "ReviewerName"), field("Card ID", "card-review"), field("Model identity", "served-id"), field("Actual provider", "actual-vendor"), field("Model version", "snap-v3"), field("Harness", "pi"), field("State", "running"))
	cardRowIs(t, capture, "CARD-1", 2, "request-alias → served-id · pi")
	for _, forbidden := range []string{cardName, "BAD-TITLE", "https://example.invalid/card"} {
		if strings.Contains(string(capture.snapshot()), forbidden) {
			t.Fatalf("identity annotation escaped as terminal instructions: %q", forbidden)
		}
	}
	quit()
	// Replay uses a distinct matching session/run and must fold identity/counts
	// without promoting historical work/output to fresh observations. Output
	// freshness starts at the journal's modification time, which is when the
	// run last wrote; replayed rows are read now and must not move it.
	replay := makeWorktree("replay")
	writeJSON(filepath.Join(replay, ".agent", "state.json"), map[string]any{"sessionId": "replay-identity", "issueIdentifier": "CARD-3", "startedAt": started, "eventLogStartOffset": int64(0)})
	appendEvent(replay, map[string]any{"kind": "system", "subtype": "model_identity", "observedModel": map[string]any{"model": "history-id", "provider": "history-vendor", "version": "history-v1"}})
	appendEvent(replay, map[string]any{"kind": "tool_use", "toolName": "Read", "input": map[string]any{}})
	appendEvent(replay, map[string]any{"kind": "result", "success": true, "cost": map[string]any{"totalCostUsd": 1.25, "numTurns": 4}, "observedCostUsd": 1.25, "observedTurns": 4})
	wroteAt := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(replay, ".agent", "events.jsonl"), wroteAt, wroteAt); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	handles = []map[string]any{{"sessionId": "replay-identity", "state": "completed", "worktreePath": replay, "model": "history-alias", "modelProvider": "configured"}}
	mu.Unlock()
	historyTerminal, history, quitHistory := startWatchPTY(t, binary, cwd, home, server.URL, 180, "--all", "--replay")
	waitWatchCard(t, history, "CARD-3")
	if _, err := historyTerminal.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	waitWatchDetail(t, history, func() []string { return watchScreen(history.snapshot(), history.columns) }, "CARD-3",
		field("Model identity", "history-id"), field("Actual provider", "history-vendor"), field("Model version", "history-v1"), field("Model", "history-alias"),
		field("Tools", "1"), field("Cost", "$1.25"), field("Turns", "4"), field("Work", "never"))
	if output, ok := watchDetailValue(watchScreen(history.snapshot(), history.columns), "Output"); !ok || !regexp.MustCompile(`^3h( \d+m)? ago$`).MatchString(output) {
		t.Fatalf("replayed rows moved output freshness: got %q (found=%t), want the journal's modification time, about 3h ago", output, ok)
	}
	// A long model name is shortened before the harness, so match its ends.
	waitCardRow(t, history, "CARD-3", 2, `"history-alias → …" ending in " · harness unknown"`, func(row string) bool {
		return strings.HasPrefix(row, "history-alias → ") && strings.HasSuffix(row, " · harness unknown")
	})
	waitCardRow(t, history, "CARD-3", 3, "4 turns and $1.25 on the state row", func(row string) bool {
		return strings.Contains(row, "· 4 turns") && strings.Contains(row, "· $1.25")
	})
	quitHistory()
	mu.Lock()
	t.Logf("compiled identity smoke completed: session polls=%d GET requests=%d", polls, gets)
	mu.Unlock()
}
