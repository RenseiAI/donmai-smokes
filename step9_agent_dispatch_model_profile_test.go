package smokes

// step9_agent_dispatch_model_profile_test.go — H-model-profile contract smoke.
//
// Asserts that the daemon honors a ResolvedModelProfile in a dispatch payload
// by routing to the declared provider. Uses a mocked daemon HTTP server —
// no live daemon, no binary build.
//
// H-model-profile contract:
//
//   - POST /api/daemon/sessions with a ResolvedModelProfile must echo back
//     a dispatch acknowledgement that includes the resolved provider name.
//   - The declared modelProfile.provider field drives ChosenLLM in the
//     routing decision for the session.
//   - A dispatch payload that carries conflicting profile.provider and
//     profile.model combinations must still accept (provider wins for
//     routing selection; model mismatch is a warn-not-reject path).
//   - A dispatch payload with an empty ResolvedModelProfile falls back to
//     the daemon's default LLM selection (non-empty ChosenLLM is sufficient).
//
// The mock shapes follow the POST /api/daemon/sessions + GET
// /api/daemon/routing/explain/<id> contract from
// ADR-2026-05-07-daemon-http-control-api.md. The mocked server stores the
// last POSTed body to let assertions cross-check that the payload survived
// deserialization on the daemon side.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// resolvedModelProfile mirrors the H-model-profile wire shape in the dispatch
// payload. Fields match the daemon's SessionSpec.ModelProfile sub-struct.
type resolvedModelProfile struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Tier     string `json:"tier,omitempty"`
}

// dispatchPayload is the POST /api/daemon/sessions request body shape.
type dispatchPayload struct {
	SessionID    string               `json:"sessionId"`
	Repository   string               `json:"repository"`
	Ref          string               `json:"ref"`
	ModelProfile *resolvedModelProfile `json:"modelProfile,omitempty"`
}

// dispatchAck mirrors the daemon's SessionHandle (202 response body).
type dispatchAck struct {
	SessionID  string `json:"sessionId"`
	AcceptedAt string `json:"acceptedAt"`
	State      string `json:"state"`
	ChosenLLM  string `json:"chosenLLM"` // echoed from routing decision
}

// routingExplain mirrors the /api/daemon/routing/explain/<id> response shape.
type routingExplain struct {
	SessionID string `json:"sessionId"`
	Decision  struct {
		SessionID     string `json:"sessionId"`
		ChosenSandbox string `json:"chosenSandbox"`
		ChosenLLM     string `json:"chosenLLM"`
		DecidedAt     string `json:"decidedAt"`
	} `json:"decision"`
}

// TestAgentDispatchWithModelProfile exercises POST /api/daemon/sessions with
// a ResolvedModelProfile field, asserting provider selection is honored in
// the routing decision returned by /api/daemon/routing/explain/<id>.
func TestAgentDispatchWithModelProfile(t *testing.T) {
	// Shared state: the mock server stores the last accepted session's
	// routing decision so explain calls can read it back deterministically.
	var mu sync.Mutex
	decisions := make(map[string]routingExplain)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/daemon/sessions":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, `{"error":"read body"}`, http.StatusInternalServerError)
				return
			}
			var payload dispatchPayload
			if err := json.Unmarshal(body, &payload); err != nil {
				http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
				return
			}
			if payload.SessionID == "" {
				http.Error(w, `{"error":"sessionId required"}`, http.StatusBadRequest)
				return
			}

			// Derive ChosenLLM: use modelProfile.provider when present,
			// fall back to "stub" (the OSS daemon's default standalone shape).
			chosenLLM := "stub"
			if payload.ModelProfile != nil && payload.ModelProfile.Provider != "" {
				chosenLLM = payload.ModelProfile.Provider
			}

			// Record routing decision.
			explain := routingExplain{SessionID: payload.SessionID}
			explain.Decision.SessionID = payload.SessionID
			explain.Decision.ChosenSandbox = "local"
			explain.Decision.ChosenLLM = chosenLLM
			explain.Decision.DecidedAt = time.Now().UTC().Format(time.RFC3339)

			mu.Lock()
			decisions[payload.SessionID] = explain
			mu.Unlock()

			ack := dispatchAck{
				SessionID:  payload.SessionID,
				AcceptedAt: explain.Decision.DecidedAt,
				State:      "accepted",
				ChosenLLM:  chosenLLM,
			}
			w.WriteHeader(http.StatusAccepted)
			if err := json.NewEncoder(w).Encode(ack); err != nil {
				t.Errorf("mock: encode ack: %v", err)
			}

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/daemon/routing/explain/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/daemon/routing/explain/")
			mu.Lock()
			explain, ok := decisions[id]
			mu.Unlock()
			if !ok {
				http.Error(w, fmt.Sprintf(`{"error":"session %q not found"}`, id), http.StatusNotFound)
				return
			}
			if err := json.NewEncoder(w).Encode(explain); err != nil {
				t.Errorf("mock: encode explain: %v", err)
			}

		default:
			http.Error(w, `{"error":"unexpected path"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	// postAndCheck is a helper that POSTs a dispatch payload and asserts the
	// 202 ack + the routing explain shape for a given session.
	postAndCheck := func(t *testing.T, payload dispatchPayload, wantProvider string) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		resp, err := client.Post(srv.URL+"/api/daemon/sessions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /api/daemon/sessions: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 202\n--- body ---\n%s", resp.StatusCode, respBody)
		}

		var ack dispatchAck
		if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
			t.Fatalf("decode ack: %v", err)
		}
		if ack.SessionID != payload.SessionID {
			t.Errorf("ack.SessionID = %q, want %q", ack.SessionID, payload.SessionID)
		}
		if ack.AcceptedAt == "" {
			t.Error("ack.AcceptedAt empty, want RFC3339 timestamp")
		}
		if ack.ChosenLLM != wantProvider {
			t.Errorf("ack.ChosenLLM = %q, want %q", ack.ChosenLLM, wantProvider)
		}

		// Cross-check via routing/explain.
		explainResp, err := client.Get(srv.URL + "/api/daemon/routing/explain/" + payload.SessionID)
		if err != nil {
			t.Fatalf("GET explain: %v", err)
		}
		defer explainResp.Body.Close()
		if explainResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(explainResp.Body)
			t.Fatalf("explain status = %d, want 200\n--- body ---\n%s", explainResp.StatusCode, b)
		}
		var explain routingExplain
		if err := json.NewDecoder(explainResp.Body).Decode(&explain); err != nil {
			t.Fatalf("decode explain: %v", err)
		}
		if explain.Decision.ChosenLLM != wantProvider {
			t.Errorf("explain.Decision.ChosenLLM = %q, want %q",
				explain.Decision.ChosenLLM, wantProvider)
		}
		if explain.Decision.ChosenSandbox != "local" {
			t.Errorf("explain.Decision.ChosenSandbox = %q, want 'local'",
				explain.Decision.ChosenSandbox)
		}
	}

	// ── Case 1: explicit provider "claude" ────────────────────────────────
	t.Run("explicit provider claude", func(t *testing.T) {
		postAndCheck(t, dispatchPayload{
			SessionID:  "dispatch-claude-001",
			Repository: "smoke-alpha",
			Ref:        "main",
			ModelProfile: &resolvedModelProfile{
				Provider: "claude",
				Model:    "claude-sonnet-4-5",
				Tier:     "standard",
			},
		}, "claude")
	})

	// ── Case 2: explicit provider "codex" ─────────────────────────────────
	t.Run("explicit provider codex", func(t *testing.T) {
		postAndCheck(t, dispatchPayload{
			SessionID:  "dispatch-codex-001",
			Repository: "smoke-alpha",
			Ref:        "main",
			ModelProfile: &resolvedModelProfile{
				Provider: "codex",
				Model:    "gpt-4o",
			},
		}, "codex")
	})

	// ── Case 3: explicit provider "gemini" ────────────────────────────────
	t.Run("explicit provider gemini", func(t *testing.T) {
		postAndCheck(t, dispatchPayload{
			SessionID:  "dispatch-gemini-001",
			Repository: "smoke-alpha",
			Ref:        "main",
			ModelProfile: &resolvedModelProfile{
				Provider: "gemini",
				Model:    "gemini-3.5-flash",
				Tier:     "standard",
			},
		}, "gemini")
	})

	// ── Case 4: provider + model mismatch — provider wins (warn not reject) ──
	t.Run("provider wins over mismatched model", func(t *testing.T) {
		// Provider "claude" but model is a non-Claude string. The daemon
		// must still accept and use "claude" as ChosenLLM — the model
		// field is informational; provider is authoritative for routing.
		postAndCheck(t, dispatchPayload{
			SessionID:  "dispatch-mismatch-001",
			Repository: "smoke-alpha",
			Ref:        "main",
			ModelProfile: &resolvedModelProfile{
				Provider: "claude",
				Model:    "totally-wrong-model-name",
			},
		}, "claude")
	})

	// ── Case 5: nil ModelProfile — daemon falls back to default ("stub") ──
	t.Run("nil model profile falls back to default", func(t *testing.T) {
		payload := dispatchPayload{
			SessionID:  "dispatch-nomodel-001",
			Repository: "smoke-alpha",
			Ref:        "main",
			// ModelProfile intentionally nil.
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		resp, err := client.Post(srv.URL+"/api/daemon/sessions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /api/daemon/sessions: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 202\n--- body ---\n%s", resp.StatusCode, b)
		}
		var ack dispatchAck
		if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
			t.Fatalf("decode ack: %v", err)
		}
		if ack.ChosenLLM == "" {
			t.Error("ack.ChosenLLM empty; daemon must emit non-empty provider even on fallback path")
		}
		t.Logf("fallback ChosenLLM = %q", ack.ChosenLLM)
	})

	// ── Case 6: dispatch without sessionId → 400 ──────────────────────────
	t.Run("missing sessionId returns 400", func(t *testing.T) {
		bad := `{"repository":"smoke-alpha","ref":"main"}`
		resp, err := client.Post(srv.URL+"/api/daemon/sessions", "application/json",
			strings.NewReader(bad))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}
