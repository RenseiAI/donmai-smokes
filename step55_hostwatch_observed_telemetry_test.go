package smokes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestHostWatchObservedTelemetryFromJournal drives the compiled OSS CLI
// against a local daemon index and native-compatible, correlated agent event
// rows. It tests the host-watch reader; it does not run a Pi binary or claim
// that a fixture is a paid provider observation.
func TestHostWatchObservedTelemetryFromJournal(t *testing.T) {
	afh.SkipIfShort(t, "compiled host-watch observed telemetry smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	binary, source := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: source, Timeout: 8 * time.Minute})
	t.Logf("compiled observed-telemetry SUT from %s", source)

	root := t.TempDir()
	home := filepath.Join(root, "home")
	worktree := filepath.Join(root, "session")
	agentDir := filepath.Join(worktree, ".agent")
	for _, dir := range []string{home, agentDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	journal := filepath.Join(agentDir, "events.jsonl")
	if err := os.WriteFile(journal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(agentDir, "state.json")
	started := time.Now().Add(-time.Minute).UnixMilli()
	writeState := func(start, offset, heartbeat int64) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"sessionId": "telemetry-session", "issueIdentifier": "OBSERVED-1",
			"startedAt": start, "eventLogStartOffset": offset, "lastHeartbeat": heartbeat,
		})
		if err != nil {
			t.Fatal(err)
		}
		next := statePath + ".next"
		if err := os.WriteFile(next, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, statePath); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent := func(event map[string]any) {
		t.Helper()
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(journal, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write(append(body, '\n'))
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("append correlated event: write=%v close=%v", writeErr, closeErr)
		}
	}
	writeState(started, 0, time.Now().Add(-3*time.Second).UnixMilli())

	var mu sync.Mutex
	sessionPolls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("host-watch attempted mutation: %s %s", request.Method, request.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var value any
		switch request.URL.Path {
		case "/api/daemon/sessions":
			mu.Lock()
			sessionPolls++
			mu.Unlock()
			value = []map[string]any{{
				"sessionId": "telemetry-session", "state": "running", "worktreePath": worktree,
				"projectName": "fixture", "repository": "example/project",
				"harness": "pi", "model": "muse-spark-1.3", "modelProvider": "openai",
				"modelAuthor": "meta", "endpointOperator": "gateway", "protocol": "openai-chat",
			}}
		case "/api/daemon/status":
			value = map[string]any{"status": "ready", "activeSessions": 1, "maxSessions": 2}
		case "/api/daemon/stats":
			value = map[string]any{"queueDepth": 0}
		default:
			t.Errorf("unexpected local daemon read: %s", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Errorf("encode local daemon read: %v", err)
		}
	}))
	defer server.Close()

	watchCard := func() string {
		t.Helper()
		mu.Lock()
		before := sessionPolls
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 3400*time.Millisecond)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "host", "watch", "--all", "--plain", "--daemon-url", server.URL) //nolint:gosec // compiled SUT and owned loopback fixture
		command.Dir = root
		command.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + root}
		raw, err := command.CombinedOutput()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("plain host-watch exited before bounded observation: err=%v output=%s", err, raw)
		}
		mu.Lock()
		polls := sessionPolls - before
		mu.Unlock()
		if polls < 2 {
			t.Fatalf("plain host-watch made %d index polls; need two refreshes: %s", polls, raw)
		}
		text := string(raw)
		marker := strings.LastIndex(text, "OBSERVED-1")
		if marker < 0 {
			t.Fatalf("actual CLI did not render session card: %s", text)
		}
		card := text[marker:]
		if end := strings.Index(card, "session stream"); end >= 0 {
			card = card[:end]
		}
		return card
	}
	require := func(card string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(card, want) {
				t.Fatalf("current plain card missing %q: %s", want, card)
			}
		}
	}

	initial := watchCard()
	require(initial, "Model author meta", "Endpoint operator gateway", "Endpoint surface openai",
		"Protocol openai-chat", "Actual provider unknown", "cost not reported", "turns not reported")
	if strings.Contains(initial, "heartbeat never") || strings.Contains(initial, "Actual provider meta") ||
		strings.Contains(initial, "Actual provider openai") {
		t.Fatalf("state heartbeat or actual-provider distinction lost: %s", initial)
	}

	// A provider-reported zero price need not imply a completed turn.
	appendEvent(map[string]any{
		"kind":        "llm_call", // local journal observation with optional span upload disabled
		"usageSource": "provider", "observedCostUsd": 0, "turnCompleted": false,
	})
	zero := watchCard()
	require(zero, "cost $0.00", "turns not reported", "Actual provider unknown")

	// A native completed turn can independently lack a cost observation.
	appendEvent(map[string]any{
		"kind": "llm_call", "spanId": "1000000000000002",
		"usageSource": "provider", "turnCompleted": true,
	})
	turn := watchCard()
	require(turn, "cost $0.00", "turns 1", "Actual provider unknown")

	// Duplicate correlated calls count once; terminal cumulative values
	// replace the provisional live sum rather than adding to it.
	call := map[string]any{
		"kind": "llm_call", "spanId": "1000000000000003",
		"usageSource": "provider", "observedCostUsd": 0.5, "turnCompleted": true,
	}
	appendEvent(call)
	appendEvent(call)
	live := watchCard()
	require(live, "cost $0.50", "turns 2")
	appendEvent(map[string]any{
		"kind": "result", "success": true,
		"observedCostUsd": 0.25, "observedTurns": 2,
	})
	late := watchCard() // new CLI process attaches after the terminal row
	require(late, "cost $0.25", "turns 2")
	if strings.Contains(late, "cost $0.75") || strings.Contains(late, "turns 4") {
		t.Fatalf("terminal aggregate inflated live observations: %s", late)
	}

	info, err := os.Stat(journal)
	if err != nil {
		t.Fatal(err)
	}
	writeState(time.Now().UnixMilli(), info.Size(), 0) // same path and ID, new run
	reset := watchCard()
	require(reset, "cost not reported", "turns not reported", "heartbeat never", "Actual provider unknown")
	if strings.Contains(reset, "cost $0.25") || strings.Contains(reset, "turns 2") {
		t.Fatalf("new run inherited old journal metrics: %s", reset)
	}
	appendEvent(map[string]any{
		"kind": "llm_call", "spanId": "2000000000000001",
		"usageSource": "provider", "observedCostUsd": 0, "turnCompleted": true,
	})
	fresh := watchCard()
	require(fresh, "cost $0.00", "turns 1", "heartbeat never")
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled plain host-watch read local daemon index, fenced journal and state across live, terminal, late-attach and reused-run phases")
}
