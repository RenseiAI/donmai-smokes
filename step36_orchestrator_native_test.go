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
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// The consumer registers the public command factories and redirects only the
// tracker's fixed HTTPS GraphQL origin to a private loopback fixture. The
// transport has no proxy and refuses every other destination.
const orchestratorNativeConsumer = `package main

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
		return nil, errors.New("smoke rejected non-fixture network request")
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
	root := &cobra.Command{Use: "smoke-consumer", SilenceUsage: true, SilenceErrors: true}
	root.SetArgs(os.Args[1:])
	afcli.RegisterCommands(root, afcli.Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
		BinaryName: "smoke-consumer",
	})
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

func buildOrchestratorNativeConsumer(t *testing.T, sourceDir string) string {
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
	goMod := fmt.Sprintf("module donmai-smokes-orchestrator-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0-00010101000000-000000000000\n\nreplace github.com/RenseiAI/donmai => %s\n", version, sourceDir)
	for name, contents := range map[string]string{"go.mod": goMod, "main.go": orchestratorNativeConsumer} {
		if err := os.WriteFile(filepath.Join(moduleDir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write consumer %s: %v", name, err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-o", filepath.Join(moduleDir, "smoke-consumer"), "."}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // fixed tool and test-controlled paths.
		cmd.Dir = moduleDir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("consumer go %s against selected source: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	return filepath.Join(moduleDir, "smoke-consumer")
}

type orchestratorFixture struct {
	issues []fixtureIssue
}

type fixtureIssue struct {
	id         string
	identifier string
	title      string
}

func (f orchestratorFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "smoke-fixture-only" {
		http.Error(w, "unexpected fixture request", http.StatusBadRequest)
		return
	}
	var request struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid GraphQL request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	write := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	switch {
	case strings.Contains(request.Query, "query GetIssue"):
		write(map[string]any{"issue": f.issue(f.issues[0])})
	case strings.Contains(request.Query, "query ListIssuesByProject"):
		nodes := make([]map[string]any, 0, len(f.issues))
		for _, issue := range f.issues {
			nodes = append(nodes, f.issue(issue))
		}
		write(map[string]any{"issues": map[string]any{"nodes": nodes}})
	default:
		http.Error(w, "unexpected GraphQL operation", http.StatusBadRequest)
	}
}

func (orchestratorFixture) issue(issue fixtureIssue) map[string]any {
	return map[string]any{
		"id": issue.id, "identifier": issue.identifier, "title": issue.title,
		"description": "Deterministic provider protocol fixture.",
		"url":         "https://example.test/issue/" + issue.identifier,
		"state":       map[string]any{"id": "state-1", "name": "Backlog"},
		"team":        map[string]any{"id": "team-1", "key": "OSS", "name": "Example"},
		"project":     map[string]any{"id": "project-1", "name": "Example"},
		"labels":      map[string]any{"nodes": []any{}},
	}
}

// This executable implements the Claude headless stream-json protocol used by
// the shipped adapter. It never contacts a model. The start and release files
// make the single-run assertion depend on the provider's terminal event.
const fakeClaudeProtocol = `#!/bin/sh
set -eu
has_print=false
has_jsonl=false
has_verbose=false
expect_format=false
for arg in "$@"; do
  case "$arg" in
    -p) has_print=true ;;
    --output-format) expect_format=true ;;
    stream-json) if [ "$expect_format" = true ]; then has_jsonl=true; fi ;;
    --verbose) has_verbose=true ;;
    --templates) echo "template adapter argument rejected" >&2; exit 98 ;;
  esac
done
if [ "$has_print" != true ] || [ "$has_jsonl" != true ] || [ "$has_verbose" != true ]; then
  echo "unexpected native provider arguments: $*" >&2
  exit 97
fi
cat >/dev/null
issue=${LINEAR_ISSUE_IDENTIFIER:-single}
printf '%s\n' '{"type":"system","subtype":"init","session_id":"smoke-session"}'
printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fixture started for %s"}]}}\n' "$issue"
if [ -n "${SMOKE_PROVIDER_STARTED_DIR:-}" ]; then
  : > "$SMOKE_PROVIDER_STARTED_DIR/$issue"
fi
if [ -n "${SMOKE_PROVIDER_RELEASE_FILE:-}" ]; then
  while [ ! -f "$SMOKE_PROVIDER_RELEASE_FILE" ]; do sleep 0.02; done
fi
if [ "$issue" = "OSS-FAIL" ] || [ "${SMOKE_PROVIDER_FAILURE:-}" = "1" ]; then
  printf '%s\n' '{"type":"result","subtype":"error_max_turns","is_error":true,"errors":[{"message":"controlled provider terminal failure"}],"num_turns":1}'
else
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"fixture completed","num_turns":1}'
fi
`

func writeOrchestratorFixtures(t *testing.T) (providerDir, startedDir, releaseFile string) {
	t.Helper()
	providerDir = t.TempDir()
	startedDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(providerDir, "claude"), []byte(fakeClaudeProtocol), 0o600); err != nil {
		t.Fatalf("write native protocol fixture: %v", err)
	}
	if err := os.Chmod(filepath.Join(providerDir, "claude"), 0o700); err != nil {
		t.Fatalf("make native protocol fixture executable: %v", err)
	}
	releaseFile = filepath.Join(t.TempDir(), "release-terminal")
	return providerDir, startedDir, releaseFile
}

func runOrchestratorCLI(t *testing.T, consumer, workDir, fixtureURL, providerDir, startedDir, releaseFile string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	fullArgs := append([]string{"orchestrator", "--linear-key", "smoke-fixture-only"}, args...)
	cmd := exec.CommandContext(ctx, consumer, fullArgs...) //nolint:gosec // fixed consumer and test-controlled arguments.
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + providerDir + ":/usr/bin:/bin",
		"SMOKE_GRAPHQL_FIXTURE=" + fixtureURL,
		"SMOKE_PROVIDER_STARTED_DIR=" + startedDir,
	}
	if releaseFile != "" {
		cmd.Env = append(cmd.Env, "SMOKE_PROVIDER_RELEASE_FILE="+releaseFile)
	}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(output), fmt.Errorf("composed CLI timed out: %w", ctx.Err())
	}
	return string(output), err
}

func newOrchestratorFixtureServer(t *testing.T, issues ...fixtureIssue) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(orchestratorFixture{issues: issues}.serve))
	t.Cleanup(server.Close)
	return server
}

func setupOrchestratorConsumer(t *testing.T) (consumer, workDir string) {
	t.Helper()
	afh.SkipIfShort(t, "build-and-run composed public orchestrator CLI smoke")
	sourceDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer = buildOrchestratorNativeConsumer(t, sourceDir)
	workDir = t.TempDir()
	init := exec.Command("git", "init", "-q") //nolint:gosec // fixed executable and arguments.
	init.Dir = workDir
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("initialize isolated git project: %v\n%s", err, output)
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "composed public afcli orchestrator from selected Donmai source")
	return consumer, workDir
}

func waitForProviderStart(t *testing.T, startedDir, issue string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(startedDir, issue)); err == nil {
			return
		}
		select {
		case <-done:
			t.Fatalf("CLI exited before native provider reached its terminal gate for %s", issue)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatalf("native provider fixture did not start for %s", issue)
}

func TestOrchestratorSingleWaitsForNativeProviderTerminal(t *testing.T) {
	consumer, workDir := setupOrchestratorConsumer(t)
	providerDir, startedDir, releaseFile := writeOrchestratorFixtures(t)
	server := newOrchestratorFixtureServer(t, fixtureIssue{id: "issue-1", identifier: "OSS-1", title: "Await provider"})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, consumer, "orchestrator", "--linear-key", "smoke-fixture-only", "--single", "OSS-1") //nolint:gosec // fixture consumer and fixed arguments.
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + t.TempDir(), "PATH=" + providerDir + ":/usr/bin:/bin",
		"SMOKE_GRAPHQL_FIXTURE=" + server.URL,
		"SMOKE_PROVIDER_STARTED_DIR=" + startedDir,
		"SMOKE_PROVIDER_RELEASE_FILE=" + releaseFile,
	}
	var output strings.Builder
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start composed CLI: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waitForProviderStart(t, startedDir, "OSS-1", done)
	select {
	case err := <-done:
		t.Fatalf("CLI completed before provider terminal release: %v\n%s", err, output.String())
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.WriteFile(releaseFile, []byte("release"), 0o600); err != nil {
		t.Fatalf("release provider terminal event: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("composed CLI failed after successful provider terminal result: %v\n%s", err, output.String())
		}
	case <-ctx.Done():
		t.Fatalf("CLI did not finish after provider terminal release: %v\n%s", ctx.Err(), output.String())
	}
	if !strings.Contains(output.String(), "completed") {
		t.Fatalf("CLI summary omitted completed terminal status:\n%s", output.String())
	}
}

func TestOrchestratorSinglePropagatesNativeProviderFailure(t *testing.T) {
	consumer, workDir := setupOrchestratorConsumer(t)
	providerDir, startedDir, _ := writeOrchestratorFixtures(t)
	server := newOrchestratorFixtureServer(t, fixtureIssue{id: "issue-1", identifier: "OSS-FAIL", title: "Fail provider"})
	output, err := runOrchestratorCLI(t, consumer, workDir, server.URL, providerDir, startedDir, "", "--single", "OSS-FAIL")
	if err == nil {
		t.Fatalf("composed CLI returned success after a provider terminal failure:\n%s", output)
	}
	if !strings.Contains(output, "controlled provider terminal failure") || !strings.Contains(output, "error_max_turns") {
		t.Fatalf("failure did not come from the native provider terminal result (err=%v):\n%s", err, output)
	}
	if _, statErr := os.Stat(filepath.Join(startedDir, "OSS-FAIL")); statErr != nil {
		t.Fatalf("native provider did not reach terminal failure path: %v", statErr)
	}
}

func TestOrchestratorBacklogAwaitsEachProviderAndReportsFailure(t *testing.T) {
	consumer, workDir := setupOrchestratorConsumer(t)
	providerDir, startedDir, _ := writeOrchestratorFixtures(t)
	server := newOrchestratorFixtureServer(t,
		fixtureIssue{id: "issue-1", identifier: "OSS-1", title: "Complete provider"},
		fixtureIssue{id: "issue-2", identifier: "OSS-FAIL", title: "Fail provider"},
	)
	output, err := runOrchestratorCLI(t, consumer, workDir, server.URL, providerDir, startedDir, "", "--project", "Example", "--max", "2")
	if err == nil {
		t.Fatalf("backlog CLI returned success after one provider terminal failure:\n%s", output)
	}
	for _, issue := range []string{"OSS-1", "OSS-FAIL"} {
		if _, statErr := os.Stat(filepath.Join(startedDir, issue)); statErr != nil {
			t.Fatalf("backlog did not run native provider for %s: %v\n%s", issue, statErr, output)
		}
	}
	if !strings.Contains(output, "completed") || !strings.Contains(output, "controlled provider terminal failure") || !strings.Contains(output, "error_max_turns") {
		t.Fatalf("backlog summary did not preserve the successful and failed terminal outcomes (err=%v):\n%s", err, output)
	}
}
