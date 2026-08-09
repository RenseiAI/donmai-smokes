package smokes

// step19_gateway_test.go — worker-local gateway completed-turn lane
// (runs/2026-07-21-open-harness-strategy/08-design-gateway-host.md §5/§9/§10;
// 12-work-breakdown.md W3's SMOKE-GAP note; and the donmai#216 follow-up
// boundary: "a separate worker-local producer must bind gateway M1 per session
// and supply the complete loopback endpoint descriptor. The paired
// completed-turn proof belongs in donmai-smokes step19").
//
// # What this lane proves that nothing else does
//
// donmai's gateway M1 module (donmai#212) is unit/integration-covered IN donmai
// for its own behavior, and donmai#216 threaded a resolved endpoint into
// agent.Spec while admitting HostGateway at pi's endpoint boundary only for an
// absolute loopback HTTP(S) URL. Neither produced a BOUND gateway session on the
// real dispatch path. The worker-local producer that does
// (afcli/gateway_bind.go, the donmai PR paired with this lane) is what this
// smoke exercises end-to-end through the shipped binaries:
//
//	daemon session detail (resolvedProfile.servingHost = "gateway")
//	  → `donmai agent run` starts a gateway in the WORKER process
//	  → gateway.Bind mints a loopback base URL + per-session bearer
//	  → runner Spec.Endpoint → pi's HostGateway admission (loopback-only)
//	  → pi registers its "donmai" provider at that base URL
//	  → a real model turn over 127.0.0.1 that COMPLETES
//	  → the gateway relays it to the upstream with the WORKER's credential
//	  → a cost-ledger row stamped harness/host=gateway
//
// This is the gap every deferred item in step20 pointed at ("genuinely blocked
// on a completed model turn"). A turn that completes needs either a real
// provider credential (out of bounds: cost + the OSS no-creds boundary) or a
// local OpenAI-compatible endpoint the harness is routed to. A worker-local
// gateway cell IS that endpoint.
//
// # Two real findings this lane was built on (both verified against the pinned
// # pi 0.80.10 binary, both invisible to any previous lane)
//
//  1. A gateway binding whose token is missing is NOT a loud failure: pi's
//     registered provider refuses the turn locally ("No API key for provider:
//     donmai"), never dials, and the session burns its stage budget with an
//     empty assistant message. The token therefore has to be minted in-process
//     (agent.EndpointBinding.Env is json:"-" and cannot serialize).
//  2. Before the producer landed, a gateway-served cell silently ran on pi's
//     OWN catalog default (provider azure-openai-responses, baseUrl "") because
//     nothing set ResolvedProfile.Endpoint on the black-box path — the daemon's
//     SessionResolvedProfile mirror carries servingHost but no endpoint. A lane
//     asserting only "the session ran" would have been green through that.
//
// # Scope honesty
//
//   - PROVEN here: the whole binding → harness → completed-turn → metering path
//     against the REAL gateway code in the REAL binaries, with no worker-only
//     gateway credential or route reaching the real Pi child.
//   - NOT proven here: a PLATFORM resolver choosing a gateway cell (this lane
//     supplies the resolved profile itself, as every step18/step20 lane does),
//     cross-protocol surfaces (gateway M2), or any matrix-cell promotion — this
//     lane flips no flag.
//
// # OSS boundary
//
// Same fixture shape as step18/step20: an httptest daemon-control fixture
// serving only /api/daemon/sessions/<id>, plus an httptest OpenAI-compatible
// UPSTREAM that the gateway dials with a fake key. No SaaS control plane, no
// real provider credential, no outbound model traffic — the only network is
// 127.0.0.1 plus pi's npm install (public registry, via harness.EnsurePiBinary).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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

// gatewayTurnMarker is the distinctive text the upstream returns. Observing it
// downstream proves the runner consumed the turn THIS upstream produced.
const gatewayTurnMarker = "step19-gateway-turn-ok"

// gatewayWorkResultMarker is the verdict marker the runner scans assistant text
// for (donmai runner/loop.go scanWorkResult). Emitting "passed" lets the session
// reach its natural terminal instead of dying at a stage budget — the point of
// this lane being the first completed-turn proof.
const gatewayWorkResultMarker = "WORK_RESULT:passed"

// gatewayUpstreamKey is the fake upstream credential the WORKER holds. It never
// reaches the harness child (runtime/env's blocklist plus the gateway's own
// design): the child only ever sees the gateway's per-session bearer. The
// upstream fixture asserts it received exactly this value.
const gatewayUpstreamKey = "sk-step19-worker-held-not-a-real-key" //nolint:gosec // G101: fixture value for a local httptest upstream, not a credential.

// gatewayEnvUpstreamBaseURL / gatewayEnvUpstreamKey mirror the operator knobs
// donmai's worker-local gateway producer reads (afcli/gateway_bind.go). Kept as
// this suite's own copies — donmai-smokes never imports donmai as a Go library
// (AGENTS.md), same convention as harness/pi_install.go's pinned-version copy.
const (
	gatewayEnvUpstreamBaseURL = "DONMAI_GATEWAY_UPSTREAM_BASE_URL"
	gatewayEnvUpstreamKey     = "DONMAI_GATEWAY_UPSTREAM_API_KEY" //nolint:gosec // G101: env-var NAME, not a credential.
	gatewayEnvOpenAIAPIKey    = "OPENAI_API_KEY"                  //nolint:gosec // G101: env-var NAME, not a credential.
)

// gatewayStageBudgetSeconds bounds the completed-turn session: enough for a cold
// pi boot + handshake + a local turn + the post-turn block, small enough that a
// hung lane fails fast rather than eating the CI budget.
const gatewayStageBudgetSeconds = 150

// gatewayFailClosedStageBudgetSeconds bounds the fail-closed lane, which never
// reaches a turn by design (it fails at preflight).
const gatewayFailClosedStageBudgetSeconds = 20

// gatewayUpstreamRequest is one inbound request the upstream fixture served.
type gatewayUpstreamRequest struct {
	Path       string
	AuthHeader string
	Model      string
	Stream     bool
	Body       string
}

// gatewayUpstream is the httptest OpenAI-compatible upstream the worker-local
// gateway dials outbound. It stands in for OpenAI/OpenRouter/any compat URL —
// the same seam donmai's own gateway tests use — and records every request so
// this lane can assert what the GATEWAY sent (notably: the worker-held
// credential, never the harness's session bearer).
type gatewayUpstream struct {
	srv   *httptest.Server
	model string

	mu       sync.Mutex
	requests []gatewayUpstreamRequest
}

func newGatewayUpstream(t *testing.T, model string) *gatewayUpstream {
	t.Helper()
	up := &gatewayUpstream{model: model}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", up.handleChatCompletions)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		up.record(r, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "step19 upstream: unsupported route " + r.URL.Path},
		})
	})
	up.srv = httptest.NewServer(mux)
	t.Cleanup(up.srv.Close)
	return up
}

// baseURL is the OpenAI-compatible API root the worker points the gateway at.
func (up *gatewayUpstream) baseURL() string { return up.srv.URL + "/v1" }

func (up *gatewayUpstream) record(r *http.Request, body []byte) {
	req := gatewayUpstreamRequest{
		Path:       r.URL.Path,
		AuthHeader: r.Header.Get("Authorization"),
		Body:       string(body),
	}
	if len(body) > 0 {
		var decoded struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(body, &decoded); err == nil {
			req.Model = decoded.Model
			req.Stream = decoded.Stream
		}
	}
	up.mu.Lock()
	up.requests = append(up.requests, req)
	up.mu.Unlock()
}

// completions returns the recorded chat-completions requests.
func (up *gatewayUpstream) completions() []gatewayUpstreamRequest {
	up.mu.Lock()
	defer up.mu.Unlock()
	var out []gatewayUpstreamRequest
	for _, r := range up.requests {
		if r.Path == "/v1/chat/completions" {
			out = append(out, r)
		}
	}
	return out
}

// all returns every recorded request (failure diagnostics).
func (up *gatewayUpstream) all() []gatewayUpstreamRequest {
	up.mu.Lock()
	defer up.mu.Unlock()
	return append([]gatewayUpstreamRequest(nil), up.requests...)
}

// assistantText is the completion the upstream returns: the lane marker plus the
// verdict marker the runner scans for.
func (up *gatewayUpstream) assistantText() string {
	return "Gateway lane check-in: " + gatewayTurnMarker + ".\n\n" + gatewayWorkResultMarker + "\n"
}

func (up *gatewayUpstream) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	up.record(r, body)

	var decoded struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &decoded)
	model := decoded.Model
	if model == "" {
		model = up.model
	}

	if decoded.Stream {
		up.writeStream(w, model)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-step19",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": up.assistantText()},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 9, "total_tokens": 20},
	})
}

// writeStream emits the OpenAI chat SSE shape: role delta, content delta, a
// terminal chunk carrying finish_reason + usage, then [DONE].
func (up *gatewayUpstream) writeStream(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	chunk := func(payload map[string]any) {
		payload["id"] = "chatcmpl-step19"
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
	chunk(map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": up.assistantText()}}}})
	chunk(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 9, "total_tokens": 20},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// gatewayResolvedProfile is the pi profile with servingHost="gateway" — the ONLY
// signal the worker needs to bind a gateway session. Note what is deliberately
// absent: no endpoint, no base URL, no token. The worker generates all three
// locally, which is why no dispatch payload can point a harness anywhere.
func gatewayResolvedProfile(model string) map[string]any {
	return map[string]any{
		"provider":    "pi",
		"model":       model,
		"servingHost": "gateway",
	}
}

// writeBackstopGhShimStep19 mirrors step17/step18/step20's own locally-named
// copy of the fake `gh` shim — steps intentionally don't share test-only helpers
// beyond the harness package, per existing convention.
func writeBackstopGhShimStep19(t *testing.T, dir string) {
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

type gatewayFixture struct {
	donmaiBinary string
	daemonSrv    *httptest.Server
	sessionID    string
	wtParent     string
	home         string
	fakeBinDir   string
	nodeDir      string
	// upstreamBaseURL/upstreamKey are the worker-side gateway knobs. An empty
	// key models an unconfigured worker (the fail-closed lane).
	upstreamBaseURL string
	upstreamKey     string
}

// gatewaySourceDir resolves the worker-local gateway checkout. The explicit
// in-flight source override wins. Otherwise it supports both the primary
// donmai-smokes checkout (../donmai) and a linked worktree (../../donmai), while
// retaining the ordinary missing-sibling skip on hosted CI.
func gatewaySourceDir() string {
	if v := strings.TrimSpace(os.Getenv("DONMAI_ARCH_SOURCE_DIR")); v != "" {
		return v
	}
	for _, candidate := range []string{"../donmai", "../../donmai"} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return "../donmai"
}

// setupGatewayFixture builds the donmai binary from the gateway source checkout
// and stands up the daemon-control fixture carrying a gateway-served resolved
// profile. Skips cleanly when the source checkout predates the worker-local
// gateway producer this lane pins against.
func setupGatewayFixture(t *testing.T, testName string, resolvedProfile map[string]any, stageBudgetSeconds int) *gatewayFixture {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	srcDir := gatewaySourceDir()
	producer := filepath.Join(srcDir, "afcli", "gateway_bind.go")
	if _, err := os.Stat(producer); err != nil {
		t.Skipf("donmai checkout at %q predates the worker-local gateway producer (afcli/gateway_bind.go) this "+
			"lane pins against — point DONMAI_ARCH_SOURCE_DIR at a checkout that has it: %v", srcDir, err)
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

	sessionRepo := afh.MakeBareFixtureRepo(t, "gateway-lane-repo-"+testName)
	sessionID := "smoke-gateway-" + testName

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/daemon/sessions/"+sessionID {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId":       sessionID,
				"issueIdentifier": "SMOKE-19",
				"title":           "gateway completed-turn smoke",
				"body":            "Reply with a one-line status. Do not use any tools.",
				"workType":        "development",
				"repository":      sessionRepo,
				"ref":             "main",
				"branch":          "agent/" + sessionID,
				"workerId":        "smoke-worker",
				"authToken":       "smoke-token",
				"platformUrl":     srv.URL,
				"resolvedProfile": resolvedProfile,
				"stageBudget":     map[string]any{"maxDurationSeconds": stageBudgetSeconds},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "refreshed": true})
	}))
	t.Cleanup(srv.Close)

	fakeBinDir := t.TempDir()
	writeBackstopGhShimStep19(t, fakeBinDir)

	return &gatewayFixture{
		donmaiBinary: donmaiBinary,
		daemonSrv:    srv,
		sessionID:    sessionID,
		wtParent:     t.TempDir(),
		home:         t.TempDir(),
		fakeBinDir:   fakeBinDir,
		nodeDir:      nodeBinDir(),
	}
}

// pathEntries assembles the PATH `donmai agent run` hands the child: the fake
// shim dir, the pi binary dir, node's dir (pi is a node script), then system
// dirs. Mirrors step20's restricted-PATH posture.
func (f *gatewayFixture) pathEntries(extra ...string) []string {
	entries := append([]string{f.fakeBinDir}, extra...)
	if f.nodeDir != "" {
		entries = append(entries, f.nodeDir)
	}
	return append(entries, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
}

// writeGatewayPiWrapper prepends a wrapper for the real pi executable. It
// records only whether sensitive worker-only variables are present in pi's RPC
// child environment, then execs the real binary without changing its arguments.
func writeGatewayPiWrapper(t *testing.T, realPi, dir, observationPath string) {
	t.Helper()

	realPiPath := filepath.Join(dir, "pi-real")
	if err := os.Symlink(realPi, realPiPath); err != nil {
		data, readErr := os.ReadFile(realPi)
		if readErr != nil {
			t.Fatalf("read real pi executable for wrapper: %v", readErr)
		}
		if writeErr := os.WriteFile(realPiPath, data, 0o755); writeErr != nil { //nolint:gosec // executable wrapper fallback needs the exec bit.
			t.Fatalf("copy real pi executable for wrapper: %v", writeErr)
		}
	}

	const script = `#!/bin/sh
case " $* " in
  *" --mode rpc "*)
    {
      for key in DONMAI_GATEWAY_UPSTREAM_API_KEY DONMAI_GATEWAY_UPSTREAM_BASE_URL OPENAI_API_KEY; do
        if printenv "$key" >/dev/null 2>&1; then
          printf '%%s=present\n' "$key"
        else
          printf '%%s=absent\n' "$key"
        fi
      done
    } > %s
    ;;
esac
exec "$(dirname "$0")/pi-real" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(fmt.Sprintf(script, gatewayShellQuote(observationPath))), 0o755); err != nil { //nolint:gosec // executable wrapper needs the exec bit.
		t.Fatalf("write pi environment-observation wrapper: %v", err)
	}
}

func gatewayShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// assertGatewayPiCredentialIsolation proves the real pi RPC child did not
// inherit worker-only gateway configuration. The wrapper records presence only,
// never any credential or route value.
func assertGatewayPiCredentialIsolation(t *testing.T, observationPath string) {
	t.Helper()

	data, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatalf("read real pi environment observation: %v", err)
	}
	observed := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, state, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed real pi environment observation")
		}
		observed[key] = state
	}
	for _, key := range []string{gatewayEnvUpstreamKey, gatewayEnvUpstreamBaseURL, gatewayEnvOpenAIAPIKey} {
		if state := observed[key]; state != "absent" {
			t.Errorf("real pi RPC child inherited %s (state=%q); want absent", key, state)
		}
	}
}

// ledgerPath finds the cost ledger the worker-local gateway wrote under the
// isolated state home this fixture hands the run. The exact leaf is
// brand-derived (DONMAI_STATE_HOME anchors <home>/.<brand>/gateway/…), so this
// searches rather than hardcoding the brand — an OSS/downstream rebrand must not
// silently turn this assertion into a no-op. Returns "" when no ledger exists.
func (f *gatewayFixture) ledgerPath() string {
	var found string
	_ = filepath.WalkDir(f.home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil //nolint:nilerr // an unreadable subtree just means "not here".
		}
		if !d.IsDir() && d.Name() == "cost-events.jsonl" {
			found = path
		}
		return nil
	})
	return found
}

// run drives `donmai agent run` to completion (or the context deadline).
//
// The upstream credential is set on the WORKER's env only. It is not a provider
// credential in any meaningful sense (the upstream is a local httptest server),
// and it must never reach the harness child — the gateway holds it for the
// outbound leg while the child gets the loopback bearer.
func (f *gatewayFixture) run(t *testing.T, timeout time.Duration, extraPathDirs ...string) (stdout, stderr string, runErr error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(
		ctx, f.donmaiBinary, //nolint:gosec // binary + flags are test-controlled.
		"--debug", "agent", "run",
		"--session-id", f.sessionID,
		"--daemon-url", f.daemonSrv.URL,
		"--worktree-dir", f.wtParent,
	)
	cmd.Env = []string{
		"PATH=" + strings.Join(f.pathEntries(extraPathDirs...), string(os.PathListSeparator)),
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

// gatewayRunResult is the narrow slice of the agent-run Result JSON this lane
// asserts on.
type gatewayRunResult struct {
	Status            string `json:"status"`
	Error             string `json:"error"`
	FailureMode       string `json:"failureMode"`
	ProviderName      string `json:"providerName"`
	ProviderSessionID string `json:"providerSessionId"`
	WorkResult        string `json:"workResult"`
	PullRequestURL    string `json:"pullRequestUrl"`
}

// gatewayCostEvent is this suite's narrow mirror of donmai's
// gateway/costfeed.Event JSONL row (independent by convention — no library
// import). The `harness` sibling column is the one ADR-2026-06-06 D4 planned and
// the gateway is its first structurally-reliable producer (08 §7).
type gatewayCostEvent struct {
	DispatchID string `json:"dispatchId"`
	SessionID  string `json:"sessionId"`
	ProviderID string `json:"providerId"`
	Host       string `json:"host"`
	Harness    string `json:"harness"`
	Model      string `json:"model"`
	TokensIn   int    `json:"tokensIn"`
	TokensOut  int    `json:"tokensOut"`
}

// readGatewayCostEvents parses the JSONL ledger, tolerating a missing file
// (returns nil) so the caller can assert on emptiness rather than on an error.
func readGatewayCostEvents(t *testing.T, path string) []gatewayCostEvent {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // path is derived from this test's own t.TempDir() state home.
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []gatewayCostEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev gatewayCostEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("gateway cost ledger has a malformed row %q: %v", line, err)
			continue
		}
		out = append(out, ev)
	}
	return out
}

// TestGatewaySmoke_WorkerLocalBinding_CompletesTurn is the load-bearing check:
// a gateway-served cell, the REAL worker-local gateway, the REAL pi binary, and
// a model turn that COMPLETES over loopback.
//
// Each assertion is a distinct regression detector:
//
//  1. The gateway relayed to the upstream at least once for the bound model — so
//     the binding travelled worker → Spec.Endpoint → pi's HostGateway admission
//     → pi's registered provider → loopback surface → outbound leg. A break
//     anywhere in that chain leaves the upstream with zero requests.
//  2. The upstream saw the WORKER-held credential, and the harness's own bearer
//     never appears in the outbound request — the credential-isolation property
//     that makes a gateway cell worth having.
//  3. The turn COMPLETED: the runner consumed the assistant text (marker /
//     verdict) and reached a terminal — not spawn-failed, not budget-exceeded.
//  4. A wrapper around the REAL Pi executable observed that the real RPC child
//     received none of the worker-only upstream credential or route variables.
//  5. A cost-ledger row landed carrying the `harness` column and host=gateway
//     (08 §7/§10 item 5), which is what makes a gateway turn accountable.
func TestGatewaySmoke_WorkerLocalBinding_CompletesTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	realPi := afh.EnsurePiBinary(t)
	piBinDir := t.TempDir()
	piObservationPath := filepath.Join(t.TempDir(), "pi-env-observation")
	writeGatewayPiWrapper(t, realPi, piBinDir, piObservationPath)
	model := piModel()
	upstream := newGatewayUpstream(t, model)

	f := setupGatewayFixture(t, "completedturn", gatewayResolvedProfile(model), gatewayStageBudgetSeconds)
	f.upstreamBaseURL = upstream.baseURL()
	f.upstreamKey = gatewayUpstreamKey
	if f.nodeDir == "" {
		t.Skip("node not on PATH — pi is a node script and cannot exec; skipping gateway completed-turn check")
	}

	stdout, stderr, runErr := f.run(t, 6*time.Minute, piBinDir)

	if strings.Contains(stderr, "must be an absolute HTTP(S) URL with a loopback hostname") {
		t.Fatalf("pi's HostGateway admission REFUSED the worker-minted binding — the gateway listens on "+
			"127.0.0.1, so this is a regression in that admission boundary (donmai#216).\n--- stderr ---\n%s", stderr)
	}
	if strings.Contains(stderr, "gateway binding:") {
		t.Fatalf("the worker could not bind a gateway session for a gateway-served cell.\n--- stderr ---\n%s", stderr)
	}

	// (1) the gateway relayed the turn to the upstream.
	completions := upstream.completions()
	if len(completions) == 0 {
		t.Fatalf("the upstream served ZERO chat-completions requests — the worker-local gateway binding never "+
			"carried a turn.\nrecorded requests: %+v\nrunErr=%v\n--- stdout ---\n%s\n--- stderr ---\n%s",
			upstream.all(), runErr, stdout, stderr)
	}
	if got := completions[0].Model; got != model {
		t.Errorf("upstream saw model %q; want the bound model %q (the cell's model pin did not travel)", got, model)
	}

	// (2) credential isolation on the outbound leg.
	for i, c := range completions {
		if c.AuthHeader != "Bearer "+gatewayUpstreamKey {
			t.Errorf("upstream request %d Authorization = %q; want the worker-held upstream credential "+
				"(the gateway owns the outbound leg)", i, c.AuthHeader)
		}
		if strings.Contains(c.Body, gatewayUpstreamKey) {
			t.Errorf("upstream request %d leaked the credential into the request body", i)
		}
	}

	// (3) the turn completed.
	var res gatewayRunResult
	if err := afh.JSONUnmarshal(stdout, &res); err != nil {
		t.Fatalf("decode agent-run Result JSON: %v\n--- stdout ---\n%s\n--- stderr ---\n%s", err, stdout, stderr)
	}
	if res.FailureMode == "spawn-failed" || strings.Contains(res.Error, "policy extension failed to load") {
		t.Fatalf("the pi session never started (spawn/handshake gate) — no turn could have completed.\n"+
			"status=%q failureMode=%q error=%q\n--- stderr ---\n%s", res.Status, res.FailureMode, res.Error, stderr)
	}
	if res.FailureMode == "budget-exceeded" {
		t.Fatalf("the session died at the stage budget instead of completing the turn.\nstatus=%q error=%q\n"+
			"upstream requests: %d\n--- stderr ---\n%s", res.Status, res.Error, len(completions), stderr)
	}
	if !strings.Contains(stderr, gatewayTurnMarker) && res.WorkResult == "" {
		t.Errorf("the runner never surfaced the assistant text the gateway turn produced (marker %q absent and no "+
			"workResult verdict recorded) — the turn's output did not reach the runner.\nstatus=%q failureMode=%q error=%q",
			gatewayTurnMarker, res.Status, res.FailureMode, res.Error)
	}
	if res.Status != "completed" {
		t.Errorf("agent-run Result status = %q (failureMode=%q error=%q); want \"completed\" — the turn ran through "+
			"the gateway binding but the session did not reach a completed terminal", res.Status, res.FailureMode, res.Error)
	}

	// (4) the real pi RPC child inherits no worker-only gateway configuration.
	assertGatewayPiCredentialIsolation(t, piObservationPath)

	// (5) the turn is accounted for: a ledger row with the harness column.
	ledger := f.ledgerPath()
	events := readGatewayCostEvents(t, ledger)
	if len(events) == 0 {
		t.Fatalf("no gateway cost-ledger rows under state home %s (ledger=%q) — a completed gateway turn must be "+
			"metered (08 §7)", f.home, ledger)
	}
	var metered bool
	for _, ev := range events {
		if ev.SessionID != f.sessionID {
			continue
		}
		metered = true
		if ev.Host != "gateway" {
			t.Errorf("cost row host = %q; want \"gateway\"", ev.Host)
		}
		if ev.Harness == "" {
			t.Errorf("cost row carries no harness column (the D4 sibling column the gateway is the first producer of): %+v", ev)
		}
		if ev.Model != model {
			t.Errorf("cost row model = %q; want %q", ev.Model, model)
		}
		if ev.TokensIn == 0 && ev.TokensOut == 0 {
			t.Errorf("cost row reports no tokens: %+v", ev)
		}
	}
	if !metered {
		t.Errorf("no cost row for session %q; rows: %+v", f.sessionID, events)
	}

	t.Logf("gateway completed-turn lane: %d upstream relay(s); status=%q workResult=%q providerSessionId=%q pr=%q; "+
		"%d ledger row(s) at %s", len(completions), res.Status, res.WorkResult, res.ProviderSessionID,
		res.PullRequestURL, len(events), ledger)
}

// TestGatewaySmoke_UnconfiguredWorker_FailsClosed pins the no-silent-fallback
// posture: when the resolved cell is gateway-served but the worker has no
// upstream credential configured, `donmai agent run` must FAIL AT PREFLIGHT
// naming the knob — never quietly run the session on the harness's own default
// endpoint.
//
// This is the exact failure the producer was written to remove: before it, a
// gateway-served cell ran on pi's catalog default (provider
// azure-openai-responses, no base URL) and nothing anywhere said so. The lane is
// cheap (no turn, no model traffic) and deterministic.
func TestGatewaySmoke_UnconfiguredWorker_FailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end agent-run smoke; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of live-process smokes")
	}

	piBinDir := resolvePiBinDir(t)
	model := piModel()

	// An upstream is started only so the lane can assert it stayed untouched.
	upstream := newGatewayUpstream(t, model)

	f := setupGatewayFixture(t, "failclosed", gatewayResolvedProfile(model), gatewayFailClosedStageBudgetSeconds)
	f.upstreamBaseURL = upstream.baseURL()
	f.upstreamKey = "" // the unconfigured worker
	if f.nodeDir == "" {
		t.Skip("node not on PATH — pi is a node script and cannot exec; skipping gateway fail-closed check")
	}

	stdout, stderr, runErr := f.run(t, 3*time.Minute, piBinDir)

	if runErr == nil {
		t.Fatalf("an unconfigured worker ran a gateway-served session to success — the silent fallback is back.\n"+
			"--- stdout ---\n%s\n--- stderr ---\n%s", stdout, stderr)
	}
	combined := stdout + stderr
	if !strings.Contains(combined, gatewayEnvUpstreamKey) {
		t.Errorf("the failure must name the operator knob to set (%s) so the operator can act on it.\n"+
			"--- stdout ---\n%s\n--- stderr ---\n%s", gatewayEnvUpstreamKey, stdout, stderr)
	}
	if got := len(upstream.all()); got != 0 {
		t.Errorf("the upstream served %d request(s) during the fail-closed lane; want 0", got)
	}
	if got := len(readGatewayCostEvents(t, f.ledgerPath())); got != 0 {
		t.Errorf("a session that never ran emitted %d cost row(s); want 0", got)
	}
}
