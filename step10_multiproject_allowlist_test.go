package smokes

// step10_multiproject_allowlist_test.go — GAP-01 coverage.
//
// Proves the donmai v0.25.0 multi-project allowlist (AddProjects /
// AllProjects + the findProjectLocked union) at the OSS level.
//
// What this exercises end-to-end against a real `donmai daemon run` process:
//
//  1. Pre-write a daemon.yaml with TWO distinct projects[] entries
//     (smoke-beta and smoke-gamma) — distinct id AND distinct repository
//     URL. This proves the yaml parser accepts multiple entries and that
//     both survive into the daemon's allowlist (not silently deduplicated
//     or truncated).
//
//  2. Confirm /api/daemon/stats reports both repository URLs in its
//     AllowedProjects slice. This is the primary "allowlist is present"
//     assertion — it exercises the safeProjectRepos → DaemonStatsResponse
//     wire path end-to-end.
//
//  3. Inject a session for project BETA (repository matched by id) via
//     POST /api/daemon/sessions and assert the daemon returns HTTP 202
//     (allowlist matcher accepted it, not "not in the project allowlist").
//
//  4. Inject a second session for project GAMMA (different id + repository)
//     and assert HTTP 202 as well. This proves the daemon's
//     findProjectLocked searches the full union, not just the first entry.
//
//  5. Verify both accepted sessions appear in GET /api/daemon/workareas
//     Active[] with their correct repository tags — the live-pool view
//     remains accurate across multiple concurrent sessions.
//
// Platform-free: no WorkOS, no Linear, no /api/cli/*, no rsk_* tokens.
// All assertions drive only /api/daemon/* on 127.0.0.1:<free-port>.
//
// Skip-mode: honours DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 and -short,
// matching the step1–step5 pattern.
//
// Timing: ~2-3s warm (single live spin-up + five HTTP calls; binary
// build dominates on cold cache: 60-90s).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestMultiProjectAllowlistRouting is the GAP-01 acceptance test.
// It spins up a daemon pre-configured with two project allowlist entries
// and proves that sessions for BOTH projects are accepted and appear in
// the workareas live-pool view.
func TestMultiProjectAllowlistRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end live-daemon test; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of the live-daemon smoke")
	}

	// daemon.yaml carries TWO distinct project entries — different id AND
	// different repository URL. MaxConcurrentSessions=4 gives enough room
	// for both injected sessions without an at-capacity rejection.
	//
	// orchestrator.url points at a localhost port that can't be reached;
	// DONMAI_DAEMON_FORCE_STUB=1 (set by LiveDaemonWithConfig's hermetic
	// env) ensures the daemon takes the stub registration path instead of
	// dialling out, so this URL is never opened.
	const daemonYAML = `apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: smoke-multiproject
capacity:
  maxConcurrentSessions: 4
  maxVCpuPerSession: 2
  maxMemoryMbPerSession: 2048
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
projects:
  - id: smoke-beta
    repository: github.com/foo/rensei-smokes-beta
    cloneStrategy: shallow
  - id: smoke-gamma
    repository: github.com/foo/rensei-smokes-gamma
    cloneStrategy: shallow
orchestrator:
  url: http://127.0.0.1:1
autoUpdate:
  channel: stable
  schedule: manual
  drainTimeoutSeconds: 5
`

	live, _, logBuf, _ := afh.LiveDaemonWithConfig(t, daemonYAML)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// ─── Assert allowlist appears in /api/daemon/stats ───────────────────
	//
	// safeProjectRepos → DaemonStatsResponse.AllowedProjects carries the
	// list of repository strings from daemon.yaml. We verify both repos
	// are present so any yaml-parse or config-load truncation is caught.
	t.Run("stats_reports_both_projects", func(t *testing.T) {
		statsCtx, statsCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer statsCancel()

		req, err := http.NewRequestWithContext(statsCtx, http.MethodGet,
			live.URL+"/api/daemon/stats", nil)
		if err != nil {
			t.Fatalf("build stats request: %v", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/daemon/stats: %v\n--- daemon log tail ---\n%s",
				err, logBuf.String())
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/daemon/stats status = %d, want 200\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				resp.StatusCode, body, logBuf.String())
		}

		var statsResp struct {
			AllowedProjects []string `json:"allowedProjects"`
		}
		if err := json.Unmarshal(body, &statsResp); err != nil {
			t.Fatalf("decode DaemonStatsResponse: %v\n--- body ---\n%s", err, body)
		}

		want := map[string]bool{
			"github.com/foo/rensei-smokes-beta":  false,
			"github.com/foo/rensei-smokes-gamma": false,
		}
		for _, repo := range statsResp.AllowedProjects {
			if _, known := want[repo]; known {
				want[repo] = true
			}
		}
		for repo, found := range want {
			if !found {
				t.Errorf("AllowedProjects missing %q; got %v\n--- body ---\n%s",
					repo, statsResp.AllowedProjects, body)
			}
		}
		t.Logf("allowlist verified: AllowedProjects=%v (len=%d)",
			statsResp.AllowedProjects, len(statsResp.AllowedProjects))
	})

	// ─── Inject sessions for both projects ───────────────────────────────
	//
	// WorkerSpawner.findProjectLocked matches by either p.ID or p.Repository.
	// We use the project id as the Repository value in the spec (matching
	// step4/step5's precedent) — the matcher accepts it and returns the
	// allowlist entry without requiring a full GitHub URL.
	ts := time.Now().UnixMilli()
	sessionBeta := fmt.Sprintf("smoke-multiproj-beta-%d-%d", live.Port(), ts)
	sessionGamma := fmt.Sprintf("smoke-multiproj-gamma-%d-%d", live.Port(), ts+1)

	inject := func(t *testing.T, sessionID, repository string) {
		t.Helper()
		specBody := map[string]any{
			"sessionId":  sessionID,
			"repository": repository,
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
			t.Fatalf("POST /api/daemon/sessions for %q: %v\n--- daemon log tail ---\n%s",
				repository, err, logBuf.String())
		}
		postBody, _ := io.ReadAll(postResp.Body)
		_ = postResp.Body.Close()
		if postResp.StatusCode != http.StatusAccepted {
			// HTTP 400 carrying "not in the project allowlist" is the
			// regression the test guards. Surface the body so the failure
			// message is immediately actionable.
			t.Fatalf("POST /api/daemon/sessions for %q: status = %d, want 202\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				repository, postResp.StatusCode, postBody, logBuf.String())
		}
		// Decode the SessionHandle envelope to confirm the session was
		// registered, not just a bare 202 with an empty body.
		var handle struct {
			SessionID  string `json:"sessionId"`
			AcceptedAt string `json:"acceptedAt"`
		}
		if err := json.Unmarshal(postBody, &handle); err != nil {
			t.Fatalf("decode SessionHandle for %q: %v\n--- body ---\n%s", repository, err, postBody)
		}
		if handle.SessionID != sessionID {
			t.Errorf("SessionHandle.SessionID = %q, want %q", handle.SessionID, sessionID)
		}
		if handle.AcceptedAt == "" {
			t.Errorf("SessionHandle.AcceptedAt empty (expected RFC3339 timestamp) for %q", repository)
		}
		t.Logf("session accepted: id=%s repository=%s", sessionID, repository)
	}

	t.Run("beta_project_accepted", func(t *testing.T) {
		inject(t, sessionBeta, "smoke-beta")
	})
	t.Run("gamma_project_accepted", func(t *testing.T) {
		inject(t, sessionGamma, "smoke-gamma")
	})

	// ─── Assert both sessions appear in the workareas live-pool view ─────
	//
	// GET /api/daemon/workareas returns Active[] derived from
	// WorkerSpawner.ActiveWorkareas(). Both sessions must appear with
	// their correct Repository tag so cross-project isolation is visible
	// to operator tooling.
	t.Run("workareas_shows_both_sessions", func(t *testing.T) {
		type workareaEntry struct {
			SessionID  string `json:"sessionId"`
			Repository string `json:"repository"`
			Ref        string `json:"ref"`
		}
		var (
			foundBeta  bool
			foundGamma bool
			lastBody   []byte
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
			lastBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			getCancel()
			if resp.StatusCode != http.StatusOK {
				time.Sleep(50 * time.Millisecond)
				continue
			}

			var listResp struct {
				Active []workareaEntry `json:"active"`
			}
			if err := json.Unmarshal(lastBody, &listResp); err != nil {
				t.Fatalf("decode ListWorkareasResponse: %v\n--- body ---\n%s", err, lastBody)
			}
			for _, w := range listResp.Active {
				switch w.SessionID {
				case sessionBeta:
					foundBeta = true
					// WorkareaSummary.Repository mirrors ss.spec.Repository
					// (the value posted in the session spec body). We sent
					// "smoke-beta", so that is what must round-trip here.
					if w.Repository != "smoke-beta" {
						t.Errorf("beta workarea Repository = %q, want smoke-beta", w.Repository)
					}
				case sessionGamma:
					foundGamma = true
					if w.Repository != "smoke-gamma" {
						t.Errorf("gamma workarea Repository = %q, want smoke-gamma", w.Repository)
					}
				}
			}
			if foundBeta && foundGamma {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		if !foundBeta {
			t.Errorf("GET /api/daemon/workareas never contained beta session %q\n--- last body ---\n%s\n--- daemon log tail ---\n%s",
				sessionBeta, lastBody, logBuf.String())
		}
		if !foundGamma {
			t.Errorf("GET /api/daemon/workareas never contained gamma session %q\n--- last body ---\n%s\n--- daemon log tail ---\n%s",
				sessionGamma, lastBody, logBuf.String())
		}
		if foundBeta && foundGamma {
			t.Logf("workareas Active[] contains both sessions: beta=%s gamma=%s", sessionBeta, sessionGamma)
		}
	})
}
