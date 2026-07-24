package smokes

// step20_pi_harness_test.go — pi harness smoke lane
// (runs/2026-07-21-open-harness-strategy/09-design-pi-adapter.md §8;
// 12-work-breakdown.md W2b). The pi provider is registered in donmai's
// agent-run ctor list (donmai#214), so a `donmai agent run` session (and this
// black-box smoke, which only ever drives the compiled binary's CLI surface,
// never donmai as a Go library, per AGENTS.md) can reach pi.New()/Spawn().
//
// # The pi RPC/extension protocol now matches the real pinned binary
//
// donmai's provider/harness/pi adapter (and its embedded TypeScript policy
// extension, extensions/donmai-policy.ts) was originally built against the
// design doc's DESCRIPTION of pi's protocol, which did not match the real
// pinned binary (@earendil-works/pi-coding-agent@0.80.10). That gap was found
// and then FIXED in donmai#215 ("fix(pi): speak the real pi 0.80.10 RPC +
// extension protocol"), verified against the binary's bundled docs/rpc.md +
// docs/extensions.md and live `pi --mode rpc` probes. The corrected protocol:
//
//   - Commands are keyed "type" (not "command"); prompt/steer/follow_up carry
//     "message"; set_model uses provider+modelId; extension_ui_response carries
//     a top-level "value".
//   - The session id comes from the get_state response (agent_start has none);
//     the terminal is agent_settled (not agent_end).
//   - The trust boundary is built on pi's REAL extension API: a
//     pi.on("tool_call") blocking hook that round-trips each guarded tool to
//     the Go policy engine over ctx.ui.input, plus a session_start handshake
//     carrying a per-session env token + the extension's self-SHA (both
//     verified Go-side, fail-closed). Loaded via `-e <path> --no-extensions`.
//
// # Two things this smoke had to fix before it could exercise the real binary
//
//  1. `pi` is a `#!/usr/bin/env node` script, so it cannot exec unless `node`
//     is on the PATH `donmai agent run` hands the child. The original lane's
//     restricted PATH (fakeBinDir + pi dir + /usr/bin:/bin:...) had no node,
//     so the real pi process died at exec, the pi child's stdout closed before
//     any handshake, and Spawn failed with "policy extension failed to load:
//     event stream closed before handshake" — which the original lane MISREAD
//     as the documented "protocol gap". It never actually ran pi. This lane
//     now puts node's directory on the child PATH (nodeBinDir).
//  2. pi resolves its --model against its built-in catalog at STARTUP, before
//     session_start/the handshake. A bogus model (the original "smoke-test-
//     model") makes pi exit with "Model not found" before the handshake can
//     fire — again masquerading as a spawn failure. This lane uses a real
//     catalog model (piCatalogModel, override with DONMAI_SMOKES_PI_MODEL) so
//     the session actually starts and the handshake completes.
//
// # What this lane validates against the real 0.80.10 binary
//
//   - TestPiHarnessSmoke_VersionPinGuard: construction-time version-pin
//     enforcement (protocol-independent).
//   - TestPiHarnessSmoke_RealBinary_HandshakeCompletes: the load-bearing
//     flip. With node on PATH and a resolvable model, `donmai agent run`
//     spawns real pi, the fail-closed handshake COMPLETES (token+SHA verified),
//     and the session proceeds PAST Spawn into a real model turn — so the run
//     does NOT fail with "policy extension failed to load" / spawn-failed. It
//     instead runs until the stage budget (the model turn cannot complete
//     without a real provider credential — the established stageBudget /
//     unresolvable-turn precedent). A regression to the old protocol would
//     re-introduce the spawn-failure and fail this test loudly.
//   - TestPiHarnessSmoke_RealBinary_Teardown (09 §8 item 8): SIGINT mid-run,
//     no orphan `pi --mode rpc` process.
//
// # Deferred — genuinely blocked on a completed model turn (documented, not
// # silently skipped)
//
// Items 3 (full prompt→assistant-text round trip), 6 (steer/queue mid-turn),
// 7 (resume/cursor replay), and 9's pin ROUND-TRIP all need a model turn that
// actually completes. Through the black-box `donmai agent run` path that needs
// EITHER a real provider credential (out of bounds: cost + the OSS no-creds
// boundary) OR a stub OpenAI-compatible endpoint that pi is routed to — and
// routing pi to a stub needs donmai to thread the resolved Endpoint from
// resolvedProfile into the pi provider (runner/spec_translation.go does not set
// Spec.Endpoint today; the pi provider only pins `--provider donmai --model` /
// registers the "donmai" provider when an Endpoint with a BaseURL is present).
// That endpoint-from-resolvedProfile wiring — analogous to the opencode
// preferServer hint (donmai#209) — is the follow-up that unblocks a completed
// turn and, with it, items 3/6/7/9 for real. The trust-boundary logic those
// items exercise (handshake, Go-side adjudication, bypass monitor, env hygiene)
// is unit-green in donmai's provider/harness/pi against real wire shapes.
//
// # OSS boundary
//
// Same fixture shape as step18: an httptest daemon-control fixture serving only
// /api/daemon/sessions/<id>, no SaaS control plane. pi installs from the public
// npm registry via harness.EnsurePiBinary. No provider credentials are used.

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

// piCatalogModel is a model id pi's built-in catalog resolves at startup so the
// session can start and the handshake can fire. It needs NO provider credential
// to resolve (the turn itself would need one, which this lane deliberately does
// not supply). Override with DONMAI_SMOKES_PI_MODEL if catalog churn retires it
// (same class as the version pin — pi ships releases frequently).
const piCatalogModel = "gpt-5.5"

func piModel() string {
	if v := strings.TrimSpace(os.Getenv("DONMAI_SMOKES_PI_MODEL")); v != "" {
		return v
	}
	return piCatalogModel
}

// oldPiVersionScript is a fake `pi` that only answers --version (with a
// version below MinVersion) — construction must fail before anything else
// is ever invoked on it.
const oldPiVersionScript = `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "0.1.0"
  exit 0
fi
echo '{"type":"response","success":false,"error":"fake pi: should not be invoked past version-pin construction"}'
exit 1
`

func writeFakeOldPi(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(oldPiVersionScript), 0o755); err != nil { //nolint:gosec // executable shim needs the exec bit.
		t.Fatalf("write fake pi shim: %v", err)
	}
}

// writeBackstopGhShimStep20 mirrors step17/step18's own locally-named copy
// of the fake `gh` shim — steps intentionally don't share test-only
// helpers beyond the harness package, per existing convention.
func writeBackstopGhShimStep20(t *testing.T, dir string) {
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

// nodeBinDir returns the directory containing `node`, or "" if node is not on
// PATH. `pi` is a node script; without node on the PATH `donmai agent run`
// hands the child, the real pi binary cannot exec at all.
func nodeBinDir() string {
	if p, err := exec.LookPath("node"); err == nil {
		return filepath.Dir(p)
	}
	return ""
}

// piSourceDir resolves the donmai checkout this smoke builds from, mirroring
// step18's openCodeSourceDir: DONMAI_ARCH_SOURCE_DIR wins, falling back to
// the canonical ../donmai sibling.
func piSourceDir() string {
	if v := strings.TrimSpace(os.Getenv("DONMAI_ARCH_SOURCE_DIR")); v != "" {
		return v
	}
	return "../donmai"
}

type piHarnessFixture struct {
	donmaiBinary string
	daemonSrv    *httptest.Server
	sessionID    string
	wtParent     string
	home         string
	fakeBinDir   string
	nodeDir      string
}

// setupPiHarnessFixture builds the donmai binary from piSourceDir() and
// stands up the daemon-control fixture. Skips cleanly when the source
// checkout is unavailable or predates the pi provider registration this
// lane pins against.
func setupPiHarnessFixture(t *testing.T, testName string, resolvedProfile map[string]any) *piHarnessFixture {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	srcDir := piSourceDir()
	if _, err := os.Stat(filepath.Join(srcDir, "provider", "harness", "pi", "probe.go")); err != nil {
		t.Skipf("donmai checkout at %q predates provider/harness/pi/probe.go — "+
			"point DONMAI_ARCH_SOURCE_DIR at a checkout that has it", srcDir)
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

	sessionRepo := afh.MakeBareFixtureRepo(t, "pi-harness-repo-"+testName)
	sessionID := "smoke-pi-harness-" + testName

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/daemon/sessions/"+sessionID {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId":       sessionID,
				"issueIdentifier": "SMOKE-20",
				"title":           "pi harness smoke",
				"body":            "say hi",
				"workType":        "development",
				"repository":      sessionRepo,
				"ref":             "main",
				"branch":          "agent/" + sessionID,
				"workerId":        "smoke-worker",
				"authToken":       "smoke-token",
				"platformUrl":     srv.URL,
				"resolvedProfile": resolvedProfile,
				// Bounded stage budget: the model turn cannot complete without a
				// provider credential (this lane supplies none), so the session
				// runs until this budget rather than reaching a natural terminal.
				// Kept short so the handshake-completes assertion is fast.
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
	writeBackstopGhShimStep20(t, fakeBinDir)

	return &piHarnessFixture{
		donmaiBinary: donmaiBinary,
		daemonSrv:    srv,
		sessionID:    sessionID,
		wtParent:     wtParent,
		home:         home,
		fakeBinDir:   fakeBinDir,
		nodeDir:      nodeBinDir(),
	}
}

// pathEntries assembles the PATH `donmai agent run` hands the child: the fake
// shim dir, any caller-supplied dirs (the pi binary dir), node's dir (pi is a
// node script), then the system dirs.
func (f *piHarnessFixture) pathEntries(extra ...string) []string {
	entries := append([]string{f.fakeBinDir}, extra...)
	if f.nodeDir != "" {
		entries = append(entries, f.nodeDir)
	}
	return append(entries, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
}

// run drives `donmai agent run` against the fixture with the given extra PATH
// dirs (typically the pi binary dir).
func (f *piHarnessFixture) run(t *testing.T, ctx context.Context, extraPathDirs ...string) (stdout, stderr string, runErr error) {
	t.Helper()

	cmd := exec.CommandContext(
		ctx, f.donmaiBinary, //nolint:gosec // binary + flags are test-controlled.
		"agent", "run",
		"--session-id", f.sessionID,
		"--daemon-url", f.daemonSrv.URL,
		"--worktree-dir", f.wtParent,
	)
	pathEntries := f.pathEntries(extraPathDirs...)
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

func piResolvedProfile() map[string]any {
	return map[string]any{
		"provider": "pi",
		"model":    piModel(),
	}
}

// resolvePiBinDir installs/resolves the pinned pi binary and returns a PATH
// directory under which it is reachable as the literal name `pi`.
func resolvePiBinDir(t *testing.T) string {
	t.Helper()
	piBin := afh.EnsurePiBinary(t)
	if filepath.Base(piBin) == "pi" {
		return filepath.Dir(piBin)
	}
	aliasDir := t.TempDir()
	linkPiAlias(t, piBin, aliasDir)
	return aliasDir
}

// TestPiHarnessSmoke_VersionPinGuard proves a below-MinVersion `pi` on PATH
// is refused at provider-construction time rather than silently accepted.
// Fully offline and deterministic; does not depend on the RPC protocol at
// all (probe.go's version check runs before any RPC session is opened).
func TestPiHarnessSmoke_VersionPinGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	f := setupPiHarnessFixture(t, "pinguard", piResolvedProfile())

	oldBinDir := t.TempDir()
	writeFakeOldPi(t, oldBinDir)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, stderr, _ := f.run(t, ctx, oldBinDir)

	if !strings.Contains(stderr, "pi") || !strings.Contains(stderr, "below the minimum supported version") {
		t.Fatalf("stderr: want a provider-probe-failed WARN for pi citing the version-pin violation, got:\n%s", stderr)
	}
}

// TestPiHarnessSmoke_RealBinary_HandshakeCompletes is the load-bearing
// real-binary check that the pi RPC/extension protocol donmai speaks matches
// the real 0.80.10 binary. With node on the child PATH and a resolvable model,
// `donmai agent run` spawns real pi, the fail-closed handshake COMPLETES
// (per-session token + extension self-SHA verified Go-side), and the session
// proceeds PAST Spawn into a real model turn.
//
// The assertion is the flip: the run must NOT fail with "policy extension
// failed to load" / failureMode "spawn-failed" (the old, now-fixed protocol
// gap). It instead runs until the short stage budget, because the model turn
// cannot complete without a provider credential this lane deliberately does not
// supply. A regression to the old protocol re-introduces the spawn failure and
// fails this test loudly.
func TestPiHarnessSmoke_RealBinary_HandshakeCompletes(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	piBinDir := resolvePiBinDir(t)

	f := setupPiHarnessFixture(t, "handshake", piResolvedProfile())
	if f.nodeDir == "" {
		t.Skip("node not on PATH — pi is a node script and cannot exec; skipping real-binary handshake check")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stdout, stderr, runErr := f.run(t, ctx, piBinDir)

	if strings.Contains(stderr, "below the minimum supported version") {
		t.Fatalf("unexpected version-pin rejection of the real pinned binary; stderr:\n%s", stderr)
	}

	var res struct {
		Status      string `json:"status"`
		Error       string `json:"error"`
		FailureMode string `json:"failureMode"`
	}
	if err := afh.JSONUnmarshal(stdout, &res); err != nil {
		t.Fatalf("decode agent-run Result JSON: %v\n--- stdout ---\n%s\n--- stderr ---\n%s", err, stdout, stderr)
	}

	// The flip: the handshake completed, so the run did NOT fail at spawn.
	const spawnGap = "policy extension failed to load"
	if strings.Contains(res.Error, spawnGap) {
		t.Fatalf("run failed with %q — the pi handshake did NOT complete against the real binary.\n"+
			"If node is on PATH and the model resolves, this means a protocol regression (donmai#215 fixed this).\n"+
			"Result.Error=%q status=%q failureMode=%q\n--- stdout ---\n%s\n--- stderr ---\n%s",
			spawnGap, res.Error, res.Status, res.FailureMode, stdout, stderr)
	}
	if res.FailureMode == "spawn-failed" {
		t.Fatalf("failureMode=spawn-failed — the pi session never started (handshake gate).\n"+
			"Result.Error=%q status=%q\n--- stdout ---\n%s\n--- stderr ---\n%s",
			res.Error, res.Status, stdout, stderr)
	}
	// runErr is expected non-nil here (the run ends unsuccessfully at the budget /
	// uncredentialed model turn) — that is the point: it got PAST the handshake.
	_ = runErr
	t.Logf("handshake completed against real pi; run reached a model turn — status=%q failureMode=%q error=%q",
		res.Status, res.FailureMode, res.Error)
}

// TestPiHarnessSmoke_RealBinary_Teardown drives the REAL pinned pi binary,
// sends SIGTERM mid-run, and asserts the child pi process is gone afterward
// (09 §8 item 8: "Stop mid-run; no orphan processes"). With node on PATH the
// pi child really runs (handshake completes, model turn in flight), so this
// exercises real mid-turn teardown.
func TestPiHarnessSmoke_RealBinary_Teardown(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	piBinDir := resolvePiBinDir(t)

	f := setupPiHarnessFixture(t, "teardown", piResolvedProfile())
	if f.nodeDir == "" {
		t.Skip("node not on PATH — pi is a node script and cannot exec; skipping real-binary teardown check")
	}

	pathEntries := f.pathEntries(piBinDir)
	cmd := exec.Command(
		f.donmaiBinary, //nolint:gosec // binary + flags are test-controlled.
		"agent", "run",
		"--session-id", f.sessionID,
		"--daemon-url", f.daemonSrv.URL,
		"--worktree-dir", f.wtParent,
	)
	stderrFile, ferr := os.CreateTemp(t.TempDir(), "stderr-*.log")
	if ferr != nil {
		t.Fatalf("create stderr temp file: %v", ferr)
	}
	cmd.Env = []string{
		"PATH=" + strings.Join(pathEntries, string(os.PathListSeparator)),
		"HOME=" + f.home,
		"XDG_CONFIG_HOME=" + filepath.Join(f.home, ".config"),
		"DONMAI_STATE_HOME=" + f.home,
		"NO_COLOR=1",
	}
	cmd.Stderr = stderrFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start donmai agent run: %v", err)
	}

	// Give it time to reach pi's Spawn, complete the handshake, and enter the
	// model turn, then SIGTERM the donmai process (which owns the pi child).
	time.Sleep(3 * time.Second)
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

	// Allow a moment for the SIGTERM→SIGKILL escalation over the pi child's
	// process group to reap it.
	time.Sleep(2 * time.Second)
	if _, err := exec.LookPath("pgrep"); err == nil {
		out, _ := exec.Command("pgrep", "-f", "pi --mode rpc").CombinedOutput() //nolint:gosec // fixed args.
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("orphan pi process(es) left running after mid-run teardown:\n%s", out)
		}
	}
}

// linkPiAlias creates a symlink (or copy, if symlinking fails) named "pi"
// inside dir pointing at src, so PATH resolution finds it under the exact
// literal name the provider execs.
func linkPiAlias(t *testing.T, src, dir string) {
	t.Helper()
	dst := filepath.Join(dir, "pi")
	if err := os.Symlink(src, dst); err == nil {
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read pi binary at %s to alias it: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil { //nolint:gosec // executable alias needs the exec bit.
		t.Fatalf("write pi alias at %s: %v", dst, err)
	}
}
