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
	"regexp"
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

	// The piped card is five rows: identity, repository and work type, model
	// and harness, state with elapsed time (and turns and cost once reported),
	// and the last activity. Everything else is in the session's detail pane,
	// which the pipe does not render, so the same phases are read in a PTY.
	var (
		turnsReported = regexp.MustCompile(`· (\d+) turns`)
		costReported  = regexp.MustCompile(`· (\$\d+\.\d\d)`)
	)
	watchCard := func() [watchCardRows]string {
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
		card, ok := watchPlainCard(string(raw), "OBSERVED-1")
		if !ok {
			t.Fatalf("actual CLI did not render session card: %s", raw)
		}
		if card[1] != "project · work type unknown" || card[2] != "muse-spark-1.3 · pi" || !strings.HasPrefix(card[3], "running") {
			t.Fatalf("plain card lost its repository, model/harness or state row: %q", card)
		}
		return card
	}
	// Turns and cost appear on the state row only once reported, never as zero.
	requireCounts := func(card [watchCardRows]string, turns, cost string) {
		t.Helper()
		var gotTurns, gotCost string
		if m := turnsReported.FindStringSubmatch(card[3]); m != nil {
			gotTurns = m[1]
		}
		if m := costReported.FindStringSubmatch(card[3]); m != nil {
			gotCost = m[1]
		}
		if gotTurns != turns || gotCost != cost {
			t.Fatalf("state row %q reports turns %q cost %q, want turns %q cost %q (empty means not reported)", card[3], gotTurns, gotCost, turns, cost)
		}
	}
	watchDetail := func(want ...watchField) []string {
		t.Helper()
		terminal, capture, quit := startWatchPTY(t, binary, root, home, server.URL, watchColumns, "--all")
		waitWatchCard(t, capture, "OBSERVED-1")
		if _, err := terminal.Write([]byte("\r")); err != nil {
			t.Fatal(err)
		}
		lines := waitWatchDetail(t, capture, func() []string { return watchScreen(capture.snapshot(), capture.columns) }, "OBSERVED-1", want...)
		quit()
		return lines
	}
	heartbeat := func(lines []string) string {
		value, _ := watchDetailValue(lines, "Heartbeat")
		return value
	}

	initial := watchCard()
	requireCounts(initial, "", "")
	detail := watchDetail(field("Model author", "meta"), field("Endpoint operator", "gateway"), field("Endpoint surface", "openai"),
		field("Protocol", "openai-chat"), field("Actual provider", "unknown"), field("Cost", "not reported"), field("Turns", "not reported"))
	if got := heartbeat(detail); got == "never" || !strings.HasSuffix(got, " ago") {
		t.Fatalf("state heartbeat lost: Heartbeat = %q", got)
	}

	// A provider-reported zero price need not imply a completed turn.
	appendEvent(map[string]any{
		"kind":        "llm_call", // local journal observation with optional span upload disabled
		"usageSource": "provider", "observedCostUsd": 0, "turnCompleted": false,
	})
	requireCounts(watchCard(), "", "$0.00")
	watchDetail(field("Cost", "$0.00"), field("Turns", "not reported"), field("Actual provider", "unknown"))

	// A native completed turn can independently lack a cost observation.
	appendEvent(map[string]any{
		"kind": "llm_call", "spanId": "1000000000000002",
		"usageSource": "provider", "turnCompleted": true,
	})
	requireCounts(watchCard(), "1", "$0.00")
	watchDetail(field("Cost", "$0.00"), field("Turns", "1"), field("Actual provider", "unknown"))

	// Duplicate correlated calls count once; terminal cumulative values
	// replace the provisional live sum rather than adding to it.
	call := map[string]any{
		"kind": "llm_call", "spanId": "1000000000000003",
		"usageSource": "provider", "observedCostUsd": 0.5, "turnCompleted": true,
	}
	appendEvent(call)
	appendEvent(call)
	requireCounts(watchCard(), "2", "$0.50")
	appendEvent(map[string]any{
		"kind": "result", "success": true,
		"observedCostUsd": 0.25, "observedTurns": 2,
	})
	// A new CLI process attaches after the terminal row. Exact counts also
	// prove the aggregate did not inflate the live observations to $0.75 / 4.
	requireCounts(watchCard(), "2", "$0.25")

	info, err := os.Stat(journal)
	if err != nil {
		t.Fatal(err)
	}
	writeState(time.Now().UnixMilli(), info.Size(), 0) // same path and ID, new run
	// A new run inherits nothing: no counts from the old journal, no heartbeat.
	requireCounts(watchCard(), "", "")
	watchDetail(field("Cost", "not reported"), field("Turns", "not reported"), field("Heartbeat", "never"), field("Actual provider", "unknown"))
	appendEvent(map[string]any{
		"kind": "llm_call", "spanId": "2000000000000001",
		"usageSource": "provider", "observedCostUsd": 0, "turnCompleted": true,
	})
	requireCounts(watchCard(), "1", "$0.00")
	watchDetail(field("Cost", "$0.00"), field("Turns", "1"), field("Heartbeat", "never"))
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled host-watch read local daemon index, fenced journal and state across live, terminal, late-attach and reused-run phases, as piped cards and detail panes")
}
