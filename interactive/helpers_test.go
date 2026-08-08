// Package interactive holds the W14 platform-free smoke coverage for the OSS
// interactive-session substrate shipped in github.com/RenseiAI/donmai:
//
//   - ptyhost: standalone PTY-session lifecycle (Spawn → drive → Snapshot →
//     Stop → Exit).
//   - attachclient/attachtest + attachclient/viewertest: local attach
//     (host leg → stub relay → viewer) with a headless VT screen-assert loop
//     against the deterministic `vtfixture` TUI (the CRITICAL-1 harness).
//   - ptyhost/testdata: vim/tmux recorded fixtures replayed through the public
//     Session for snapshot correctness (name+description+cmd fixtures).
//   - attachwire + attachwire/sanitize fuzz-regression corpora fed through the
//     public decode/sanitize surface (panic-free invariant).
//
// PLATFORM-FREE BY CONTRACT (../AGENTS.md §Boundary): nothing here touches the
// SaaS control plane — no WorkOS or Linear calls, no platform auth or worker
// HTTP namespaces, no service-key or platform test-token credentials.
// boundary_test.go enforces that on this package's own source.
//
// TEMPORARY DEPENDENCY: these smokes build against the unreleased viewertest
// harness via the absolute-path `replace` in ../go.mod — see that directive's
// comment. They compile only where the donmai worktree the replace points at is
// present; the helpers below t.Skip cleanly when the donmai module or the Go
// toolchain cannot be resolved.
package interactive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
)

// donmaiModuleDir resolves the on-disk root of the github.com/RenseiAI/donmai
// module, honoring the ../go.mod replace directive (so testdata is read from
// exactly the source the code under test was built from). It skips the calling
// test when the Go toolchain or the module cannot be resolved — the same
// "no sibling / no toolchain => skip" contract the rest of the harness follows.
func donmaiModuleDir(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("skipping: Go toolchain not found on PATH: %v", err)
	}
	cmd := exec.Command(goBin, "list", "-m", "-f", "{{.Dir}}", "github.com/RenseiAI/donmai")
	// GOWORK=off keeps resolution decoupled from the org go.work (which resolves
	// donmai to the main checkout that lacks viewertest); the replace in go.mod
	// is what points at the worktree that has it.
	cmd.Env = append(os.Environ(), "GOWORK=off")
	// These three used to skip. They cannot legitimately: this file IMPORTS
	// donmai packages, so the test binary only exists because the module
	// already resolved and compiled. A `go list -m` that then fails, or
	// returns a path that is not on disk, is a broken toolchain or a
	// corrupted module cache — an error. Skipping it reported `ok` for a
	// suite that never read a single fixture.
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve donmai module dir (the module compiled, so this should not fail): %v\n%s", err, out)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatal("donmai module dir resolved empty (the module compiled, so this should not happen)")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("donmai module dir %q is not on disk — corrupted module cache?: %v", dir, err)
	}
	return dir
}

// discardLogger silences ptyhost/attachclient diagnostics in the smoke output.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// buildFixture compiles the deterministic vtfixture TUI from the donmai module
// and returns the binary path. GOWORK=off + the go.mod replace resolve it to the
// viewertest worktree.
func buildFixture(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("skipping: Go toolchain not found on PATH: %v", err)
	}
	// Locate the package before building it, so the two cases stop looking
	// alike. "the pinned donmai has no viewertest harness" is a precondition;
	// "the viewertest harness is there and does not compile" is a failure.
	// One t.Skipf used to cover both, and its message ("viewertest harness
	// unavailable?" — with the question mark) records that even its author
	// could not tell which had happened.
	const fixturePkg = "github.com/RenseiAI/donmai/attachclient/viewertest/fixturetui"
	pkgDir := filepath.Join(donmaiModuleDir(t), "attachclient", "viewertest", "fixturetui")
	if _, err := os.Stat(pkgDir); err != nil {
		t.Skipf("pinned donmai has no viewertest fixture package at %s — nothing to drive", pkgDir)
	}

	bin := filepath.Join(t.TempDir(), "vtfixture")
	cmd := exec.Command(goBin, "build", "-o", bin, fixturePkg) //nolint:gosec // G204: fixed in-repo package path built with the toolchain — no untrusted input
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build vtfixture from %s: %v\n%s", pkgDir, err, out)
	}
	return bin
}

// sessAdapter bridges *ptyhost.Session to attachclient.Session — the ~5-line
// adapter documented in the viewertest e2e template (only Subscribe's return
// type differs).
type sessAdapter struct{ *ptyhost.Session }

func (a sessAdapter) Subscribe(from attachwire.HostSeq) (attachclient.Subscription, error) {
	sub, err := a.Session.Subscribe(from)
	return sub, err
}

// waitBound blocks until the stub relay reports the host leg has bound.
func waitBound(t *testing.T, relay *attachtest.StubRelay) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if relay.HostBound() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for host leg to bind")
}

// startHostLeg runs attachclient.RunHost against the relay for the given session
// and registers cleanup that cancels it and waits for a clean return.
func startHostLeg(t *testing.T, ctx context.Context, relay *attachtest.StubRelay, sess *ptyhost.Session, sessionID string) {
	t.Helper()
	hostTok := mkHostToken(sessionID, 1, "host-jti")
	done := make(chan error, 1)
	hctx, cancel := context.WithCancel(ctx)
	go func() {
		done <- attachclient.RunHost(hctx, attachclient.HostConfig{
			AttachURL:         relay.BaseWSURL(),
			TokenSource:       func(context.Context) (string, error) { return hostTok, nil },
			Session:           sessAdapter{sess},
			BackoffMin:        5 * time.Millisecond,
			BackoffMax:        50 * time.Millisecond,
			FinalScreenWindow: 300 * time.Millisecond,
			Logger:            discardLogger(),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("RunHost did not return after cancel")
		}
	})
	waitBound(t, relay)
}

// ---- unsigned test tokens (the stub relay checks aud + epoch presence only) --

func mkHostToken(sessionID string, epoch int64, jti string) string {
	return fakeJWT(map[string]any{
		"sessionId": sessionID, "roomId": sessionID, "role": "host",
		"orgId": "org-1", "aud": "relay", "jti": jti, "epoch": epoch,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func mkViewerToken(sessionID, userID, jti, role string) string {
	return fakeJWT(map[string]any{
		"sessionId": sessionID, "roomId": sessionID, "userId": userID, "role": role,
		"orgId": "org-1", "aud": "relay", "jti": jti,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func fakeJWT(claims map[string]any) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	pb, _ := json.Marshal(claims)
	return strings.Join([]string{
		hdr,
		base64.RawURLEncoding.EncodeToString(pb),
		base64.RawURLEncoding.EncodeToString([]byte("sig")),
	}, ".")
}

// ---- recorded-fixture sidecar (subset of ptyhost/testdata/*.json) -----------

type fixtureMeta struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Cmd         []string `json:"cmd"`
	EnvVars     []string `json:"env"`
	Cols        int      `json:"cols"`
	Rows        int      `json:"rows"`
	Checkpoints []struct {
		Label  string `json:"label"`
		Offset int    `json:"offset"`
	} `json:"checkpoints"`
	Reference *struct {
		Panes []struct {
			ID       string `json:"id"`
			Left     int    `json:"left"`
			Top      int    `json:"top"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Active   bool   `json:"active"`
			CursorX  int    `json:"cursor_x"`
			CursorY  int    `json:"cursor_y"`
			CaptureE string `json:"capture_e"`
		} `json:"panes"`
	} `json:"reference,omitempty"`
}

func (m fixtureMeta) offset(label string) (int, bool) {
	for _, c := range m.Checkpoints {
		if c.Label == label {
			return c.Offset, true
		}
	}
	return 0, false
}

// loadRecordedFixture reads the .raw byte stream and its sidecar .json from the
// donmai module's ptyhost/testdata directory.
func loadRecordedFixture(t *testing.T, moduleDir, name string) ([]byte, fixtureMeta) {
	t.Helper()
	base := filepath.Join(moduleDir, "ptyhost", "testdata")
	// Fatal, not skip: these fixtures ship INSIDE the pinned donmai module,
	// so their absence means the pin moved out from under this suite. That
	// is exactly the regression these tests exist to catch, and skipping it
	// turned the catch into a silent `ok`.
	raw, err := os.ReadFile(filepath.Join(base, name+".raw"))
	if err != nil {
		t.Fatalf("recorded fixture %s.raw missing from the pinned donmai module at %s: %v", name, base, err)
	}
	metaBytes, err := os.ReadFile(filepath.Join(base, name+".json"))
	if err != nil {
		t.Fatalf("recorded fixture %s.json missing from the pinned donmai module at %s: %v", name, base, err)
	}
	var m fixtureMeta
	if err := json.Unmarshal(metaBytes, &m); err != nil {
		t.Fatalf("parse %s.json: %v", name, err)
	}
	return raw, m
}

// ---- go fuzz corpus parsing -------------------------------------------------

// parseGoFuzzCorpus extracts the single []byte argument from a Go corpus file
// ("go test fuzz v1\n[]byte(\"...\")\n"). It returns ok=false for a corpus entry
// whose argument is not a []byte (none of the corpora used here are), so a
// caller skips it rather than misreading it.
func parseGoFuzzCorpus(data []byte) (payload []byte, ok bool) {
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "go test fuzz v1" {
		return nil, false
	}
	arg := strings.TrimSpace(lines[1])
	const pfx, sfx = "[]byte(", ")"
	if !strings.HasPrefix(arg, pfx) || !strings.HasSuffix(arg, sfx) {
		return nil, false
	}
	quoted := strings.TrimSuffix(strings.TrimPrefix(arg, pfx), sfx)
	unq, err := strconv.Unquote(quoted)
	if err != nil {
		return nil, false
	}
	return []byte(unq), true
}

// readCorpusDir reads and parses every []byte corpus file under dir. Missing dir
// skips the caller.
func readCorpusDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	// Fatal, not skip: like the recorded fixtures, the corpus ships inside
	// the pinned donmai module. An unreadable corpus dir means the pin moved,
	// not that this machine is unsuitable.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("fuzz corpus dir %q missing from the pinned donmai module: %v", dir, err)
	}
	out := make(map[string][]byte)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read corpus %s: %v", e.Name(), err)
		}
		if payload, ok := parseGoFuzzCorpus(b); ok {
			out[e.Name()] = payload
		}
	}
	if len(out) == 0 {
		t.Fatalf("fuzz corpus dir %q holds no []byte entries — nothing to replay, so a green run here would mean nothing", dir)
	}
	return out
}
