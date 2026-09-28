package smokes

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

type hostLogOutput struct {
	mu     sync.Mutex
	text   strings.Builder
	notify chan struct{}
}

func (o *hostLogOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	n, err := o.text.Write(p)
	o.mu.Unlock()
	select {
	case o.notify <- struct{}{}:
	default:
	}
	return n, err
}

func (o *hostLogOutput) snapshot() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

func waitForHostLog(o *hostLogOutput, want string, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if strings.Contains(o.snapshot(), want) {
			return true
		}
		select {
		case <-o.notify:
		case <-timer.C:
			return strings.Contains(o.snapshot(), want)
		}
	}
}

// TestHostLogsFollowAppendAfterEOF drives the compiled CLI, not a log-reader
// helper. Its bounded quiet interval makes the post-EOF append observable in a
// black-box process; the Donmai unit test supplies the deterministic EOF gate.
func TestHostLogsFollowAppendAfterEOF(t *testing.T) {
	afh.SkipIfShort(t, "compiled host logs --follow smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")

	bin, source := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{})
	t.Logf("compiled donmai from %s", source)
	home := t.TempDir()
	logPath := filepath.Join(home, ".donmai", "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("{\"level\":\"info\",\"msg\":\"follow fixture initial\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout := &hostLogOutput{notify: make(chan struct{}, 1)}
	stderr := &hostLogOutput{notify: make(chan struct{}, 1)}
	cmd := exec.Command(bin, "host", "logs", "--follow") //nolint:gosec // compiled SUT and fixed args.
	cmd.Env = []string{
		"HOME=" + home,
		"DONMAI_STATE_HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"PATH=/usr/bin:/bin",
		"NO_COLOR=1",
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start compiled host logs --follow: %v", err)
	}
	t.Logf("owned host-logs PID=%d", cmd.Process.Pid)
	t.Cleanup(func() {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("signal exact host-logs PID %d: %v", cmd.Process.Pid, err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if cmd.ProcessState == nil {
				t.Error("host-logs process was not joined")
			}
			t.Logf("joined exact host-logs PID=%d: %v", cmd.Process.Pid, err)
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill() // exact owned PID only
			<-done
			t.Error("host-logs process did not exit after SIGTERM; exact PID was killed and joined")
		}
	})

	if !waitForHostLog(stdout, "follow fixture initial", 3*time.Second) {
		t.Fatalf("initial line absent\nstdout: %s\nstderr: %s", stdout.snapshot(), stderr.snapshot())
	}
	// Seeing the first line does not itself prove EOF. Allow multiple 250 ms
	// production poll intervals before append; the producer's unit test uses an
	// instrumented reader for an exact EOF barrier.
	time.Sleep(750 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("host-logs PID %d exited before append: %v", cmd.Process.Pid, err)
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"level\":\"info\",\"msg\":\"follow fixture appended\"}\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !waitForHostLog(stdout, "follow fixture appended", 3*time.Second) {
		t.Fatalf("appended line absent after bounded post-EOF observation\nstdout: %s\nstderr: %s",
			stdout.snapshot(), stderr.snapshot())
	}
}
