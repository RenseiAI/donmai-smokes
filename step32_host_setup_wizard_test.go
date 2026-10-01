package smokes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"gopkg.in/yaml.v3"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

type wizardPrompt struct {
	marker string
	answer string
}

var hostSetupPrompts = []wizardPrompt{
	{"Machine ID (auto-generated)", "smoke-local-setup"},
	{"Region (helps the scheduler with latency)", "offline-smoke"},
	{"Continue? [Y/n]", "y"},
	{"Reserve cores for system", "1"},
	{"Reserve memory MB for system", "1024"},
	{"Max concurrent sessions", "2"},
	{"Continue? [Y/n]", "y"},
	{"Choice [1]", "2"},
	{"Continue? [Y/n]", "y"},
	{"Native profile number", "1"},
	{"GitHub owner/repository", "example/project"},
	{"Issue label to watch", "work-ready"},
	{"Base branch for new PRs", "release/next"},
	{"Continue? [Y/n]", "y"},
	{"Channel (stable/beta/main)", "stable"},
	{"Schedule (nightly/on-release/manual)", "manual"},
	{"Drain timeout seconds", "120"},
}

type wizardPTYOutput struct {
	mu     sync.Mutex
	text   strings.Builder
	notify chan struct{}
}

func (o *wizardPTYOutput) append(p []byte) {
	o.mu.Lock()
	_, _ = o.text.Write(p)
	o.mu.Unlock()
	select {
	case o.notify <- struct{}{}:
	default:
	}
}

func (o *wizardPTYOutput) snapshot() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

func waitWizardMarker(o *wizardPTYOutput, marker string, after int, timeout time.Duration) (int, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		text := o.snapshot()
		if at := strings.Index(text[after:], marker); at >= 0 {
			return after + at + len(marker), true
		}
		select {
		case <-o.notify:
		case <-timer.C:
			return 0, false
		}
	}
}

// TestHostSetupWizardGuidance runs the compiled OSS CLI through a real PTY.
// Piped stdin would silently take the noninteractive default path and prove
// nothing about the interactive wizard or its completion guidance.
func TestHostSetupWizardGuidance(t *testing.T) {
	afh.SkipIfShort(t, "interactive compiled host setup wizard smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")

	binary, source := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir: inFlightSourceDir(),
		Timeout:   8 * time.Minute, // cold offline builds can exceed the harness's three-minute default
	})
	t.Logf("compiled donmai from %s", source)
	home := t.TempDir()
	cwd := filepath.Join(home, "cwd")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The real setup resolver probes the native CLI's version and first-party
	// login. Reuse the finite external fixture from the whole-runtime smoke;
	// only its read-only Claude probes are reachable in this setup test.
	vendorModule := filepath.Join(home, "vendor-module")
	if err := os.Mkdir(vendorModule, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendorModule, "go.mod"), []byte("module private-wizard-vendor\n\ngo 1.26.6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendorModule, "main.go"), []byte(profileVendorSource), 0o600); err != nil {
		t.Fatal(err)
	}
	buildCtx, buildCancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer buildCancel()
	vendor := filepath.Join(bin, "vendor-fixture")
	build := exec.CommandContext(buildCtx, "go", "build", "-o", vendor, ".") //nolint:gosec // private fixed fixture source and output.
	build.Dir = vendorModule
	build.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + home, "GOWORK=off", "GOTOOLCHAIN=local"}
	if cache := os.Getenv("GOCACHE"); cache != "" {
		build.Env = append(build.Env, "GOCACHE="+cache)
	}
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build private native login fixture: %v: %s", err, out)
	}
	if err := os.Symlink(vendor, filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	// Production setup keeps the GitHub origin fixed. Its HTTPS traffic is
	// confined to a private TLS server through a loopback CONNECT proxy.
	external := &profileGitHub{baseSHA: strings.Repeat("a", 40), requests: map[string]int{}}
	proxy, ca := profileTLSProxy(t, home, external)
	certDir := filepath.Join(home, "empty-cert-dir")
	if err := os.Mkdir(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, ".donmai", "daemon.yaml")
	cmd := exec.Command(binary, "host", "setup", "--config", config) //nolint:gosec // compiled SUT and fixed fixture args.
	cmd.Dir = cwd
	cmd.Env = []string{
		"HOME=" + home,
		"DONMAI_STATE_HOME=" + home,
		"DONMAI_DAEMON_SKIP_WIZARD=",
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"PATH=" + bin + ":/usr/bin:/bin",
		"LANG=C",
		"TERM=dumb",
		"NO_COLOR=1",
		"GITHUB_TOKEN=" + profileFixtureToken,
		"HTTPS_PROXY=" + proxy,
		"HTTP_PROXY=" + proxy,
		"NO_PROXY=127.0.0.1,localhost",
		"SSL_CERT_FILE=" + ca,
		"SSL_CERT_DIR=" + certDir,
	}
	terminal, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("start compiled wizard in real PTY: %v", err)
	}
	t.Logf("owned wizard PID=%d", cmd.Process.Pid)
	output := &wizardPTYOutput{notify: make(chan struct{}, 1)}
	readerDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := terminal.Read(buf)
			if n > 0 {
				output.append(buf[:n])
			}
			if err != nil {
				readerDone <- err
				return
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	joined := false
	readerJoined := false
	t.Cleanup(func() {
		if !joined {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("signal exact wizard PID %d: %v", cmd.Process.Pid, err)
			}
			select {
			case err := <-waitDone:
				t.Logf("joined exact wizard PID=%d: %v", cmd.Process.Pid, err)
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill() // exact owned PID only
				<-waitDone
				t.Error("wizard did not stop after SIGTERM; exact PID was killed and joined")
			}
		}
		_ = terminal.Close()
		if !readerJoined {
			select {
			case <-readerDone:
			case <-time.After(5 * time.Second):
				t.Error("wizard PTY reader did not join after close")
			}
		}
	})

	cursor := 0
	for i, prompt := range hostSetupPrompts {
		next, ok := waitWizardMarker(output, prompt.marker, cursor, 5*time.Second)
		if !ok {
			t.Fatalf("wizard prompt %d/%d %q missing\ntranscript:\n%s", i+1, len(hostSetupPrompts), prompt.marker, output.snapshot())
		}
		cursor = next
		if _, err := fmt.Fprintln(terminal, prompt.answer); err != nil {
			t.Fatalf("answer wizard prompt %d: %v", i+1, err)
		}
	}
	t.Logf("consumed %d/%d ordered wizard prompts", len(hostSetupPrompts), len(hostSetupPrompts))
	select {
	case err := <-waitDone:
		joined = true
		if err != nil {
			t.Fatalf("wizard exited nonzero after all prompts: %v\ntranscript:\n%s", err, output.snapshot())
		}
		t.Logf("joined exact wizard PID=%d with exit 0", cmd.Process.Pid)
	case <-time.After(15 * time.Second):
		t.Fatalf("wizard did not exit after all prompts\ntranscript:\n%s", output.snapshot())
	}
	// Let the reader drain the child's final output before closing the master.
	// Process exit alone does not mean those bytes have reached output yet.
	select {
	case <-readerDone:
		readerJoined = true
		t.Log("wizard PTY reader joined")
	case <-time.After(5 * time.Second):
		t.Fatal("PTY reader did not join after wizard exit")
	}
	if err := terminal.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close wizard PTY: %v", err)
	}

	info, err := os.Stat(config)
	if err != nil {
		t.Fatalf("wizard did not write config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("config mode = %#o, want 0600", got)
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apiVersion: donmai.dev/v2", "kind: LocalDaemon", "projectAdmissionVersion: 2",
		"id: smoke-local-setup", "region: offline-smoke", "maxConcurrentSessions: 2",
		"vCpu: 1", "memoryMb: 1024", "projectAdmissionMode: enumerated",
		"url: file://" + filepath.Join(home, ".donmai", "queue"),
		"channel: stable", "schedule: manual", "drainTimeoutSeconds: 120",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("wizard config missing %q\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "authToken:") {
		t.Errorf("local queue config unexpectedly contains registration token")
	}
	var local struct {
		LocalRuntime struct {
			Harness           string `yaml:"harness"`
			Model             string `yaml:"model"`
			ModelAuthor       string `yaml:"modelAuthor"`
			ExecutionSecurity struct {
				ToolApproval string `yaml:"toolApproval"`
				FileRead     string `yaml:"fileRead"`
				FileWrite    string `yaml:"fileWrite"`
				Network      string `yaml:"network"`
				Credentials  string `yaml:"credentials"`
				Isolation    string `yaml:"isolation"`
			} `yaml:"executionSecurity"`
			Repositories []struct {
				RepositoryID int64  `yaml:"repositoryId"`
				OwnerRepo    string `yaml:"ownerRepo"`
				Label        string `yaml:"label"`
				Ref          string `yaml:"ref"`
			} `yaml:"repositories"`
		} `yaml:"localRuntime"`
	}
	if err := yaml.Unmarshal(data, &local); err != nil {
		t.Fatalf("decode written local runtime config: %v", err)
	}
	runtime := local.LocalRuntime
	if runtime.Harness != "claude-code" || runtime.Model != "claude-sonnet-5" || runtime.ModelAuthor != "anthropic" || len(runtime.Repositories) != 1 {
		t.Errorf("wizard config lost authenticated native profile or singular source: %+v", runtime)
	} else if repository := runtime.Repositories[0]; repository.RepositoryID != 42 || repository.OwnerRepo != "example/project" || repository.Label != "work-ready" || repository.Ref != "release/next" {
		t.Errorf("wizard config lost independently verified GitHub repository/base: %+v", repository)
	}
	policy := runtime.ExecutionSecurity
	if policy.ToolApproval != "bypass" || policy.FileRead != "host" || policy.FileWrite != "host" || policy.Network != "open" || policy.Credentials != "ambient-host-login" || policy.Isolation != "host-user" {
		t.Errorf("wizard config lost explicit local execution security: %+v", policy)
	}
	external.mu.Lock()
	repositoryReads := external.requests["GET /repos/example/project"]
	branchReads := external.requests["GET /repos/example/project/branches/release%2Fnext"]
	remoteErrors := append([]string(nil), external.errors...)
	external.mu.Unlock()
	if repositoryReads < 2 || branchReads < 1 || len(remoteErrors) != 0 {
		t.Errorf("real setup omitted independent GitHub metadata/base verification: repository_reads=%d branch_reads=%d errors=%v", repositoryReads, branchReads, remoteErrors)
	}
	versionProbes, loginProbes := 0, 0
	for _, record := range profileRecords(t, home) {
		switch record["kind"] {
		case "version":
			versionProbes++
		case "login":
			loginProbes++
		}
	}
	if versionProbes < 1 || loginProbes < 1 {
		t.Errorf("real setup omitted native version/login probes: version=%d login=%d", versionProbes, loginProbes)
	}
	transcript := strings.ReplaceAll(output.snapshot(), "\r", "")
	if strings.Contains(string(data), profileFixtureToken) || strings.Contains(transcript, profileFixtureToken) {
		t.Error("synthetic GitHub source credential leaked to config or setup output")
	}
	for _, want := range []string{
		"Setup complete. Config written to " + config,
		"Status: donmai host status", "Logs:   donmai host logs", "Stop:   donmai host stop",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("wizard completion missing %q\ntranscript:\n%s", want, transcript)
		}
	}
}
