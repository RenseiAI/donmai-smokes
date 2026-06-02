package smokes

// step13_model_profile_dispatch_chain_test.go — H-model-profile multi-provider chain smoke.
//
// Asserts that the model-profile dispatch chain is exercised for every
// first-class provider. Uses a mocked daemon HTTP server — no live daemon,
// no binary build.
//
// The test iterates over a provider matrix and, for each entry:
//
//   - POSTs a dispatch payload carrying the catalogId as the model field and
//     the provider string as the routing selector.
//   - Verifies the 202 ack echoes ChosenLLM == provider.
//   - Calls GET /api/daemon/routing/explain/<id> and cross-checks the stored
//     routing decision.
//   - Confirms the catalogId round-trips unchanged in the explain payload.
//
// Provider matrix (first-class as of 2026-06-02):
//
//	{provider: "claude",  catalogId: "mdl_claude_opus_4_7"}
//	{provider: "codex",   catalogId: "mdl_codex_gpt_5_4"}
//	{provider: "gemini",  catalogId: "mdl_gemini_3_5_flash"}
//
// The mock shapes follow the POST /api/daemon/sessions +
// GET /api/daemon/routing/explain/<id> contract from
// ADR-2026-05-07-daemon-http-control-api.md.

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

// chainDispatchPayload is the POST /api/daemon/sessions request body for the
// dispatch-chain smoke. It extends the base shape with a CatalogID field so
// the mock can verify round-trip fidelity.
type chainDispatchPayload struct {
	SessionID  string               `json:"sessionId"`
	Repository string               `json:"repository"`
	Ref        string               `json:"ref"`
	ModelProfile *chainModelProfile `json:"modelProfile,omitempty"`
}

// chainModelProfile is the ResolvedModelProfile sub-struct for the chain smoke.
// CatalogID carries the platform catalog identifier (e.g. "mdl_gemini_3_5_flash").
type chainModelProfile struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	CatalogID string `json:"catalogId,omitempty"`
}

// chainDispatchAck mirrors the daemon's 202 response body for the chain smoke.
type chainDispatchAck struct {
	SessionID  string `json:"sessionId"`
	AcceptedAt string `json:"acceptedAt"`
	State      string `json:"state"`
	ChosenLLM  string `json:"chosenLLM"`
}

// chainRoutingExplain mirrors /api/daemon/routing/explain/<id> for the chain smoke.
type chainRoutingExplain struct {
	SessionID string `json:"sessionId"`
	Decision  struct {
		SessionID     string `json:"sessionId"`
		ChosenSandbox string `json:"chosenSandbox"`
		ChosenLLM     string `json:"chosenLLM"`
		CatalogID     string `json:"catalogId,omitempty"`
		DecidedAt     string `json:"decidedAt"`
	} `json:"decision"`
}

// providerEntry is one row in the provider matrix.
type providerEntry struct {
	provider  string
	catalogID string
}

// TestModelProfileDispatchChain exercises the model-profile dispatch chain for
// every first-class provider. For each provider the test dispatches a session
// and verifies ChosenLLM + catalogId round-trip through both the 202 ack and
// the routing/explain endpoint.
func TestModelProfileDispatchChain(t *testing.T) {
	// Provider matrix: all first-class providers as of 2026-06-02.
	matrix := []providerEntry{
		{provider: "claude", catalogID: "mdl_claude_opus_4_7"},
		{provider: "codex", catalogID: "mdl_codex_gpt_5_4"},
		{provider: "gemini", catalogID: "mdl_gemini_3_5_flash"},
	}

	// Shared state: the mock stores routing decisions keyed by sessionId so
	// the explain endpoint can serve them back deterministically.
	var mu sync.Mutex
	decisions := make(map[string]chainRoutingExplain)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/daemon/sessions":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, `{"error":"read body"}`, http.StatusInternalServerError)
				return
			}
			var payload chainDispatchPayload
			if err := json.Unmarshal(body, &payload); err != nil {
				http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
				return
			}
			if payload.SessionID == "" {
				http.Error(w, `{"error":"sessionId required"}`, http.StatusBadRequest)
				return
			}

			// Derive ChosenLLM from modelProfile.provider; fall back to "stub".
			chosenLLM := "stub"
			catalogID := ""
			if payload.ModelProfile != nil && payload.ModelProfile.Provider != "" {
				chosenLLM = payload.ModelProfile.Provider
				catalogID = payload.ModelProfile.CatalogID
			}

			// Record routing decision.
			explain := chainRoutingExplain{SessionID: payload.SessionID}
			explain.Decision.SessionID = payload.SessionID
			explain.Decision.ChosenSandbox = "local"
			explain.Decision.ChosenLLM = chosenLLM
			explain.Decision.CatalogID = catalogID
			explain.Decision.DecidedAt = time.Now().UTC().Format(time.RFC3339)

			mu.Lock()
			decisions[payload.SessionID] = explain
			mu.Unlock()

			ack := chainDispatchAck{
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

	for _, entry := range matrix {
		entry := entry // capture loop variable
		t.Run(fmt.Sprintf("provider=%s catalogId=%s", entry.provider, entry.catalogID), func(t *testing.T) {
			sessionID := fmt.Sprintf("chain-%s-001", entry.provider)

			payload := chainDispatchPayload{
				SessionID:  sessionID,
				Repository: "smoke-alpha",
				Ref:        "main",
				ModelProfile: &chainModelProfile{
					Provider:  entry.provider,
					Model:     entry.catalogID, // use catalogId as model to exercise round-trip
					CatalogID: entry.catalogID,
				},
			}

			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}

			// ── POST /api/daemon/sessions ─────────────────────────────────
			resp, err := client.Post(srv.URL+"/api/daemon/sessions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /api/daemon/sessions: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusAccepted {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 202\n--- body ---\n%s", resp.StatusCode, b)
			}

			var ack chainDispatchAck
			if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
				t.Fatalf("decode ack: %v", err)
			}

			// Ack shape assertions.
			if ack.SessionID != sessionID {
				t.Errorf("ack.SessionID = %q, want %q", ack.SessionID, sessionID)
			}
			if ack.State != "accepted" {
				t.Errorf("ack.State = %q, want 'accepted'", ack.State)
			}
			if ack.AcceptedAt == "" {
				t.Error("ack.AcceptedAt empty, want RFC3339 timestamp")
			}
			if ack.ChosenLLM != entry.provider {
				t.Errorf("ack.ChosenLLM = %q, want %q", ack.ChosenLLM, entry.provider)
			}

			// ── GET /api/daemon/routing/explain/<id> ──────────────────────
			explainResp, err := client.Get(srv.URL + "/api/daemon/routing/explain/" + sessionID)
			if err != nil {
				t.Fatalf("GET routing/explain: %v", err)
			}
			defer explainResp.Body.Close()

			if explainResp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(explainResp.Body)
				t.Fatalf("explain status = %d, want 200\n--- body ---\n%s", explainResp.StatusCode, b)
			}

			var explain chainRoutingExplain
			if err := json.NewDecoder(explainResp.Body).Decode(&explain); err != nil {
				t.Fatalf("decode explain: %v", err)
			}

			// Decision shape assertions.
			if explain.Decision.ChosenLLM != entry.provider {
				t.Errorf("explain.Decision.ChosenLLM = %q, want %q",
					explain.Decision.ChosenLLM, entry.provider)
			}
			if explain.Decision.ChosenSandbox != "local" {
				t.Errorf("explain.Decision.ChosenSandbox = %q, want 'local'",
					explain.Decision.ChosenSandbox)
			}
			if explain.Decision.CatalogID != entry.catalogID {
				t.Errorf("explain.Decision.CatalogID = %q, want %q",
					explain.Decision.CatalogID, entry.catalogID)
			}
		})
	}
}
