package smokes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

func TestLinearNamedReadyStatesCLI(t *testing.T) {
	afh.SkipIfShort(t, "compile-and-run shared blocker readiness commands")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer := buildLinearPriorityConsumer(t, source)
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled public check-blocked and list-unblocked-backlog")
	for _, state := range []string{"Done", "Accepted", "Finished", "Delivered", "Started", "Backlog", "Icebox", "lookup-refusal"} {
		t.Run(state, func(t *testing.T) {
			var blockerReads atomic.Int32
			node := func(id, identifier, status string) map[string]any {
				return map[string]any{"id": id, "identifier": identifier, "title": "fixture issue", "priority": 2, "state": map[string]any{"id": "state-fixture", "name": status}, "team": map[string]any{"id": "team-fixture", "key": "ENG", "name": "Engineering"}, "project": map[string]any{"id": "project-fixture", "name": "FixtureProject"}, "labels": map[string]any{"nodes": []any{}}, "parent": nil}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "smoke-fixture-only" {
					http.Error(w, "fixture rejected transport", http.StatusBadRequest)
					return
				}
				var req struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				var data map[string]any
				switch {
				case strings.Contains(req.Query, "ListProjects"):
					data = map[string]any{"projects": map[string]any{"nodes": []any{map[string]any{"id": "project-fixture", "name": "FixtureProject"}}}}
				case strings.Contains(req.Query, "ListBacklogIssues"):
					if req.Variables["projectId"] != "project-fixture" {
						http.Error(w, "project scope changed", http.StatusBadRequest)
						return
					}
					data = map[string]any{"issues": map[string]any{"nodes": []any{node("candidate-id", "ENG-1", "Backlog")}}}
				case strings.Contains(req.Query, "ListRelations"):
					if req.Variables["issueId"] != "candidate-id" {
						http.Error(w, "relation target changed", http.StatusBadRequest)
						return
					}
					page := map[string]any{"hasNextPage": false, "endCursor": nil}
					relation := map[string]any{"id": "relation-fixture", "type": "blocks", "issue": map[string]any{"id": "blocker-id", "identifier": "ENG-OLD"}, "createdAt": "2026-09-30T10:00:00Z"}
					data = map[string]any{"issue": map[string]any{"relations": map[string]any{"nodes": []any{}, "pageInfo": page}, "inverseRelations": map[string]any{"nodes": []any{relation}, "pageInfo": page}}}
				case strings.Contains(req.Query, "GetIssue"):
					switch req.Variables["id"] {
					case "blocker-id":
						blockerReads.Add(1)
						if state == "lookup-refusal" {
							http.Error(w, "lookup refused", http.StatusForbidden)
							return
						}
						data = map[string]any{"issue": node("blocker-id", "ENG-99", state)}
					case "ENG-1":
						data = map[string]any{"issue": node("candidate-id", "ENG-1", "Backlog")}
					default:
						http.Error(w, "unexpected issue", http.StatusBadRequest)
						return
					}
				default:
					http.Error(w, "unexpected operation", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Errorf("fixture response: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			ready := state == "Done" || state == "Accepted"
			for _, command := range []string{"check-blocked", "list-unblocked-backlog"} {
				t.Run(command, func(t *testing.T) {
					args := []string{"linear", command, "ENG-1"}
					if command == "list-unblocked-backlog" {
						args = []string{"linear", command, "--project", "FixtureProject", "--statuses", "Backlog"}
					}
					before := blockerReads.Load()
					result := runLinearCommentCLI(t, consumer, server.URL, args...)
					if blockerReads.Load() != before+1 {
						t.Fatalf("actual blocker lookup count=%d before=%d", blockerReads.Load(), before)
					}
					if state == "lookup-refusal" {
						if result.err == nil || strings.TrimSpace(result.stdout) != "" {
							t.Fatalf("lookup failure returned success: err=%v stdout=%s", result.err, result.stdout)
						}
						return
					}
					if result.err != nil {
						t.Fatalf("command failed: %v stdout=%s stderr=%s", result.err, result.stdout, result.stderr)
					}
					if command == "check-blocked" {
						var row struct {
							Blocked   bool `json:"blocked"`
							BlockedBy []struct {
								Identifier string `json:"identifier"`
								Status     string `json:"status"`
							} `json:"blockedBy"`
						}
						if err := json.Unmarshal([]byte(result.stdout), &row); err != nil {
							t.Fatal(err)
						}
						if row.Blocked == ready || (!ready && (len(row.BlockedBy) != 1 || row.BlockedBy[0].Identifier != "ENG-99" || row.BlockedBy[0].Status != state)) || (ready && len(row.BlockedBy) != 0) {
							t.Fatalf("named readiness state=%s output=%s", state, result.stdout)
						}
					} else {
						var rows []map[string]any
						if err := json.Unmarshal([]byte(result.stdout), &rows); err != nil {
							t.Fatal(err)
						}
						want := 0
						if ready {
							want = 1
						}
						if len(rows) != want || (ready && (rows[0]["identifier"] != "ENG-1" || rows[0]["blocked"] != false)) {
							t.Fatalf("named readiness state=%s output=%s", state, result.stdout)
						}
					}
				})
			}
		})
	}
}
