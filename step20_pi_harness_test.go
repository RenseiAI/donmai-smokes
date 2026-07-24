package smokes

// step20_pi_harness_test.go — pi harness smoke lane
// (runs/2026-07-21-open-harness-strategy/09-design-pi-adapter.md §8;
// 12-work-breakdown.md W2b's completion note + W2b follow-up: the pi
// provider is now registered in donmai's agent-run ctor list — see
// harness/pi_install.go's package doc and the donmai PR immediately
// preceding this one — so a `donmai agent run` session (and this
// black-box smoke, which only ever drives the compiled binary's CLI
// surface, never donmai as a Go library, per AGENTS.md) can reach
// pi.New()/Spawn() at all.
//
// # A major real-binary finding: the pi RPC/extension protocol donmai's
// # adapter targets does not match the real pinned binary
//
// donmai's provider/harness/pi package (and its embedded TypeScript policy
// extension, extensions/donmai-policy.ts) was built against the design
// doc's (09-design-pi-adapter.md) own description of pi's RPC and
// extension-UI protocol. Its own doc.go and probe.go candidly flag this as
// UNVERIFIED-LOCALLY ("pi --version was absent from the authoring host").
// Installing the REAL pinned binary (@earendil-works/pi-coding-agent@
// 0.80.10) for this smoke and reading its BUNDLED, authoritative protocol
// docs (docs/rpc.md, docs/extensions.md — shipped inside the npm package
// itself, not third-party) surfaces concrete, verified mismatches:
//
//  1. Command envelope key: donmai sends {"command": "prompt", "text": ...}
//     (pi.go/handle.go, throughout). The real wire protocol uses
//     {"type": "prompt", "message": ...} — confirmed by directly piping
//     both shapes into a real `pi --mode rpc` process: the "command" key
//     produces {"type":"response","success":false,"error":"Unknown
//     command: undefined"}; only the "type" key is recognized at all.
//  2. get_entries (the Resume/cursor-replay command, design §4): donmai
//     sends {"command":"get_entries","session":sessionID,"since":
//     sessionID} — besides the wrong envelope key, real pi's get_entries
//     takes no "session" field (docs/rpc.md §get_entries): it operates on
//     whatever session is already loaded (selected via the CLI's
//     --session/--resume flags, not an RPC parameter), and "since" is an
//     ENTRY id (a cursor within that session), not the session id itself.
//  3. set_model: donmai sends {"command":"set_model","model":"donmai/"+
//     model}. Real pi wants {"type":"set_model","provider":...,
//     "modelId":...} — two separate fields, not one combined string.
//  4. Extension UI protocol (the entire trust boundary, design §5): donmai's
//     embedded extensions/donmai-policy.ts calls a fictional
//     `pi.ui.request({type: "donmai.handshake"|"donmai.adjudicate", ...})`
//     and `pi.defineTool({name, override: true, run(args, orig)})`. Real
//     pi's documented extension API (docs/extensions.md) has neither
//     method: tool interception is `pi.on("tool_call", async (event, ctx)
//     => ({block: true, reason}))`, and the only extension_ui_request
//     "method" values are the fixed dialog/fire-and-forget set (select,
//     confirm, input, editor, notify, setStatus, setWidget, setTitle,
//     set_editor_text — docs/rpc.md "Extension UI Protocol"). There is no
//     generic custom-request channel a `pi.ui.request({type: "donmai.…"})`
//     call could ride on.
//
// The practical consequence: Spawn's fail-closed handshake gate (pi.go
// launch(), design §2 step 3 — "no handshake within 10s ⇒ kill child,
// return agent.ErrSpawnFailed") ALWAYS times out against the real binary,
// because the embedded extension never emits anything the real protocol
// recognizes as a request pi will forward, and even if it did, the Go side
// replies in a shape ({"command":"extension_ui_response","response":
// {...}}) the real protocol does not expect either (docs/rpc.md: replies
// are {"type":"extension_ui_response","id":...,"value"|"confirmed"|
// "cancelled":...} at the top level, not nested under "response"). Spawn
// NEVER completes handshake, so it never reaches the RPC commands named
// above at all.
//
// This means smoke items 1-10 (09 §8) are not really "unit-green" in any
// sense that predicts real-binary behavior — every existing fixture (this
// repo's own future ones, and donmai's pipe-stub unit tests) encodes the
// SAME wrong protocol assumptions the design doc made, so the mismatch
// only surfaces against the real binary. This is the identical disease
// class the opencode Lane-B SSE "properties" vs "data" bug was (see
// donmai#211, merged), except broader: it blocks EVERY item, not one event
// type, because it blocks Spawn's handshake gate itself.
//
// Given this, items 3/6/7/9 (the real-binary complements this lane owns
// per 12-work-breakdown.md W2b) cannot be exercised as designed: there is
// no live session to prompt, steer, resume, or pin a provider on. Fixing
// this is a real, scoped, but non-trivial rewrite: donmai's rpc.go command
// construction (pi.go/handle.go send sites) needs the "type" envelope, and
// extensions/donmai-policy.ts needs to be rebuilt against pi.on("tool_call")
// + pi.registerTool() + ctx.ui.confirm() (a real, viable mechanism — pi
// DOES support synchronous tool-call blocking via the event handler's
// return value, so the trust boundary is buildable, just not as currently
// coded). That rewrite is out of scope for this lane; it is recorded here,
// verified, and dated so it is not silently lost.
//
// What THIS lane covers instead, honestly:
//
//   - TestPiHarnessSmoke_VersionPinGuard: construction-time version-pin
//     enforcement (probe.go) — does not depend on the RPC protocol at all,
//     proven green against the real pinned binary.
//   - TestPiHarnessSmoke_RealBinary_Teardown (item 8): SIGINT while Spawn
//     is blocked in the handshake wait — pi.go's launch() explicitly kills
//     the child on ctx.Done() even pre-handshake, so this is real,
//     protocol-independent, verified coverage of "no orphan pi process."
//   - TestPiHarnessSmoke_RealBinary_HandshakeFailsClosed_KnownProtocolGap:
//     a red-then-green-when-fixed regression proof — the SPECIFIC,
//     verified failure this smoke expects (a spawn failure whose message
//     names the policy-extension handshake timeout) is asserted
//     explicitly. If donmai's protocol implementation is ever corrected
//     and Spawn starts completing handshake against the real binary, this
//     assertion FAILS LOUDLY (an unexpected success, not a silent skip) —
//     the intended signal that items 3/6/7/9 (and 1/2/4/5/10's real
//     validity) can be attempted for real.
//
// # OSS boundary
//
// Same fixture shape as step18: an httptest daemon-control fixture serving
// only /api/daemon/sessions/<id>, no SaaS control plane. pi installs from
// the public npm registry via harness.EnsurePiBinary.

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
				// Bounded stage budget mirrors step18's own rationale: without
				// one, a session that never reaches a terminal event (pi's
				// handshake gate fails closed and Spawn errors quickly here,
				// but this is defense-in-depth against the runner's own
				// retry/steering loop) could run indefinitely.
				"stageBudget": map[string]any{"maxDurationSeconds": 30},
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
	}
}

// run drives `donmai agent run` against the fixture with the given PATH.
func (f *piHarnessFixture) run(t *testing.T, ctx context.Context, extraPathDirs ...string) (stdout, stderr string, runErr error) {
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

func piResolvedProfile() map[string]any {
	return map[string]any{
		"provider": "pi",
		"model":    "smoke-test-model",
	}
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

// TestPiHarnessSmoke_RealBinary_HandshakeFailsClosed_KnownProtocolGap is a
// red-then-green-when-fixed regression proof for the wire-protocol
// mismatch documented in this file's package doc. It asserts the SPECIFIC,
// verified failure mode: Spawn's fail-closed handshake gate times out
// against the real binary (policy extension never completes a handshake
// the real protocol recognizes). If donmai's pi adapter is ever corrected
// to speak the real RPC/extension-UI protocol, this assertion fails loudly
// (unexpected success) instead of silently staying green for the wrong
// reason — the signal that items 3/6/7/9 can then be attempted for real.
func TestPiHarnessSmoke_RealBinary_HandshakeFailsClosed_KnownProtocolGap(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	piBin := afh.EnsurePiBinary(t)
	piBinDir := filepath.Dir(piBin)
	binDirForPath := piBinDir
	if filepath.Base(piBin) != "pi" {
		aliasDir := t.TempDir()
		linkPiAlias(t, piBin, aliasDir)
		binDirForPath = aliasDir
	}

	f := setupPiHarnessFixture(t, "handshake", piResolvedProfile())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stdout, stderr, runErr := f.run(t, ctx, binDirForPath)

	if strings.Contains(stderr, "below the minimum supported version") {
		t.Fatalf("unexpected version-pin rejection of the real pinned binary; stderr:\n%s", stderr)
	}
	if runErr == nil {
		t.Fatalf("donmai agent run: want a non-nil error (spawn failure expected — see package doc)\n--- stdout ---\n%s\n--- stderr ---\n%s", stdout, stderr)
	}

	var res struct {
		Status      string `json:"status"`
		Error       string `json:"error"`
		FailureMode string `json:"failureMode"`
	}
	if err := afh.JSONUnmarshal(stdout, &res); err != nil {
		t.Fatalf("decode agent-run Result JSON: %v\n--- stdout ---\n%s\n--- stderr ---\n%s", err, stdout, stderr)
	}

	const wantSubstring = "policy extension failed to load"
	if !strings.Contains(res.Error, wantSubstring) {
		t.Fatalf("Result.Error = %q; want it to contain %q (the documented, verified real-binary protocol gap — "+
			"if this changed, the pi adapter's RPC/extension protocol may have been fixed: re-read this file's "+
			"package doc and, if so, promote items 3/6/7/9 out of skip)\n--- stdout ---\n%s\n--- stderr ---\n%s",
			res.Error, wantSubstring, stdout, stderr)
	}
	t.Logf("confirmed known protocol gap: Result.Error=%q status=%q failureMode=%q", res.Error, res.Status, res.FailureMode)
}

// TestPiHarnessSmoke_RealBinary_Teardown drives the REAL pinned pi binary,
// sends SIGTERM mid-run (during Spawn's handshake wait — the handshake
// never completes per the documented protocol gap, so the process is
// reliably still inside that wait when the signal lands), and asserts the
// child process is gone afterward (09 §8 item 8: "Stop mid-run; no orphan
// processes"). pi.go's launch() explicitly kills the child on ctx.Done()
// even pre-handshake (`case <-ctx.Done(): _ = h.Stop(...)`), so this
// exercises real, protocol-independent teardown coverage.
func TestPiHarnessSmoke_RealBinary_Teardown(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	piBin := afh.EnsurePiBinary(t)
	piBinDir := filepath.Dir(piBin)
	binDirForPath := piBinDir
	if filepath.Base(piBin) != "pi" {
		aliasDir := t.TempDir()
		linkPiAlias(t, piBin, aliasDir)
		binDirForPath = aliasDir
	}

	f := setupPiHarnessFixture(t, "teardown", piResolvedProfile())

	pathEntries := append([]string{f.fakeBinDir, binDirForPath}, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
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

	// Give it a moment to reach pi's Spawn (materialize extension, exec the
	// child, enter the handshake wait), then SIGTERM the donmai process
	// (which owns the pi child) mid-flight.
	time.Sleep(1 * time.Second)
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
