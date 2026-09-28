package smokes

import (
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
	{"Add another project? [y/N]", "n"},
	{"Accept any project routed to this machine? [Y/n]", "n"},
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
	config := filepath.Join(home, ".donmai", "daemon.yaml")
	cmd := exec.Command(binary, "host", "setup", "--config", config) //nolint:gosec // compiled SUT and fixed fixture args.
	cmd.Dir = cwd
	cmd.Env = []string{
		"HOME=" + home,
		"DONMAI_STATE_HOME=" + home,
		"DONMAI_DAEMON_SKIP_WIZARD=",
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"PATH=/usr/bin:/bin",
		"LANG=C",
		"TERM=dumb",
		"NO_COLOR=1",
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
	if err := terminal.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close wizard PTY: %v", err)
	}
	select {
	case <-readerDone:
		readerJoined = true
		t.Log("wizard PTY reader joined")
	case <-time.After(5 * time.Second):
		t.Fatal("PTY reader did not join after wizard exit")
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
		"apiVersion: donmai.dev/v1", "kind: LocalDaemon", "projectAdmissionVersion: 2",
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
	transcript := strings.ReplaceAll(output.snapshot(), "\r", "")
	for _, want := range []string{
		"Setup complete. Config written to " + config,
		"Status: donmai host status", "Logs:   donmai host logs", "Stop:   donmai host stop",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("wizard completion missing %q\ntranscript:\n%s", want, transcript)
		}
	}
}
