package smokes

// This smoke composes the public afcli command factory in a temporary consumer.
// Its direct GraphQL transport is intercepted inside that process and forwarded
// only to the local fixture; it neither contacts a tracker nor starts a daemon.

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

const linearPriorityConsumer = `package main

import (
	"errors"
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
	// NewClient uses a nil Transport, so it inherits this strict process-local
	// transport. Proxy is explicitly nil and no real network origin is reachable.
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
		os.Exit(1)
	}
}
`

func buildLinearPriorityConsumer(t *testing.T, sourceDir string) string {
	t.Helper()
	moduleBytes, err := os.ReadFile(filepath.Join(sourceDir, "go.mod"))
	if err != nil {
		t.Fatalf("read donmai go.mod: %v", err)
	}
	version := ""
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			version = fields[1]
			break
		}
	}
	if version == "" {
		t.Fatal("donmai go.mod lacks a go directive")
	}
	moduleDir := t.TempDir()
	goMod := fmt.Sprintf("module donmai-smokes-linear-priority-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0-00010101000000-000000000000\n\nreplace github.com/RenseiAI/donmai => %s\n", version, sourceDir)
	for name, contents := range map[string]string{"go.mod": goMod, "main.go": linearPriorityConsumer} {
		if err := os.WriteFile(filepath.Join(moduleDir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write consumer %s: %v", name, err)
		}
	}
	env := append(os.Environ(), "GOWORK=off")
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-o", filepath.Join(moduleDir, "smoke-consumer"), "."}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // test-controlled arguments and directory.
		cmd.Dir, cmd.Env = moduleDir, env
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("consumer go %s against %s: %v\n%s", strings.Join(args, " "), sourceDir, err, output)
		}
	}
	return filepath.Join(moduleDir, "smoke-consumer")
}

func TestLinearUpdatePriorityResponseCLI(t *testing.T) {
	afh.SkipIfShort(t, "build-and-run public CLI composition smoke")
	sourceDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer := buildLinearPriorityConsumer(t, sourceDir)
	afh.RecordLive(t.Name(), afh.LiveExercised, "composed afcli from donmai source")

	tests := []struct {
		name         string
		args         []string
		initial      int
		result       int
		wantInput    int
		inputPresent bool
	}{
		{name: "nonzero_uses_result", args: []string{"--priority", "1"}, initial: 4, result: 2, wantInput: 1, inputPresent: true},
		{name: "explicit_zero", args: []string{"--priority", "0"}, initial: 3, result: 0, wantInput: 0, inputPresent: true},
		{name: "omitted_retains_result", args: []string{"--title", "Retitled"}, initial: 3, result: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			mutationCount := 0
			var mutationInput map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/graphql" || request.Header.Get("Authorization") != "smoke-fixture-only" {
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				var body struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				issue := func(priority int) string {
					return fmt.Sprintf(`{"id":"issue-1","identifier":"OSS-1","title":"Fixture","url":"https://example.test/issue/OSS-1","priority":%d,"state":{"id":"state-1","name":"Backlog"},"team":{"id":"team-1","key":"OSS","name":"OSS"},"project":{"id":"project-1","name":"Example"},"labels":{"nodes":[]}}`, priority)
				}
				switch {
				case strings.Contains(body.Query, "query GetIssue"):
					_, _ = fmt.Fprintf(w, `{"data":{"issue":%s}}`, issue(test.initial))
				case strings.Contains(body.Query, "mutation UpdateIssue"):
					mu.Lock()
					mutationCount++
					mutationInput, _ = body.Variables["input"].(map[string]any)
					mu.Unlock()
					_, _ = fmt.Fprintf(w, `{"data":{"issueUpdate":{"success":true,"issue":%s}}}`, issue(test.result))
				default:
					http.Error(w, "unexpected operation", http.StatusBadRequest)
				}
			}))
			t.Cleanup(server.Close)

			args := append([]string{"linear", "update-issue", "OSS-1"}, test.args...)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, consumer, args...) //nolint:gosec // fixed fixture binary and table-controlled arguments.
			cmd.Env = append(os.Environ(), "SMOKE_GRAPHQL_FIXTURE="+server.URL, "LINEAR_ACCESS_TOKEN=", "WORKER_AUTH_TOKEN=")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("composed CLI update failed: %v\n%s", err, output)
			}
			mu.Lock()
			defer mu.Unlock()
			if mutationCount != 1 {
				t.Fatalf("mutation calls = %d, want 1", mutationCount)
			}
			value, hasInput := mutationInput["priority"]
			if hasInput != test.inputPresent || (hasInput && value != float64(test.wantInput)) {
				t.Fatalf("mutation priority = (%v, %v), want (%d, %v)", value, hasInput, test.wantInput, test.inputPresent)
			}
			var response map[string]json.RawMessage
			if err := json.Unmarshal(output, &response); err != nil {
				t.Fatalf("decode CLI output %q: %v", output, err)
			}
			rawPriority, present := response["priority"]
			if !present {
				t.Fatalf("CLI update response omitted priority: %s", output)
			}
			var priority int
			if err := json.Unmarshal(rawPriority, &priority); err != nil || priority != test.result {
				t.Fatalf("CLI priority = %s (error %v), want authoritative %d", rawPriority, err, test.result)
			}
		})
	}
}
