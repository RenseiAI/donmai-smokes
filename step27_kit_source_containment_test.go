package smokes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const kitContainmentID = "smoke-containment-kit"

const kitContainmentManifest = `api = "donmai.dev/v1"

[kit]
id = "smoke-containment-kit"
version = "1.0.0"
name = "Containment fixture"
`

const kitContainmentDaemonYAML = `apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: smoke-kit-containment
capacity:
  maxConcurrentSessions: 2
  maxVCpuPerSession: 2
  maxMemoryMbPerSession: 2048
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
projects:
  - id: fixture
    repository: github.com/example/kit-fixture
    cloneStrategy: shallow
orchestrator:
  url: http://127.0.0.1:1
autoUpdate:
  channel: stable
  schedule: manual
  drainTimeoutSeconds: 5
trust:
  mode: signed-by-allowlist
kit:
  scanPaths:
    - %q
`

// TestKitSourceContainmentThroughCLI follows the shipped CLI source flags into
// the real localhost daemon and Git fetcher. All paths and the Git source are
// test-owned; no user kit directory or remote registry is consulted.
func TestKitSourceContainmentThroughCLI(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end live-daemon test")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of the live-daemon smoke")
	afh.SkipIfToolMissing(t, "git", "local kit source fixtures are Git repositories")

	bin, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir:  inFlightSourceDir(),
		OutputPath: filepath.Join(t.TempDir(), "donmai"),
		Env:        append(os.Environ(), "GOWORK="),
	})
	scanDir := filepath.Join(t.TempDir(), "kits")
	if err := os.MkdirAll(scanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	configDir := filepath.Join(home, ".donmai")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "daemon.yaml"), []byte(fmt.Sprintf(kitContainmentDaemonYAML, scanDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	port, err := afh.PickFreePort()
	if err != nil {
		t.Fatal(err)
	}
	daemonURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	logs := afh.NewLogTail(64 * 1024)
	startCtx, cancelStart := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStart()
	live, err := afh.SpawnDaemon(startCtx, afh.SpawnOptions{
		Binary: bin,
		Args:   []string{"daemon", "run", "--port", fmt.Sprint(port), "--skip-wizard"},
		Env: []string{
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			"DONMAI_DAEMON_URL=" + daemonURL,
			"DONMAI_DAEMON_FORCE_STUB=1",
			"DONMAI_STATE_HOME=" + home,
			"NO_COLOR=1",
		},
		HomeDir: home, LogSink: logs, HealthzBaseURL: daemonURL,
		HealthzTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("start isolated daemon: %v\n--- daemon log tail ---\n%s", err, logs.String())
	}
	t.Cleanup(live.Stop)

	runKit := func(args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return afh.RunHermeticAgainstDaemon(ctx, afh.HermeticRunOptions{
			Binary: bin, Args: append([]string{"kit"}, args...), HomeDir: t.TempDir(),
			DaemonURLEnvVar: "DONMAI_DAEMON_URL", DaemonURL: live.URL,
		})
	}

	makeRepo := func(bundleTarget string) string {
		t.Helper()
		repoDir := t.TempDir()
		manifest := filepath.Join(repoDir, kitContainmentID+".kit.toml")
		if err := os.WriteFile(manifest, []byte(kitContainmentManifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if bundleTarget != "" {
			if err := os.Symlink(bundleTarget, manifest+".sigstore"); err != nil {
				t.Fatal(err)
			}
		}
		gitInitFixture(t, repoDir)
		return (&url.URL{Scheme: "file", Path: repoDir}).String()
	}

	// Missing sibling is an unsigned kit, accepted only by this one-shot
	// explicit override. Verify the installed copy, not just the CLI message.
	unsignedURL := makeRepo("")
	installed, err := runKit("install", kitContainmentID, "--source-kind", "git", "--source-url", unsignedURL, "--allow-unsigned", "--plain")
	if err != nil {
		t.Fatalf("install unsigned local fixture: %v\n--- output ---\n%s\n--- daemon log tail ---\n%s", err, installed, logs.String())
	}
	if !strings.Contains(installed, kitContainmentID) {
		t.Fatalf("install output lacks kit identity: %s", installed)
	}
	persisted := filepath.Join(scanDir, kitContainmentID+".kit.toml")
	before, err := os.ReadFile(persisted)
	if err != nil || !bytes.Equal(before, []byte(kitContainmentManifest)) {
		t.Fatalf("installed manifest differs from local fixture: %v", err)
	}
	if _, err := os.Lstat(persisted + ".sigstore"); !os.IsNotExist(err) {
		t.Fatalf("unsigned install persisted a signature sibling: %v", err)
	}
	verified, err := runKit("verify", kitContainmentID, "--json")
	if err != nil {
		t.Fatalf("verify installed unsigned kit: %v\n%s", err, verified)
	}
	var trust struct {
		KitID string `json:"kitId"`
		Trust string `json:"trust"`
	}
	if err := json.Unmarshal([]byte(verified), &trust); err != nil || trust.KitID != kitContainmentID || trust.Trust != "unsigned" {
		t.Fatalf("installed unsigned readback = %+v, decode=%v, output=%s", trust, err, verified)
	}

	// A Git-tracked sibling link points only to this test's synthetic file.
	// The fetcher must reject it before parse or trust override, leaving the
	// earlier installed kit and the outside sentinel byte-identical.
	outside := filepath.Join(t.TempDir(), "synthetic-outside-bundle")
	sentinel := []byte("synthetic outside sentinel")
	if err := os.WriteFile(outside, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	unsafeURL := makeRepo(outside)
	refused, err := runKit("install", kitContainmentID, "--source-kind", "git", "--source-url", unsafeURL, "--allow-unsigned", "--plain")
	if err == nil || !strings.Contains(refused, "unsafe legacy signature bundle") {
		t.Fatalf("linked sibling was not refused before trust override: err=%v\n--- output ---\n%s\n--- daemon log tail ---\n%s", err, refused, logs.String())
	}
	if after, err := os.ReadFile(persisted); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("refused install changed manifest: %v", err)
	}
	if _, err := os.Lstat(persisted + ".sigstore"); !os.IsNotExist(err) {
		t.Fatalf("refused install published a linked bundle: %v", err)
	}
	if after, err := os.ReadFile(outside); err != nil || !bytes.Equal(after, sentinel) {
		t.Fatalf("refused install touched synthetic outside file: %v", err)
	}
}
