package smokes

// step5_af_daemon_operator_endpoints_honest_test.go — customer-visible
// Wave 11 acceptance criterion. Validates that the four daemon control
// endpoints Wave 9 shipped now reflect real daemon state end-to-end,
// configurable via daemon.yaml, against a real `af daemon` binary.
//
// Per WAVE11_PLAN.md § "Phase 8 — Validation + acceptance" + Q5: the
// explicit test name lands as drafted so future failures grep cleanly.
//
// What this exercises end-to-end against a real `af daemon run` process:
//
//   1. S4 — daemon.yaml `kit.scanPaths` wire-up.
//      A pre-written daemon.yaml carries `kit.scanPaths: [<temp-kit-dir>]`
//      pointing at a fake .kit.toml. After daemon start,
//      GET /api/daemon/kits returns the fake kit — proving the config
//      flows through Server.kitRegistryOrEmpty → KitRegistry.scan.
//
//   2. S5 — Workarea pool live-view wire-up.
//      A session is injected via POST /api/daemon/sessions (the same
//      no-orchestrator queued-work path Phase 7d's TestAfAgentRunSmoke
//      uses). After acceptance, GET /api/daemon/workareas returns at
//      least one entry in the Active[] slice with the spawned session's
//      Repository / Ref / SessionID — proving WorkerSpawner's
//      ActiveWorkareas() projection is connected to the operator surface.
//
//   3. S6a — Routing decision recording.
//      The same session's recorded routing decision is read back via
//      GET /api/daemon/routing/explain/<sessionID> with ChosenSandbox=local,
//      a non-empty ChosenLLM, and ≥1 trace step — proving the
//      SessionEventStarted listener fires synchronously and writes to the
//      RoutingTraceStore the operator surface reads.
//
// Skip-mode: honours RENSEI_SMOKES_SKIP_LIVE_DAEMON=1 + -short, matching
// step1-step4's pattern.
//
// Timing: warm cache ~2-3s (single live spin-up + three HTTP calls; the
// build cache + healthz wait dominate). Cold cache adds 60-90s for the
// af binary build.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/agentfactory-smokes/harness"
)

// TestAfDaemonOperatorEndpointsHonestEndToEnd is the Wave 11 customer-
// visible acceptance criterion. Drives a single live `af daemon run`
// through S4 (kit scan-paths), S5 (workarea live-pool view), and S6a
// (routing decision recording) end-to-end.
func TestAfDaemonOperatorEndpointsHonestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end live-daemon test; skipped under -short")
	}
	if os.Getenv("RENSEI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("RENSEI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of the live-daemon smoke")
	}

	// Build af from the sibling agentfactory-tui checkout. Cold cache
	// 60-90s; warm sub-second.
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()

	binDir := t.TempDir()
	afBinary, err := afh.BuildAfBinary(buildCtx, afh.BuildOptions{
		OutputPath: filepath.Join(binDir, "af"),
		Env:        append(os.Environ(), "GOWORK="),
	})
	if err != nil {
		if strings.Contains(err.Error(), "resolve ../") ||
			strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("live-daemon af binary unavailable: %v", err)
		}
		t.Fatalf("build af binary: %v", err)
	}

	port, err := afh.PickFreePort()
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}

	daemonHome := t.TempDir()

	// S4 setup — write a minimal-valid .kit.toml under a dedicated kit
	// scan dir. The TOML schema mirrors 005-kit-manifest-spec.md; the
	// fields the daemon actually reads to surface this kit on
	// /api/daemon/kits are kit.id (required, drops the kit otherwise via
	// kit_registry.go's "manifest missing kit.id" warn-and-skip),
	// kit.name, kit.version. The api field is parsed but not validated.
	kitDir := filepath.Join(daemonHome, "smoke-kits")
	if err := os.MkdirAll(kitDir, 0o700); err != nil {
		t.Fatalf("mkdir kit dir: %v", err)
	}
	kitManifestPath := filepath.Join(kitDir, "smoke-fake-kit.kit.toml")
	const kitManifestTOML = `api = "rensei.dev/v1"

[kit]
id = "smoke-fake-kit"
name = "Fake Kit"
version = "0.0.1"
description = "Fixture kit for TestAfDaemonOperatorEndpointsHonestEndToEnd."
`
	if err := os.WriteFile(kitManifestPath, []byte(kitManifestTOML), 0o600); err != nil {
		t.Fatalf("write kit manifest: %v", err)
	}

	// Pre-write daemon.yaml under <HOME>/.rensei/daemon.yaml. Mirrors
	// step4_af_agent_run_test.go's allowlist + orchestrator stub setup
	// and adds the S4 kit.scanPaths block pointing at the kit dir above.
	daemonYAMLDir := filepath.Join(daemonHome, ".rensei")
	if err := os.MkdirAll(daemonYAMLDir, 0o700); err != nil {
		t.Fatalf("mkdir daemon yaml dir: %v", err)
	}
	daemonYAMLPath := filepath.Join(daemonYAMLDir, "daemon.yaml")
	daemonYAML := fmt.Sprintf(`apiVersion: rensei.dev/v1
kind: LocalDaemon
machine:
  id: smoke-machine
capacity:
  maxConcurrentSessions: 2
  maxVCpuPerSession: 2
  maxMemoryMbPerSession: 2048
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
projects:
  - id: smoke-alpha
    repository: github.com/foo/rensei-smokes-alpha
    cloneStrategy: shallow
orchestrator:
  url: http://127.0.0.1:1
autoUpdate:
  channel: stable
  schedule: manual
  drainTimeoutSeconds: 5
kit:
  scanPaths:
    - %s
`, kitDir)
	if err := os.WriteFile(daemonYAMLPath, []byte(daemonYAML), 0o600); err != nil {
		t.Fatalf("write daemon.yaml: %v", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	logBuf := afh.NewLogTail(64 * 1024)

	startCtx, startCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startCancel()

	live, err := afh.SpawnDaemon(startCtx, afh.SpawnOptions{
		Binary: afBinary,
		Args: []string{
			"daemon", "run",
			"--port", fmt.Sprintf("%d", port),
			"--skip-wizard",
		},
		Env: []string{
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
			"HOME=" + daemonHome,
			"XDG_CONFIG_HOME=" + filepath.Join(daemonHome, ".config"),
			"RENSEI_DAEMON_FORCE_STUB=1",
			"RENSEI_LOG_DIR=" + filepath.Join(daemonHome, ".rensei", "logs"),
			"NO_COLOR=1",
		},
		HomeDir:        daemonHome,
		LogSink:        logBuf,
		HealthzBaseURL: url,
		HealthzTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("spawn af daemon: %v\n--- daemon log tail ---\n%s", err, logBuf.String())
	}
	t.Cleanup(live.Stop)
	t.Logf("af daemon up at %s (pid %d, port %d)", live.URL, live.Cmd.Process.Pid, live.Port())

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// ─── S4: GET /api/daemon/kits returns the fake kit ──────────────────
	//
	// The daemon's KitRegistry rescans on every List call, so by the time
	// /healthz returned 200 the registry will see the manifest. No poll
	// needed.
	{
		kitsCtx, kitsCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer kitsCancel()
		req, err := http.NewRequestWithContext(kitsCtx, http.MethodGet,
			live.URL+"/api/daemon/kits", nil)
		if err != nil {
			t.Fatalf("build kits request: %v", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/daemon/kits: %v\n--- daemon log tail ---\n%s",
				err, logBuf.String())
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/daemon/kits status = %d, want 200\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				resp.StatusCode, body, logBuf.String())
		}

		// Decode the ListKitsResponse envelope. We only assert the load-
		// bearing fields — the fake kit's id surfacing here is the proof
		// that daemon.yaml's kit.scanPaths was consumed end-to-end (the
		// default scan path ~/.rensei/kits doesn't exist under the
		// hermetic HOME, so the only way this id appears is via the
		// override).
		var kitsResp struct {
			Kits []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Version string `json:"version"`
				Source  string `json:"source"`
				Status  string `json:"status"`
			} `json:"kits"`
		}
		if err := json.Unmarshal(body, &kitsResp); err != nil {
			t.Fatalf("decode ListKitsResponse: %v\n--- body ---\n%s", err, body)
		}
		var foundKit bool
		for _, k := range kitsResp.Kits {
			if k.ID == "smoke-fake-kit" {
				foundKit = true
				if k.Name != "Fake Kit" {
					t.Errorf("kit.Name = %q, want %q", k.Name, "Fake Kit")
				}
				if k.Version != "0.0.1" {
					t.Errorf("kit.Version = %q, want %q", k.Version, "0.0.1")
				}
				if k.Source != "local" {
					t.Errorf("kit.Source = %q, want local (manifest came from kit.scanPaths)", k.Source)
				}
				if k.Status != "active" {
					t.Errorf("kit.Status = %q, want active (no .state.json, kit not disabled)", k.Status)
				}
				break
			}
		}
		if !foundKit {
			t.Fatalf("GET /api/daemon/kits did not return smoke-fake-kit; got %d kits\n--- body ---\n%s",
				len(kitsResp.Kits), body)
		}
		t.Logf("S4 verified: GET /api/daemon/kits surfaced smoke-fake-kit from kit.scanPaths=%s", kitDir)
	}

	// ─── Inject a session via POST /api/daemon/sessions (S5/S6a setup) ──
	//
	// Same shape Phase 7d's step4 uses: minimal SessionSpec carrying
	// repository="smoke-alpha" (matches the allowlist entry's id) +
	// ref="main". WorkerSpawner.AcceptWork validates the allowlist,
	// finds capacity, registers the session, and synchronously fires
	// SessionEventStarted before returning the handle — which means by
	// the time POST returns 202, the routing trace recording has already
	// happened on the same goroutine.
	sessionID := fmt.Sprintf("smoke-operator-endpoints-%d-%d",
		live.Port(), time.Now().UnixMilli())
	specBody := map[string]any{
		"sessionId":  sessionID,
		"repository": "smoke-alpha",
		"ref":        "main",
	}
	specBytes, err := json.Marshal(specBody)
	if err != nil {
		t.Fatalf("marshal session spec: %v", err)
	}

	postCtx, postCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer postCancel()
	postReq, err := http.NewRequestWithContext(postCtx, http.MethodPost,
		live.URL+"/api/daemon/sessions", bytes.NewReader(specBytes))
	if err != nil {
		t.Fatalf("build accept-work request: %v", err)
	}
	postReq.Header.Set("Content-Type", "application/json")

	postResp, err := httpClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST /api/daemon/sessions: %v\n--- daemon log tail ---\n%s",
			err, logBuf.String())
	}
	postBody, _ := io.ReadAll(postResp.Body)
	_ = postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/daemon/sessions status = %d, want 202\n--- body ---\n%s\n--- daemon log tail ---\n%s",
			postResp.StatusCode, postBody, logBuf.String())
	}
	t.Logf("session accepted: id=%s", sessionID)

	// ─── S5: GET /api/daemon/workareas returns the live-pool entry ──────
	//
	// WorkerSpawner.ActiveWorkareas() is invoked from
	// WorkareaArchiveRegistry's ActiveProvider hook, which the workareas
	// handler uses to populate the response's Active[] slice. The
	// projection is pull-based, so by the time the spawner has registered
	// the session (the POST already returned 202) the entry is visible.
	// Brief poll-loop to stay resilient against any future tweak that
	// makes the registration order matter.
	{
		var (
			workareasStatus int
			workareasBody   []byte
			foundActive     bool
		)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			getCtx, getCancel := context.WithTimeout(context.Background(), 2*time.Second)
			req, rerr := http.NewRequestWithContext(getCtx, http.MethodGet,
				live.URL+"/api/daemon/workareas", nil)
			if rerr != nil {
				getCancel()
				t.Fatalf("build workareas request: %v", rerr)
			}
			resp, gerr := httpClient.Do(req)
			if gerr != nil {
				getCancel()
				time.Sleep(50 * time.Millisecond)
				continue
			}
			workareasStatus = resp.StatusCode
			workareasBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			getCancel()
			if workareasStatus != http.StatusOK {
				time.Sleep(50 * time.Millisecond)
				continue
			}

			// Decode the ListWorkareasResponse. Active[] is the slice of
			// live pool members; Archived[] is the on-disk archives.
			// We only assert the active half — archives aren't part of
			// the S5 contract.
			var listResp struct {
				Active []struct {
					Kind       string `json:"kind"`
					SessionID  string `json:"sessionId"`
					Repository string `json:"repository"`
					Ref        string `json:"ref"`
					Status     string `json:"status"`
				} `json:"active"`
				Archived []json.RawMessage `json:"archived"`
			}
			if err := json.Unmarshal(workareasBody, &listResp); err != nil {
				t.Fatalf("decode ListWorkareasResponse: %v\n--- body ---\n%s",
					err, workareasBody)
			}
			for _, w := range listResp.Active {
				if w.SessionID != sessionID {
					continue
				}
				foundActive = true
				if w.Kind != "active" {
					t.Errorf("workarea.Kind = %q, want active", w.Kind)
				}
				if w.Repository != "smoke-alpha" {
					t.Errorf("workarea.Repository = %q, want smoke-alpha (allowlist id)",
						w.Repository)
				}
				if w.Ref != "main" {
					t.Errorf("workarea.Ref = %q, want main", w.Ref)
				}
				break
			}
			if foundActive {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		if workareasStatus != http.StatusOK {
			t.Fatalf("GET /api/daemon/workareas never reached 200; last status = %d\n--- last body ---\n%s\n--- daemon log tail ---\n%s",
				workareasStatus, workareasBody, logBuf.String())
		}
		if !foundActive {
			t.Fatalf("GET /api/daemon/workareas Active[] never contained session %q\n--- last body ---\n%s\n--- daemon log tail ---\n%s",
				sessionID, workareasBody, logBuf.String())
		}
		t.Logf("S5 verified: GET /api/daemon/workareas Active[] contains session %s (repository=smoke-alpha ref=main)",
			sessionID)
	}

	// ─── S6a: GET /api/daemon/routing/explain/<sessionID> ────────────────
	//
	// The Wave 11 / S6a SessionEventStarted listener records the
	// degenerate "always pick local" decision synchronously before
	// AcceptWork returns the handle, so by POST return the recording is
	// already visible. Brief poll for resilience (mirrors step4's
	// pattern).
	{
		explainURL := live.URL + "/api/daemon/routing/explain/" + sessionID
		deadline := time.Now().Add(5 * time.Second)
		var explainStatus int
		var explainBody []byte
		for time.Now().Before(deadline) {
			getCtx, getCancel := context.WithTimeout(context.Background(), 2*time.Second)
			req, rerr := http.NewRequestWithContext(getCtx, http.MethodGet, explainURL, nil)
			if rerr != nil {
				getCancel()
				t.Fatalf("build explain request: %v", rerr)
			}
			resp, gerr := httpClient.Do(req)
			if gerr != nil {
				getCancel()
				time.Sleep(50 * time.Millisecond)
				continue
			}
			explainStatus = resp.StatusCode
			explainBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			getCancel()
			if explainStatus == http.StatusOK {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		if explainStatus != http.StatusOK {
			t.Fatalf("GET %s never reached 200; last status = %d\n--- last body ---\n%s\n--- daemon log tail ---\n%s",
				explainURL, explainStatus, explainBody, logBuf.String())
		}

		// Decode the RoutingExplainResponse. Per
		// agentfactory-tui/afclient/routing_types.go:
		//   { sessionId, decision: { sessionId, chosenSandbox, chosenLLM, decidedAt },
		//     trace: [ { step, phase, dimension, remaining, note } ] }
		var explain struct {
			SessionID string `json:"sessionId"`
			Decision  struct {
				SessionID     string `json:"sessionId"`
				ChosenSandbox string `json:"chosenSandbox"`
				ChosenLLM     string `json:"chosenLLM"`
				DecidedAt     string `json:"decidedAt"`
			} `json:"decision"`
			Trace []struct {
				Step      int      `json:"step"`
				Phase     string   `json:"phase"`
				Dimension string   `json:"dimension"`
				Remaining []string `json:"remaining"`
			} `json:"trace"`
		}
		if err := json.Unmarshal(explainBody, &explain); err != nil {
			t.Fatalf("decode RoutingExplainResponse: %v\n--- body ---\n%s",
				err, explainBody)
		}
		if explain.SessionID != sessionID {
			t.Errorf("RoutingExplainResponse.SessionID = %q, want %q",
				explain.SessionID, sessionID)
		}
		if explain.Decision.SessionID != sessionID {
			t.Errorf("Decision.SessionID = %q, want %q",
				explain.Decision.SessionID, sessionID)
		}
		if explain.Decision.ChosenSandbox != "local" {
			t.Errorf("Decision.ChosenSandbox = %q, want local (OSS daemon ships single sandbox)",
				explain.Decision.ChosenSandbox)
		}
		if explain.Decision.ChosenLLM == "" {
			t.Errorf("Decision.ChosenLLM empty, want a non-empty provider name (stub fallback when registry is nil)")
		}
		if len(explain.Trace) == 0 {
			t.Errorf("Trace is empty, want at least one step (capability-filter)")
		}
		t.Logf("S6a verified: routing decision recorded sandbox=%s llm=%s traceSteps=%d",
			explain.Decision.ChosenSandbox, explain.Decision.ChosenLLM, len(explain.Trace))
	}
}
