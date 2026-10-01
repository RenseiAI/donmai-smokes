package smokes

// This smoke compiles a tiny consumer against the selected Donmai source and
// calls the exported Codex host-login check. It exercises that library API,
// not the donmai CLI or a daemon. The credential and CLI are synthetic.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const codexLoginConsumer = `package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/RenseiAI/donmai/provider/harness/codex"
)

func main() {
	if len(os.Args) != 3 {
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marked := make(chan time.Time, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Args[2]); err == nil {
				// TERM can reach the child just before the owner starts its
				// grace clock. Let that owner enter its inspection loop.
				time.Sleep(50 * time.Millisecond)
				at := time.Now()
				marked <- at
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	err := codex.CheckHostSessionLogin(ctx, os.Args[1])
	returned := time.Now()
	fmt.Println("api_called=1")
	fmt.Printf("check_rejected=%t\n", err != nil)
	select {
	case at := <-marked:
		fmt.Println("marker_seen=true")
		fmt.Printf("cancel_return_ms=%d\n", returned.Sub(at).Milliseconds())
	default:
		fmt.Println("marker_seen=false")
		fmt.Println("cancel_return_ms=-1")
	}
}
`

func buildCodexLoginConsumer(t *testing.T, sourceDir string) string {
	t.Helper()
	modDir := t.TempDir()
	goMod := fmt.Sprintf("module codex-login-smoke\n\ngo 1.26.6\n\nrequire github.com/RenseiAI/donmai v0.0.0\nreplace github.com/RenseiAI/donmai => %q\n", sourceDir)
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write consumer module: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.go"), []byte(codexLoginConsumer), 0o600); err != nil {
		t.Fatalf("write consumer source: %v", err)
	}
	output := filepath.Join(t.TempDir(), "codex-login-consumer")
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

func shellQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func syntheticCodexStatusCLI(t *testing.T, root string) (binary, marker, pidFile, pgidFile string) {
	t.Helper()
	binary = filepath.Join(root, "synthetic-codex")
	marker = filepath.Join(root, "term-marker")
	ready := filepath.Join(root, "child-ready")
	pidFile = filepath.Join(root, "child-pid")
	pgidFile = filepath.Join(root, "child-pgid")
	script := "#!/bin/sh\n" +
		"MARKER=" + shellQuoted(marker) + "\n" +
		"READY=" + shellQuoted(ready) + "\n" +
		"PID_FILE=" + shellQuoted(pidFile) + "\n" +
		"PGID_FILE=" + shellQuoted(pgidFile) + "\n" +
		"(\n" +
		"  trap 'printf term > \"$MARKER\"' TERM\n" +
		"  : > \"$READY\"\n" +
		"  i=0\n" +
		"  while [ \"$i\" -lt 15 ]; do i=$((i + 1)); sleep 1; done\n" +
		") >/dev/null 2>&1 &\n" +
		"child_pid=$!\n" +
		"printf '%s\\n' \"$child_pid\" > \"$PID_FILE\"\n" +
		"i=0\n" +
		"while [ ! -f \"$READY\" ] && [ \"$i\" -lt 100 ]; do i=$((i + 1)); sleep 0.01; done\n" +
		"[ -f \"$READY\" ] || exit 7\n" +
		"ps -o pgid= -p \"$child_pid\" > \"$PGID_FILE\" || exit 7\n" +
		"printf 'Logged in using ChatGPT\\n' >&2\n"
	if err := os.WriteFile(binary, []byte(script), 0o500); err != nil {
		t.Fatalf("write synthetic CLI: %v", err)
	}
	return binary, marker, pidFile, pgidFile
}

func readPositiveID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	id, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid synthetic process identity")
	}
	return id, nil
}

// waitCodexFixtureGroup reads only the group ID emitted by our private CLI.
// It never signals a PID or group. A missing/ambiguous observation refuses
// cleanup of the private fixture root, which may still hold a writer.
func waitCodexFixtureGroup(ctx context.Context, pidFile, pgidFile string) error {
	childPID, err := readPositiveID(pidFile)
	if err != nil {
		return fmt.Errorf("read synthetic child PID: %w", err)
	}
	group, err := readPositiveID(pgidFile)
	if err != nil {
		return fmt.Errorf("read synthetic child group: %w", err)
	}
	for {
		cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,stat=") //nolint:gosec // fixed read-only process inspection.
		raw, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("inspect synthetic owned group: %w", err)
		}
		running := false
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) != 3 {
				return fmt.Errorf("ambiguous process table row")
			}
			pid, pidErr := strconv.Atoi(fields[0])
			pgid, groupErr := strconv.Atoi(fields[1])
			if pidErr != nil || groupErr != nil {
				return fmt.Errorf("ambiguous process table identity")
			}
			if pid == childPID && pgid != group && !strings.HasPrefix(fields[2], "Z") {
				// The numeric PID was reused; do not claim we still observed
				// our child by PID alone. The recorded group is checked below.
				continue
			}
			if pgid == group && !strings.HasPrefix(fields[2], "Z") {
				running = true
			}
		}
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("synthetic owned group remains active: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestCodexLoginCancellationStopsOwnedWriter(t *testing.T) {
	afh.SkipIfShort(t, "compiled exported-API cancellation smoke")
	sourceDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	sourceBytes, err := os.ReadFile(filepath.Join(sourceDir, "provider", "harness", "codex", "signal_unix.go"))
	if err != nil {
		t.Fatalf("selected Donmai source lacks owned-process implementation: %v", err)
	}
	t.Logf("selected Donmai source=%s signal_unix_sha256=%x", sourceDir, sha256.Sum256(sourceBytes))
	consumer := buildCodexLoginConsumer(t, sourceDir)
	privateRoot, err := os.MkdirTemp("", "donmai-codex-login-smoke-")
	if err != nil {
		t.Fatalf("create private synthetic fixture root: %v", err)
	}
	quiescent := true
	t.Cleanup(func() {
		if !quiescent {
			t.Logf("retained private synthetic fixture until writer identity can be resolved: %s", privateRoot)
			return
		}
		if err := os.RemoveAll(privateRoot); err != nil {
			t.Errorf("remove quiescent private fixture: %v", err)
		}
	})
	authHome := filepath.Join(privateRoot, "auth-home")
	if err := os.Mkdir(authHome, 0o700); err != nil {
		t.Fatalf("create synthetic auth home: %v", err)
	}
	const auth = `{"auth_mode":"chatgpt","tokens":{"id_token":"e30.e30.signature","access_token":"synthetic-access","refresh_token":"synthetic-refresh"}}`
	if err := os.WriteFile(filepath.Join(authHome, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatalf("write synthetic auth: %v", err)
	}
	binary, marker, pidFile, pgidFile := syntheticCodexStatusCLI(t, privateRoot)
	runCtx, runCancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer runCancel()
	cmd := exec.CommandContext(runCtx, consumer, binary, marker) //nolint:gosec // compiled fixture consumer and private CLI.
	cmd.Env = []string{
		"HOME=" + privateRoot, "CODEX_HOME=" + authHome, "TMPDIR=" + privateRoot,
		"PATH=/usr/bin:/bin", "NO_COLOR=1",
	}
	quiescent = false // the CLI may start a writer; cleanup requires observation
	runOutput, runErr := cmd.CombinedOutput()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cleanupCancel()
	cleanupStarted := time.Now()
	cleanupErr := waitCodexFixtureGroup(cleanupCtx, pidFile, pgidFile)
	quiescent = cleanupErr == nil
	if cleanupErr != nil {
		t.Fatalf("synthetic writer quiescence unproved after consumer exit: %v; consumer error=%v", cleanupErr, runErr)
	}
	if runErr != nil {
		t.Fatalf("exported-API consumer failed: %v\n%s", runErr, runOutput)
	}
	output := string(runOutput)
	if !strings.Contains(output, "api_called=1\n") || !strings.Contains(output, "marker_seen=true\n") ||
		!strings.Contains(output, "check_rejected=true\n") {
		t.Fatalf("consumer did not observe cancellation during owned cleanup: %q", output)
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled and called exported Codex host-login API from "+sourceDir)
	var elapsed int64 = -1
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, "cancel_return_ms="); ok {
			elapsed, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("parse consumer cancellation duration: %v", err)
			}
		}
	}
	if elapsed < 0 || elapsed > 1000 {
		t.Fatalf("cancelled exported login check held caller for %dms after TERM marker", elapsed)
	}
	if time.Since(cleanupStarted) > time.Second {
		t.Fatal("consumer returned before its owned fixture writer was quiescent")
	}
}
