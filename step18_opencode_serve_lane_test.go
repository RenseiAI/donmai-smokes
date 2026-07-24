package smokes

// step18_opencode_serve_lane_test.go — opencode Lane-B (serve/HTTP adapter)
// serve-lifecycle smoke, the Lane-B companion to
// step18_opencode_harness_test.go's Lane-A coverage
// (runs/2026-07-21-open-harness-strategy/07-design-opencode-spawn.md §2, §6;
// 12-work-breakdown.md's W2a completion note recorded this as the concrete
// follow-up once donmai threads a preferServer signal from the resolved
// profile into the agent-run registry — afcli/agent_run.go, merged as the
// donmai PR immediately preceding this one).
//
// # Why this smoke needs no completed model turn
//
// Lane B's opencode.json injection (donmai's provider/harness/opencode/
// config.go) always declares a provider+model pair that matches whatever
// model string the resolved profile carries — CreateSession and the config
// are self-consistent by construction, so session creation succeeds
// regardless of whether that model string names anything real. This smoke
// deliberately uses a bogus model (mirroring step18's Lane-A
// bogusModelResolvedProfile) for the same reason step18 does: it proves the
// real spawn -> readiness -> session-create -> event pipeline without a
// live model backend or network dependency. What differs from Lane A is
// which event proves the pipeline worked: instead of waiting for a terminal
// Result, this smoke polls the session's on-disk state.json (written by
// donmai's runner.observeEvent on agent.InitEvent — runtime/state package,
// JSON key providerSessionId) until it's populated, THEN drives teardown by
// sending SIGINT — exactly like step18's
// TestOpenCodeHarnessSmoke_RealBinary_Teardown, just triggered off a
// positive readiness signal instead of a fixed sleep. This is deliberately
// decoupled from whatever the eventual (bogus-model) prompt admission does
// next — it may fail fast over the network, hang, or never resolve; none of
// that is this smoke's concern, and the SIGINT lands regardless.
//
// Readiness and spawn are proven transitively: Provider.Spawn's Lane-B path
// (opencode.go spawnServer) writes the per-session opencode.json, spawns
// `opencode serve`, and blocks on GET /api/health before ever creating a
// session (server.go waitHealthy) — a construction/readiness failure would
// return an error before any state.json write ever happens, and CI has no
// way to observe a false positive here: this smoke asserts the write DID
// happen.
//
// # OSS boundary
//
// Same fixture shape as step18_opencode_harness_test.go: an httptest
// daemon-control fixture serving only /api/daemon/sessions/<id>, no SaaS
// control plane. opencode-ai installs from the public npm registry via
// harness.EnsureOpenCodeBinary (shared with the Lane-A lane).

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// serveLaneStatePollInterval is how often this smoke polls state.json for
// the InitEvent-populated providerSessionId. Short enough to keep the smoke
// fast; the poll itself is a cheap local file stat + read.
const serveLaneStatePollInterval = 200 * time.Millisecond

// serveLaneResolvedProfile is bogusModelResolvedProfile (step18's Lane-A
// fixture) plus the opencode.preferServer providerConfig hint
// (afcli/agent_run.go's opencodeCtorHintKey) that selects Lane B for this
// session's opencode provider registration.
func serveLaneResolvedProfile() map[string]any {
	base := bogusModelResolvedProfile()
	base["providerConfig"] = map[string]any{
		"opencode.preferServer": true,
	}
	return base
}

// stateJSONDoc is the minimal subset of runtime/state.State this smoke reads
// (donmai-smokes never imports donmai as a Go library, per AGENTS.md — this
// is an independent, deliberately-narrow mirror of the on-disk JSON shape,
// same convention as harness/opencode_install.go keeping its own pinned-
// version copy independent of donmai's).
type stateJSONDoc struct {
	ProviderSessionID string `json:"providerSessionId"`
	CurrentStep       string `json:"currentStep"`
	ProviderName      string `json:"providerName"`
}

// readSessionStateJSON reads and parses <wtParent>/<sessionID>/.agent/state.json
// — the default worktree leaf name is the session id itself (runtime/
// worktree.Manager.Provision falls back to spec.SessionID when LeafName is
// unset, which every production call site leaves unset). Returns
// (doc, true) on success; (zero, false) when the file doesn't exist yet or
// fails to parse (both treated as "not ready", not an error — the runner
// may not have created the worktree/.agent dir at all yet at the start of
// the poll window).
func readSessionStateJSON(wtParent, sessionID string) (stateJSONDoc, bool) {
	path := filepath.Join(wtParent, sessionID, ".agent", "state.json")
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is built from test-controlled t.TempDir() + a literal session id this file owns.
	if err != nil {
		return stateJSONDoc{}, false
	}
	var doc stateJSONDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return stateJSONDoc{}, false
	}
	return doc, true
}

// waitForProviderSessionID polls readSessionStateJSON until
// ProviderSessionID is non-empty, the process exits (procDone closes), or
// ctx is done. Returns the last-observed doc and which condition ended the
// wait.
func waitForProviderSessionID(ctx context.Context, wtParent, sessionID string, procDone <-chan struct{}) (doc stateJSONDoc, sawInit bool, procExited bool) {
	ticker := time.NewTicker(serveLaneStatePollInterval)
	defer ticker.Stop()
	for {
		if d, ok := readSessionStateJSON(wtParent, sessionID); ok && d.ProviderSessionID != "" {
			return d, true, false
		}
		select {
		case <-procDone:
			// One last read in case the write raced the process exit.
			d, _ := readSessionStateJSON(wtParent, sessionID)
			return d, d.ProviderSessionID != "", true
		case <-ctx.Done():
			d, _ := readSessionStateJSON(wtParent, sessionID)
			return d, d.ProviderSessionID != "", false
		case <-ticker.C:
		}
	}
}

// TestOpenCodeHarnessSmoke_ServeLane_SpawnReadySessionInitTeardown drives the
// REAL CI-installed pinned opencode binary through Lane B end-to-end without
// a completed model turn: spawn (`opencode serve`) -> readiness
// (GET /api/health, internal to Spawn) -> session create -> InitEvent
// (observed via state.json) -> deliberate SIGINT teardown -> no orphan
// `opencode serve` process.
func TestOpenCodeHarnessSmoke_ServeLane_SpawnReadySessionInitTeardown(t *testing.T) {
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

	// A real `opencode serve` child needs materially more headroom than
	// Lane A's one-shot CLI failure: process boot + GET /api/health polling
	// (server.go's own internal readiness budget is 20s) plus session
	// creation. 45s comfortably covers that without approaching this test's
	// own 90s overall wait bound.
	const serveLaneStageBudgetSeconds = 45
	f := setupOpenCodeHarnessFixture(t, "servelane", serveLaneResolvedProfile(), serveLaneStageBudgetSeconds)

	pathEntries := append([]string{f.fakeBinDir, binDirForPath}, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	cmd := exec.Command(
		f.donmaiBinary, //nolint:gosec // binary + flags are test-controlled.
		"--debug", "agent", "run",
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
	stderrFile, ferr := os.CreateTemp(t.TempDir(), "stderr-*.log")
	if ferr != nil {
		t.Fatalf("create stderr temp file: %v", ferr)
	}
	cmd.Stderr = stderrFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start donmai agent run: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	procDone := make(chan struct{})
	go func() {
		<-done
		close(procDone)
	}()

	// Bounded overall wait: cold npm-installed opencode + a real `opencode
	// serve` spawn + health poll (server.go's own internal budget is 20s)
	// needs headroom beyond that; 90s leaves ample margin without letting a
	// genuinely hung smoke run indefinitely.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer waitCancel()
	doc, sawInit, procExited := waitForProviderSessionID(waitCtx, f.wtParent, f.sessionID, procDone)

	if !procExited {
		// The session is live and past InitEvent (or the wait window lapsed
		// without one) — drive the deliberate mid-run teardown. 45s (vs
		// step18 Lane-A Teardown's 30s) gives the extra child-process
		// (opencode serve, not just opencode run) margin: SIGTERM ->
		// stopGracePeriod (5s) -> SIGKILL, plus the SSE-subscription and
		// abort round trips ahead of it — all internally bounded, but with
		// real subprocess scheduling on a loaded CI runner, extra headroom
		// costs nothing on the happy path and avoids a flaky false-hang
		// verdict under transient scheduling pressure.
		sigT0 := time.Now()
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatalf("signal donmai agent run: %v", err)
		}
		select {
		case <-procDone:
			t.Logf("donmai agent run exited %s after SIGINT (mid-run teardown)", time.Since(sigT0))
		case <-time.After(45 * time.Second):
			// Diagnostic-before-kill: SIGABRT triggers the Go runtime's
			// default crash handler, which dumps every goroutine's stack to
			// stderr before the process dies — actionable evidence in CI
			// logs instead of an opaque "it hung" verdict. Best-effort: if
			// the process ignores/mishandles it, the Kill() below still
			// guarantees teardown of this test. procDone (not done) is safe
			// to select on repeatedly: it's a channel this test closes once
			// after done fires, whereas done itself is a single-value
			// channel that would deadlock a second receive.
			_ = cmd.Process.Signal(syscall.SIGABRT)
			select {
			case <-procDone:
			case <-time.After(5 * time.Second):
			}
			_ = cmd.Process.Kill()
			<-procDone
			dumpBytes, _ := os.ReadFile(stderrFile.Name())
			t.Fatalf("donmai agent run did not exit within 45s of SIGINT (mid-run teardown hung); elapsed=%s\n--- stderr (incl. SIGABRT goroutine dump if captured) ---\n%s",
				time.Since(sigT0), dumpBytes)
		}
		// Re-read post-exit in case the write raced the signal.
		if !sawInit {
			if d, ok := readSessionStateJSON(f.wtParent, f.sessionID); ok && d.ProviderSessionID != "" {
				doc, sawInit = d, true
			}
		}
	}

	stderrBytes, _ := os.ReadFile(stderrFile.Name())
	stderr := string(stderrBytes)
	if strings.Contains(stderr, "below the minimum supported version") {
		t.Fatalf("unexpected version-pin rejection of the real pinned binary; stderr:\n%s", stderr)
	}

	if !sawInit {
		t.Fatalf("state.json never showed a populated providerSessionId within the wait window "+
			"(spawn -> readiness -> session-create -> InitEvent did not complete)\n--- stderr ---\n%s", stderr)
	}
	if doc.ProviderName != "" && doc.ProviderName != "opencode" {
		t.Errorf("state.json providerName = %q; want \"opencode\"", doc.ProviderName)
	}
	t.Logf("observed InitEvent: providerSessionId=%q currentStep=%q", doc.ProviderSessionID, doc.CurrentStep)

	// No orphan `opencode serve` process left behind after teardown.
	if _, err := exec.LookPath("pgrep"); err == nil {
		out, _ := exec.Command("pgrep", "-f", "opencode serve").CombinedOutput() //nolint:gosec // fixed args.
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("orphan opencode-serve process(es) left running after teardown:\n%s", out)
		}
	}
}
