package smokes

import (
	"context"
	"encoding/json"
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

// The real CLI must return sanitized named-session bootstrap diagnostics even
// with logging disabled. The local Codex peer never contacts a model provider.
func TestCodexDiagnosticCLI(t *testing.T) {
	afh.SkipIfShort(t, "builds and drives the Donmai CLI")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "local session-control smoke")
	afh.SkipIfToolMissing(t, "git", "creates a local bare fixture repository")
	binary, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: inFlightSourceDir(), OutputPath: filepath.Join(t.TempDir(), "donmai")})
	for _, mode := range []string{"before-response", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			binDir := t.TempDir()
			_, err := afh.BuildBinary(context.Background(), afh.BuildOptions{SourceDir: ".", EntryPoint: "./testdata/codex-diagnostic-fixture", OutputPath: filepath.Join(binDir, "codex"), Env: append(os.Environ(), "GOWORK=off")})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, "mode"), []byte(mode), 0o600); err != nil {
				t.Fatal(err)
			}
			repo := afh.MakeBareFixtureRepo(t, "codex-diagnostic")
			const sessionID = "smoke-codex-diagnostic"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/daemon/sessions/"+sessionID {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"sessionId": sessionID, "sessionName": "Diagnostic smoke", "mode": "interactive",
					"title": "Local diagnostic smoke", "body": "Reply with a status.", "workType": "development",
					"workerId": "smoke-worker", "platformUrl": "http://" + r.Host, "repository": repo, "ref": "main", "branch": "agent/" + sessionID,
					"resolvedProfile": map[string]string{"provider": "codex", "model": "gpt-5.5"},
					"stageBudget":     map[string]int{"maxDurationSeconds": 20},
				})
			}))
			t.Cleanup(srv.Close)
			home := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "--quiet", "agent", "run", "--session-id", sessionID, "--daemon-url", srv.URL, "--worktree-dir", t.TempDir())
			cmd.Env = []string{"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "DONMAI_STATE_HOME=" + home, "NO_COLOR=1"}
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("CLI timed out: %s", output)
			}
			if runErr == nil {
				t.Fatalf("expected bootstrap failure: %s", output)
			}
			for _, want := range []string{"codex interactive name bootstrap did not confirm the selected config home", "app-server stderr:", "fatal: named Codex diagnostic fixture", "[REDACTED]"} {
				if !strings.Contains(string(output), want) {
					t.Errorf("CLI output missing %q: %s", want, output)
				}
			}
			if strings.Contains(string(output), "sk-smoke-diagnostic-not-a-real-key") {
				t.Error("CLI leaked synthetic bearer value")
			}
			trace, err := os.ReadFile(filepath.Join(binDir, "trace"))
			if err != nil {
				t.Fatal(err)
			}
			if string(trace) != "named-initialize\nterminated\n" {
				t.Errorf("unexpected peer lifecycle: %q", trace)
			}
		})
	}
}
