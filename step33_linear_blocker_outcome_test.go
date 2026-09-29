package smokes

// This smoke composes the public CLI command factory from the selected Donmai
// source in a temporary consumer. Its strict transport forwards GraphQL only
// to an httptest fixture; no tracker credential or remote API is used.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const blockerOutcomeConsumer = `package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/RenseiAI/donmai/afcli"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

type fixtureTransport struct {
	fixture *url.URL
	next http.RoundTripper
}

func (f fixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "api.linear.app" || request.URL.Path != "/graphql" {
		return nil, errors.New("smoke rejected non-GraphQL network request")
	}
	forward := request.Clone(request.Context())
	forward.URL.Scheme = f.fixture.Scheme
	forward.URL.Host = f.fixture.Host
	forward.Host = f.fixture.Host
	return f.next.RoundTrip(forward)
}

func main() {
	fixture, err := url.Parse(os.Getenv("SMOKE_GRAPHQL_FIXTURE"))
	if err != nil || fixture.Scheme != "http" || fixture.Hostname() != "127.0.0.1" || fixture.Port() == "" || fixture.Path != "" {
		os.Exit(2)
	}
	http.DefaultTransport = fixtureTransport{fixture: fixture, next: &http.Transport{Proxy: nil}}
	if err := os.Setenv("LINEAR_API_KEY", "smoke-fixture-only"); err != nil {
		os.Exit(2)
	}
	root := &cobra.Command{Use: "smoke-consumer", SilenceUsage: true, SilenceErrors: true}
	afcli.RegisterCommands(root, afcli.Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
		BinaryName: "smoke-consumer",
	})
	root.SetArgs(os.Args[1:])
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

func buildBlockerOutcomeConsumer(t *testing.T, sourceDir string) string {
	t.Helper()
	moduleBytes, err := os.ReadFile(filepath.Join(sourceDir, "go.mod"))
	if err != nil {
		t.Fatalf("read selected donmai go.mod: %v", err)
	}
	version := ""
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			version = fields[1]
			break
		}
	}
	if version == "" {
		t.Fatal("selected donmai go.mod lacks a go directive")
	}
	moduleDir := t.TempDir()
	goMod := fmt.Sprintf("module donmai-smokes-blocker-outcome-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0-00010101000000-000000000000\n\nreplace github.com/RenseiAI/donmai => %s\n", version, sourceDir)
	for name, contents := range map[string]string{"go.mod": goMod, "main.go": blockerOutcomeConsumer} {
		if err := os.WriteFile(filepath.Join(moduleDir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write blocker consumer %s: %v", name, err)
		}
	}
	env := append(os.Environ(), "GOWORK=off")
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-o", filepath.Join(moduleDir, "smoke-consumer"), "."}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // test-controlled command and validated source.
		cmd.Dir, cmd.Env = moduleDir, env
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("consumer go %s against %s: %v\n%s", strings.Join(args, " "), sourceDir, err, out)
		}
	}
	return filepath.Join(moduleDir, "smoke-consumer")
}

type blockerSmokeEdge struct {
	id, kind, from, to string
}

type blockerSmokeFixture struct {
	t *testing.T

	mu               sync.Mutex
	calls            []string
	edges            []blockerSmokeEdge
	created          int
	relationAttempts int
	relationReads    int
	commentAttempts  int
	comments         []string

	reuse, alreadyLinked, relationRefused, concurrentEdge, ackMissing, noticeRefused bool
	lookupRefused, incompletePage, projectRefused                                    bool
}

func newBlockerSmokeFixture(t *testing.T, tc blockerSmokeCase) *blockerSmokeFixture {
	t.Helper()
	f := &blockerSmokeFixture{
		t:     t,
		reuse: tc.reuse, alreadyLinked: tc.alreadyLinked,
		relationRefused: tc.relationRefused, concurrentEdge: tc.concurrentEdge,
		ackMissing: tc.ackMissing, noticeRefused: tc.noticeRefused,
		lookupRefused: tc.lookupRefused, incompletePage: tc.incompletePage,
		projectRefused: tc.projectRefused,
		edges: []blockerSmokeEdge{
			{id: "reverse-edge", kind: "blocks", from: "source-1", to: "blocker-1"},
			{id: "other-blocker-edge", kind: "blocks", from: "other-1", to: "source-1"},
			{id: "wrong-type-edge", kind: "related", from: "blocker-1", to: "source-1"},
			{id: "different-source-edge", kind: "blocks", from: "blocker-1", to: "other-source-1"},
		},
	}
	if tc.alreadyLinked {
		f.edges = append(f.edges, blockerSmokeEdge{id: "exact-edge", kind: "blocks", from: "blocker-1", to: "source-1"})
	}
	return f
}

func blockerSmokeIssue(id, identifier, title, state string, labels ...string) map[string]any {
	labelNodes := make([]map[string]any, 0, len(labels))
	for _, name := range labels {
		labelNodes = append(labelNodes, map[string]any{"id": "label-nh", "name": name})
	}
	return map[string]any{
		"id": id, "identifier": identifier, "title": title, "description": "desc",
		"url": "https://issues.example.invalid/i/" + identifier, "priority": 2,
		"createdAt": "2025-01-01T00:00:00Z", "updatedAt": "2025-01-02T00:00:00Z",
		"state":   map[string]any{"id": "state-1", "name": state},
		"team":    map[string]any{"id": "team-1", "key": "OSS", "name": "OSS Team"},
		"project": map[string]any{"id": "proj-1", "name": "TestProject"},
		"labels":  map[string]any{"nodes": labelNodes}, "parent": nil, "assignee": nil,
	}
}

func blockerSmokePage(nodes any) map[string]any {
	return map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
}

func blockerSmokeIdentifier(id string) string {
	switch id {
	case "source-1":
		return "OSS-50"
	case "blocker-1":
		return "OSS-99"
	case "other-1":
		return "OSS-70"
	case "other-source-1":
		return "OSS-51"
	default:
		return "unknown"
	}
}

func (f *blockerSmokeFixture) relationPage() map[string]any {
	forward := []map[string]any{}
	inverse := []map[string]any{}
	for _, edge := range f.edges {
		base := map[string]any{"id": edge.id, "type": edge.kind, "createdAt": "2025-01-01T00:00:00Z"}
		if edge.from == "source-1" {
			row := map[string]any{"id": base["id"], "type": base["type"], "createdAt": base["createdAt"]}
			row["relatedIssue"] = map[string]any{"id": edge.to, "identifier": blockerSmokeIdentifier(edge.to)}
			forward = append(forward, row)
		}
		if edge.to == "source-1" {
			row := map[string]any{"id": base["id"], "type": base["type"], "createdAt": base["createdAt"]}
			row["issue"] = map[string]any{"id": edge.from, "identifier": blockerSmokeIdentifier(edge.from)}
			inverse = append(inverse, row)
		}
	}
	return map[string]any{"issue": map[string]any{
		"relations": blockerSmokePage(forward), "inverseRelations": blockerSmokePage(inverse),
	}}
}

func (f *blockerSmokeFixture) respond(w http.ResponseWriter, data any) {
	f.t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		f.t.Errorf("write GraphQL fixture response: %v", err)
	}
}

func (f *blockerSmokeFixture) refuse(w http.ResponseWriter, message string) {
	f.t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": message}}}); err != nil {
		f.t.Errorf("write GraphQL fixture refusal: %v", err)
	}
}

func (f *blockerSmokeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "smoke-fixture-only" {
		f.t.Errorf("unexpected GraphQL request method=%s path=%s authorization-present=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decode GraphQL request: %v", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	appendCall := func(name string) { f.calls = append(f.calls, name) }
	switch {
	case strings.Contains(req.Query, "query GetIssue("):
		appendCall("GetIssue")
		if req.Variables["id"] != "OSS-50" {
			f.t.Errorf("GetIssue variables = %#v", req.Variables)
		}
		f.respond(w, map[string]any{"issue": blockerSmokeIssue("source-1", "OSS-50", "Source issue", "Started")})
	case strings.Contains(req.Query, "query ListProjects("):
		appendCall("ListProjects")
		filter, _ := req.Variables["filter"].(map[string]any)
		want := []any{
			map[string]any{"name": map[string]any{"eqIgnoreCase": "TestProject"}},
			map[string]any{"slugId": map[string]any{"eqIgnoreCase": "TestProject"}},
		}
		if !reflect.DeepEqual(filter["or"], want) || req.Variables["after"] != nil {
			f.t.Errorf("ListProjects variables = %#v, want scoped TestProject", req.Variables)
		}
		if f.projectRefused {
			f.refuse(w, "candidate project lookup refused")
			return
		}
		f.respond(w, map[string]any{"projects": blockerSmokePage([]map[string]any{{
			"id": "proj-1", "name": "TestProject", "slugId": "test-project",
			"teams": blockerSmokePage([]any{}),
		}})})
	case strings.Contains(req.Query, "query ListIssues("):
		appendCall("ListIssues")
		wantFilter := map[string]any{
			"project": map[string]any{"id": map[string]any{"eq": "proj-1"}},
			"state":   map[string]any{"name": map[string]any{"eqIgnoreCase": "Icebox"}},
			"labels":  map[string]any{"name": map[string]any{"eqIgnoreCase": "Needs Human"}},
		}
		if !reflect.DeepEqual(req.Variables["filter"], wantFilter) || req.Variables["first"] != float64(250) || req.Variables["after"] != nil || req.Variables["orderBy"] != "createdAt" {
			f.t.Errorf("ListIssues variables = %#v, want complete scoped candidate page", req.Variables)
		}
		if f.lookupRefused {
			f.refuse(w, "candidate lookup refused")
			return
		}
		if f.incompletePage {
			f.respond(w, map[string]any{"issues": map[string]any{
				"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": nil},
			}})
			return
		}
		nodes := []map[string]any{}
		if f.reuse {
			nodes = append(nodes, blockerSmokeIssue("blocker-1", "OSS-99", "Need human review", "Icebox", "Needs Human"))
		}
		f.respond(w, map[string]any{"issues": blockerSmokePage(nodes)})
	case strings.Contains(req.Query, "query ListRelations("):
		appendCall("ListRelations")
		f.relationReads++
		if req.Variables["issueId"] != "source-1" || req.Variables["relationsAfter"] != nil || req.Variables["inverseRelationsAfter"] != nil {
			f.t.Errorf("ListRelations variables = %#v, want complete source inventory", req.Variables)
		}
		f.respond(w, f.relationPage())
	case strings.Contains(req.Query, "query ListTeams("):
		appendCall("ListTeams")
		f.respond(w, map[string]any{"teams": blockerSmokePage([]map[string]any{{"id": "team-1", "key": "OSS", "name": "OSS Team"}})})
	case strings.Contains(req.Query, "query ListWorkflowStates("):
		appendCall("ListWorkflowStates")
		if req.Variables["teamId"] != "team-1" {
			f.t.Errorf("ListWorkflowStates team = %#v", req.Variables)
		}
		f.respond(w, map[string]any{"workflowStates": map[string]any{"nodes": []map[string]any{{"id": "state-icebox", "name": "Icebox", "type": "triage"}}}})
	case strings.Contains(req.Query, "query ListLabels("):
		appendCall("ListLabels")
		f.respond(w, map[string]any{"issueLabels": blockerSmokePage([]map[string]any{{"id": "label-nh", "name": "Needs Human"}})})
	case strings.Contains(req.Query, "mutation CreateIssue("):
		appendCall("CreateIssue")
		f.created++
		want := map[string]any{
			"teamId": "team-1", "title": "Need human review", "description": "\n---\n*Source issue: OSS-50*",
			"stateId": "state-icebox", "projectId": "proj-1", "labelIds": []any{"label-nh"},
		}
		if !reflect.DeepEqual(req.Variables["input"], want) {
			f.t.Errorf("CreateIssue input = %#v, want %#v", req.Variables["input"], want)
		}
		f.respond(w, map[string]any{"issueCreate": map[string]any{
			"success": true, "issue": blockerSmokeIssue("blocker-1", "OSS-99", "Need human review", "Icebox", "Needs Human"),
		}})
	case strings.Contains(req.Query, "mutation CreateRelation("):
		appendCall("CreateRelation")
		f.relationAttempts++
		want := map[string]any{"issueId": "blocker-1", "relatedIssueId": "source-1", "type": "blocks"}
		if !reflect.DeepEqual(req.Variables, want) {
			f.t.Errorf("CreateRelation variables = %#v, want %#v", req.Variables, want)
		}
		if f.relationRefused {
			if f.concurrentEdge {
				f.edges = append(f.edges, blockerSmokeEdge{id: "exact-edge", kind: "blocks", from: "blocker-1", to: "source-1"})
			}
			f.respond(w, map[string]any{"issueRelationCreate": map[string]any{"success": false}})
			return
		}
		if !f.ackMissing {
			f.edges = append(f.edges, blockerSmokeEdge{id: "exact-edge", kind: "blocks", from: "blocker-1", to: "source-1"})
		}
		f.respond(w, map[string]any{"issueRelationCreate": map[string]any{"success": true, "issueRelation": map[string]any{"id": "exact-edge"}}})
	case strings.Contains(req.Query, "mutation CreateComment("):
		appendCall("CreateComment")
		f.commentAttempts++
		wantIssue := "source-1"
		wantBody := "🚧 Human blocker created: [OSS-99](https://issues.example.invalid/i/OSS-99) - Need human review"
		if f.reuse {
			wantIssue = "blocker-1"
			wantBody = "+1 - Also needed by OSS-50"
		}
		if req.Variables["issueId"] != wantIssue || req.Variables["body"] != wantBody {
			f.t.Errorf("CreateComment variables = %#v, want issue=%s body=%q", req.Variables, wantIssue, wantBody)
		}
		if f.noticeRefused {
			f.respond(w, map[string]any{"commentCreate": map[string]any{"success": false}})
			return
		}
		f.comments = append(f.comments, wantBody)
		f.respond(w, map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{
			"id": "notice-1", "body": wantBody, "createdAt": "2025-01-01T00:00:00Z",
		}}})
	default:
		f.t.Errorf("unexpected GraphQL operation: %.100s", req.Query)
		f.refuse(w, "unexpected operation")
	}
}

type blockerSmokeCase struct {
	name            string
	reuse           bool
	alreadyLinked   bool
	relationRefused bool
	concurrentEdge  bool
	ackMissing      bool
	noticeRefused   bool
	lookupRefused   bool
	incompletePage  bool
	projectRefused  bool
	wantError       string
	wantCreated     int
	wantRelation    int
	wantReads       int
	wantComments    int
}

func TestLinearCreateBlockerDirectedOutcomeCLI(t *testing.T) {
	afh.SkipIfShort(t, "build-and-run public blocker CLI composition smoke")
	sourceDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer := buildBlockerOutcomeConsumer(t, sourceDir)

	cases := []blockerSmokeCase{
		{name: "new_verified", wantCreated: 1, wantRelation: 1, wantReads: 2, wantComments: 1},
		{name: "reused_from_different_source", reuse: true, wantRelation: 1, wantReads: 2, wantComments: 1},
		{name: "already_linked_reuse", reuse: true, alreadyLinked: true, wantReads: 1, wantComments: 1},
		{name: "new_relation_refused", relationRefused: true, wantCreated: 1, wantRelation: 1, wantReads: 2, wantError: "directed blocks relation not verified"},
		{name: "reused_relation_refused", reuse: true, relationRefused: true, wantRelation: 1, wantReads: 2, wantError: "directed blocks relation not verified"},
		{name: "new_notice_refused", noticeRefused: true, wantCreated: 1, wantRelation: 1, wantReads: 2, wantComments: 1, wantError: "source notice failed"},
		{name: "reused_notice_refused", reuse: true, noticeRefused: true, wantRelation: 1, wantReads: 2, wantComments: 1, wantError: "+1 notice failed"},
		{name: "concurrent_edge_reconciled", reuse: true, relationRefused: true, concurrentEdge: true, wantRelation: 1, wantReads: 2, wantComments: 1},
		{name: "acknowledged_without_edge", ackMissing: true, wantCreated: 1, wantRelation: 1, wantReads: 2, wantError: "directed blocks relation absent"},
		{name: "candidate_lookup_refused", lookupRefused: true, wantError: "blocker candidate lookup"},
		{name: "candidate_page_incomplete", incompletePage: true, wantError: "blocker candidate lookup"},
		{name: "candidate_project_refused", projectRefused: true, wantError: "resolve blocker candidate project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBlockerSmokeFixture(t, tc)
			server := httptest.NewServer(http.HandlerFunc(f.ServeHTTP))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, consumer, "linear", "create-blocker", "OSS-50", "--title", "Need human review") //nolint:gosec // fixed fixture binary and arguments.
			cmd.Env = []string{
				"HOME=" + t.TempDir(), "SMOKE_GRAPHQL_FIXTURE=" + server.URL,
				"LINEAR_ACCESS_TOKEN=", "WORKER_AUTH_TOKEN=", "GOWORK=off",
			}
			out, runErr := cmd.CombinedOutput()
			if tc.wantError == "" {
				if runErr != nil {
					t.Fatalf("create-blocker failed: %v\n%s", runErr, out)
				}
				var result map[string]any
				if err := json.Unmarshal(out, &result); err != nil {
					t.Fatalf("decode CLI success %q: %v", out, err)
				}
				want := map[string]any{
					"id": "blocker-1", "identifier": "OSS-99", "title": "Need human review",
					"url": "https://issues.example.invalid/i/OSS-99", "sourceIssue": "OSS-50",
					"relation": "blocks", "deduplicated": tc.reuse,
				}
				if !reflect.DeepEqual(result, want) {
					t.Errorf("success JSON = %#v, want %#v", result, want)
				}
			} else {
				if runErr == nil {
					t.Fatalf("create-blocker succeeded despite %s: %s", tc.wantError, out)
				}
				if !strings.Contains(string(out), tc.wantError) {
					t.Errorf("CLI failure = %q, want %q", out, tc.wantError)
				}
				if tc.wantCreated > 0 || tc.reuse {
					for _, want := range []string{"create-blocker partial:", "blocker-1", "OSS-99"} {
						if !strings.Contains(string(out), want) {
							t.Errorf("partial outcome omitted %q: %s", want, out)
						}
					}
				}
				if strings.Contains(string(out), `"relation":"blocks"`) {
					t.Errorf("failure emitted success relation JSON: %s", out)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.created != tc.wantCreated || f.relationAttempts != tc.wantRelation || f.relationReads != tc.wantReads || f.commentAttempts != tc.wantComments {
				t.Errorf("calls=%v created=%d relation=%d reads=%d comments=%d; want %d/%d/%d/%d",
					f.calls, f.created, f.relationAttempts, f.relationReads, f.commentAttempts,
					tc.wantCreated, tc.wantRelation, tc.wantReads, tc.wantComments)
			}
			for _, id := range []string{"reverse-edge", "other-blocker-edge", "wrong-type-edge", "different-source-edge"} {
				found := false
				for _, edge := range f.edges {
					found = found || edge.id == id
				}
				if !found {
					t.Errorf("unrelated relationship %s was lost", id)
				}
			}
			wantExact := tc.alreadyLinked || tc.concurrentEdge || (tc.wantRelation == 1 && !tc.relationRefused && !tc.ackMissing)
			exact := 0
			for _, edge := range f.edges {
				if edge.kind == "blocks" && edge.from == "blocker-1" && edge.to == "source-1" {
					exact++
				}
			}
			if (wantExact && exact != 1) || (!wantExact && exact != 0) {
				t.Errorf("exact directed blocker edges = %d, want one=%t", exact, wantExact)
			}
			wantPosted := tc.wantComments == 1 && !tc.noticeRefused
			if (len(f.comments) == 1) != wantPosted {
				t.Errorf("posted notices=%v, want posted=%t", f.comments, wantPosted)
			}
			t.Logf("calls=%v created=%d relation-attempts=%d relation-reads=%d comments=%d exact-edges=%d",
				f.calls, f.created, f.relationAttempts, f.relationReads, f.commentAttempts, exact)
		})
	}
	if !t.Failed() {
		afh.RecordLive(t.Name(), afh.LiveExercised, "composed create-blocker CLI against complete local GraphQL fixture")
	}
}
