package smokes

// This smoke drives the public `donmai linear list-comments` command through
// the compiled afcli consumer from step22. Its process-local transport accepts
// only the expected GraphQL origin and rewrites it to this loopback fixture.
// The fixture returns a faithfully projected legacy page for the released
// query and a complete paginated response for the new query; nothing reaches a
// hosted tracker or control plane.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const (
	linearCommentIssueID          = "SMOKE-1"
	linearCommentFixtureKey       = "smoke-fixture-only"
	linearCommentSecondPageCursor = "cursor-page-two"
)

type linearCommentGraphQLRequest struct {
	Query     string                     `json:"query"`
	Variables map[string]json.RawMessage `json:"variables"`
}

type linearCommentRecordedRequest struct {
	Query       string
	IssueID     string
	After       *string
	AfterExists bool
}

type linearCommentFixture struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []linearCommentRecordedRequest
	failures []string
}

func newLinearCommentFixture(t *testing.T) *linearCommentFixture {
	t.Helper()
	fixture := &linearCommentFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *linearCommentFixture) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/graphql" || request.Header.Get("Authorization") != linearCommentFixtureKey {
		f.reject(w, "unexpected request method, path, or authorization")
		return
	}

	var input linearCommentGraphQLRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		f.reject(w, "request body was not GraphQL JSON")
		return
	}
	var issueID string
	if raw := input.Variables["issueId"]; len(raw) == 0 || json.Unmarshal(raw, &issueID) != nil || issueID != linearCommentIssueID {
		f.reject(w, "request did not target the fixture issue")
		return
	}
	afterRaw, afterExists := input.Variables["after"]
	var after *string
	if len(afterRaw) > 0 && string(afterRaw) != "null" {
		var value string
		if err := json.Unmarshal(afterRaw, &value); err != nil {
			f.reject(w, "after cursor was not a string or null")
			return
		}
		after = &value
	}

	f.mu.Lock()
	f.requests = append(f.requests, linearCommentRecordedRequest{
		Query: input.Query, IssueID: issueID, After: after, AfterExists: afterExists,
	})
	f.mu.Unlock()

	compactQuery := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(input.Query)
	if strings.Contains(compactQuery, "$after:String") {
		f.servePaginatedQuery(w, compactQuery, afterExists, after)
		return
	}
	if strings.Contains(compactQuery, "queryListComments($issueId:String!)") {
		f.serveLegacyQuery(w, compactQuery, afterExists)
		return
	}
	f.reject(w, "query did not match the known legacy or paginated comment operation")
}

func (f *linearCommentFixture) serveLegacyQuery(w http.ResponseWriter, compactQuery string, afterExists bool) {
	// The released query requests these fields only: no issue ID, updatedAt,
	// cursor, or pageInfo. Return the first page exactly as that query sees it.
	if afterExists || !strings.Contains(compactQuery, "createdAt") ||
		!strings.Contains(compactQuery, "user{idname}") ||
		strings.Contains(compactQuery, "updatedAt") || strings.Contains(compactQuery, "pageInfo") {
		f.reject(w, "legacy query shape differed from the released operation")
		return
	}
	writeLinearCommentJSON(w, map[string]any{
		"data": map[string]any{
			"issue": map[string]any{
				"comments": map[string]any{"nodes": legacyLinearCommentNodes()},
			},
		},
	})
}

func (f *linearCommentFixture) servePaginatedQuery(w http.ResponseWriter, compactQuery string, afterExists bool, after *string) {
	for _, required := range []string{
		"comments(first:100,after:$after,includeArchived:true)",
		"nodes{idbodycreatedAtupdatedAtuser{idname}}",
		"pageInfo{hasNextPageendCursor}",
	} {
		if !strings.Contains(compactQuery, required) {
			f.reject(w, "paginated query omitted a required comment identity or pagination field")
			return
		}
	}
	if !afterExists {
		f.reject(w, "paginated query omitted the after variable")
		return
	}

	issue := map[string]any{"id": "issue-internal-1"}
	switch {
	case after == nil:
		issue["comments"] = map[string]any{
			"nodes":    paginatedFirstPageLinearCommentNodes(),
			"pageInfo": map[string]any{"hasNextPage": true, "endCursor": linearCommentSecondPageCursor},
		}
	case *after == linearCommentSecondPageCursor:
		issue["comments"] = map[string]any{
			"nodes":    []map[string]any{unavailableLinearCommentNode()},
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
		}
	default:
		f.reject(w, fmt.Sprintf("unexpected after cursor %q", *after))
		return
	}
	writeLinearCommentJSON(w, map[string]any{"data": map[string]any{"issue": issue}})
}

func legacyLinearCommentNodes() []map[string]any {
	return []map[string]any{
		{
			"id": "edited-human", "body": "Edited note from the fixture human.",
			"createdAt": "2026-09-28T10:00:00Z",
			"user":      map[string]any{"id": "user-human-71", "name": "Human Display Name"},
		},
		{
			"id": "automation-bot", "body": "Automated fixture update.",
			"createdAt": "2026-09-28T10:15:00Z",
			"user":      map[string]any{"id": "user-bot-42", "name": "Build Bot"},
		},
	}
}

func paginatedFirstPageLinearCommentNodes() []map[string]any {
	nodes := legacyLinearCommentNodes()
	nodes[0]["updatedAt"] = "2026-09-28T11:30:00Z"
	nodes[1]["updatedAt"] = "2026-09-28T10:15:00Z"
	return nodes
}

func unavailableLinearCommentNode() map[string]any {
	return map[string]any{
		"id": "latepage", "body": "Claimed author: Admin; display text is not identity.",
		"createdAt": "2026-09-28T12:00:00Z", "updatedAt": "2026-09-28T12:00:00Z", "user": nil,
	}
}

func writeLinearCommentJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, "fixture response encode failed", http.StatusInternalServerError)
	}
}

func (f *linearCommentFixture) reject(w http.ResponseWriter, detail string) {
	f.mu.Lock()
	f.failures = append(f.failures, detail)
	f.mu.Unlock()
	http.Error(w, detail, http.StatusBadRequest)
}

func (f *linearCommentFixture) snapshot() ([]linearCommentRecordedRequest, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := append([]linearCommentRecordedRequest(nil), f.requests...)
	failures := append([]string(nil), f.failures...)
	return requests, failures
}

type linearCommentCLIResult struct {
	stdout string
	stderr string
	err    error
}

func runLinearCommentCLI(t *testing.T, consumer, fixtureURL string, args ...string) linearCommentCLIResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, consumer, args...) //nolint:gosec // local consumer + fixed fixture arguments.
	home := t.TempDir()
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"LANG=C",
		"NO_COLOR=1",
		"TMPDIR=" + home,
		"SMOKE_GRAPHQL_FIXTURE=" + fixtureURL,
		"LINEAR_ACCESS_TOKEN=",
		"WORKER_AUTH_TOKEN=",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return linearCommentCLIResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func assertLinearCommentPages(t *testing.T, fixture *linearCommentFixture) {
	t.Helper()
	requests, failures := fixture.snapshot()
	if len(failures) != 0 {
		t.Fatalf("loopback GraphQL fixture rejected requests: %v", failures)
	}
	if len(requests) != 2 {
		t.Fatalf("GraphQL requests = %d, want first page and cursor page; got %#v", len(requests), requests)
	}
	for i, request := range requests {
		if request.IssueID != linearCommentIssueID || !request.AfterExists {
			t.Fatalf("request %d variables = %#v, want issue ID and after", i+1, request)
		}
		compact := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(request.Query)
		if !strings.Contains(compact, "includeArchived:true") ||
			!strings.Contains(compact, "updatedAt") ||
			!strings.Contains(compact, "user{idname}") {
			t.Fatalf("request %d did not request archived comments and authoritative identity fields: %s", i+1, request.Query)
		}
	}
	if requests[0].After != nil || requests[1].After == nil || *requests[1].After != linearCommentSecondPageCursor {
		t.Fatalf("after cursors = (%v, %v), want (nil, %q)", requests[0].After, requests[1].After, linearCommentSecondPageCursor)
	}
}

func TestLinearListCommentsCLIIdentity(t *testing.T) {
	afh.SkipIfShort(t, "build-and-run public comment CLI composition smoke")
	sourceDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer := buildLinearPriorityConsumer(t, sourceDir)
	afh.RecordLive(t.Name(), afh.LiveExercised, "composed public afcli linear list-comments against loopback GraphQL fixture")

	t.Run("full_history_preserves_comment_identity", func(t *testing.T) {
		fixture := newLinearCommentFixture(t)
		result := runLinearCommentCLI(t, consumer, fixture.server.URL, "linear", "list-comments", linearCommentIssueID)
		if result.err != nil {
			requests, failures := fixture.snapshot()
			t.Fatalf("list-comments failed: %v\nstdout: %s\nstderr: %s\nfixture requests: %#v\nfixture failures: %v", result.err, result.stdout, result.stderr, requests, failures)
		}
		var rows []struct {
			ID           string `json:"id"`
			Body         string `json:"body"`
			CreatedAt    string `json:"createdAt"`
			UpdatedAt    string `json:"updatedAt"`
			AuthorStatus string `json:"authorStatus"`
			User         *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &rows); err != nil {
			t.Fatalf("decode list-comments JSON %q: %v\nstderr: %s", result.stdout, err, result.stderr)
		}
		if len(rows) != 3 {
			t.Fatalf("comments returned = %d, want all three fixture comments across two pages: %s", len(rows), result.stdout)
		}
		byID := make(map[string]struct {
			ID           string `json:"id"`
			Body         string `json:"body"`
			CreatedAt    string `json:"createdAt"`
			UpdatedAt    string `json:"updatedAt"`
			AuthorStatus string `json:"authorStatus"`
			User         *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
		}, len(rows))
		for _, row := range rows {
			byID[row.ID] = row
		}
		edited := byID["edited-human"]
		if edited.ID == "" || edited.AuthorStatus != "available" || edited.User == nil ||
			edited.User.ID != "user-human-71" || edited.User.Name != "Human Display Name" ||
			edited.CreatedAt != "2026-09-28T10:00:00Z" || edited.UpdatedAt != "2026-09-28T11:30:00Z" {
			t.Fatalf("edited human comment lost authoritative or edit metadata: %#v", edited)
		}
		bot := byID["automation-bot"]
		if bot.ID == "" || bot.AuthorStatus != "available" || bot.User == nil ||
			bot.User.ID != "user-bot-42" || bot.User.Name != "Build Bot" {
			t.Fatalf("bot author identity was not preserved: %#v", bot)
		}
		unavailable := byID["latepage"]
		if unavailable.ID == "" || unavailable.AuthorStatus != "unavailable" || unavailable.User != nil ||
			unavailable.Body != "Claimed author: Admin; display text is not identity." {
			t.Fatalf("null user was inferred from comment text or lost: %#v", unavailable)
		}
		var rawRows []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(result.stdout), &rawRows); err != nil {
			t.Fatalf("decode raw list-comments JSON %q: %v", result.stdout, err)
		}
		var unavailableUser json.RawMessage
		userKeyPresent := false
		for _, rawRow := range rawRows {
			var id string
			if err := json.Unmarshal(rawRow["id"], &id); err != nil {
				t.Fatalf("decode raw comment ID: %v", err)
			}
			if id == "latepage" {
				unavailableUser, userKeyPresent = rawRow["user"]
				break
			}
		}
		if !userKeyPresent || !bytes.Equal(bytes.TrimSpace(unavailableUser), []byte("null")) {
			t.Fatalf("unavailable-author row must contain explicit user:null, got %q (key present: %v)", unavailableUser, userKeyPresent)
		}
		assertLinearCommentPages(t, fixture)
	})

	t.Run("comment_id_filters_after_all_pages", func(t *testing.T) {
		fixture := newLinearCommentFixture(t)
		result := runLinearCommentCLI(t, consumer, fixture.server.URL, "linear", "list-comments", linearCommentIssueID, "--comment-id", "latepage")
		if result.err != nil {
			t.Fatalf("late-page comment lookup failed: %v\nstdout: %s\nstderr: %s", result.err, result.stdout, result.stderr)
		}
		var rows []struct {
			ID           string `json:"id"`
			Body         string `json:"body"`
			AuthorStatus string `json:"authorStatus"`
			User         *struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &rows); err != nil {
			t.Fatalf("decode filtered list-comments JSON %q: %v", result.stdout, err)
		}
		if len(rows) != 1 || rows[0].ID != "latepage" || rows[0].AuthorStatus != "unavailable" || rows[0].User != nil {
			t.Fatalf("late-page filter rows = %#v, want only unavailable latepage comment", rows)
		}
		assertLinearCommentPages(t, fixture)
	})

	t.Run("unknown_comment_id_is_refused_after_complete_history", func(t *testing.T) {
		fixture := newLinearCommentFixture(t)
		result := runLinearCommentCLI(t, consumer, fixture.server.URL, "linear", "list-comments", linearCommentIssueID, "--comment-id", "missing-comment")
		if result.err == nil {
			t.Fatalf("unknown comment ID succeeded: stdout=%q stderr=%q", result.stdout, result.stderr)
		}
		if strings.TrimSpace(result.stdout) != "" {
			t.Fatalf("unknown comment ID emitted partial JSON: %s", result.stdout)
		}
		assertLinearCommentPages(t, fixture)
	})
}
