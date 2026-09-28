package smokes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// This fixture exercises the OSS CLI's direct Linear adapter. The composed
// consumer's transport only forwards Linear GraphQL requests to localhost.
type labelGroupSmokeFixture struct {
	mu          sync.Mutex
	labels      []map[string]any
	issueLabels []string
	creates     int
	updates     int
	adds        int
}

func (f *labelGroupSmokeFixture) label(id string) map[string]any {
	for _, label := range f.labels {
		if label["id"] == id {
			return label
		}
	}
	return nil
}

func (f *labelGroupSmokeFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "smoke-fixture-only" {
		http.Error(w, "unexpected transport", http.StatusBadRequest)
		return
	}
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid GraphQL body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(data map[string]any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	switch {
	case strings.Contains(request.Query, "ListTeams"):
		write(map[string]any{"teams": map[string]any{"nodes": []map[string]any{
			{"id": "team-1", "key": "OSS", "name": "Example", "parent": map[string]any{"id": "parent-team"}},
			{"id": "parent-team", "key": "PARENT", "name": "Parent", "parent": nil},
		}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}})
	case strings.Contains(request.Query, "ListLabelDetails"):
		// The catalog is deliberately active-only. Archived attached labels must
		// be classified from their own parent metadata during selection.
		active := make([]map[string]any, 0, len(f.labels))
		allowed := map[string]bool{}
		if filter, ok := request.Variables["filter"].(map[string]any); ok {
			for _, clause := range filter["or"].([]any) {
				team := clause.(map[string]any)["team"].(map[string]any)
				if team["null"] == true {
					allowed[""] = true
				} else {
					allowed[team["id"].(map[string]any)["eq"].(string)] = true
				}
			}
		}
		for _, label := range f.labels {
			if label["archived"] == true {
				continue
			}
			if len(allowed) > 0 {
				team, _ := label["team"].(map[string]any)
				if team == nil && !allowed[""] || team != nil && !allowed[team["id"].(string)] {
					continue
				}
			}
			active = append(active, label)
		}
		write(map[string]any{"issueLabels": map[string]any{"nodes": active, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}})
	case strings.Contains(request.Query, "CreateNativeLabel"):
		f.creates++
		input := request.Variables["input"].(map[string]any)
		id := fmt.Sprintf("created-%d", f.creates)
		label := map[string]any{"id": id, "name": input["name"], "isGroup": false, "groupType": nil, "team": nil, "parent": nil}
		if input["isGroup"] == true {
			label["isGroup"], label["groupType"] = true, input["groupType"]
		}
		if parentID, ok := input["parentId"].(string); ok {
			label["parent"] = map[string]any{"id": parentID, "name": f.label(parentID)["name"]}
		}
		f.labels = append(f.labels, label)
		write(map[string]any{"issueLabelCreate": map[string]any{"success": true, "issueLabel": label}})
	case strings.Contains(request.Query, "ReparentLabel"):
		f.updates++
		id := request.Variables["id"].(string)
		parentID := request.Variables["input"].(map[string]any)["parentId"].(string)
		label := f.label(id)
		label["parent"] = map[string]any{"id": parentID, "name": f.label(parentID)["name"]}
		write(map[string]any{"issueLabelUpdate": map[string]any{"success": true, "issueLabel": label}})
	case strings.Contains(request.Query, "ListIssueLabels"):
		start := 0
		if request.Variables["after"] == "first-page" {
			start = 1
		}
		end := len(f.issueLabels)
		if start == 0 && end > 1 {
			end = 1
		}
		rows := make([]map[string]any, 0, end-start)
		for _, id := range f.issueLabels[start:end] {
			label := f.label(id)
			var parent any
			if relation, ok := label["parent"].(map[string]any); ok {
				parent = map[string]any{"id": relation["id"]}
			}
			rows = append(rows, map[string]any{"id": id, "name": label["name"], "isGroup": label["isGroup"], "parent": parent})
		}
		hasNext := end < len(f.issueLabels)
		var cursor any
		if hasNext {
			cursor = "first-page"
		}
		write(map[string]any{"issue": map[string]any{"id": "issue-1", "labels": map[string]any{"nodes": rows, "pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor}}}})
	case strings.Contains(request.Query, "GetIssue"):
		// The ordinary issue fragment intentionally exposes only its first label.
		// Selection must use the complete paginated issue-label read above.
		rows := make([]map[string]any, 0, 1)
		if len(f.issueLabels) > 0 {
			id := f.issueLabels[0]
			rows = append(rows, map[string]any{"id": id, "name": f.label(id)["name"]})
		}
		write(map[string]any{"issue": f.issue(rows)})
	case strings.Contains(request.Query, "AddIssueLabel"):
		f.adds++
		targetID := request.Variables["labelId"].(string)
		parent := f.label(targetID)["parent"].(map[string]any)["id"]
		kept := make([]string, 0, len(f.issueLabels)+1)
		for _, id := range f.issueLabels {
			otherParent, _ := f.label(id)["parent"].(map[string]any)
			if otherParent == nil || otherParent["id"] != parent {
				kept = append(kept, id)
			}
		}
		f.issueLabels = append(kept, targetID)
		write(map[string]any{"issueAddLabel": map[string]any{"success": true, "issue": f.issue(nil)}})
	default:
		http.Error(w, "unexpected GraphQL operation", http.StatusBadRequest)
	}
}

func (f *labelGroupSmokeFixture) issue(labels []map[string]any) map[string]any {
	return map[string]any{
		"id": "issue-1", "identifier": "OSS-1", "title": "Fixture",
		"team":   map[string]any{"id": "team-1", "key": "OSS", "name": "Example"},
		"state":  map[string]any{"id": "state-1", "name": "Backlog"},
		"labels": map[string]any{"nodes": labels},
	}
}

func buildLinearLabelConsumer(t *testing.T, sourceDir string) string {
	t.Helper()
	consumer := buildLinearPriorityConsumer(t, sourceDir)
	mainPath := filepath.Join(filepath.Dir(consumer), "main.go")
	contents, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	withError := strings.Replace(string(contents), "\"errors\"", "\"errors\"\n\t\"fmt\"", 1)
	withError = strings.Replace(withError, "if err := root.Execute(); err != nil {\n\t\tos.Exit(1)", "if err := root.Execute(); err != nil {\n\t\tfmt.Fprintln(os.Stderr, err)\n\t\tos.Exit(1)", 1)
	if withError == string(contents) {
		t.Fatal("consumer error-reporting transform did not apply")
	}
	if err := os.WriteFile(mainPath, []byte(withError), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", consumer, ".") //nolint:gosec // isolated generated fixture source.
	cmd.Dir = filepath.Dir(consumer)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture consumer: %v\n%s", err, output)
	}
	return consumer
}

func TestLinearNativeLabelGroupCLI(t *testing.T) {
	afh.SkipIfShort(t, "build-and-run public CLI composition smoke")
	sourceDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer := buildLinearLabelConsumer(t, sourceDir)
	afh.RecordLive(t.Name(), afh.LiveExercised, "composed afcli from donmai source")

	fixture := &labelGroupSmokeFixture{labels: []map[string]any{
		{"id": "manual-group", "name": "Type", "isGroup": true, "groupType": nil, "team": nil, "parent": nil},
		{"id": "team-group", "name": "Type", "isGroup": true, "groupType": "singleSelect", "team": map[string]any{"id": "team-1", "key": "OSS"}, "parent": nil},
		{"id": "manual-child", "name": "Bug", "isGroup": false, "groupType": nil, "team": nil, "parent": map[string]any{"id": "manual-group", "name": "Type"}},
		{"id": "parent-group", "name": "Priority", "isGroup": true, "groupType": "singleSelect", "team": map[string]any{"id": "parent-team", "key": "PARENT"}, "parent": nil},
		{"id": "parent-old", "name": "Old", "isGroup": false, "groupType": nil, "team": map[string]any{"id": "parent-team", "key": "PARENT"}, "parent": map[string]any{"id": "parent-group", "name": "Priority"}},
		{"id": "archived-parent-old", "name": "Archived Old", "isGroup": false, "groupType": nil, "team": map[string]any{"id": "parent-team", "key": "PARENT"}, "parent": map[string]any{"id": "parent-group", "name": "Priority"}, "archived": true},
		{"id": "parent-next", "name": "New", "isGroup": false, "groupType": nil, "team": map[string]any{"id": "parent-team", "key": "PARENT"}, "parent": map[string]any{"id": "parent-group", "name": "Priority"}},
		{"id": "flat", "name": "Type: legacy flat", "isGroup": false, "groupType": nil, "team": nil, "parent": nil},
		{"id": "other", "name": "Urgent", "isGroup": false, "groupType": nil, "team": nil, "parent": nil},
		{"id": "archived-other", "name": "Old Urgent", "isGroup": false, "groupType": nil, "team": nil, "parent": nil, "archived": true},
	}, issueLabels: []string{"parent-old", "archived-parent-old", "other", "archived-other"}}
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(server.Close)
	run := func(args ...string) map[string]any {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, consumer, append([]string{"linear"}, args...)...) //nolint:gosec // fixed fixture binary and test-controlled arguments.
		cmd.Env = append(os.Environ(), "SMOKE_GRAPHQL_FIXTURE="+server.URL, "LINEAR_ACCESS_TOKEN=", "WORKER_AUTH_TOKEN=")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("composed CLI %v failed: %v\n%s", args, err, output)
		}
		var value map[string]any
		if args[0] == "list-labels" {
			var labels []map[string]any
			if err := json.Unmarshal(output, &labels); err != nil {
				t.Fatalf("decode catalog: %v\n%s", err, output)
			}
			return map[string]any{"labels": labels}
		}
		if err := json.Unmarshal(output, &value); err != nil {
			t.Fatalf("decode command output: %v\n%s", err, output)
		}
		return value
	}

	catalog := run("list-labels")["labels"].([]map[string]any)
	byID := map[string]map[string]any{}
	for _, label := range catalog {
		byID[label["id"].(string)] = label
	}
	if len(catalog) != 8 || byID["manual-group"]["scope"] != "workspace" || byID["manual-group"]["isGroup"] != true ||
		byID["team-group"]["scope"] != "team" || byID["team-group"]["teamId"] != "team-1" ||
		byID["manual-child"]["parentId"] != "manual-group" || byID["parent-next"]["teamId"] != "parent-team" ||
		byID["flat"]["isGroup"] != false {
		t.Fatalf("native catalog lost stable identity/scope/membership: %v", catalog)
	}
	for i := 0; i < 2; i++ {
		if result := run("create-label-group", "--name", "Type", "--workspace"); result["reused"] != true {
			t.Fatalf("manual group was not reused: %v", result)
		}
		if result := run("create-group-label", "--name", "Bug", "--group-id", "manual-group", "--workspace"); result["reused"] != true {
			t.Fatalf("manual child was not reused: %v", result)
		}
	}
	if result := run("create-label-group", "--name", "Kind", "--workspace"); result["reused"] != false || result["label"].(map[string]any)["id"] != "created-1" {
		t.Fatalf("new native group not created: %v", result)
	}
	if result := run("create-group-label", "--name", "Feature", "--group-id", "created-1", "--workspace"); result["reused"] != false || result["label"].(map[string]any)["parentId"] != "created-1" {
		t.Fatalf("new child not attached: %v", result)
	}
	if result := run("reparent-label", "--label-id", "flat", "--group-id", "created-1", "--workspace"); result["label"].(map[string]any)["id"] != "flat" || result["label"].(map[string]any)["parentId"] != "created-1" {
		t.Fatalf("reparent changed identity or failed membership: %v", result)
	}
	if result := run("select-group-label", "OSS-1", "--label-id", "parent-next"); result["appliedLabelId"] != "parent-next" {
		t.Fatalf("inherited parent-team selection failed: %v", result)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.creates != 2 || fixture.updates != 1 || fixture.adds != 1 || strings.Join(fixture.issueLabels, ",") != "other,archived-other,parent-next" {
		t.Fatalf("native workflow drift: creates=%d updates=%d adds=%d labels=%v", fixture.creates, fixture.updates, fixture.adds, fixture.issueLabels)
	}
}
