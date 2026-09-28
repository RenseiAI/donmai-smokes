package smokes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const controlBindReleaseVersion = "0.72.50"

var controlBindReleaseArchiveSHA256 = map[string]string{
	"darwin/amd64": "6f79f4b45d7eccc068e5202820b8f93dbc7c2dcc6b23c90efb4e4bba6f9c0bc5",
	"darwin/arm64": "36b4696ef491eae071ea9fde49fb07e18ac8eb70ad8352ec1a4c1b7fc7f49c51",
	"linux/amd64":  "b287f713a3a60bb0e5e2382781c51510123ab7d3fbf2e34ac72a8d03cc7bded7",
	"linux/arm64":  "e0020b7430cc9098654efb4939b65e7b8c28f185c0721aeb77490fd7a97915ee",
}

// TestDaemonControlBindRelease exercises the shipped artifact and the selected
// source checkout through the same foreground CLI seam. Both must reject
// unsafe control listeners before startup and serve health on default loopback.
func TestDaemonControlBindRelease(t *testing.T) {
	afh.SkipIfShort(t, "release and source daemon process smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of the live-daemon smoke")

	sourceBinary, sourceDir := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir:  inFlightSourceDir(),
		OutputPath: filepath.Join(t.TempDir(), "donmai-source"),
		Env:        append(os.Environ(), "GOWORK=off"),
	})
	releaseBinary := controlBindReleasedBinary(t)
	for _, tt := range []struct {
		name   string
		binary string
	}{
		{name: "released-v" + controlBindReleaseVersion, binary: releaseBinary},
		{name: "source", binary: sourceBinary},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, host := range []string{"0.0.0.0", "::", "192.0.2.1"} {
				t.Run("reject-"+host, func(t *testing.T) {
					assertControlBindRejected(t, tt.binary, host)
				})
			}
			t.Run("default-loopback", func(t *testing.T) {
				assertDefaultControlLoopback(t, tt.binary)
			})
		})
	}
	t.Logf("source checkout: %s", sourceDir)
}

func controlBindReleasedBinary(t *testing.T) string {
	t.Helper()
	platform := runtime.GOOS + "/" + runtime.GOARCH
	wantSHA, ok := controlBindReleaseArchiveSHA256[platform]
	if !ok {
		t.Fatalf("release v%s has no pinned archive for %s", controlBindReleaseVersion, platform)
	}
	archiveName := fmt.Sprintf("donmai_%s_%s_%s.tar.gz", controlBindReleaseVersion, runtime.GOOS, runtime.GOARCH)
	var archive []byte
	var err error
	// A pinned local archive lets offline runners exercise the same released
	// bytes. The digest check below applies to either acquisition path.
	if local := os.Getenv("DONMAI_SMOKES_RELEASE_ARCHIVE"); local != "" {
		archive, err = os.ReadFile(local) //nolint:gosec // explicit operator-selected test artifact, verified by digest below
	} else {
		url := "https://github.com/RenseiAI/donmai/releases/download/v" + controlBindReleaseVersion + "/" + archiveName
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("construct release request: %v", err)
		}
		var resp *http.Response
		resp, err = (&http.Client{Timeout: 90 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("download release archive: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("download release archive: HTTP %d", resp.StatusCode)
		}
		archive, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20+1))
	}
	if err != nil {
		t.Fatalf("read release archive: %v", err)
	}
	if len(archive) > 64<<20 {
		t.Fatal("release archive exceeds 64 MiB")
	}
	digest := sha256.Sum256(archive)
	if got := hex.EncodeToString(digest[:]); got != wantSHA {
		t.Fatalf("release archive sha256 = %s, want %s", got, wantSHA)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("open release gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	binary := filepath.Join(t.TempDir(), "donmai-release")
	var found bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read release tar: %v", err)
		}
		if hdr.Name != "donmai" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if found || hdr.Size <= 0 || hdr.Size > 64<<20 {
			t.Fatalf("unexpected release binary entry: %+v", hdr)
		}
		payload, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if err != nil || int64(len(payload)) != hdr.Size {
			t.Fatalf("read release binary: size=%d err=%v", len(payload), err)
		}
		if err := os.WriteFile(binary, payload, 0o700); err != nil {
			t.Fatalf("write release binary: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatal("release archive has no donmai binary")
	}
	versionCtx, versionCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer versionCancel()
	versionHome := t.TempDir()
	versionCmd := exec.CommandContext(versionCtx, binary, "--version") //nolint:gosec // verified release archive and fixed arguments
	versionCmd.Dir = versionHome
	versionCmd.Env = controlBindSmokeEnv(versionHome)
	version, err := versionCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(version), "donmai version "+controlBindReleaseVersion) {
		t.Fatalf("released binary version: err=%v timeout=%v output=%q", err, versionCtx.Err(), version)
	}
	t.Logf("verified release v%s archive %s sha256=%s", controlBindReleaseVersion, archiveName, wantSHA)
	return binary
}

func controlBindSmokeEnv(home string) []string {
	return []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"DONMAI_STATE_HOME=" + home,
		"DONMAI_DAEMON_FORCE_STUB=1",
		"NO_COLOR=1",
	}
}

func controlBindSmokeConfig(t *testing.T, home string) string {
	t.Helper()
	// The explicit manual schedule prevents this foreground fixture from
	// starting a release updater, regardless of the host clock.
	const body = `apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: smoke-control-bind
orchestrator:
  url: http://127.0.0.1:1
autoUpdate:
  channel: stable
  schedule: manual
`
	path := filepath.Join(home, "daemon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write private daemon config: %v", err)
	}
	return path
}

func assertControlBindRejected(t *testing.T, binary, host string) {
	t.Helper()
	port, err := afh.PickClosedPort(8)
	if err != nil {
		t.Fatalf("pick private closed port: %v", err)
	}
	home := t.TempDir()
	configPath := controlBindSmokeConfig(t, home)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "daemon", "run", "--host", host, "--port", strconv.Itoa(port), "--config", configPath, "--skip-wizard", "--standalone-creds=off") //nolint:gosec // verified binary and fixed fixture arguments
	cmd.Dir = home
	cmd.Env = controlBindSmokeEnv(home)
	started := time.Now()
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("daemon did not reject %q before startup; owned pid=%d executable=%s cwd=%s started=%s output=%q", host, cmd.Process.Pid, binary, home, started.Format(time.RFC3339Nano), output)
	}
	if runErr == nil || !strings.Contains(string(output), "refusing non-loopback control bind") {
		t.Fatalf("daemon did not reject %q with preflight error: err=%v output=%q", host, runErr, output)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("owned daemon pid=%d was not reaped", cmd.Process.Pid)
	}
	assertControlPortFree(t, port)
	t.Logf("rejected host=%s before listener; owned pid=%d executable=%s cwd=%s started=%s", host, cmd.Process.Pid, binary, home, started.Format(time.RFC3339Nano))
}

func assertDefaultControlLoopback(t *testing.T, binary string) {
	t.Helper()
	port, err := afh.PickClosedPort(8)
	if err != nil {
		t.Fatalf("pick private closed port: %v", err)
	}
	home := t.TempDir()
	configPath := controlBindSmokeConfig(t, home)
	t.Chdir(home)
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	started := time.Now()
	logs := afh.NewLogTail(32 * 1024)
	live, err := afh.SpawnDaemon(ctx, afh.SpawnOptions{
		Binary:         binary,
		Args:           []string{"daemon", "run", "--port", strconv.Itoa(port), "--config", configPath, "--skip-wizard", "--standalone-creds=off"},
		Env:            controlBindSmokeEnv(home),
		HomeDir:        home,
		LogSink:        logs,
		HealthzBaseURL: url,
		HealthzTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("start default loopback daemon: %v\n%s", err, logs.String())
	}
	t.Cleanup(live.Stop)
	t.Logf("loopback healthy; owned pid=%d executable=%s cwd=%s started=%s", live.Cmd.Process.Pid, binary, home, started.Format(time.RFC3339Nano))
	live.Stop()
	if live.Cmd.ProcessState == nil || !live.Cmd.ProcessState.Exited() {
		t.Fatalf("owned daemon pid=%d was not reaped", live.Cmd.Process.Pid)
	}
	assertControlPortFree(t, port, func() string {
		return controlPortFailureReceipt(port, live.Cmd, logs.String())
	})
}

func assertControlPortFree(t *testing.T, port int, failureReceipt ...func() string) {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if len(failureReceipt) != 0 {
			t.Fatalf("private control port %d remained bound: %v\n%s", port, err, failureReceipt[0]())
		}
		t.Fatalf("private control port %d remained bound: %v", port, err)
	}
	_ = l.Close()
}

// This runs only after the immediate rebind fails. A reaped parent PID does
// not identify who owns a socket now, so capture the current port-specific
// socket view separately from the owned process's terminal state.
func controlPortFailureReceipt(port int, owned *exec.Cmd, daemonLogs string) string {
	const maxLogBytes = 2048
	if len(daemonLogs) > maxLogBytes {
		daemonLogs = daemonLogs[len(daemonLogs)-maxLogBytes:]
	}
	state := "unavailable"
	pid := 0
	if owned != nil {
		if owned.Process != nil {
			pid = owned.Process.Pid
		}
		if owned.ProcessState != nil {
			state = fmt.Sprintf("exited=%t exitCode=%d status=%s", owned.ProcessState.Exited(), owned.ProcessState.ExitCode(), owned.ProcessState.String())
		}
	}
	return fmt.Sprintf("former owned pid=%d processState=%s\n%s\nprivate daemon log tail (last %d bytes):\n%s",
		pid, state, controlPortSocketReceipt(port), maxLogBytes, daemonLogs)
}

func controlPortSocketReceipt(port int) string {
	const maxOutputBytes = 4096
	var lsofPath string
	for _, candidate := range []string{"/usr/sbin/lsof", "/usr/bin/lsof"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			lsofPath = candidate
			break
		}
	}
	if lsofPath == "" {
		return "target-port lsof unavailable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, lsofPath, "-nP", "-iTCP:"+strconv.Itoa(port), "-FpcTn") //nolint:gosec // fixed system tool, selected private port only
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	output := &controlPortCappedOutput{limit: maxOutputBytes}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	return fmt.Sprintf("target-port lsof path=%s exit=%v timeout=%t truncated=%t output=%q",
		lsofPath, err, ctx.Err() == context.DeadlineExceeded, output.truncated, output.buf.String())
}

type controlPortCappedOutput struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (o *controlPortCappedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	remaining := o.limit - o.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = o.buf.Write(p)
	}
	if n > len(p) || (remaining <= 0 && n > 0) {
		o.truncated = true
	}
	return n, nil
}
