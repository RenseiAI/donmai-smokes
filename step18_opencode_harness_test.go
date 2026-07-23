package smokes

// step18_opencode_harness_test.go — Wave 0 opencode harness smoke lane
// (runs/2026-07-21-open-harness-strategy/07-design-opencode-spawn.md §8,
// Lane-A subset per 12-work-breakdown.md W0 item 4: 07 §8 items 1-2, 5-6
// — spawn/prompt/event-stream, teardown, and the version-pin guard,
// against the CI-installed opencode binary pinned to
// harness.OpenCodePinnedVersion).
//
// Driven the same way step17 drives a stub session: a real `donmai
// agent run` subprocess against an httptest daemon-control fixture,
// resolvedProfile selecting the "opencode" harness. Two PATH shapes:
//
//   - TestOpenCodeHarnessSmoke_VersionPinGuard puts a FAKE `opencode`
//     script on PATH that answers "--version" with a version below
//     donmai's MinVersion. Construction-time enforcement
//     (provider/harness/opencode/probe.go, added alongside donmai PR
//     containing the binaryPins matrix section) must refuse to register
//     the provider; the registry-build WARN log line on stderr is the
//     observable signal (agentRunProviderCtors probes EVERY provider at
//     startup regardless of which one resolvedProfile names — see
//     afcli/agent_run.go buildRegistryFromCtors — so a below-pin binary
//     never reaches Spawn at all).
//
//   - TestOpenCodeHarnessSmoke_RealBinary_SpawnEventStreamTerminal and
//     TestOpenCodeHarnessSmoke_RealBinary_Teardown put the REAL
//     CI-installed pinned binary (harness.EnsureOpenCodeBinary) on PATH
//     and drive a genuine `opencode run --format json` child process.
//     The model name is deliberately unresolvable (no such
//     provider/model configured) so opencode fails fast, synchronously,
//     with NO network dependency — proving the real spawn → NDJSON
//     parse → terminal-event pipeline against the pinned binary without
//     the flakiness a live model round-trip over a stub endpoint would
//     add to CI. The fixture also sets a short stageBudget: empirically,
//     with no budget the runner's own steering/retry loop keeps
//     respawning opencode on every failed attempt rather than settling
//     on one terminal Result (a fixture-completeness gap, not opencode
//     adapter behavior) — the budget caps that at a few seconds so the
//     smoke stays fast and deterministic. A full assistant-text happy
//     path requires per-session opencode.json config injection
//     (07 §5 / config.go), which is W2a (Lane B) scope, not W0 — see the
//     completion note in
//     runs/2026-07-21-open-harness-strategy/12-work-breakdown.md.
//
// # OSS boundary
//
// No platform dependencies: the httptest fixture serves only the OSS
// daemon's /api/daemon/sessions/<id> read plus a generic 200 JSON
// catch-all, exactly step17's pattern. opencode-ai installs from the
// public npm registry; no SaaS control plane involved.

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

// oldOpenCodeVersionScript is a fake `opencode` that only answers
// --version (with a version below any real MinVersion) — construction
// must fail before anything else is ever invoked on it.
const oldOpenCodeVersionScript = `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "0.1.0"
  exit 0
fi
echo '{"type":"error","error":{"message":"fake opencode: should not be invoked past version-pin construction"}}'
exit 1
`

// writeFakeOldOpenCode installs the below-pin fake `opencode` at
// dir/opencode (executable).
func writeFakeOldOpenCode(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(oldOpenCodeVersionScript), 0o755); err != nil { //nolint:gosec // executable shim needs the exec bit.
		t.Fatalf("write fake opencode shim: %v", err)
	}
}

// writeBackstopGhShimStep18 mirrors step17's writeBackstopGhShim (kept
// local + separately named — donmai-smokes' steps intentionally don't
// share test-only helpers across files beyond the harness package, per
// existing convention: step17 doesn't export its own either).
func writeBackstopGhShimStep18(t *testing.T, dir string) {
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

// openCodeSourceDir resolves the donmai checkout this smoke builds from,
// mirroring step17's siblingSourceDir: DONMAI_ARCH_SOURCE_DIR wins (the
// harness-wide in-flight-source override), then the feature worktree
// while W0's binaryPins/probe-enforcement change is unmerged, falling
// back to the canonical ../donmai sibling once it lands on main.
func openCodeSourceDir() string {
	if v := strings.TrimSpace(os.Getenv("DONMAI_ARCH_SOURCE_DIR")); v != "" {
		return v
	}
	const featureWorktree = "../donmai.wt/oc-matrix-pins"
	if fi, err := os.Stat(featureWorktree); err == nil && fi.IsDir() {
		return featureWorktree
	}
	return "../donmai"
}

// opencodeHarnessFixture bundles the built donmai binary and a fresh
// daemon-control httptest fixture for one opencode-harness smoke.
// resolvedProfile is injected verbatim as the SessionDetail's
// resolvedProfile object, so callers control provider/model.
type opencodeHarnessFixture struct {
	donmaiBinary string
	daemonSrv    *httptest.Server
	sessionID    string
	wtParent     string
	home         string
	fakeBinDir   string
}

// setupOpenCodeHarnessFixture builds the donmai binary from
// openCodeSourceDir() and stands up the daemon-control fixture. Skips
// cleanly when the source checkout is unavailable or predates the
// probe.go version-pin enforcement this smoke lane pins against.
func setupOpenCodeHarnessFixture(t *testing.T, testName string, resolvedProfile map[string]any) *opencodeHarnessFixture {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	srcDir := openCodeSourceDir()
	if _, err := os.Stat(filepath.Join(srcDir, "provider", "harness", "opencode", "probe.go")); err != nil {
		t.Skipf("donmai checkout at %q predates provider/harness/opencode/probe.go — "+
			"point DONMAI_ARCH_SOURCE_DIR at the feature worktree to exercise this smoke", srcDir)
	}

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

	sessionRepo := afh.MakeBareFixtureRepo(t, "opencode-harness-repo-"+testName)
	sessionID := "smoke-opencode-harness-" + testName

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/daemon/sessions/"+sessionID {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId":       sessionID,
				"issueIdentifier": "SMOKE-18",
				"title":           "opencode harness smoke",
				"body":            "say hi",
				"workType":        "development",
				"repository":      sessionRepo,
				"ref":             "main",
				"branch":          "agent/" + sessionID,
				"workerId":        "smoke-worker",
				"authToken":       "smoke-token",
				"platformUrl":     srv.URL,
				"resolvedProfile": resolvedProfile,
				// Bounded stage budget: the bogus-model fixture makes
				// opencode fail every attempt, and with no budget the
				// runner's own steering/retry loop keeps respawning it
				// indefinitely rather than settling on a terminal
				// Result (observed empirically — a fixture gap, not an
				// opencode adapter behavior). A short duration cap
				// gives a real, bounded FailureBudgetExceeded Result
				// instead of an unbounded retry loop.
				"stageBudget": map[string]any{"maxDurationSeconds": 8},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "refreshed": true})
	}))
	t.Cleanup(srv.Close)

	wtParent := t.TempDir()
	home := t.TempDir()
	fakeBinDir := t.TempDir()
	writeBackstopGhShimStep18(t, fakeBinDir)

	return &opencodeHarnessFixture{
		donmaiBinary: donmaiBinary,
		daemonSrv:    srv,
		sessionID:    sessionID,
		wtParent:     wtParent,
		home:         home,
		fakeBinDir:   fakeBinDir,
	}
}

// run drives `donmai agent run` against the fixture with the given PATH
// (fakeBinDir is always first so the gh shim wins; extraPathDirs are
// inserted between fakeBinDir and the system PATH — earlier entries
// resolve first). Returns stdout, stderr, and the run error (nil on
// exit 0).
func (f *opencodeHarnessFixture) run(t *testing.T, ctx context.Context, extraPathDirs ...string) (stdout, stderr string, runErr error) {
	t.Helper()

	pathEntries := append([]string{f.fakeBinDir}, extraPathDirs...)
	pathEntries = append(pathEntries, "/usr/bin", "/bin", "/usr/sbin", "/sbin")

	cmd := exec.CommandContext(
		ctx, f.donmaiBinary, //nolint:gosec // binary + flags are test-controlled.
		"agent", "run",
		"--session-id", f.sessionID,
		"--daemon-url", f.daemonSrv.URL,
		"--worktree-dir", f.wtParent,
	)
	cmd.Env = []string{
		"PATH=" + strings.Join(pathEntries, string(os.PathListSeparator)),
		"HOME=" + f.home,
		"XDG_CONFIG_HOME=" + filepath.Join(f.home, ".config"),
		"DONMAI_STATE_HOME=" + f.home,
		"NO_COLOR=1",
	}
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr = cmd.Run()
	return out.String(), errBuf.String(), runErr
}

// ─── Item 6: version-pin guard ─────────────────────────────────────────────

// TestOpenCodeHarnessSmoke_VersionPinGuard proves that a below-MinVersion
// `opencode` on PATH is refused at provider-construction time rather than
// silently accepted. Fully offline and deterministic — no real opencode
// binary needed for this one (that is exactly the point: donmai must
// reject it before ever trying to spawn it).
func TestOpenCodeHarnessSmoke_VersionPinGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	f := setupOpenCodeHarnessFixture(t, "pinguard", map[string]any{
		"provider": "opencode",
		"model":    "irrelevant/irrelevant",
	})

	oldBinDir := t.TempDir()
	writeFakeOldOpenCode(t, oldBinDir)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, stderr, _ := f.run(t, ctx, oldBinDir)

	// The registry build probes EVERY provider at startup (buildRegistryFromCtors
	// in afcli/agent_run.go), independent of which one resolvedProfile
	// names, so the WARN line fires regardless of run outcome.
	if !strings.Contains(stderr, "opencode") || !strings.Contains(stderr, "below the minimum supported version") {
		t.Fatalf("stderr: want a provider-probe-failed WARN for opencode citing the version-pin violation, got:\n%s", stderr)
	}
}

// ─── Items 1-2, 5: real pinned binary ──────────────────────────────────────

// bogusModelResolvedProfile names a provider/model pair opencode cannot
// resolve to any configured provider, so it fails fast (synchronously,
// before any network I/O) with a well-formed NDJSON error envelope — see
// the package doc above for why this replaces a live model round-trip.
func bogusModelResolvedProfile() map[string]any {
	return map[string]any{
		"provider": "opencode",
		"model":    "donmai-smokes-nonexistent-provider/donmai-smokes-nonexistent-model",
	}
}

// TestOpenCodeHarnessSmoke_RealBinary_SpawnEventStreamTerminal drives the
// REAL CI-installed pinned opencode binary end-to-end through `donmai
// agent run`: real spawn, real `opencode run --format json` NDJSON
// stream, real donmai-side event mapping, and a well-formed terminal
// Result on stdout (07 §8 items 1-2, adapted for the deterministic,
// network-free error path — see the package doc).
func TestOpenCodeHarnessSmoke_RealBinary_SpawnEventStreamTerminal(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	opencodeBin := afh.EnsureOpenCodeBinary(t)
	opencodeBinDir := filepath.Dir(opencodeBin)
	// EnsureOpenCodeBinary may return an operator override (arbitrary
	// name); ensure PATH resolution finds it as literally "opencode" —
	// the provider always execs DefaultBinary ("opencode").
	binDirForPath := opencodeBinDir
	if filepath.Base(opencodeBin) != "opencode" {
		aliasDir := t.TempDir()
		linkOpenCodeAlias(t, opencodeBin, aliasDir)
		binDirForPath = aliasDir
	}

	f := setupOpenCodeHarnessFixture(t, "spawn", bogusModelResolvedProfile())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stdout, stderr, runErr := f.run(t, ctx, binDirForPath)

	// Must NOT hit the version-pin-guard path: the pinned real binary
	// registers cleanly.
	if strings.Contains(stderr, "below the minimum supported version") {
		t.Fatalf("unexpected version-pin rejection of the real pinned binary; stderr:\n%s", stderr)
	}

	// A bogus model is a session-level failure (Status="failed"), not a
	// pre-flight error (exit 2) — the provider registered and spawned
	// fine; the session itself just couldn't complete. runAgentRun exits
	// non-zero (1) for a non-"completed" Result, so runErr is expected.
	if runErr == nil {
		t.Fatalf("donmai agent run: want a non-nil error for a Result.Status != completed run\n--- stdout ---\n%s\n--- stderr ---\n%s", stdout, stderr)
	}

	var res struct {
		Status string `json:"status"`
	}
	if err := afh.JSONUnmarshal(stdout, &res); err != nil {
		t.Fatalf("decode agent-run Result JSON: %v\n--- stdout ---\n%s\n--- stderr ---\n%s", err, stdout, stderr)
	}
	if res.Status == "" {
		t.Errorf("Result.Status is empty — want a terminal status (e.g. \"failed\") proving the real opencode spawn produced a terminal event\n--- stdout ---\n%s\n--- stderr ---\n%s", stdout, stderr)
	}
	if res.Status == "completed" {
		t.Errorf("Result.Status = completed; want a failure status — the model %q does not exist, so a real completion here would mean the bogus-model fixture is broken", bogusModelResolvedProfile()["model"])
	}
}

// TestOpenCodeHarnessSmoke_RealBinary_Teardown drives the REAL pinned
// opencode binary, sends SIGTERM mid-run, and asserts the child process
// group is gone afterward (07 §8 item 5: "Stop mid-run; no orphan
// processes; port released" — Lane-A has no port, so the orphan-process
// half is what's assertable here).
func TestOpenCodeHarnessSmoke_RealBinary_Teardown(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	opencodeBin := afh.EnsureOpenCodeBinary(t)
	opencodeBinDir := filepath.Dir(opencodeBin)
	binDirForPath := opencodeBinDir
	if filepath.Base(opencodeBin) != "opencode" {
		aliasDir := t.TempDir()
		linkOpenCodeAlias(t, opencodeBin, aliasDir)
		binDirForPath = aliasDir
	}

	f := setupOpenCodeHarnessFixture(t, "teardown", bogusModelResolvedProfile())

	pathEntries := append([]string{f.fakeBinDir, binDirForPath}, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	cmd := exec.Command(
		f.donmaiBinary, //nolint:gosec // binary + flags are test-controlled.
		"agent", "run",
		"--session-id", f.sessionID,
		"--daemon-url", f.daemonSrv.URL,
		"--worktree-dir", f.wtParent,
	)
	cmd.Env = []string{
		"PATH=" + strings.Join(pathEntries, string(os.PathListSeparator)),
		"HOME=" + f.home,
		"XDG_CONFIG_HOME=" + filepath.Join(f.home, ".config"),
		"DONMAI_STATE_HOME=" + f.home,
		"NO_COLOR=1",
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start donmai agent run: %v", err)
	}

	// Give it a moment to reach the opencode spawn, then SIGTERM the
	// donmai process (which owns the opencode child) mid-flight.
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal donmai agent run: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("donmai agent run did not exit within 30s of SIGINT (mid-run teardown hung)")
	}

	// No orphan opencode process left behind. Best-effort `pgrep` check
	// (skipped on hosts without it rather than failing the smoke on an
	// environment gap).
	if _, err := exec.LookPath("pgrep"); err == nil {
		out, _ := exec.Command("pgrep", "-f", "opencode run").CombinedOutput() //nolint:gosec // fixed args.
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("orphan opencode process(es) left running after mid-run teardown:\n%s", out)
		}
	}
}

// linkOpenCodeAlias creates a symlink (or copy, if symlinking fails)
// named "opencode" inside dir pointing at src, so PATH resolution finds
// it under the exact literal name the provider execs.
func linkOpenCodeAlias(t *testing.T, src, dir string) {
	t.Helper()
	dst := filepath.Join(dir, "opencode")
	if err := os.Symlink(src, dst); err == nil {
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read opencode binary at %s to alias it: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil { //nolint:gosec // executable alias needs the exec bit.
		t.Fatalf("write opencode alias at %s: %v", dst, err)
	}
}
