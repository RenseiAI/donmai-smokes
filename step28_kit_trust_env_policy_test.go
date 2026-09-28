package smokes

// This smoke drives the shipped kit source flags into an owned foreground
// daemon. It verifies that an environment-only permissive request cannot
// downgrade the default trust policy for an unsigned local Git kit.

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

const kitTrustEnvDaemonYAML = `apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: smoke-kit-trust-env
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
%skit:
  scanPaths:
    - %q
`

func TestKitTrustEnvPolicyThroughCLI(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end live-daemon test")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of the live-daemon smoke")
	afh.SkipIfToolMissing(t, "git", "local unsigned kit fixture is a Git repository")

	bin, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir:  inFlightSourceDir(),
		OutputPath: filepath.Join(t.TempDir(), "donmai"),
	})
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, kitContainmentID+".kit.toml"), []byte(kitContainmentManifest), 0o600); err != nil {
		t.Fatalf("write unsigned kit manifest: %v", err)
	}
	gitInitFixture(t, repoDir)
	sourceURL := (&url.URL{Scheme: "file", Path: repoDir}).String()

	cases := []struct {
		name           string
		mode           string
		envMode        string
		override       bool
		wantAllow      bool
		wantWarning    bool
		wantDeniedMode string
	}{
		{name: "omitted_yaml_mode_refuses_env_permissive", envMode: "permissive", wantWarning: true},
		{name: "omitted_yaml_mode_accepts_stricter_env", envMode: "attested", wantDeniedMode: "attested"},
		{name: "explicit_allowlist_overrides_env_permissive", mode: "signed-by-allowlist", envMode: "permissive"},
		{name: "explicit_permissive_overrides_strict_env", mode: "permissive", envMode: "signed-by-allowlist", wantAllow: true},
		{name: "explicit_permissive_allows", mode: "permissive", envMode: "permissive", wantAllow: true},
		{name: "one_install_override_survives_env_refusal", envMode: "permissive", override: true, wantAllow: true, wantWarning: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scanDir := filepath.Join(t.TempDir(), "kits")
			if err := os.MkdirAll(scanDir, 0o700); err != nil {
				t.Fatalf("make kit scan path: %v", err)
			}
			trustBlock := ""
			if tc.mode != "" {
				trustBlock = "trust:\n  mode: " + tc.mode + "\n"
			}
			home := t.TempDir()
			configDir := filepath.Join(home, ".donmai")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatalf("make private config directory: %v", err)
			}
			config := fmt.Sprintf(kitTrustEnvDaemonYAML, trustBlock, scanDir)
			configPath := filepath.Join(configDir, "daemon.yaml")
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatalf("write private daemon config: %v", err)
			}
			t.Chdir(home)
			port, err := afh.PickFreePort()
			if err != nil {
				t.Fatalf("pick daemon port: %v", err)
			}
			daemonURL := fmt.Sprintf("http://127.0.0.1:%d", port)
			logs := afh.NewLogTail(64 * 1024)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			live, err := afh.SpawnDaemon(ctx, afh.SpawnOptions{
				Binary: bin,
				Args:   []string{"host", "run", "--port", fmt.Sprint(port), "--config", configPath, "--skip-wizard", "--standalone-creds=off"},
				Env: []string{
					"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
					"HOME=" + home,
					"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
					"DONMAI_DAEMON_URL=" + daemonURL,
					"DONMAI_DAEMON_FORCE_STUB=1",
					"DONMAI_STATE_HOME=" + home,
					"DONMAI_KIT_TRUST_MODE=" + tc.envMode,
					"NO_COLOR=1",
				},
				HomeDir: home, LogSink: logs, HealthzBaseURL: daemonURL,
				HealthzTimeout: 30 * time.Second,
			})
			if err != nil {
				t.Fatalf("start private daemon: %v\n--- daemon log tail ---\n%s", err, logs.String())
			}
			t.Cleanup(live.Stop)

			args := []string{"kit", "install", kitContainmentID, "--source-kind", "git", "--source-url", sourceURL, "--plain"}
			if tc.override {
				args = append(args, "--allow-unsigned")
			}
			runKit := func(args ...string) (string, error) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				return afh.RunHermeticAgainstDaemon(ctx, afh.HermeticRunOptions{
					Binary: bin, Args: args, HomeDir: t.TempDir(),
					DaemonURLEnvVar: "DONMAI_DAEMON_URL", DaemonURL: live.URL,
				})
			}
			output, installErr := runKit(args...)
			persisted := filepath.Join(scanDir, kitContainmentID+".kit.toml")
			if !tc.wantAllow {
				if installErr == nil || !strings.Contains(output, "trust gate") {
					t.Fatalf("unsigned kit was not refused by CLI: err=%v output=%q\n--- daemon log tail ---\n%s", installErr, output, logs.String())
				}
				if tc.wantDeniedMode != "" && !strings.Contains(output, tc.wantDeniedMode) {
					t.Fatalf("CLI denial did not name selected strict mode %q: %s", tc.wantDeniedMode, output)
				}
				if _, err := os.Stat(persisted); !os.IsNotExist(err) {
					t.Fatalf("refused kit persisted: %v", err)
				}
				if tc.wantWarning != strings.Contains(logs.String(), "DONMAI_KIT_TRUST_MODE=permissive cannot lower the default") {
					t.Fatalf("warning presence differs from expectation\n--- daemon log tail ---\n%s", logs.String())
				}
				return
			}
			if installErr != nil {
				t.Fatalf("unsigned kit install failed: %v\n--- CLI output ---\n%s\n--- daemon log tail ---\n%s", installErr, output, logs.String())
			}
			if tc.wantWarning != strings.Contains(logs.String(), "DONMAI_KIT_TRUST_MODE=permissive cannot lower the default") {
				t.Fatalf("warning presence differs from expectation\n--- daemon log tail ---\n%s", logs.String())
			}
			if tc.override && !strings.Contains(output, "--allow-unsigned bypasses signature verification") {
				t.Fatalf("one-install override lacked CLI warning: %s", output)
			}
			installed, err := os.ReadFile(persisted)
			if err != nil || !bytes.Equal(installed, []byte(kitContainmentManifest)) {
				t.Fatalf("installed manifest differs from unsigned source: %v", err)
			}
			verified, err := runKit("kit", "verify", kitContainmentID, "--json")
			if err != nil {
				t.Fatalf("verify installed kit: %v\n%s", err, verified)
			}
			var trust struct {
				KitID string `json:"kitId"`
				Trust string `json:"trust"`
			}
			if err := json.Unmarshal([]byte(verified), &trust); err != nil || trust.KitID != kitContainmentID || trust.Trust != "unsigned" {
				t.Fatalf("installed kit trust = %+v, decode=%v, output=%s", trust, err, verified)
			}
		})
	}
}
