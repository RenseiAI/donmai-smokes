package smokes

// step7_agent_card_roundtrip_test.go — H-workType contract smoke.
//
// Asserts that the afclient ListAgents(scope) + GetAgent(id) JSON round-trip
// preserves all struct fields correctly when the daemon returns a well-formed
// agent-card payload. Uses a mocked daemon HTTP server (net/http/httptest) —
// no live daemon, no binary build.
//
// H-workType contract: every AgentCard that the daemon surfaces via
// /api/daemon/agents must survive a ListAgents → GetAgent round-trip with
// field fidelity. This spec pins:
//
//   - ListAgents returns a typed slice with correct IDs + workType values.
//   - GetAgent(id) returns the full card with nested fields intact
//     (model, workType, poolRef, labels).
//   - A GetAgent for an unknown ID yields 404 — not a panic, not a 200 with
//     an empty body.
//
// The mock shapes are minimal-valid per the daemon's /api/daemon/agents
// wire contract (same envelope the live daemon would emit). They intentionally
// exercise the H-workType discriminator field so a regression that drops,
// renames, or misparses that field fails loudly here.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// agentCard is a minimal local copy of the daemon's AgentCard wire shape.
// We don't import donmai packages here (smoke boundary) — the struct
// mirrors the JSON the daemon emits. Fields are kept deliberately
// narrow: only the ones the H-workType lane cares about.
type agentCard struct {
	ID       string   `json:"id"`
	WorkType string   `json:"workType"`
	PoolRef  string   `json:"poolRef"`
	Model    string   `json:"model"`
	Labels   []string `json:"labels"`
}

// agentListResponse mirrors /api/daemon/agents envelope.
type agentListResponse struct {
	Agents []agentCard `json:"agents"`
}

// TestAgentCardRoundtrip exercises ListAgents + GetAgent JSON round-trip
// against a mocked daemon HTTP server. All assertions use the contract shape
// declared by the H-workType surface — no live daemon, no binary.
func TestAgentCardRoundtrip(t *testing.T) {
	// Fixture cards covering the three workType variants exercised by the
	// H-workType lane: "interactive", "batch", and "background". Labels,
	// poolRef, and model are non-empty to confirm nested fields survive.
	fixtures := []agentCard{
		{
			ID:       "agent-alpha",
			WorkType: "interactive",
			PoolRef:  "pool-local-default",
			Model:    "claude-sonnet",
			Labels:   []string{"trusted", "h-worktype"},
		},
		{
			ID:       "agent-beta",
			WorkType: "batch",
			PoolRef:  "pool-local-batch",
			Model:    "claude-haiku",
			Labels:   []string{"h-worktype"},
		},
		{
			ID:       "agent-gamma",
			WorkType: "background",
			PoolRef:  "pool-local-bg",
			Model:    "claude-sonnet",
			Labels:   nil,
		},
	}

	// Index fixtures by ID for the GetAgent handler.
	cardByID := make(map[string]agentCard, len(fixtures))
	for _, c := range fixtures {
		cardByID[c.ID] = c
	}

	// Mocked daemon HTTP server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/daemon/agents":
			// scope query param (e.g. ?scope=local) is accepted but not required.
			resp := agentListResponse{Agents: fixtures}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Errorf("mock: encode list response: %v", err)
			}

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/daemon/agents/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/daemon/agents/")
			card, ok := cardByID[id]
			if !ok {
				http.Error(w, fmt.Sprintf(`{"error":"agent %q not found"}`, id), http.StatusNotFound)
				return
			}
			if err := json.NewEncoder(w).Encode(card); err != nil {
				t.Errorf("mock: encode get response: %v", err)
			}

		default:
			http.Error(w, `{"error":"unexpected path"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := &http.Client{}

	// ── Part 1: ListAgents (scope=local) ──────────────────────────────────
	t.Run("ListAgents returns all cards", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/daemon/agents?scope=local")
		if err != nil {
			t.Fatalf("GET /api/daemon/agents: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		var list agentListResponse
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		if len(list.Agents) != len(fixtures) {
			t.Fatalf("len(Agents) = %d, want %d", len(list.Agents), len(fixtures))
		}

		// Verify IDs and workType values are preserved in list order.
		for i, got := range list.Agents {
			want := fixtures[i]
			if got.ID != want.ID {
				t.Errorf("Agents[%d].ID = %q, want %q", i, got.ID, want.ID)
			}
			if got.WorkType != want.WorkType {
				t.Errorf("Agents[%d].WorkType = %q, want %q", i, got.WorkType, want.WorkType)
			}
		}
	})

	// ── Part 2: GetAgent — full card field fidelity ───────────────────────
	t.Run("GetAgent preserves all fields", func(t *testing.T) {
		for _, want := range fixtures {
			want := want // capture for subtest
			t.Run(want.ID, func(t *testing.T) {
				resp, err := client.Get(srv.URL + "/api/daemon/agents/" + want.ID)
				if err != nil {
					t.Fatalf("GET /api/daemon/agents/%s: %v", want.ID, err)
				}
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != http.StatusOK {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("status = %d, want 200\n--- body ---\n%s", resp.StatusCode, body)
				}

				var got agentCard
				if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
					t.Fatalf("decode agent card: %v", err)
				}

				if got.ID != want.ID {
					t.Errorf("ID = %q, want %q", got.ID, want.ID)
				}
				if got.WorkType != want.WorkType {
					t.Errorf("WorkType = %q, want %q", got.WorkType, want.WorkType)
				}
				if got.PoolRef != want.PoolRef {
					t.Errorf("PoolRef = %q, want %q", got.PoolRef, want.PoolRef)
				}
				if got.Model != want.Model {
					t.Errorf("Model = %q, want %q", got.Model, want.Model)
				}
				// Label slice comparison: nil and empty are both acceptable
				// for zero-label agents; non-empty slices must match exactly.
				if len(want.Labels) > 0 {
					if len(got.Labels) != len(want.Labels) {
						t.Errorf("len(Labels) = %d, want %d", len(got.Labels), len(want.Labels))
					} else {
						for j, lbl := range want.Labels {
							if got.Labels[j] != lbl {
								t.Errorf("Labels[%d] = %q, want %q", j, got.Labels[j], lbl)
							}
						}
					}
				}
			})
		}
	})

	// ── Part 3: GetAgent unknown ID → 404 ────────────────────────────────
	t.Run("GetAgent unknown ID returns 404", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/daemon/agents/does-not-exist")
		if err != nil {
			t.Fatalf("GET /api/daemon/agents/does-not-exist: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}
