package smokes

// step17_sibling_context_repos_test.go — end-to-end smoke for the runner's
// sibling context-repo provisioning (DONMAI_SIBLING_REPOS, donmai
// docs/sibling-context-repos.md + ADR-2026-07-07-sibling-context-repos).
//
// Feature under test: when the runner's process env carries
// DONMAI_SIBLING_REPOS (comma-separated `<git-url>[#ref]` entries), it
// shallow-clones each entry as a read-only sibling directory of the
// session worktree, after worktree provisioning. Failures are never
// fatal — a bad entry logs a warning and the session proceeds.
//
// Entry point: `donmai agent run --daemon-url <fixture>` against an
// httptest daemon-control fixture serving GET /api/daemon/sessions/<id>
// (the only endpoint with an asserted contract; it ships in OSS and is
// exactly the seam runAgentRun's doc comment blesses for tests). The
// daemon-DISPATCH route (step4's POST /api/daemon/sessions) cannot reach
// this feature: the daemon stores no SessionDetail for locally-injected
// work, so its spawned worker exits at preflight before worktree
// provisioning. Driving the worker-level subcommand directly is the
// lightest OSS surface that executes the real runner loop — provision
// worktree → provision siblings → spawn stub provider → complete.
//
// What this pins, end-to-end against a real binary:
//
//  1. A stub-provider session against a local bare fixture repo runs to
//     Status="completed" (exit 0) with DONMAI_SIBLING_REPOS set.
//  2. Both good sibling entries materialize NEXT TO the session
//     worktree (same parent dir), shallow-cloned, with seeded content.
//  3. A `<url>#<ref>` entry checks out the named ref.
//  4. A third, unresolvable entry is non-fatal: the run still completes
//     and no directory appears for it.
//
// # OSS boundary
//
// No platform dependencies. The httptest fixture serves the OSS daemon's
// own /api/daemon/* session-detail read plus a generic 200 JSON catch-all
// for the worker's loopback status/activity/completion posts (asserting
// nothing through them). Repositories are local bare fixtures over
// file:// — no network, no WorkOS, no Linear, no /api/cli/*, no rsk_*
// tokens. A forked OSS deployment runs this unchanged.

import (
	"context"
	"encoding/json"
	"errors"
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

// writeBackstopGhShim installs a POSIX `gh` shim at dir/gh that answers
// `gh pr create` with a fixture PR URL. The production agent-run path
// runs the backstop tail, and the stub provider's happy path ends in a
// `gh pr create` — which must succeed offline (AGENTS.md iron rule:
// fake `gh` shims, never the real tool). Any other invocation exits
// non-zero so an unexpected `gh` call surfaces loudly.
func writeBackstopGhShim(t *testing.T, dir string) {
	t.Helper()
	const script = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  echo "https://github.com/acme/session-repo/pull/1"
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil { //nolint:gosec // executable shim needs the exec bit.
		t.Fatalf("write fake gh shim: %v", err)
	}
}

// siblingSourceDir resolves the donmai checkout this smoke builds from.
// DONMAI_ARCH_SOURCE_DIR (the harness-wide in-flight-source override,
// see step16) wins; otherwise the sibling-context-repos feature worktree
// is preferred while the branch is unmerged (org worktree convention:
// ../donmai.wt/<branch>), falling back to the canonical ../donmai
// sibling checkout — valid post-merge, when the feature is on main.
func siblingSourceDir() string {
	if v := strings.TrimSpace(os.Getenv("DONMAI_ARCH_SOURCE_DIR")); v != "" {
		return v
	}
	const featureWorktree = "../donmai.wt/sibling-context-repos"
	if fi, err := os.Stat(featureWorktree); err == nil && fi.IsDir() {
		return featureWorktree
	}
	return "../donmai"
}

// TestSiblingContextReposSmoke builds the donmai binary, seeds local bare
// fixture repos, and drives one `donmai agent run` stub session with
// DONMAI_SIBLING_REPOS in the worker's process env, asserting the sibling
// directories on disk afterward.
//
// Skipped under -short and when DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 is set
// (step1's operator opt-out; no daemon is spawned here, but the smoke
// runs a live worker process end-to-end, which is what the knob gates).
func TestSiblingContextReposSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	srcDir := siblingSourceDir()

	// Capability probe: the sibling-provisioning seam lives in
	// runner/siblings.go. A checkout that predates the feature has no
	// surface to assert against — skip cleanly so the gate stays green
	// until the branch merges (mirror of step16's predates-the-port skip).
	if _, err := os.Stat(filepath.Join(srcDir, "runner", "siblings.go")); err != nil {
		t.Skipf("donmai checkout at %q predates the sibling-context-repos feature "+
			"(no runner/siblings.go) — point DONMAI_ARCH_SOURCE_DIR at the feature "+
			"worktree to exercise this smoke", srcDir)
	}

	// Build the donmai binary. GOWORK=off decouples the build from any
	// org-root go.work overlay (matching step16 — a worktree source dir
	// is typically not listed in the workspace).
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()
	donmaiBinary, err := afh.BuildBinary(buildCtx, afh.BuildOptions{
		SourceDir:  srcDir,
		EntryPoint: "./cmd/donmai",
		OutputPath: filepath.Join(t.TempDir(), "donmai"),
		Env:        append(os.Environ(), "GOWORK=off"),
	})
	if err != nil {
		if strings.Contains(err.Error(), "resolve ../") ||
			strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("donmai binary unavailable (source %q): %v", srcDir, err)
		}
		t.Fatalf("build donmai binary: %v", err)
	}

	// Local bare fixture repos: one the session clones as its worktree,
	// two good siblings (the second consumed via #<ref>), plus one
	// deliberately unresolvable entry.
	sessionRepo := afh.MakeBareFixtureRepo(t, "session-repo")
	alphaBare := afh.MakeBareFixtureRepo(t, "ctx-corpus-alpha")
	betaBare := afh.MakeBareFixtureRepo(t, "ctx-corpus-beta", "smoke-ref")
	const missingURL = "file:///nonexistent-donmai-smokes/ctx-corpus-missing.git"

	// Fake daemon-control fixture. GET /api/daemon/sessions/<id> serves
	// the SessionDetail the worker preflight fetches (repository = local
	// bare fixture, provider = stub, platformUrl = this same server);
	// everything else — the worker's best-effort status / activity /
	// lock-refresh / completion posts — gets a generic 200 JSON ack, the
	// same shape donmai's own runner integration mock returns.
	sessionID := "smoke-sibling-ctx-17"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/daemon/sessions/"+sessionID {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId":       sessionID,
				"issueIdentifier": "SMOKE-17",
				"title":           "sibling context repos smoke",
				"body":            "Assert DONMAI_SIBLING_REPOS provisioning.",
				"workType":        "development",
				"repository":      sessionRepo,
				"ref":             "main",
				"branch":          "agent/" + sessionID,
				"workerId":        "smoke-worker",
				"authToken":       "smoke-token",
				"platformUrl":     srv.URL,
				"resolvedProfile": map[string]any{"provider": "stub"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "refreshed": true})
	}))
	defer srv.Close()

	// Hermetic worker env (mirroring harness/live_daemon.go): isolated
	// HOME + state home so ~/.donmai is never touched, plus the feature
	// env under test. The second entry carries a leading space (the
	// parser must trim) and the #<ref> suffix; the third is the bad URL
	// that must not fail the run.
	wtParent := t.TempDir()
	home := t.TempDir()
	fakeBinDir := t.TempDir()
	writeBackstopGhShim(t, fakeBinDir)
	siblingSpec := "file://" + alphaBare +
		", file://" + betaBare + "#smoke-ref" +
		"," + missingURL

	runCtx, runCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer runCancel()
	cmd := exec.CommandContext(runCtx, donmaiBinary, //nolint:gosec // binary + flags are test-controlled.
		"agent", "run",
		"--session-id", sessionID,
		"--daemon-url", srv.URL,
		"--worktree-dir", wtParent,
	)
	cmd.Env = []string{
		"PATH=" + fakeBinDir + string(os.PathListSeparator) + "/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"DONMAI_STATE_HOME=" + home,
		"NO_COLOR=1",
		"DONMAI_SIBLING_REPOS=" + siblingSpec,
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("donmai agent run timed out\n--- stdout ---\n%s\n--- stderr ---\n%s",
			stdout.String(), stderr.String())
	}
	// The bad third sibling entry must be NON-fatal: the run completes
	// with exit 0 despite it.
	if runErr != nil {
		t.Fatalf("donmai agent run: %v (sibling failures must be non-fatal)\n"+
			"--- stdout ---\n%s\n--- stderr ---\n%s", runErr, stdout.String(), stderr.String())
	}

	// Terminal Result envelope on stdout (agent run --json default).
	var res struct {
		Status       string `json:"status"`
		WorktreePath string `json:"worktreePath"`
	}
	if err := afh.JSONUnmarshal(stdout.String(), &res); err != nil {
		t.Fatalf("decode agent-run Result JSON: %v\n--- stdout ---\n%s", err, stdout.String())
	}
	if res.Status != "completed" {
		t.Errorf("Result.Status = %q, want completed\n--- stderr ---\n%s", res.Status, stderr.String())
	}

	// The session worktree is <worktree-dir>/<sessionID>; siblings must
	// land NEXT TO it — same parent directory. The Result envelope pins
	// where the worktree lived; the dir itself is torn down after a
	// successful run (--preserve-worktree only preserves on failure),
	// while siblings are deliberately left in place for reuse across
	// sessions — so the location assertion goes through WorktreePath.
	wantWorktree := filepath.Join(wtParent, sessionID)
	if res.WorktreePath != wantWorktree {
		t.Errorf("Result.WorktreePath = %q, want %q (siblings must be provisioned next to it)",
			res.WorktreePath, wantWorktree)
	}

	// Sibling 1: plain <url> entry → shallow clone of the default branch.
	alphaDir := filepath.Join(wtParent, "ctx-corpus-alpha")
	assertSiblingClone(t, alphaDir, "ctx-corpus-alpha", stderr.String())
	if got := gitOut(t, alphaDir, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Errorf("sibling %s: --is-shallow-repository = %q, want true (shallow-clone contract)",
			alphaDir, got)
	}

	// Sibling 2: <url>#<ref> entry → the named ref is checked out.
	betaDir := filepath.Join(wtParent, "ctx-corpus-beta")
	assertSiblingClone(t, betaDir, "ctx-corpus-beta", stderr.String())
	if got := gitOut(t, betaDir, "rev-parse", "--abbrev-ref", "HEAD"); got != "smoke-ref" {
		t.Errorf("sibling %s: checked-out ref = %q, want smoke-ref (#<ref> contract)", betaDir, got)
	}

	// Bad entry: no directory may appear for it.
	if _, err := os.Stat(filepath.Join(wtParent, "ctx-corpus-missing")); !os.IsNotExist(err) {
		t.Errorf("unresolvable sibling entry left a directory at %s (stat err=%v)",
			filepath.Join(wtParent, "ctx-corpus-missing"), err)
	}
}

// assertSiblingClone asserts dir is a git clone carrying the fixture's
// seeded README content. daemonStderr is attached to failures for
// worker-side context.
func assertSiblingClone(t *testing.T, dir, fixtureName, workerStderr string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("sibling %s not provisioned next to the worktree (err=%v)\n--- worker stderr ---\n%s",
			dir, err, workerStderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("sibling %s has no .git (not a clone): %v", dir, err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("sibling %s: read seeded README.md: %v", dir, err)
	}
	if !strings.Contains(string(body), fixtureName) {
		t.Errorf("sibling %s: README.md missing seeded fixture name %q; got %q",
			dir, fixtureName, body)
	}
}

// gitOut runs a git subcommand in dir and returns its trimmed stdout,
// failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	full := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", full, err, out)
	}
	return strings.TrimSpace(string(out))
}
