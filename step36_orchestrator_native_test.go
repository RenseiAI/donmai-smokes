package smokes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// This executable accepts the old --print form and the shipped adapter's
// native headless stream-json form. Both emit a deterministic protocol result;
// it never contacts a model. Start and release files make the single-run
// assertion depend on the provider's terminal event.
const fakeClaudeProtocol = `#!/bin/sh
set -eu
mode=
has_print=false
has_jsonl=false
has_verbose=false
expect_format=false
for arg in "$@"; do
  case "$arg" in
    -p) has_print=true ;;
    --print) mode=legacy ;;
    --output-format) expect_format=true ;;
    stream-json) if [ "$expect_format" = true ]; then has_jsonl=true; fi ;;
    --verbose) has_verbose=true ;;
    --templates) echo "template adapter argument rejected" >&2; exit 98 ;;
  esac
done
if [ "$has_print" = true ] && [ "$has_jsonl" = true ] && [ "$has_verbose" = true ]; then
  mode=native
elif [ "$mode" != legacy ]; then
  echo "unexpected native provider arguments: $*" >&2
  exit 97
fi
cat >/dev/null
issue=${LINEAR_ISSUE_IDENTIFIER:-single}
if [ -n "${SMOKE_PROVIDER_MODE_DIR:-}" ]; then
  printf '%s' "$mode" > "$SMOKE_PROVIDER_MODE_DIR/$issue"
fi
if [ -n "${SMOKE_PROVIDER_PID_DIR:-}" ]; then
  printf '%s' "$$" > "$SMOKE_PROVIDER_PID_DIR/$issue"
fi
printf '%s\n' '{"type":"system","subtype":"init","session_id":"smoke-session"}'
printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fixture started for %s"}]}}\n' "$issue"
if [ -n "${SMOKE_PROVIDER_STARTED_DIR:-}" ]; then
  : > "$SMOKE_PROVIDER_STARTED_DIR/$issue"
fi
if [ "$issue" = "OSS-MUTATE" ]; then
  git -C "$SMOKE_PROVIDER_GIT_DIR" remote set-url origin "https://example.test/changed/repo.git"
fi
if [ -n "${SMOKE_PROVIDER_RELEASE_FILE:-}" ]; then
  attempts=0
  while [ ! -f "$SMOKE_PROVIDER_RELEASE_FILE" ] && [ "$attempts" -lt 500 ]; do
    sleep 0.02
    attempts=$((attempts + 1))
  done
  if [ ! -f "$SMOKE_PROVIDER_RELEASE_FILE" ]; then
    echo "fixture release timed out" >&2
    exit 96
  fi
fi
if [ "$issue" = "OSS-FAIL" ] || [ "${SMOKE_PROVIDER_FAILURE:-}" = "1" ]; then
  printf '%s\n' '{"type":"result","subtype":"error_max_turns","is_error":true,"errors":[{"message":"controlled provider terminal failure"}],"num_turns":1}'
  if [ -n "${SMOKE_PROVIDER_TERMINAL_DIR:-}" ]; then : > "$SMOKE_PROVIDER_TERMINAL_DIR/$issue"; fi
  if [ -n "${SMOKE_PROVIDER_FINISHED_DIR:-}" ]; then : > "$SMOKE_PROVIDER_FINISHED_DIR/$issue"; fi
  exit 23
else
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"fixture completed","num_turns":1}'
  if [ -n "${SMOKE_PROVIDER_TERMINAL_DIR:-}" ]; then : > "$SMOKE_PROVIDER_TERMINAL_DIR/$issue"; fi
  if [ -n "${SMOKE_PROVIDER_FINISHED_DIR:-}" ]; then : > "$SMOKE_PROVIDER_FINISHED_DIR/$issue"; fi
fi
`

func writeOrchestratorFixtures(t *testing.T) (providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, releaseFile string) {
	t.Helper()
	providerDir = t.TempDir()
	startedDir = t.TempDir()
	terminalDir = t.TempDir()
	finishedDir = t.TempDir()
	modeDir = t.TempDir()
	pidDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(providerDir, "claude"), []byte(fakeClaudeProtocol), 0o600); err != nil {
		t.Fatalf("write native protocol fixture: %v", err)
	}
	if err := os.Chmod(filepath.Join(providerDir, "claude"), 0o700); err != nil {
		t.Fatalf("make native protocol fixture executable: %v", err)
	}
	releaseFile = filepath.Join(t.TempDir(), "release-terminal")
	return providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, releaseFile
}

type orchestratorCLIProcess struct {
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	done       chan struct{}
	err        error
	outputFile *os.File
	outputPath string
	release    string
	pidDir     string
}

func startOrchestratorCLI(t *testing.T, consumer, workDir, fixtureURL, providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, releaseFile string, args ...string) *orchestratorCLIProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	fullArgs := append([]string{"orchestrator", "--linear-key", "smoke-fixture-only"}, args...)
	cmd := exec.CommandContext(ctx, consumer, fullArgs...) //nolint:gosec // fixed consumer and test-controlled arguments.
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + providerDir + ":/usr/bin:/bin",
		"SMOKE_GRAPHQL_FIXTURE=" + fixtureURL,
		"SMOKE_PROVIDER_STARTED_DIR=" + startedDir,
		"SMOKE_PROVIDER_TERMINAL_DIR=" + terminalDir,
		"SMOKE_PROVIDER_FINISHED_DIR=" + finishedDir,
		"SMOKE_PROVIDER_MODE_DIR=" + modeDir,
		"SMOKE_PROVIDER_PID_DIR=" + pidDir,
		"SMOKE_PROVIDER_GIT_DIR=" + workDir,
	}
	if releaseFile != "" {
		cmd.Env = append(cmd.Env, "SMOKE_PROVIDER_RELEASE_FILE="+releaseFile)
	}
	outputFile, err := os.CreateTemp(t.TempDir(), "orchestrator-cli-output-*.log")
	if err != nil {
		cancel()
		t.Fatalf("create CLI output file: %v", err)
	}
	cmd.Stdout, cmd.Stderr = outputFile, outputFile
	process := &orchestratorCLIProcess{
		cmd: cmd, cancel: cancel, done: make(chan struct{}), outputFile: outputFile,
		outputPath: outputFile.Name(), release: releaseFile, pidDir: pidDir,
	}
	if err := cmd.Start(); err != nil {
		_ = outputFile.Close()
		cancel()
		t.Fatalf("start composed CLI: %v", err)
	}
	go func() {
		process.err = cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		if process.release != "" {
			_ = os.WriteFile(process.release, []byte("cleanup release"), 0o600)
		}
		if entries, err := os.ReadDir(process.pidDir); err == nil {
			for _, entry := range entries {
				if !waitForProviderExitUntil(process.pidDir, entry.Name(), time.Now().Add(3*time.Second)) {
					t.Errorf("provider fixture process for %s was not reaped during cleanup", entry.Name())
				}
			}
		}
		select {
		case <-process.done:
		case <-time.After(3 * time.Second):
			process.cancel()
			select {
			case <-process.done:
			case <-time.After(2 * time.Second):
				t.Errorf("composed CLI process did not exit during cleanup")
			}
		}
		process.cancel()
		if err := outputFile.Close(); err != nil {
			t.Errorf("close CLI output file: %v", err)
		}
	})
	return process
}

func (p *orchestratorCLIProcess) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-p.done:
		return p.err
	case <-time.After(42 * time.Second):
		p.cancel()
		select {
		case <-p.done:
			return fmt.Errorf("composed CLI timed out")
		case <-time.After(2 * time.Second):
			t.Fatal("composed CLI did not exit after its context timeout")
			return fmt.Errorf("composed CLI did not exit")
		}
	}
}

func (p *orchestratorCLIProcess) output(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(p.outputPath); err != nil {
		t.Fatalf("stat CLI output: %v", err)
	}
	contents, err := os.ReadFile(p.outputPath)
	if err != nil {
		t.Fatalf("read CLI output: %v", err)
	}
	return string(contents)
}

func waitForMarkerUntil(directory, name string, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(directory, name)); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func waitForMarker(t *testing.T, directory, name, label string) {
	t.Helper()
	if !waitForMarkerUntil(directory, name, time.Now().Add(8*time.Second)) {
		t.Fatalf("native/legacy provider fixture did not reach %s marker for %s", label, name)
	}
}

func waitForProviderExitUntil(pidDir, issue string, deadline time.Time) bool {
	data, err := os.ReadFile(filepath.Join(pidDir, issue))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		return false
	}
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func waitForProviderExit(t *testing.T, pidDir, issue string) {
	t.Helper()
	if !waitForProviderExitUntil(pidDir, issue, time.Now().Add(8*time.Second)) {
		t.Fatalf("provider fixture process for %s was not reaped", issue)
	}
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

func TestOrchestratorSingleWaitsForNativeProviderTerminal(t *testing.T) {
	consumer, workDir := setupOrchestratorConsumer(t)
	providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, releaseFile := writeOrchestratorFixtures(t)
	server := newOrchestratorFixtureServer(t, fixtureIssue{id: "issue-1", identifier: "OSS-1", title: "Await provider"})
	process := startOrchestratorCLI(t, consumer, workDir, server.URL, providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, releaseFile, "--single", "OSS-1")
	waitForMarker(t, startedDir, "OSS-1", "start")
	select {
	case <-process.done:
		if _, err := os.Stat(filepath.Join(terminalDir, "OSS-1")); err == nil {
			t.Fatalf("CLI exited after terminal event but before release gate check; output:\n%s", process.output(t))
		}
		t.Fatalf("CLI exited before its started provider reached the held terminal result; mode=%s output:\n%s", readMarker(t, modeDir, "OSS-1"), process.output(t))
	case <-time.After(300 * time.Millisecond):
	}
	if got := readMarker(t, modeDir, "OSS-1"); got != "native" {
		t.Fatalf("successful candidate used provider invocation mode %q, want native", got)
	}
	if err := os.WriteFile(releaseFile, []byte("release"), 0o600); err != nil {
		t.Fatalf("release provider terminal event: %v", err)
	}
	waitForMarker(t, terminalDir, "OSS-1", "terminal")
	waitForMarker(t, finishedDir, "OSS-1", "finished")
	waitForProviderExit(t, pidDir, "OSS-1")
	select {
	case <-process.done:
		err := process.err
		if err != nil {
			t.Fatalf("composed CLI failed after successful provider terminal result: %v\n%s", err, process.output(t))
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("CLI did not finish after provider terminal release:\n%s", process.output(t))
	}
	if !strings.Contains(process.output(t), "completed") {
		t.Fatalf("CLI summary omitted completed terminal status:\n%s", process.output(t))
	}
}

func TestOrchestratorSinglePropagatesNativeProviderFailure(t *testing.T) {
	consumer, workDir := setupOrchestratorConsumer(t)
	providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, _ := writeOrchestratorFixtures(t)
	server := newOrchestratorFixtureServer(t, fixtureIssue{id: "issue-1", identifier: "OSS-FAIL", title: "Fail provider"})
	process := startOrchestratorCLI(t, consumer, workDir, server.URL, providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, "", "--single", "OSS-FAIL")
	waitForMarker(t, startedDir, "OSS-FAIL", "start")
	waitForMarker(t, terminalDir, "OSS-FAIL", "terminal")
	waitForMarker(t, finishedDir, "OSS-FAIL", "finished")
	waitForProviderExit(t, pidDir, "OSS-FAIL")
	err := process.wait(t)
	output := process.output(t)
	if err == nil {
		t.Fatalf("composed CLI returned success after the provider wrote its terminal failure and exited 23 (mode=%s):\n%s", readMarker(t, modeDir, "OSS-FAIL"), output)
	}
	if got := readMarker(t, modeDir, "OSS-FAIL"); got != "native" {
		t.Fatalf("successful candidate used provider invocation mode %q, want native", got)
	}
	if !strings.Contains(output, "controlled provider terminal failure") || !strings.Contains(output, "error_max_turns") {
		t.Fatalf("failure did not come from the native provider terminal result (err=%v):\n%s", err, output)
	}
}

func TestOrchestratorBacklogAwaitsEachProviderAndReportsFailure(t *testing.T) {
	consumer, workDir := setupOrchestratorConsumer(t)
	providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, _ := writeOrchestratorFixtures(t)
	server := newOrchestratorFixtureServer(t,
		fixtureIssue{id: "issue-1", identifier: "OSS-1", title: "Complete provider"},
		fixtureIssue{id: "issue-2", identifier: "OSS-FAIL", title: "Fail provider"},
	)
	process := startOrchestratorCLI(t, consumer, workDir, server.URL, providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, "", "--project", "Example", "--max", "2")
	for _, issue := range []string{"OSS-1", "OSS-FAIL"} {
		waitForMarker(t, startedDir, issue, "start")
		waitForMarker(t, terminalDir, issue, "terminal")
		waitForMarker(t, finishedDir, issue, "finished")
		waitForProviderExit(t, pidDir, issue)
	}
	err := process.wait(t)
	output := process.output(t)
	if err == nil {
		t.Fatalf("backlog CLI returned success after both providers wrote terminal results and the failing provider exited 23 (failure mode=%s):\n%s", readMarker(t, modeDir, "OSS-FAIL"), output)
	}
	for _, issue := range []string{"OSS-1", "OSS-FAIL"} {
		if got := readMarker(t, modeDir, issue); got != "native" {
			t.Fatalf("successful candidate used provider invocation mode %q for %s, want native", got, issue)
		}
	}
	if !strings.Contains(output, "completed") || !strings.Contains(output, "controlled provider terminal failure") || !strings.Contains(output, "error_max_turns") {
		t.Fatalf("backlog summary did not preserve the successful and failed terminal outcomes (err=%v):\n%s", err, output)
	}
}

func TestOrchestratorBacklogRevalidatesRepositoryBeforeProviderSpawn(t *testing.T) {
	consumer, workDir := setupOrchestratorConsumer(t)
	remote := "https://example.test/expected/repo.git"
	addRemote := exec.Command("git", "remote", "add", "origin", remote) //nolint:gosec // fixed executable and fixture URL.
	addRemote.Dir = workDir
	if output, err := addRemote.CombinedOutput(); err != nil {
		t.Fatalf("set isolated fixture origin: %v\n%s", err, output)
	}
	initialOrigin := exec.Command("git", "remote", "get-url", "origin") //nolint:gosec // fixed executable and arguments.
	initialOrigin.Dir = workDir
	initialOutput, initialErr := initialOrigin.Output()
	if initialErr != nil || strings.TrimSpace(string(initialOutput)) != remote {
		t.Fatalf("initial fixture origin = %q, want %q (error %v)", strings.TrimSpace(string(initialOutput)), remote, initialErr)
	}
	providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, _ := writeOrchestratorFixtures(t)
	server := newOrchestratorFixtureServer(t,
		fixtureIssue{id: "issue-1", identifier: "OSS-MUTATE", title: "Change private origin"},
		fixtureIssue{id: "issue-2", identifier: "OSS-SECOND", title: "Must not start"},
	)
	process := startOrchestratorCLI(t, consumer, workDir, server.URL, providerDir, startedDir, terminalDir, finishedDir, modeDir, pidDir, "", "--project", "Example", "--max", "1", "--repo", remote)
	waitForMarker(t, startedDir, "OSS-MUTATE", "first provider start")
	waitForMarker(t, terminalDir, "OSS-MUTATE", "first provider terminal")
	waitForMarker(t, finishedDir, "OSS-MUTATE", "first provider finish")
	waitForProviderExit(t, pidDir, "OSS-MUTATE")
	err := process.wait(t)
	output := process.output(t)
	origin := exec.Command("git", "remote", "get-url", "origin") //nolint:gosec // fixed executable and arguments.
	origin.Dir = workDir
	originOutput, originErr := origin.Output()
	if originErr != nil || strings.TrimSpace(string(originOutput)) != "https://example.test/changed/repo.git" {
		t.Fatalf("first fixture provider did not change only its private origin: got=%q err=%v", strings.TrimSpace(string(originOutput)), originErr)
	}
	if _, statErr := os.Stat(filepath.Join(startedDir, "OSS-SECOND")); statErr == nil {
		waitForMarker(t, terminalDir, "OSS-SECOND", "second provider terminal")
		waitForMarker(t, finishedDir, "OSS-SECOND", "second provider finish")
		waitForProviderExit(t, pidDir, "OSS-SECOND")
	}
	if err == nil {
		t.Fatalf("CLI succeeded after the first provider changed the private git origin (mode=%s):\n%s", readMarker(t, modeDir, "OSS-MUTATE"), output)
	}
	if got := readMarker(t, modeDir, "OSS-MUTATE"); got != "native" {
		t.Fatalf("successful candidate used provider invocation mode %q, want native", got)
	}
	if _, statErr := os.Stat(filepath.Join(startedDir, "OSS-SECOND")); statErr == nil {
		t.Fatalf("second provider started after the repository origin changed:\n%s", output)
	}
	if !strings.Contains(output, "repository mismatch") || !strings.Contains(output, "changed/repo") {
		t.Fatalf("CLI did not report the per-dispatch repository mismatch (err=%v):\n%s", err, output)
	}
}

func readMarker(t *testing.T, directory, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatalf("read fixture marker %s: %v", name, err)
	}
	return string(contents)
}
