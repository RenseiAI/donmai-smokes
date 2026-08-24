package smokes

// step5_af_daemon_operator_endpoints_honest_test.go — customer-visible
// Wave 11 acceptance criterion. Validates that the four daemon control
// endpoints Wave 9 shipped now reflect real daemon state end-to-end,
// configurable via daemon.yaml, against a real `donmai daemon` binary.
//
// Per WAVE11_PLAN.md § "Phase 8 — Validation + acceptance" + Q5: the
// explicit test name lands as drafted so future failures grep cleanly.
//
// What this exercises end-to-end against a real `donmai daemon run` process:
//
//   1. S4 — daemon.yaml `kit.scanPaths` wire-up.
//      A pre-written daemon.yaml carries `kit.scanPaths: [<temp-kit-dir>]`
//      pointing at a fake .kit.toml. After daemon start,
//      GET /api/daemon/kits returns the fake kit — proving the config
//      flows through Server.kitRegistryOrEmpty → KitRegistry.scan.
//
//   2. S5 — Workarea pool live-view wire-up.
//      A local orchestrator double returns one real poll item, driving the
//      daemon's PollItemToSessionDetail → AcceptWorkWithDetail path. After
//      acceptance, GET /api/daemon/workareas returns at
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
// Skip-mode: honours DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 + -short, matching
// step1-step4's pattern.
//
// Timing: warm cache ~2-3s (single live spin-up + three HTTP calls; the
// build cache + healthz wait dominate). Cold cache adds 60-90s for the
// donmai binary build.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// TestAfDaemonOperatorEndpointsHonestEndToEnd is the Wave 11 customer-
// visible acceptance criterion. Drives a single live `donmai daemon run`
// through S4 (kit scan-paths), S5 (workarea live-pool view), and S6a
// (routing decision recording) end-to-end.
func TestAfDaemonOperatorEndpointsHonestEndToEnd(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end live-daemon test")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of the live-daemon smoke")
	afh.SkipIfToolMissing(t, "git", "the workarea poll item clones a local bare fixture")

	// S4 setup — write a minimal-valid .kit.toml under a dedicated kit
	// scan dir. The TOML schema mirrors 005-kit-manifest-spec.md; the
	// fields the daemon actually reads to surface this kit on
	// /api/daemon/kits are kit.id (required, drops the kit otherwise via
	// kit_registry.go's "manifest missing kit.id" warn-and-skip),
	// kit.name, kit.version. The api field is parsed but not validated.
	//
	// We allocate the kit dir from a separate t.TempDir() so it can be
	// interpolated into daemon.yaml BEFORE LiveDaemonWithConfig writes
	// the file under the daemon's HOME and spawns the process. The
	// path is absolute so it works regardless of where the daemon
	// resolves $HOME.
	kitDir := filepath.Join(t.TempDir(), "smoke-kits")
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

	repository := "file://" + afh.MakeBareFixtureRepo(t, "operator-endpoints")
	sessionID := fmt.Sprintf("smoke-operator-endpoints-%d", time.Now().UnixNano())
	const (
		workerID     = "worker-smoke-operator-endpoints"
		runtimeToken = "smoke.runtime.token"
	)
	registrationToken := "rs" + "k_live_smoke_operator_endpoints" //nolint:gosec // synthetic httptest credential
	var pollRequests atomic.Int32
	orchestrator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workers/register":
			if r.Header.Get("Authorization") != "Bearer "+registrationToken {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workerId": workerID, "runtimeToken": runtimeToken,
				"heartbeatInterval": 30000, "pollInterval": 1000,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workers/"+workerID+"/poll":
			if r.Header.Get("Authorization") != "Bearer "+runtimeToken {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if pollRequests.Add(1) != 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"work": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"work": []any{map[string]any{
				"sessionId": sessionID, "projectId": "smoke-alpha", "projectName": "smoke-alpha",
				"repository": repository, "requiresRepository": true, "ref": "main",
				"workType": "development", "promptContext": "operator endpoint smoke",
				"maxDurationSeconds": 60, "queuedAt": time.Now().UnixMilli(),
				"resolvedProfile": map[string]any{
					"provider":       "stub",
					"providerConfig": map[string]any{"stub.behavior": "hang-then-timeout"},
				},
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/workers/"+workerID+"/heartbeat":
			_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/lock-refresh"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "refreshed": true})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/sessions/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(orchestrator.Close)

	// daemon.yaml mirrors step4_af_agent_run_test.go's allowlist +
	// local orchestrator setup and adds the S4 kit.scanPaths block
	// pointing at the kit dir above. LiveDaemonWithConfig writes this
	// under <home>/.donmai/daemon.yaml before spawn — LoadConfig reads
	// it BEFORE the wizard fallback in daemon.Start.
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
    repository: %q
    cloneStrategy: shallow
orchestrator:
  url: %q
  authToken: %q
autoUpdate:
  channel: stable
  schedule: manual
  drainTimeoutSeconds: 5
kit:
  scanPaths:
    - %s
`, repository, orchestrator.URL, registrationToken, kitDir)

	live, _, logBuf, _ := afh.LiveDaemonWithConfig(t, daemonYAML, "DONMAI_DAEMON_FORCE_STUB=0")

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
		// default scan path ~/.donmai/kits doesn't exist under the
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

	// ─── Real poll / AcceptWorkWithDetail setup for S5 and S6a ────
	//
	// The child process requires the full SessionDetail that only the worker
	// poll path supplies. Waiting for the detail endpoint to return 200 is the
	// discriminating control: the former direct AcceptWork path returned 202,
	// but the child then failed its detail preflight with 404.
	{
		var (
			detailStatus int
			detailBody   []byte
		)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			getCtx, getCancel := context.WithTimeout(context.Background(), 2*time.Second)
			req, err := http.NewRequestWithContext(getCtx, http.MethodGet,
				live.URL+"/api/daemon/sessions/"+sessionID, nil)
			if err != nil {
				getCancel()
				t.Fatalf("build session-detail request: %v", err)
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				getCancel()
				time.Sleep(50 * time.Millisecond)
				continue
			}
			detailStatus = resp.StatusCode
			detailBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			getCancel()
			if detailStatus == http.StatusOK {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if detailStatus != http.StatusOK {
			t.Fatalf("session detail %s never reached 200; last status=%d\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				sessionID, detailStatus, detailBody, logBuf.String())
		}
		var detail struct {
			SessionID  string `json:"sessionId"`
			ProjectID  string `json:"projectId"`
			Repository string `json:"repository"`
			Ref        string `json:"ref"`
		}
		if err := json.Unmarshal(detailBody, &detail); err != nil {
			t.Fatalf("decode SessionDetail: %v\n--- body ---\n%s", err, detailBody)
		}
		if detail.SessionID != sessionID || detail.ProjectID != "smoke-alpha" ||
			detail.Repository != repository || detail.Ref != "main" {
			t.Errorf("SessionDetail = %+v, want session=%q project=smoke-alpha repository=%q ref=main",
				detail, sessionID, repository)
		}
		t.Logf("real poll accepted session with durable detail: id=%s repository=%s", sessionID, repository)
	}

	// ─── S5: GET /api/daemon/workareas returns the live-pool entry ──────
	//
	// WorkerSpawner.ActiveWorkareas() is invoked from
	// WorkareaArchiveRegistry's ActiveProvider hook, which the workareas
	// handler uses to populate the response's Active[] slice. The
	// projection is pull-based, so after the detail endpoint proves the
	// spawner registered the polled session the entry is visible.
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
				if w.Repository != repository {
					t.Errorf("workarea.Repository = %q, want %q (polled repository resource)",
						w.Repository, repository)
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
		t.Logf("S5 verified: GET /api/daemon/workareas Active[] contains session %s (repository=%s ref=main)",
			sessionID, repository)
	}

	// ─── S6a: GET /api/daemon/routing/explain/<sessionID> ────────────────
	//
	// The Wave 11 / S6a SessionEventStarted listener records the
	// degenerate "always pick local" decision synchronously before
	// AcceptWorkWithDetail returns the handle, so once SessionDetail is
	// visible the recording is already available. Brief poll for resilience
	// (mirrors step4's pattern).
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
		// donmai/afclient/routing_types.go:
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
