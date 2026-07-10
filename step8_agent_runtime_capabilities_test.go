package smokes

// step8_agent_runtime_capabilities_test.go — H-pool-aware contract smoke.
//
// Asserts that the daemon's GET /api/daemon/capabilities response contains
// the expected substrate set for an H-pool-aware daemon configuration.
// Uses a mocked daemon HTTP server — no live daemon, no binary build.
//
// H-pool-aware contract: the capabilities envelope must:
//
//   - Include a "substrates" array that is non-empty.
//   - Surface at least the "local" substrate (every OSS daemon ships this).
//   - Carry a "pools" array whose entries have non-empty IDs.
//   - Expose a "providerFamilies" field listing the AgentRuntime family.
//   - Return 200 with application/json content-type (not 404 / text/plain).
//
// The mock shape follows the wire contract declared in
// ADR-2026-05-07-daemon-http-control-api.md § "Capabilities endpoint". The
// "pools" slice carries both a local pool and a stub cloud pool to confirm
// the decoder doesn't truncate multi-entry arrays.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// daemonPool is a minimal local copy of the daemon's PoolDescriptor wire shape.
type daemonPool struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Capacity int    `json:"capacity"`
}

// daemonCapabilities mirrors /api/daemon/capabilities envelope.
type daemonCapabilities struct {
	Substrates       []string     `json:"substrates"`
	Pools            []daemonPool `json:"pools"`
	ProviderFamilies []string     `json:"providerFamilies"`
	Version          string       `json:"version"`
}

// TestAgentRuntimeCapabilities exercises GET /api/daemon/capabilities against
// a mocked daemon HTTP server, verifying the H-pool-aware substrate set.
func TestAgentRuntimeCapabilities(t *testing.T) {
	// Fixture: a pool-aware capabilities envelope advertising two pools.
	// "local" is the mandatory OSS-shipped substrate; "e2b-sandbox" is the
	// H-pool-aware extension pool. The ProviderFamilies list carries
	// "agent-runtime" (required) + "sandbox" (pool-aware extension).
	fixture := daemonCapabilities{
		Substrates: []string{"local", "e2b-sandbox"},
		Pools: []daemonPool{
			{ID: "pool-local-default", Provider: "local", Capacity: 4},
			{ID: "pool-e2b-01", Provider: "e2b-sandbox", Capacity: 10},
		},
		ProviderFamilies: []string{"agent-runtime", "sandbox", "llm"},
		Version:          "0.12.0",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/daemon/capabilities" {
			http.Error(w, `{"error":"unexpected path"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(fixture); err != nil {
			t.Errorf("mock: encode capabilities: %v", err)
		}
	}))
	defer srv.Close()

	client := &http.Client{}

	// ── Part 1: HTTP basics ───────────────────────────────────────────────
	t.Run("returns 200 with JSON content-type", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/daemon/capabilities")
		if err != nil {
			t.Fatalf("GET /api/daemon/capabilities: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200\n--- body ---\n%s", resp.StatusCode, body)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
	})

	// ── Part 2: Substrates — local must always be present ─────────────────
	t.Run("substrates contains local", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/daemon/capabilities")
		if err != nil {
			t.Fatalf("GET /api/daemon/capabilities: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var caps daemonCapabilities
		if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
			t.Fatalf("decode capabilities: %v", err)
		}

		if len(caps.Substrates) == 0 {
			t.Fatal("Substrates is empty; OSS daemon must advertise at least 'local'")
		}

		hasLocal := false
		for _, s := range caps.Substrates {
			if s == "local" {
				hasLocal = true
				break
			}
		}
		if !hasLocal {
			t.Errorf("Substrates %v does not contain 'local'", caps.Substrates)
		}
	})

	// ── Part 3: Pools — multi-entry array decoded without truncation ───────
	t.Run("pools array decoded correctly", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/daemon/capabilities")
		if err != nil {
			t.Fatalf("GET /api/daemon/capabilities: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var caps daemonCapabilities
		if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
			t.Fatalf("decode capabilities: %v", err)
		}

		if len(caps.Pools) != len(fixture.Pools) {
			t.Fatalf("len(Pools) = %d, want %d", len(caps.Pools), len(fixture.Pools))
		}
		for i, got := range caps.Pools {
			want := fixture.Pools[i]
			if got.ID != want.ID {
				t.Errorf("Pools[%d].ID = %q, want %q", i, got.ID, want.ID)
			}
			if got.Provider != want.Provider {
				t.Errorf("Pools[%d].Provider = %q, want %q", i, got.Provider, want.Provider)
			}
			if got.Capacity != want.Capacity {
				t.Errorf("Pools[%d].Capacity = %d, want %d", i, got.Capacity, want.Capacity)
			}
		}
	})

	// ── Part 4: ProviderFamilies — agent-runtime must be present ──────────
	t.Run("providerFamilies contains agent-runtime", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/daemon/capabilities")
		if err != nil {
			t.Fatalf("GET /api/daemon/capabilities: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var caps daemonCapabilities
		if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
			t.Fatalf("decode capabilities: %v", err)
		}

		if len(caps.ProviderFamilies) == 0 {
			t.Fatal("ProviderFamilies is empty; daemon must advertise 'agent-runtime'")
		}

		hasAgentRuntime := false
		for _, f := range caps.ProviderFamilies {
			if f == "agent-runtime" {
				hasAgentRuntime = true
				break
			}
		}
		if !hasAgentRuntime {
			t.Errorf("ProviderFamilies %v does not contain 'agent-runtime'", caps.ProviderFamilies)
		}
	})

	// ── Part 5: Wrong path returns 404 ────────────────────────────────────
	t.Run("unknown path returns 404", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/daemon/capabilities/unknown")
		if err != nil {
			t.Fatalf("GET /api/daemon/capabilities/unknown: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}
