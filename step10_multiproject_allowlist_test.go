package smokes

// step10_multiproject_allowlist_test.go — explicit project-admission coverage.
//
// This live-daemon smoke pins the v2 contract that project admission and
// repository resources are independent:
//
//  1. daemon.yaml enables two project IDs while configuring repositories for
//     only one of them, plus a repository for a disabled project.
//  2. /api/daemon/stats reports admission version 2, the exact enabled/applied
//     project IDs, and every repository resource independently.
//  3. A repository-backed session for an enabled project is accepted.
//  4. A repository-free session for another enabled project is accepted.
//  5. A session for the disabled project is rejected even though that project
//     has a configured repository resource.
//  6. Both accepted sessions appear in /api/daemon/workareas.
//
// The test uses only the OSS daemon and its localhost /api/daemon/* surface.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

func TestExplicitProjectAdmissionRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end live-daemon test; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of the live-daemon smoke")
	}

	const daemonYAML = `apiVersion: donmai.dev/v1
kind: LocalDaemon
projectAdmissionVersion: 2
enabledProjectIds:
  - smoke-alpha
  - smoke-beta
machine:
  id: smoke-project-admission
capacity:
  maxConcurrentSessions: 4
  maxVCpuPerSession: 2
  maxMemoryMbPerSession: 2048
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
repositories:
  - id: alpha-primary
    projectId: smoke-alpha
    source: github.com/example/alpha-app
    primary: true
    cloneStrategy: shallow
  - id: alpha-docs
    projectId: smoke-alpha
    source: github.com/example/alpha-docs
    cloneStrategy: shallow
  - id: gamma-primary
    projectId: smoke-gamma
    source: github.com/example/gamma-app
    primary: true
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

	t.Run("stats_separates_admission_from_repositories", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, live.URL+"/api/daemon/stats", nil)
		if err != nil {
			t.Fatalf("build stats request: %v", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/daemon/stats: %v\n--- daemon log tail ---\n%s", err, logBuf.String())
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/daemon/stats status = %d, want 200\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				resp.StatusCode, body, logBuf.String())
		}

		var statsResp struct {
			ProjectAdmissionVersion int      `json:"projectAdmissionVersion"`
			AllowedProjects         []string `json:"allowedProjects"`
			EnabledProjectIDs       []string `json:"enabledProjectIds"`
			AppliedProjectIDs       []string `json:"appliedProjectIds"`
		}
		if err := json.Unmarshal(body, &statsResp); err != nil {
			t.Fatalf("decode DaemonStatsResponse: %v\n--- body ---\n%s", err, body)
		}
		if statsResp.ProjectAdmissionVersion != 2 {
			t.Errorf("ProjectAdmissionVersion = %d, want 2", statsResp.ProjectAdmissionVersion)
		}
		wantEnabled := []string{"smoke-alpha", "smoke-beta"}
		if !slices.Equal(statsResp.EnabledProjectIDs, wantEnabled) {
			t.Errorf("EnabledProjectIDs = %v, want %v", statsResp.EnabledProjectIDs, wantEnabled)
		}
		if !slices.Equal(statsResp.AppliedProjectIDs, wantEnabled) {
			t.Errorf("AppliedProjectIDs = %v, want %v", statsResp.AppliedProjectIDs, wantEnabled)
		}
		wantRepositories := []string{
			"github.com/example/alpha-app",
			"github.com/example/alpha-docs",
			"github.com/example/gamma-app",
		}
		if !slices.Equal(statsResp.AllowedProjects, wantRepositories) {
			t.Errorf("AllowedProjects = %v, want repository resources %v", statsResp.AllowedProjects, wantRepositories)
		}
		t.Logf("v2 admission=%v applied=%v repositories=%v",
			statsResp.EnabledProjectIDs, statsResp.AppliedProjectIDs, statsResp.AllowedProjects)
	})

	ts := time.Now().UnixMilli()
	sessionAlpha := fmt.Sprintf("smoke-admission-alpha-%d-%d", live.Port(), ts)
	sessionBeta := fmt.Sprintf("smoke-admission-beta-%d-%d", live.Port(), ts+1)

	postSession := func(t *testing.T, specBody map[string]any, wantStatus int) []byte {
		t.Helper()
		specBytes, err := json.Marshal(specBody)
		if err != nil {
			t.Fatalf("marshal session spec: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			live.URL+"/api/daemon/sessions", bytes.NewReader(specBytes))
		if err != nil {
			t.Fatalf("build accept-work request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("POST /api/daemon/sessions: %v\n--- daemon log tail ---\n%s", err, logBuf.String())
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Fatalf("POST /api/daemon/sessions status = %d, want %d\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				resp.StatusCode, wantStatus, body, logBuf.String())
		}
		return body
	}

	assertAccepted := func(t *testing.T, body []byte, wantSessionID string) {
		t.Helper()
		var handle struct {
			SessionID  string `json:"sessionId"`
			AcceptedAt string `json:"acceptedAt"`
		}
		if err := json.Unmarshal(body, &handle); err != nil {
			t.Fatalf("decode SessionHandle: %v\n--- body ---\n%s", err, body)
		}
		if handle.SessionID != wantSessionID {
			t.Errorf("SessionHandle.SessionID = %q, want %q", handle.SessionID, wantSessionID)
		}
		if handle.AcceptedAt == "" {
			t.Error("SessionHandle.AcceptedAt empty, want RFC3339 timestamp")
		}
	}

	t.Run("enabled_project_with_repository_accepted", func(t *testing.T) {
		body := postSession(t, map[string]any{
			"sessionId":          sessionAlpha,
			"projectId":          "smoke-alpha",
			"repositoryId":       "alpha-primary",
			"repository":         "github.com/example/alpha-app",
			"requiresRepository": true,
			"ref":                "main",
		}, http.StatusAccepted)
		assertAccepted(t, body, sessionAlpha)
	})

	t.Run("enabled_project_without_repository_accepted", func(t *testing.T) {
		body := postSession(t, map[string]any{
			"sessionId": sessionBeta,
			"projectId": "smoke-beta",
			"ref":       "main",
		}, http.StatusAccepted)
		assertAccepted(t, body, sessionBeta)
	})

	t.Run("repository_does_not_enable_project", func(t *testing.T) {
		body := postSession(t, map[string]any{
			"sessionId":    fmt.Sprintf("smoke-admission-gamma-%d-%d", live.Port(), ts+2),
			"projectId":    "smoke-gamma",
			"repositoryId": "gamma-primary",
			"repository":   "github.com/example/gamma-app",
			"ref":          "main",
		}, http.StatusBadRequest)
		var errorResp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &errorResp); err != nil {
			t.Fatalf("decode disabled-project rejection: %v\n--- body ---\n%s", err, body)
		}
		if errorResp.Error != `project "smoke-gamma" is not allowed` {
			t.Errorf("disabled-project error = %q, want project-not-allowed error", errorResp.Error)
		}
	})

	t.Run("workareas_show_both_enabled_projects", func(t *testing.T) {
		type workareaEntry struct {
			SessionID  string `json:"sessionId"`
			ProjectID  string `json:"projectId"`
			Repository string `json:"repository"`
		}
		var (
			foundAlpha bool
			foundBeta  bool
			lastBody   []byte
		)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, live.URL+"/api/daemon/workareas", nil)
			if err != nil {
				cancel()
				t.Fatalf("build workareas request: %v", err)
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				cancel()
				time.Sleep(50 * time.Millisecond)
				continue
			}
			lastBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			cancel()
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
			for _, workarea := range listResp.Active {
				switch workarea.SessionID {
				case sessionAlpha:
					foundAlpha = true
					if workarea.ProjectID != "smoke-alpha" || workarea.Repository != "github.com/example/alpha-app" {
						t.Errorf("alpha workarea = %+v, want project and repository resource", workarea)
					}
				case sessionBeta:
					foundBeta = true
					if workarea.ProjectID != "smoke-beta" || workarea.Repository != "" {
						t.Errorf("beta workarea = %+v, want repository-free project", workarea)
					}
				}
			}
			if foundAlpha && foundBeta {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !foundAlpha || !foundBeta {
			t.Errorf("workareas missing accepted sessions: alpha=%t beta=%t\n--- last body ---\n%s\n--- daemon log tail ---\n%s",
				foundAlpha, foundBeta, lastBody, logBuf.String())
		}
	})
}
