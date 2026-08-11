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
//   - TestPiHarnessSmoke_RealBinary_CompletedTurn: drives ONE full
//     prompt->event-stream round trip (item 3) through a local stub
//     OpenAI-compatible endpoint threaded via ResolvedProfile.Endpoint, and
//     asserts every request the stub received was addressed to the pinned
//     model and nothing else (item 9's pin lockout half — see its own doc
//     comment for exactly which of items 3/6/7/9 this proves and which it
//     does not).
//
// # Items 3/6/7/9 — where each is now proven, and where it genuinely cannot be
//
// The endpoint-from-resolvedProfile wiring this file's previous revision
// named as the unblocking follow-up (analogous to the opencode preferServer
// hint, donmai#209) landed as runner/spec_translation.go's
// `Endpoint: copyEndpointBinding(qw.ResolvedProfile.Endpoint)` — but the
// ONLY producer that can put an arbitrary stub base URL onto
// ResolvedProfile.Endpoint today is the worker-local gateway
// (afcli/gateway_bind.go, servingHost:"gateway"): the daemon's
// SessionEndpointBinding wire struct deliberately has no baseUrl field, so
// no dispatch payload can point a harness at an arbitrary URL. That
// producer is what TestPiHarnessSmoke_RealBinary_CompletedTurn (below) and
// step19_gateway_test.go's TestGatewaySmoke_WorkerLocalBinding_CompletesTurn
// both drive.
//
//   - Item 3 (assistant-text round trip): PROVEN here and in step19.
//   - Item 9 (provider-pin round trip): PROVEN here (pin lockout: every
//     stub request addressed the pinned model) and in step19 (credential
//     isolation: the real pi RPC child inherits none of the worker-only
//     gateway config).
//   - Item 6 (steer mid-turn / follow_up idle) and item 7 (resume/cursor
//     replay): NOT reachable from this black-box `donmai agent run` CLI
//     path at all, regardless of endpoint wiring — two independent findings,
//     both verified against the real pi binary:
//     (a) `donmai agent run`'s only production call site for
//     Handle.Inject (runner/loop.go's drainMemoryInjects) fires exclusively
//     at the post-terminal seam, and a pi Handle closes itself
//     (h.closed/h.events) in the same goroutine that processes its terminal
//     event — so by the time drainMemoryInjects could ever call Inject on a
//     pi handle, it already returns "pi: session closed". Steering a turn
//     WHILE it is in flight has no reachable trigger in the headless
//     dispatch path at all.
//     (b) Provider.Resume has no production call site outside
//     agent/conformance/checks.go (nothing drives that suite for pi today);
//     `donmai agent run` never calls it.
//     Both are proven directly against the real binary in donmai's own
//     provider/harness/pi package (real_binary_test.go) using the
//     Provider/Handle Go API — legitimate there since that package IS the
//     library under test; illegitimate here per this file's own boundary
//     note below (never donmai as a Go library). That file also pins the
//     finding in (a) with its own regression test
//     (TestRealBinary_Inject_AfterTerminal_FailsClosed), so a future change
//     to the Handle's post-terminal behavior is caught where the capability
//     actually lives, not silently rediscovered here.
//
// # OSS boundary
//
// Same fixture shape as step18: an httptest daemon-control fixture serving only
// /api/daemon/sessions/<id>, no SaaS control plane. pi installs from the public
// npm registry via harness.EnsurePiBinary. No provider credentials are used.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

type piHarnessFixture struct {
	donmaiBinary string
	daemonSrv    *httptest.Server
	sessionID    string
	wtParent     string
	home         string
	fakeBinDir   string
	nodeDir      string

	// upstreamBaseURL/upstreamKey are the worker-side gateway knobs
	// (afcli/gateway_bind.go's DONMAI_GATEWAY_UPSTREAM_BASE_URL /
	// DONMAI_GATEWAY_UPSTREAM_API_KEY), set only by
	// TestPiHarnessSmoke_RealBinary_CompletedTurn. Every other test in this
	// file leaves them empty, so `run` emits no gateway env and behavior for
	// the existing tests is unchanged.
	upstreamBaseURL string
	upstreamKey     string
}

// piHarnessStageBudgetSeconds is the default stage-budget bound: short,
// because the model turn cannot complete without a provider credential (the
// version-pin/handshake/teardown lanes supply none), so those sessions run
// until this budget rather than reaching a natural terminal.
const piHarnessStageBudgetSeconds = 8

// setupPiHarnessFixture builds the donmai binary from piSourceDir() and
// stands up the daemon-control fixture. Skips cleanly when the source
// checkout is unavailable or predates the pi provider registration this
// lane pins against. stageBudgetSeconds overrides the default (8s) bound —
// callers driving a real completed turn need enough room for a cold pi boot
// plus a real HTTP round trip; callers proving only the handshake keep the
// short default by passing nothing.
func setupPiHarnessFixture(t *testing.T, testName string, resolvedProfile map[string]any, stageBudgetSeconds ...int) *piHarnessFixture {
	t.Helper()

	afh.SkipIfToolMissing(t, "git", "the bare fixture repo this smoke clones is created with git")

	// Locate first, probe second: see step17 for why the old ordering made a
	// missing checkout indistinguishable from an outdated one.
	srcDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	if _, err := os.Stat(filepath.Join(srcDir, "provider", "harness", "pi", "probe.go")); err != nil {
		afh.DeclineLive(t, "donmai checkout at %q predates provider/harness/pi/probe.go — "+
			"point %s at a checkout that has it", srcDir, inFlightSourceDirEnv)
	}

	donmaiBinary, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir:  srcDir,
		OutputPath: filepath.Join(t.TempDir(), "donmai"),
	})

	sessionRepo := afh.MakeBareFixtureRepo(t, "pi-harness-repo-"+testName)
	sessionID := "smoke-pi-harness-" + testName

	budget := piHarnessStageBudgetSeconds
	if len(stageBudgetSeconds) > 0 {
		budget = stageBudgetSeconds[0]
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/daemon/sessions/"+sessionID {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId":       sessionID,
				"issueIdentifier": "SMOKE-20",
				"title":           "pi harness smoke",
				"body":            "Reply with a one-line status. Do not use any tools.",
				"workType":        "development",
				"repository":      sessionRepo,
				"ref":             "main",
				"branch":          "agent/" + sessionID,
				"workerId":        "smoke-worker",
				"authToken":       "smoke-token",
				"platformUrl":     srv.URL,
				"resolvedProfile": resolvedProfile,
				// Bounded stage budget — see the field doc above.
				"stageBudget": map[string]any{"maxDurationSeconds": budget},
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
// dirs (typically the pi binary dir). The worker-local gateway env
// (upstreamBaseURL/upstreamKey) rides the WORKER's own env, exactly as
// afcli/gateway_bind.go documents — it is never part of the daemon-fixture
// JSON payload above, and step19_gateway_test.go's credential-isolation
// wrapper is the existing proof that it never reaches the pi RPC child.
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
	if f.upstreamBaseURL != "" {
		cmd.Env = append(cmd.Env, gatewayEnvUpstreamBaseURL+"="+f.upstreamBaseURL)
	}
	if f.upstreamKey != "" {
		cmd.Env = append(cmd.Env, gatewayEnvUpstreamKey+"="+f.upstreamKey)
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

// piCompletedTurnResolvedProfile is piResolvedProfile plus servingHost:
// "gateway" — the ONE signal that makes the worker bind a gateway session
// (afcli/gateway_bind.go's isGatewayServed) and thread the resulting
// EndpointBinding through ResolvedProfile.Endpoint ->
// runner/spec_translation.go's Spec.Endpoint copy -> pi's applyEndpoint ->
// pi.registerProvider("donmai", ...). This is the ONLY producer in donmai
// today that can put an arbitrary (non-wire-carried) base URL onto
// ResolvedProfile.Endpoint: the daemon's SessionEndpointBinding wire struct
// has no baseUrl field by design (no dispatch payload may point a harness
// at an arbitrary URL — gateway_bind.go's file doc), so a completed turn
// against a stub endpoint can only be produced via this same worker-local
// gateway path step19_gateway_test.go already exercises for the
// credential-isolation/cost-ledger claims. This profile reuses it to
// exercise it FROM step20's own pi-focused fixture instead, for the
// items step20's real-binary lane specifically owns: assistant round trip
// (item 3) and provider-pin lockout (item 9) from
// runs/2026-07-21-open-harness-strategy/09-design-pi-adapter.md §8.
func piCompletedTurnResolvedProfile(model string) map[string]any {
	return map[string]any{
		"provider":    "pi",
		"model":       model,
		"servingHost": "gateway",
	}
}

// piCompletedTurnStub is a minimal local httptest OpenAI-compatible upstream
// for TestPiHarnessSmoke_RealBinary_CompletedTurn. It answers every
// chat-completions call with a short streamed reply carrying a distinctive
// per-call marker plus the runner's WORK_RESULT verdict marker (so the
// session reaches a NATURAL "completed" terminal instead of dying at a
// stage budget), and records every request's model so the test can assert
// the pinned model — and nothing else — was ever addressed.
type piCompletedTurnStub struct {
	srv   *httptest.Server
	model string

	mu     sync.Mutex
	models []string
}

func newPiCompletedTurnStub(t *testing.T, model string) *piCompletedTurnStub {
	t.Helper()
	s := &piCompletedTurnStub{model: model}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("step20 stub: unsupported route"))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *piCompletedTurnStub) baseURL() string { return s.srv.URL + "/v1" }

func (s *piCompletedTurnStub) assistantText() string {
	return "step20 completed-turn check-in: " + piCompletedTurnMarker + ".\n\n" + piCompletedTurnWorkResultMarker + "\n"
}

func (s *piCompletedTurnStub) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	model := body.Model
	if model == "" {
		model = s.model
	}
	s.mu.Lock()
	s.models = append(s.models, model)
	s.mu.Unlock()

	if !body.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-step20",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": s.assistantText()},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 9, "total_tokens": 20},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	chunk := func(payload map[string]any) {
		payload["id"] = "chatcmpl-step20"
		payload["object"] = "chat.completion.chunk"
		payload["created"] = time.Now().Unix()
		payload["model"] = model
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk(map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}}})
	chunk(map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": s.assistantText()}}}})
	chunk(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 9, "total_tokens": 20},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// modelsAddressed returns every model name this stub was asked for, in call
// order.
func (s *piCompletedTurnStub) modelsAddressed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.models...)
}

// piCompletedTurnMarker is the distinctive text the stub returns. Observing
// it downstream proves the runner consumed the turn THIS stub produced.
const piCompletedTurnMarker = "step20-pi-completed-turn-ok"

// piCompletedTurnWorkResultMarker lets the session reach a natural
// "completed" terminal instead of dying at the stage budget (runner
// loop.go scanWorkResult) — the point of this lane being pi's own
// completed-turn proof (mirrors step19's gatewayWorkResultMarker).
const piCompletedTurnWorkResultMarker = "WORK_RESULT:passed"

// piCompletedTurnStageBudgetSeconds bounds the completed-turn session: room
// for a cold pi boot + handshake + a local HTTP round trip through the
// worker-local gateway, small enough that a hung lane fails fast.
const piCompletedTurnStageBudgetSeconds = 150

// piCompletedTurnRunResult is the narrow slice of the agent-run Result JSON
// this test asserts on.
type piCompletedTurnRunResult struct {
	Status      string `json:"status"`
	Error       string `json:"error"`
	FailureMode string `json:"failureMode"`
	WorkResult  string `json:"workResult"`
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
	afh.SkipIfShort(t, "end-to-end agent-run smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")

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
	afh.SkipIfShort(t, "end-to-end agent-run smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")

	piBinDir := resolvePiBinDir(t)

	f := setupPiHarnessFixture(t, "handshake", piResolvedProfile())
	if f.nodeDir == "" {
		afh.DeclineLive(t, "node not on PATH — pi is a node script and cannot exec, "+
			"so the real-binary handshake check has nothing to drive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stdout, stderr, runErr := f.run(t, ctx, piBinDir)

	if strings.Contains(stderr, "below the minimum supported version") {
		t.Fatalf("unexpected version-pin rejection of the real pinned binary; stderr:\n%s", stderr)
	}

	// Diagnose a PRE-SPAWN denial before the protocol asserts below. donmai's
	// tool/lifecycle adaptation compiler can reject the compiled Spec before pi
	// is ever executed; that surfaces as failureMode "spawn-failed" and would
	// otherwise be reported by the checks below as a pi handshake/protocol
	// regression, which is the wrong subsystem entirely. Additive: the asserts
	// that follow are unchanged.
	if detail := afh.ExplainAdaptationDenial(stdout + stderr); detail != "" {
		t.Fatalf("the pi session was denied before spawn, so the real-binary handshake was never reached.\n\n%s\n"+
			"--- stdout ---\n%s\n--- stderr ---\n%s", detail, stdout, stderr)
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

// TestPiHarnessSmoke_RealBinary_CompletedTurn extends the flip above: with a
// stub OpenAI-compatible endpoint threaded through ResolvedProfile.Endpoint
// (via the worker-local gateway producer — see the file doc comment for why
// that is the ONLY producer that can do this from a black-box `donmai agent
// run` invocation), the REAL pi binary completes a real model turn instead
// of dying at the stage budget. Two independent, regression-detecting
// assertions:
//
//  1. Item 3, assistant round trip: the runner consumed the stub's reply
//     (marker observed) and the session reached status "completed" — not
//     spawn-failed, not budget-exceeded.
//  2. Item 9, provider-pin lockout: every chat-completions request the stub
//     served named the pinned model. A session that silently fell back to
//     pi's own catalog default (the exact failure step19's file doc
//     describes finding before its producer existed) would show a different
//     model here, or zero requests at all.
func TestPiHarnessSmoke_RealBinary_CompletedTurn(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end agent-run smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")

	piBinDir := resolvePiBinDir(t)
	model := piModel()
	stub := newPiCompletedTurnStub(t, model)

	f := setupPiHarnessFixture(t, "completedturn", piCompletedTurnResolvedProfile(model), piCompletedTurnStageBudgetSeconds)
	f.upstreamBaseURL = stub.baseURL()
	f.upstreamKey = gatewayUpstreamKey
	if f.nodeDir == "" {
		afh.DeclineLive(t, "node not on PATH — pi is a node script and cannot exec, "+
			"so the completed-turn check has nothing to drive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	stdout, stderr, runErr := f.run(t, ctx, piBinDir)

	if strings.Contains(stderr, "must be an absolute HTTP(S) URL with a loopback hostname") {
		t.Fatalf("pi's HostGateway admission REFUSED the worker-minted binding — the gateway listens on "+
			"127.0.0.1, so this is a regression in that admission boundary.\n--- stderr ---\n%s", stderr)
	}
	if strings.Contains(stderr, "gateway binding:") {
		t.Fatalf("the worker could not bind a gateway session for a gateway-served cell.\n--- stderr ---\n%s", stderr)
	}

	// A PRE-SPAWN adaptation denial kills the session before pi is executed,
	// so zero turns reach the stub and every assertion below would report a
	// downstream symptom instead of the cause. Diagnose it first (mirrors
	// TestPiHarnessSmoke_RealBinary_HandshakeCompletes and step19's own
	// completed-turn test). Additive: the asserts that follow are unchanged.
	if detail := afh.ExplainAdaptationDenial(stdout + stderr); detail != "" {
		t.Fatalf("the completed-turn session was denied before spawn, so no turn could reach the stub.\n\n%s\n"+
			"--- stdout ---\n%s\n--- stderr ---\n%s", detail, stdout, stderr)
	}

	// (1) provider-pin lockout: every request the stub served named the
	// pinned model.
	models := stub.modelsAddressed()
	if len(models) == 0 {
		t.Fatalf("the stub recorded ZERO chat-completions requests — the ResolvedProfile.Endpoint -> "+
			"Spec.Endpoint -> applyEndpoint -> pi.registerProvider seam did not route a turn here at all.\n"+
			"runErr=%v\n--- stdout ---\n%s\n--- stderr ---\n%s", runErr, stdout, stderr)
	}
	for i, got := range models {
		if got != model {
			t.Errorf("stub request %d addressed model %q; want the pinned model %q — a turn reaching any "+
				"other model is a provider-pin lockout failure", i, got, model)
		}
	}

	// (2) the turn completed: the runner consumed the assistant text and
	// reached the "completed" terminal.
	var res piCompletedTurnRunResult
	if err := afh.JSONUnmarshal(stdout, &res); err != nil {
		t.Fatalf("decode agent-run Result JSON: %v\n--- stdout ---\n%s\n--- stderr ---\n%s", err, stdout, stderr)
	}
	if res.FailureMode == "spawn-failed" || strings.Contains(res.Error, "policy extension failed to load") {
		t.Fatalf("the pi session never started (spawn/handshake gate) — no turn could have completed.\n"+
			"status=%q failureMode=%q error=%q\n--- stderr ---\n%s", res.Status, res.FailureMode, res.Error, stderr)
	}
	if res.FailureMode == "budget-exceeded" {
		t.Fatalf("the session died at the stage budget instead of completing the turn.\n"+
			"status=%q error=%q\nrequests observed: %d\n--- stderr ---\n%s",
			res.Status, res.Error, len(models), stderr)
	}
	if !strings.Contains(stdout+stderr, piCompletedTurnMarker) && res.WorkResult == "" {
		t.Errorf("the runner never surfaced the stub's assistant text (marker %q absent from stdout/stderr and no "+
			"workResult verdict recorded) — the turn's output did not reach the runner.\n"+
			"status=%q failureMode=%q error=%q", piCompletedTurnMarker, res.Status, res.FailureMode, res.Error)
	}
	if res.Status != "completed" {
		t.Errorf("agent-run Result status = %q (failureMode=%q error=%q); want \"completed\" — the turn ran "+
			"through the endpoint-threaded stub but the session did not reach a completed terminal",
			res.Status, res.FailureMode, res.Error)
	}

	t.Logf("pi completed-turn lane: %d stub request(s), all addressed model %q; status=%q failureMode=%q",
		len(models), model, res.Status, res.FailureMode)
}

// TestPiHarnessSmoke_RealBinary_Teardown drives the REAL pinned pi binary,
// sends SIGTERM mid-run, and asserts the child pi process is gone afterward
// (09 §8 item 8: "Stop mid-run; no orphan processes"). With node on PATH the
// pi child really runs (handshake completes, model turn in flight), so this
// exercises real mid-turn teardown.
func TestPiHarnessSmoke_RealBinary_Teardown(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end agent-run smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")

	piBinDir := resolvePiBinDir(t)

	f := setupPiHarnessFixture(t, "teardown", piResolvedProfile())
	if f.nodeDir == "" {
		afh.DeclineLive(t, "node not on PATH — pi is a node script and cannot exec, "+
			"so the real-binary teardown check has nothing to drive")
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
