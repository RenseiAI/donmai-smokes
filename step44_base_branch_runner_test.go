package smokes

// This smoke compiles a temporary consumer against the selected Donmai source
// and calls exported Runner and Worktree Manager APIs. Git acts only on a
// private bare repository; the provider, gh, and required result transport are
// in-process or local fixtures. No model, GitHub account, or daemon is used.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const baseBranchConsumer = `package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

type report struct {
	BaseSHA string ` + "`json:\"base_sha\"`" + `
	MainBefore string ` + "`json:\"main_before\"`" + `
	MainAfter string ` + "`json:\"main_after\"`" + `
	StartHead string ` + "`json:\"start_head\"`" + `
	StartBranch string ` + "`json:\"start_branch\"`" + `
	PromptNamesBase bool ` + "`json:\"prompt_names_base\"`" + `
	ProviderCalls int ` + "`json:\"provider_calls\"`" + `
	ResultStatus string ` + "`json:\"result_status\"`" + `
	ResultURL string ` + "`json:\"result_url\"`" + `
	ResultCommit string ` + "`json:\"result_commit\"`" + `
	PRCreated bool ` + "`json:\"pr_created\"`" + `
	RunError string ` + "`json:\"run_error\"`" + `
	GHCalls string ` + "`json:\"gh_calls\"`" + `
	WorkTip string ` + "`json:\"work_tip\"`" + `
	TagRunRefused bool ` + "`json:\"tag_run_refused\"`" + `
	TagProviderCalls int ` + "`json:\"tag_provider_calls\"`" + `
	TagGHUnchanged bool ` + "`json:\"tag_gh_unchanged\"`" + `
	TagWorkBranchAbsent bool ` + "`json:\"tag_work_branch_absent\"`" + `
	StaleRefRefused bool ` + "`json:\"stale_ref_refused\"`" + `
	StaleWorkBranchAbsent bool ` + "`json:\"stale_work_branch_absent\"`" + `
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func mustGit(dir string, args ...string) string {
	out, err := git(dir, args...)
	if err != nil {
		fail("private git fixture %v: %v: %s", args, err, out)
	}
	return out
}

type ackTransport struct{}

func (ackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" || req.URL.Host != "127.0.0.1:1" {
		return nil, errors.New("fixture refused non-loopback transport")
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader("{\"ok\":true,\"refreshed\":true}")), Request: req,
	}, nil
}

type fakeHandle struct { events chan agent.Event }

func (h *fakeHandle) SessionID() string { return "synthetic-base-branch-session" }
func (h *fakeHandle) Events() <-chan agent.Event { return h.events }
func (*fakeHandle) Inject(context.Context, string) error { return agent.ErrUnsupported }
func (*fakeHandle) Stop(context.Context) error { return nil }

type observerProvider struct {
	agent.Provider
	calls int
	startHead, startBranch string
	promptNamesBase bool
}

func (p *observerProvider) Manifest() agent.HarnessManifest {
	return p.Provider.(agent.HarnessProvider).Manifest()
}

func (p *observerProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.calls++
	var err error
	p.startHead, err = git(spec.Cwd, "rev-parse", "HEAD")
	if err != nil { return nil, err }
	p.startBranch, err = git(spec.Cwd, "symbolic-ref", "--quiet", "HEAD")
	if err != nil { return nil, err }
	p.promptNamesBase = strings.Contains(spec.Prompt, "the base branch is \"release/next\"")
	if err := os.WriteFile(filepath.Join(spec.Cwd, "branch-proof.md"), []byte("synthetic work\n"), 0o600); err != nil {
		return nil, err
	}
	h := &fakeHandle{events: make(chan agent.Event, 3)}
	h.events <- agent.InitEvent{SessionID: "synthetic-base-branch-session"}
	h.events <- agent.AssistantTextEvent{Text: "WORK_RESULT:passed\n"}
	h.events <- agent.ResultEvent{Success: true, Message: "done"}
	close(h.events)
	return h, nil
}

func privateRepository(root string) (remote, parent, baseSHA, mainSHA string) {
	remote = filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	parent = filepath.Join(root, "parent")
	mustGit("", "init", "--bare", remote)
	mustGit("", "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "source.txt"), []byte("main\n"), 0o600); err != nil { fail("write main fixture: %v", err) }
	mustGit(seed, "add", "source.txt")
	mustGit(seed, "commit", "-m", "main source")
	mainSHA = mustGit(seed, "rev-parse", "HEAD")
	mustGit(seed, "remote", "add", "origin", remote)
	mustGit(seed, "push", "origin", "main")
	mustGit(seed, "checkout", "-b", "release/next")
	if err := os.WriteFile(filepath.Join(seed, "source.txt"), []byte("selected base\n"), 0o600); err != nil { fail("write base fixture: %v", err) }
	mustGit(seed, "commit", "-am", "selected base")
	baseSHA = mustGit(seed, "rev-parse", "HEAD")
	mustGit(seed, "push", "origin", "release/next")
	mustGit("", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	mustGit("", "clone", remote, parent) // retain a stale origin/release/next tracking ref
	return remote, parent, baseSHA, mainSHA
}

func queuedWork(remote, platformURL, sessionID, branch string) runner.QueuedWork {
	return runner.QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID: sessionID, IssueID: "synthetic-issue", IssueIdentifier: "SMOKE-BASE",
			WorkType: "development", ProjectName: "Fixture", OrganizationID: "fixture",
			Title: "Verify selected base", Body: "Synthetic authored work", Repository: remote,
			BaseRef: "release/next",
		},
		Branch: branch, WorkerID: "fixture-worker", PlatformURL: platformURL,
		ResolvedProfile: runner.ResolvedProfile{Provider: agent.ProviderStub},
	}
}

func run(root string) report {
	var out report
	remote, parent, base, main := privateRepository(root)
	out.BaseSHA, out.MainBefore = base, main
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil { fail("create synthetic gh directory: %v", err) }
	gh := "#!/bin/sh\ndir=\"${0%/*}\"\nprintf '%s\\n' \"$@\" >> \"$dir/calls\"\n" +
		"if [ \"$1\" = pr ] && [ \"$2\" = create ]; then printf 'https://github.com/example/project/pull/1\\n'; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o500); err != nil { fail("write synthetic gh: %v", err) }
	if err := os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil { fail("isolate command path: %v", err) }
	client := &http.Client{Transport: ackTransport{}, Timeout: time.Second}
	registry, err := runner.NewRegistryWithOptions(runner.RegistryOptions{RuntimeTransportMode: runner.RuntimeTransportMode("local/v2")})
	if err != nil { fail("local/v2 registry required: %v", err) }
	inner, err := stub.New()
	if err != nil { fail("construct synthetic provider: %v", err) }
	provider := &observerProvider{Provider: inner}
	if err := registry.Register(provider); err != nil { fail("register synthetic provider: %v", err) }
	manager, err := worktree.NewManager(worktree.Options{ParentDir: filepath.Join(root, "workareas")})
	if err != nil { fail("construct real worktree manager: %v", err) }
	poster, err := result.NewPoster(result.Options{
		PlatformURL: "http://127.0.0.1:1", WorkerID: "fixture-worker", HTTPClient: client,
		MaxAttempts: 1, BaseDelay: 0,
	})
	if err != nil { fail("construct inert result transport: %v", err) }
	run, err := runner.New(runner.Options{
		Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxSessionDuration: 15*time.Second,
		IdleTimeout: -1, HeartbeatInterval: time.Hour, SkipSteering: true,
		SkipPostSession: true,
	})
	if err != nil { fail("construct real runner: %v", err) }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, runErr := run.Run(ctx, queuedWork(remote, "http://127.0.0.1:1", "base-success", "work/session"))
	if runErr != nil { out.RunError = runErr.Error() }
	if result != nil {
		out.ResultStatus, out.ResultURL, out.ResultCommit = result.Status, result.PullRequestURL, result.CommitSHA
		out.PRCreated = result.BackstopReport != nil && result.BackstopReport.PRCreated
	}
	out.ProviderCalls, out.StartHead, out.StartBranch, out.PromptNamesBase = provider.calls, provider.startHead, provider.startBranch, provider.promptNamesBase
	if calls, err := os.ReadFile(filepath.Join(bin, "calls")); err == nil { out.GHCalls = string(calls) }
	out.MainAfter = mustGit("", "--git-dir", remote, "rev-parse", "refs/heads/main")
	out.WorkTip, _ = git("", "--git-dir", remote, "rev-parse", "refs/heads/work/session")

	// A same-name tag plus the parent clone's stale tracking ref cannot stand
	// in for the now-deleted remote branch. Test Runner and Manager separately.
	mustGit("", "--git-dir", remote, "tag", "release/next", base)
	mustGit("", "--git-dir", remote, "update-ref", "-d", "refs/heads/release/next")
	_, tagErr := run.Run(ctx, queuedWork(remote, "http://127.0.0.1:1", "tag-refused", "work/tag"))
	out.TagRunRefused = tagErr != nil
	out.TagProviderCalls = provider.calls
	if calls, err := os.ReadFile(filepath.Join(bin, "calls")); err == nil { out.TagGHUnchanged = string(calls) == out.GHCalls }
	_, tagBranchErr := git("", "--git-dir", remote, "show-ref", "--verify", "refs/heads/work/tag")
	out.TagWorkBranchAbsent = tagBranchErr != nil
	path, staleErr := manager.Provision(ctx, worktree.ProvisionSpec{
		SessionID: "stale-refused", RepoURL: remote, Strategy: worktree.StrategyWorktreeAdd,
		ParentRepoPath: parent, Branch: "work/stale", BaseRef: "release/next", RequireBranchBase: true,
	})
	out.StaleRefRefused = errors.Is(staleErr, worktree.ErrInvalidBaseRef)
	if path != "" { _ = manager.Teardown(context.Background(), "stale-refused") }
	_, staleBranchErr := git(parent, "show-ref", "--verify", "refs/heads/work/stale")
	out.StaleWorkBranchAbsent = staleBranchErr != nil
	return out
}

func main() {
	if len(os.Args) != 2 { os.Exit(2) }
	if err := json.NewEncoder(os.Stdout).Encode(run(os.Args[1])); err != nil { os.Exit(2) }
}
`

type baseBranchReport struct {
	BaseSHA               string `json:"base_sha"`
	MainBefore            string `json:"main_before"`
	MainAfter             string `json:"main_after"`
	StartHead             string `json:"start_head"`
	StartBranch           string `json:"start_branch"`
	PromptNamesBase       bool   `json:"prompt_names_base"`
	ProviderCalls         int    `json:"provider_calls"`
	ResultStatus          string `json:"result_status"`
	ResultURL             string `json:"result_url"`
	ResultCommit          string `json:"result_commit"`
	PRCreated             bool   `json:"pr_created"`
	RunError              string `json:"run_error"`
	GHCalls               string `json:"gh_calls"`
	WorkTip               string `json:"work_tip"`
	TagRunRefused         bool   `json:"tag_run_refused"`
	TagProviderCalls      int    `json:"tag_provider_calls"`
	TagGHUnchanged        bool   `json:"tag_gh_unchanged"`
	TagWorkBranchAbsent   bool   `json:"tag_work_branch_absent"`
	StaleRefRefused       bool   `json:"stale_ref_refused"`
	StaleWorkBranchAbsent bool   `json:"stale_work_branch_absent"`
}

func buildBaseBranchConsumer(t *testing.T, sourceDir string) string {
	t.Helper()
	modDir := t.TempDir()
	goMod := fmt.Sprintf("module base-branch-smoke\n\ngo 1.26.6\n\nrequire github.com/RenseiAI/donmai v0.0.0\nreplace github.com/RenseiAI/donmai => %q\n", sourceDir)
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write consumer module: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.go"), []byte(baseBranchConsumer), 0o600); err != nil {
		t.Fatalf("write consumer source: %v", err)
	}
	output := filepath.Join(t.TempDir(), "base-branch-consumer")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=mod", "-o", output, ".") //nolint:gosec // fixed fixture source and args.
	cmd.Dir = modDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build exported-API consumer against selected Donmai source: %v\n%s", err, out)
	}
	return output
}

func TestBaseBranchRunnerAndWorktreeAPIs(t *testing.T) {
	afh.SkipIfShort(t, "compiled exported-API base-branch smoke")
	afh.SkipIfToolMissing(t, "git", "private bare-repository fixture")
	sourceDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	sourceBytes, err := os.ReadFile(filepath.Join(sourceDir, "runner", "base_ref.go"))
	if err != nil {
		t.Fatalf("selected Donmai source lacks BaseRef implementation: %v", err)
	}
	t.Logf("selected Donmai source=%s base_ref_sha256=%x", sourceDir, sha256.Sum256(sourceBytes))
	consumer := buildBaseBranchConsumer(t, sourceDir)
	root := t.TempDir()
	runCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, consumer, root) //nolint:gosec // compiled consumer with private fixture root.
	cmd.Env = []string{
		"HOME=" + root, "PATH=/usr/bin:/bin", "TMPDIR=" + root,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("exported-API consumer: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	var got baseBranchReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode exported-API result: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled and called exported Runner and Worktree Manager APIs from "+sourceDir)
	if got.RunError != "" || got.ProviderCalls != 1 || got.ResultStatus != "completed" || !got.PRCreated || got.ResultURL != "https://github.com/example/project/pull/1" {
		t.Errorf("new-branch runner completion: %+v", got)
	}
	if got.BaseSHA == "" || got.BaseSHA == got.MainBefore || got.StartHead != got.BaseSHA || got.StartBranch != "refs/heads/work/session" || !got.PromptNamesBase {
		t.Errorf("runner did not start provider on the named non-default base and new work branch: %+v", got)
	}
	if got.MainAfter != got.MainBefore || got.WorkTip == "" || got.WorkTip == got.BaseSHA || got.WorkTip != got.ResultCommit {
		t.Errorf("runner changed main or failed to publish work branch: %+v", got)
	}
	if !strings.Contains(got.GHCalls, "--head\nwork/session\n") || !strings.Contains(got.GHCalls, "--base\nrelease/next\n") {
		t.Errorf("actual gh pr create omitted explicit head/base: %q", got.GHCalls)
	}
	if !got.TagRunRefused || got.TagProviderCalls != got.ProviderCalls || !got.TagGHUnchanged || !got.TagWorkBranchAbsent || !got.StaleRefRefused || !got.StaleWorkBranchAbsent {
		t.Errorf("same-name tag or stale tracking ref stood in for missing remote base: %+v", got)
	}
}
