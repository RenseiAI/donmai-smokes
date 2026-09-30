package smokes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestLinearCommentCanonicalCLI exercises the public composed command tree and
// both supported invocation spellings; only loopback fixture transport is used.
func TestLinearCommentCanonicalCLI(t *testing.T) {
	afh.SkipIfShort(t, "build-and-run public comment composition smoke")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer := buildLinearPriorityConsumer(t, source)
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled public canonical comment command and compatibility spelling")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "smoke-fixture-only" {
			http.Error(w, "fixture rejected transport", http.StatusBadRequest)
			return
		}
		var input struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !strings.Contains(input.Query, "commentCreate") || input.Variables["issueId"] != "ENG-1" {
			http.Error(w, "fixture rejected mutation", http.StatusBadRequest)
			return
		}
		if input.Variables["body"] == "backend-refusal" {
			http.Error(w, "fixture refusal", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": "fixture-comment", "body": input.Variables["body"], "createdAt": "2026-09-30T10:00:00Z"}}}}); err != nil {
			t.Errorf("fixture response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	for _, args := range [][]string{{"linear", "--help"}, {"__complete", "linear", ""}} {
		result := runLinearCommentCLI(t, consumer, server.URL, args...)
		if result.err != nil {
			t.Fatalf("command %v: %v stderr=%s", args, result.err, result.stderr)
		}
		canonical, compatibility := 0, 0
		for _, line := range strings.Split(result.stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if fields[0] == "create-comment" {
				canonical++
			}
			if fields[0] == "comment" {
				compatibility++
			}
		}
		if canonical != 1 || compatibility != 0 {
			t.Fatalf("command %v exposes create-comment=%d comment=%d: %s", args, canonical, compatibility, result.stdout)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("help/completion mutated tracker")
	}
	body := "Résumé 日本語\nsecond line\n"
	file := filepath.Join(t.TempDir(), "comment.md")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"create-comment", "comment"} {
		t.Run(spelling, func(t *testing.T) {
			for _, args := range [][]string{{"--body", body}, {"--body-file", file}, {"--body", "ignored", "--body-file", file}} {
				before := requests.Load()
				argv := append([]string{"linear", spelling, "ENG-1"}, args...)
				result := runLinearCommentCLI(t, consumer, server.URL, argv...)
				if result.err != nil {
					t.Fatalf("argv=%v: %v stderr=%s", argv, result.err, result.stderr)
				}
				var row struct {
					ID        string `json:"id"`
					Body      string `json:"body"`
					CreatedAt string `json:"createdAt"`
				}
				if err := json.Unmarshal([]byte(result.stdout), &row); err != nil || row.ID != "fixture-comment" || row.Body != body || row.CreatedAt != "2026-09-30T10:00:00Z" {
					t.Fatalf("comment output changed: err=%v stdout=%s", err, result.stdout)
				}
				if requests.Load() != before+1 {
					t.Fatalf("request count before=%d after=%d", before, requests.Load())
				}
			}
			for _, args := range [][]string{{}, {"--body", ""}, {"--body", "ignored", "--body-file", file + ".absent"}} {
				before := requests.Load()
				result := runLinearCommentCLI(t, consumer, server.URL, append([]string{"linear", spelling, "ENG-1"}, args...)...)
				if result.err == nil || requests.Load() != before {
					t.Fatalf("input refusal err=%v requests before=%d after=%d", result.err, before, requests.Load())
				}
			}
			before := requests.Load()
			result := runLinearCommentCLI(t, consumer, server.URL, "linear", spelling, "ENG-1", "--body", "backend-refusal")
			if result.err == nil || requests.Load() != before+1 {
				t.Fatalf("backend refusal err=%v requests before=%d after=%d", result.err, before, requests.Load())
			}
		})
	}
}
