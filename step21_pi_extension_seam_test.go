package smokes

// step21_pi_extension_seam_test.go — conformance smoke for the additional-
// extension delivery seam (donmai-architecture
// ADR-2026-08-12-pi-extension-delivery-seam-and-capability-pack-boundary.md
// D1-D5) and the embedder decorator hook that is the ONLY sanctioned way an
// embedding binary can populate it (donmai's agent/extension_delivery.go +
// agent/spec_decorator.go).
//
// # Why this lane compiles its own tiny embedder binary
//
// Spec.AdditionalExtensions is populated by agent-run orchestration's own
// unexported logic; there is no CLI flag for it — agent/spec_decorator.go's
// own doc comment says so explicitly. The ONE sanctioned way an embedding
// binary reaches it is agent.DecorateProvider /
// afcli.Config.AgentSpecExtensionDecorator, a Go-API-level hook, not a CLI
// surface. donmai-smokes otherwise never imports donmai as a Go library
// (AGENTS.md) and drives only the compiled `donmai` binary's CLI, so to
// exercise this hook against real compiled code — not donmai's own unit
// invariants — this lane builds a SECOND, tiny "embedder-shaped" binary from
// the SAME located donmai source checkout: an isolated Go module (its own
// go.mod, `replace`-pointed at the checkout by absolute path, built with
// `go build`) that imports afcli/agent exactly the way a real downstream
// embedder would, sets AgentSpecExtensionDecorator, and registers commands
// via afcli.RegisterCommands. It is compiled into a t.TempDir(),
// never checked in, and this repo's own module graph (go.mod, pinned to the
// released github.com/RenseiAI/donmai v0.54.0 per
// released/terminal_acknowledgement_test.go) is untouched — no replace, no
// go.work, no version bump.
//
// This mirrors, at one remove, exactly what harness.RequireDonmaiBinary
// already does for the `donmai` binary itself (locate source, build, exec,
// never import): the embedder binary is additional SUT scaffolding, not a
// departure from the black-box discipline.
//
// # What each check proves
//
//   - TestPiExtensionSeam_OfflineEnvPosture: PI_OFFLINE / PI_SKIP_VERSION_CHECK
//     land in the spawned pi child's env (D4.3). No embedder binary and no
//     real pi/node needed at all — a fake `pi` shim captures its own env.
//   - TestPiExtensionSeam_DecoratorSeam: (a) a tampered digest denies
//     pre-spawn with zero credential delivery (D1.2/D2(b)); (b) a
//     well-formed delivery is materialized byte-identical to what the
//     decorator supplied — the "embedder-shaped registration observed"
//     proof. Neither sub-check needs a real pi/node binary: materialization
//     and digest verification both happen in Go, before any child process
//     execs.
//   - TestPiExtensionSeam_RealPiBinaryConformance: gated on a real `pi` +
//     `node` (afh.EnsurePiBinary / nodeBinDir; DeclineLive cleanly when
//     either is absent — the sub-check this lane cannot run without them,
//     per donmai-smokes#30 this never lets the WHOLE suite report skip: the
//     two checks above always run). Drives one real session with an inline
//     delivery of donmai's own provider/harness/pi/testdata/conformance-
//     fixture.ts (read directly from the located checkout — never copied,
//     so this lane can never silently drift from the fixture donmai's own
//     real-binary tests exercise) and a workspace-planted copy of
//     workspace-discovery-canary.ts; asserts tool registration succeeded,
//     the fixture's own UI round-trip resolved as a prompt refusal rather
//     than hanging (D3), and the workspace-discovered canary never loaded
//     (D2) even with a seam delivery present in the same argv.
//
// # OSS boundary
//
// Same fixture shape as step20: an httptest daemon-control fixture serving
// only /api/daemon/sessions/<id>, no SaaS control plane. pi installs from
// the public npm registry via afh.EnsurePiBinary. No provider credentials
// are used. No platform tracker ids appear anywhere in this file.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// ── capability probe ────────────────────────────────────────────────────

// piExtensionSeamCapabilityFiles names the files whose presence in the
// located donmai checkout proves the additional-extension delivery seam
// (extension_delivery.go) AND its embedder decorator hook
// (spec_decorator.go) are both present. Probed by identity, exactly like
// step20 probes provider/harness/pi/probe.go — never by a version number —
// so a checkout predating both declines rather than fails.
var piExtensionSeamCapabilityFiles = []string{
	filepath.Join("agent", "extension_delivery.go"),
	filepath.Join("agent", "spec_decorator.go"),
}

// requirePiExtensionSeamSource locates the donmai checkout (FAILS if
// unlocatable or unbuildable, per AGENTS.md's iron rule) and DECLINES
// (skips, recorded via afh.DeclineLive) when it predates the seam.
func requirePiExtensionSeamSource(t *testing.T) string {
	t.Helper()
	srcDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	for _, rel := range piExtensionSeamCapabilityFiles {
		if _, err := os.Stat(filepath.Join(srcDir, rel)); err != nil {
			afh.DeclineLive(t, "donmai checkout at %q predates %s — "+
				"point %s at a checkout that has the additional-extension delivery seam",
				srcDir, rel, inFlightSourceDirEnv)
		}
	}
	return srcDir
}

// ── embedder harness scaffolding ────────────────────────────────────────

// embedderGoModTemplate is the isolated Go module the decorator-seam checks
// compile against. `replace` points at the ALREADY-LOCATED donmai checkout
// by absolute path — never a version string — so it always builds the exact
// commit requirePiExtensionSeamSource resolved, with zero risk of drifting
// to a stale pin (AGENTS.md warns against exactly that for feature
// worktrees). This module is never written into the checkout itself (a hard
// stop) and is never part of donmai-smokes' own module graph — it lives
// entirely under a t.TempDir(), built and discarded per test run.
const embedderGoModTemplate = `module donmai-smokes-pi-seam-embedder

go %s

require github.com/RenseiAI/donmai v0.0.0-00010101000000-000000000000

replace github.com/RenseiAI/donmai => %s
`

// embedderMainSource is an embedder-shaped Go program: it imports afcli +
// agent exactly the way a real downstream embedding binary does, registers
// the shared commands via afcli.RegisterCommands, and — when
// SMOKE_EXT_ID is set — wires a single agent.ExtensionDelivery through
// afcli.Config.AgentSpecExtensionDecorator, the sanctioned hook
// agent/spec_decorator.go names as the only way an embedder reaches
// Spec.AdditionalExtensions. One compiled binary is reused across every
// decorator-seam sub-check in this lane, parameterized per invocation by
// env rather than recompiled per case.
const embedderMainSource = `package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afcli"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
)

func buildDelivery() (agent.ExtensionDelivery, bool) {
	id := os.Getenv("SMOKE_EXT_ID")
	if id == "" {
		return agent.ExtensionDelivery{}, false
	}
	d := agent.ExtensionDelivery{
		ID:       id,
		Kind:     agent.ExtensionDeliveryKind(os.Getenv("SMOKE_EXT_KIND")),
		Digest:   os.Getenv("SMOKE_EXT_DIGEST"),
		Basename: os.Getenv("SMOKE_EXT_BASENAME"),
		Required: true,
	}
	sourceFile := os.Getenv("SMOKE_EXT_SOURCE_FILE")
	switch d.Kind {
	case agent.ExtensionDeliveryPath:
		d.Path = sourceFile
	case agent.ExtensionDeliveryInline:
		b, err := os.ReadFile(sourceFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "smoke-embedder: read inline source:", err)
			os.Exit(2)
		}
		d.Source = b
	}
	return d, true
}

func main() {
	root := &cobra.Command{Use: "smoke-embedder"}
	cfg := afcli.Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
		BinaryName:    "smoke-embedder",
	}
	if d, ok := buildDelivery(); ok {
		delivery := d
		cfg.AgentSpecExtensionDecorator = func(agent.Spec) []agent.ExtensionDelivery {
			return []agent.ExtensionDelivery{delivery}
		}
	}
	afcli.RegisterCommands(root, cfg)
	root.SetArgs(os.Args[1:])
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
`

// goDirectiveRe extracts the `go X.Y.Z` directive from a go.mod file.
var goDirectiveRe = regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`)

// sourceGoDirective reads the `go` directive out of sourceDir/go.mod so the
// generated embedder module declares a compatible version, never a
// hard-coded one — a module's go directive must be >= the max of its
// dependencies', the same rule this repo's own go.mod header documents.
func sourceGoDirective(t *testing.T, sourceDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sourceDir, "go.mod"))
	if err != nil {
		t.Fatalf("read %s/go.mod: %v", sourceDir, err)
	}
	m := goDirectiveRe.FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s/go.mod has no `go` directive", sourceDir)
	}
	return string(m[1])
}

// buildPiSeamEmbedderHarness compiles the embedder-shaped harness binary
// against the located donmai source. Every failure here is fatal — mirrors
// harness.RequireDonmaiBinary: a positively-located checkout that will not
// compile a program this lane's own scaffolding controls is an environment
// defect, not a precondition to skip.
func buildPiSeamEmbedderHarness(t *testing.T, sourceDir string) string {
	t.Helper()

	modDir := t.TempDir()
	goMod := fmt.Sprintf(embedderGoModTemplate, sourceGoDirective(t, sourceDir), sourceDir)
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write embedder harness go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.go"), []byte(embedderMainSource), 0o600); err != nil {
		t.Fatalf("write embedder harness main.go: %v", err)
	}

	env := append(os.Environ(), "GOWORK=off")

	tidyCtx, tidyCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer tidyCancel()
	// nolint:gosec // G204: fixed args, test-controlled working directory.
	tidyCmd := exec.CommandContext(tidyCtx, "go", "mod", "tidy")
	tidyCmd.Dir = modDir
	tidyCmd.Env = env
	if out, err := tidyCmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy for the pi-seam embedder harness (against donmai source %s): %v\n%s", sourceDir, err, out)
	}

	outPath := filepath.Join(t.TempDir(), "pi-seam-embedder")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()
	// nolint:gosec // G204: fixed args, test-controlled working directory.
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", outPath, ".")
	buildCmd.Dir = modDir
	buildCmd.Env = env
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build the pi-seam embedder harness (against donmai source %s): %v\n%s", sourceDir, err, out)
	}
	return outPath
}

// ── daemon fixture ──────────────────────────────────────────────────────

// piSeamFixture drives a binary (the plain donmai binary, or the compiled
// embedder harness) through `agent run` against a stub daemon-control HTTP
// fixture — same fixture shape as step20 (a single
// /api/daemon/sessions/<id> route, no SaaS control plane).
type piSeamFixture struct {
	binary     string
	daemonSrv  *httptest.Server
	sessionID  string
	wtParent   string
	home       string
	fakeBinDir string
}

func setupPiSeamFixture(t *testing.T, binary, testName, repository string) *piSeamFixture {
	t.Helper()

	sessionID := "smoke-pi-seam-" + testName
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/daemon/sessions/"+sessionID {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId":       sessionID,
				"issueIdentifier": "SMOKE-21",
				"title":           "pi extension seam smoke",
				"body":            "say hi",
				"workType":        "development",
				"repository":      repository,
				"ref":             "main",
				"branch":          "agent/" + sessionID,
				"workerId":        "smoke-worker",
				"authToken":       "smoke-token",
				"platformUrl":     srv.URL,
				"resolvedProfile": map[string]any{"provider": "pi", "model": piModel()},
				// Bounded stage budget: none of these runs are expected to
				// reach a natural terminal (no provider credential supplied),
				// same posture as step20's real-binary lanes.
				"stageBudget": map[string]any{"maxDurationSeconds": 8},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "refreshed": true})
	}))
	t.Cleanup(srv.Close)

	fakeBinDir := t.TempDir()
	writeBackstopGhShimStep21(t, fakeBinDir)

	return &piSeamFixture{
		binary:     binary,
		daemonSrv:  srv,
		sessionID:  sessionID,
		wtParent:   t.TempDir(),
		home:       t.TempDir(),
		fakeBinDir: fakeBinDir,
	}
}

// run drives `<binary> agent run` against the fixture. extraEnv is merged
// onto the child's env last (so it can, and is meant to, reach the pi
// grandchild too: composeChildEnv in donmai's pi provider starts from
// os.Environ() of the running `agent run` process, exactly the process this
// method execs). pathDirs are prepended ahead of node's dir (if present) and
// the system dirs, mirroring step20's pathEntries.
func (f *piSeamFixture) run(t *testing.T, ctx context.Context, extraEnv map[string]string, pathDirs ...string) (stdout, stderr string, runErr error) {
	t.Helper()

	// nolint:gosec // G204: binary + flags are test-controlled.
	cmd := exec.CommandContext(ctx, f.binary,
		"agent", "run",
		"--session-id", f.sessionID,
		"--daemon-url", f.daemonSrv.URL,
		"--worktree-dir", f.wtParent,
	)
	entries := append([]string{f.fakeBinDir}, pathDirs...)
	if nd := nodeBinDir(); nd != "" {
		entries = append(entries, nd)
	}
	entries = append(entries, "/usr/bin", "/bin", "/usr/sbin", "/sbin")

	env := []string{
		"PATH=" + strings.Join(entries, string(os.PathListSeparator)),
		"HOME=" + f.home,
		"XDG_CONFIG_HOME=" + filepath.Join(f.home, ".config"),
		"DONMAI_STATE_HOME=" + f.home,
		"NO_COLOR=1",
	}
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr = cmd.Run()
	return out.String(), errBuf.String(), runErr
}

// writeBackstopGhShimStep21 mirrors step17/18/19/20's own locally-named copy
// of the fake `gh` shim — steps intentionally don't share test-only helpers
// beyond the harness package, per existing convention.
func writeBackstopGhShimStep21(t *testing.T, dir string) {
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

// makeSeededBareFixtureRepoStep21 mirrors harness.MakeBareFixtureRepo but
// additionally commits files (relative path -> content) before the bare
// clone — used to plant the workspace-discovery canary at the exact
// auto-discovery location docs/extensions.md names, so it lands in the
// cloned workarea without this suite needing to guess or race the clone's
// dynamic checkout path.
func makeSeededBareFixtureRepoStep21(t *testing.T, name string, files map[string]string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=donmai-smokes",
		"GIT_AUTHOR_EMAIL=smokes@donmai.invalid",
		"GIT_COMMITTER_NAME=donmai-smokes",
		"GIT_COMMITTER_EMAIL=smokes@donmai.invalid",
	)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seeded fixture repo %s: git %v: %v\n%s", name, args, err, out)
		}
	}

	work := t.TempDir()
	run(work, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# fixture "+name+"\n"), 0o600); err != nil {
		t.Fatalf("seeded fixture repo %s: write README.md: %v", name, err)
	}
	for rel, content := range files {
		full := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("seeded fixture repo %s: mkdir for %s: %v", name, rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("seeded fixture repo %s: write %s: %v", name, rel, err)
		}
	}
	run(work, "add", ".")
	run(work, "commit", "-m", "seed "+name)

	bare := filepath.Join(t.TempDir(), name+".git")
	run(work, "clone", "--bare", work, bare)
	return bare
}

// ── small helpers ───────────────────────────────────────────────────────

func sha256HexStep21(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeTempFileStep21 writes content to a fresh temp file and returns its
// path.
func writeTempFileStep21(t *testing.T, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatalf("write temp file %s: %v", p, err)
	}
	return p
}

// findFileByNameStep21 walks root looking for a file whose base name equals
// name, returning its path or "" if not found.
func findFileByNameStep21(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" || d == nil {
			return nil
		}
		if !d.IsDir() && d.Name() == name {
			found = path
		}
		return nil
	})
	return found
}

// hasEnvLineStep21 reports whether env (an `env`-command dump) contains want
// as a whole line — never a substring match, so e.g. PI_OFFLINE=1 never
// false-matches SOME_OTHER_PI_OFFLINE=1.
func hasEnvLineStep21(env, want string) bool {
	for _, line := range strings.Split(env, "\n") {
		if strings.TrimRight(line, "\r") == want {
			return true
		}
	}
	return false
}

// writeEnvCapturePiShim writes a fake `pi` that answers --version (so
// provider construction's version-pin probe succeeds) and otherwise dumps
// its own environment to $DONMAI_SMOKES_ENV_CAPTURE and exits — never
// speaking real RPC. This proves D4.3's env posture (PI_OFFLINE=1,
// PI_SKIP_VERSION_CHECK=1 on every spawned pi child) without needing a real
// pi/node at all.
func writeEnvCapturePiShim(t *testing.T, dir string) {
	t.Helper()
	const script = `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "0.80.10"
  exit 0
fi
env > "$DONMAI_SMOKES_ENV_CAPTURE"
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(script), 0o755); err != nil { //nolint:gosec // executable shim needs the exec bit.
		t.Fatalf("write env-capture pi shim: %v", err)
	}
}

// ── checks ──────────────────────────────────────────────────────────────

// TestPiExtensionSeam_OfflineEnvPosture proves ADR-2026-08-12 D4.3: every
// pi child this execution layer spawns gets PI_OFFLINE=1 and
// PI_SKIP_VERSION_CHECK=1 by default. No real pi/node binary and no
// embedder harness are needed — a fake `pi` shim answers --version, then
// dumps its own env and exits before any real RPC would begin. This check
// always runs (no external-tool gate), so it — together with
// TestPiExtensionSeam_DecoratorSeam — keeps this suite from ever reporting
// an all-skip run per donmai-smokes#30, even on a machine with no pi/node.
func TestPiExtensionSeam_OfflineEnvPosture(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end agent-run smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	afh.SkipIfToolMissing(t, "git", "the bare fixture repo this smoke clones is created with git")

	sourceDir := requirePiExtensionSeamSource(t)
	binary, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: sourceDir})

	repository := afh.MakeBareFixtureRepo(t, "pi-seam-offline-env-repo")
	fixture := setupPiSeamFixture(t, binary, "offline-env", repository)

	shimDir := t.TempDir()
	writeEnvCapturePiShim(t, shimDir)
	envCapture := filepath.Join(t.TempDir(), "env-capture.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, stderr, _ := fixture.run(t, ctx, map[string]string{"DONMAI_SMOKES_ENV_CAPTURE": envCapture}, shimDir)

	captured, err := os.ReadFile(envCapture)
	if err != nil {
		t.Fatalf("fake pi shim never captured its env at %s (stderr:\n%s): %v", envCapture, stderr, err)
	}
	env := string(captured)
	if !hasEnvLineStep21(env, "PI_OFFLINE=1") {
		t.Errorf("PI_OFFLINE=1 not present in the spawned pi child's env (ADR-2026-08-12 D4.3):\n%s", env)
	}
	if !hasEnvLineStep21(env, "PI_SKIP_VERSION_CHECK=1") {
		t.Errorf("PI_SKIP_VERSION_CHECK=1 not present in the spawned pi child's env (ADR-2026-08-12 D4.3):\n%s", env)
	}
}

// TestPiExtensionSeam_DecoratorSeam proves the seam's fail-closed digest
// contract (D1.2/D2(b)) AND that the embedder decorator hook's registration
// is actually observed reaching materialization (agent/spec_decorator.go's
// "embedder-shaped registration" — afcli.Config.AgentSpecExtensionDecorator
// -> agent.DecorateProvider -> Spec.AdditionalExtensions). Neither
// sub-check needs a real pi/node binary: both properties are decided in Go,
// before any child process execs (materializeAdditionalExtensions runs
// ahead of spawnChild in donmai's pi provider). This always runs — see
// TestPiExtensionSeam_OfflineEnvPosture's doc comment on donmai-smokes#30.
func TestPiExtensionSeam_DecoratorSeam(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end agent-run smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	afh.SkipIfToolMissing(t, "git", "the bare fixture repo this smoke clones is created with git")

	sourceDir := requirePiExtensionSeamSource(t)
	embedder := buildPiSeamEmbedderHarness(t, sourceDir)

	t.Run("DigestMismatchFailsClosedPreSpawn", func(t *testing.T) {
		repository := afh.MakeBareFixtureRepo(t, "pi-seam-digest-mismatch-repo")
		fixture := setupPiSeamFixture(t, embedder, "digest-mismatch", repository)

		claimed := []byte("claimed bytes the decorator never actually delivers")
		actual := []byte("actual on-disk bytes differ from the claimed digest")
		sourceFile := writeTempFileStep21(t, "tampered.ts", actual)

		extraEnv := map[string]string{
			"SMOKE_EXT_ID":          "smoke-digest-mismatch",
			"SMOKE_EXT_KIND":        "inline",
			"SMOKE_EXT_BASENAME":    "tampered.ts",
			"SMOKE_EXT_SOURCE_FILE": sourceFile,
			"SMOKE_EXT_DIGEST":      sha256HexStep21(claimed), // digest of bytes never written
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		stdout, stderr, runErr := fixture.run(t, ctx, extraEnv)
		if runErr == nil {
			t.Fatalf("agent run succeeded despite a tampered digest delivery; want a fail-closed pre-spawn denial.\n"+
				"--- stdout ---\n%s\n--- stderr ---\n%s", stdout, stderr)
		}
		combined := stdout + stderr
		if !strings.Contains(combined, "smoke-digest-mismatch") || !strings.Contains(combined, "digest mismatch") {
			t.Fatalf("run failed, but not with the expected digest-mismatch denial naming the delivery id — "+
				"want both %q and %q in the output (D1.2/D2(b) fail-closed pre-spawn).\n"+
				"--- stdout ---\n%s\n--- stderr ---\n%s",
				"smoke-digest-mismatch", "digest mismatch", stdout, stderr)
		}
		t.Logf("digest-mismatch delivery denied pre-spawn as expected")
	})

	t.Run("MaterializedArtifactProvesDecoratorPassthrough", func(t *testing.T) {
		repository := afh.MakeBareFixtureRepo(t, "pi-seam-decorator-passthrough-repo")
		fixture := setupPiSeamFixture(t, embedder, "decorator-passthrough", repository)

		body := []byte("// smoke-decorator-passthrough marker\nexport default function activate(pi) {}\n")
		sourceFile := writeTempFileStep21(t, "passthrough.ts", body)

		extraEnv := map[string]string{
			"SMOKE_EXT_ID":          "smoke-decorator-passthrough",
			"SMOKE_EXT_KIND":        "inline",
			"SMOKE_EXT_BASENAME":    "passthrough.ts",
			"SMOKE_EXT_SOURCE_FILE": sourceFile,
			"SMOKE_EXT_DIGEST":      sha256HexStep21(body),
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// The run itself is expected to fail or time out later (no real pi,
		// no model credential) — irrelevant here. What matters is whether
		// materializeAdditionalExtensions ran at all, which only happens if
		// the embedder's AgentSpecExtensionDecorator successfully appended
		// this delivery onto Spec.AdditionalExtensions before Spawn.
		stdout, stderr, _ := fixture.run(t, ctx, extraEnv)

		// donmai's sanitizeInjectedBasename names the materialized file
		// "<id>-<basename>" under <cwd>/.pi/extensions-injected/.
		wantName := "smoke-decorator-passthrough-passthrough.ts"
		materialized := findFileByNameStep21(t, fixture.wtParent, wantName)
		if materialized == "" {
			t.Fatalf("no file named %q was materialized under the session workarea (%s) — the embedder-shaped "+
				"AgentSpecExtensionDecorator registration was never observed reaching the seam.\n"+
				"--- stdout ---\n%s\n--- stderr ---\n%s", wantName, fixture.wtParent, stdout, stderr)
		}
		got, err := os.ReadFile(materialized)
		if err != nil {
			t.Fatalf("read materialized delivery at %s: %v", materialized, err)
		}
		if string(got) != string(body) {
			t.Errorf("materialized artifact at %s does not match the decorator-delivered Source:\ngot:  %q\nwant: %q",
				materialized, got, body)
		}
		t.Logf("embedder-shaped decorator registration observed: %s materialized byte-identical to the delivered Source", materialized)
	})
}

// TestPiExtensionSeam_RealPiBinaryConformance is gated on a real pi + node
// (DeclineLive cleanly when either is absent — this is the ONE sub-check
// this lane cannot run without them; TestPiExtensionSeam_OfflineEnvPosture
// and TestPiExtensionSeam_DecoratorSeam above always run regardless, so a
// machine with no pi/node still exercises this suite per donmai-smokes#30).
// Drives one real session delivering donmai's own conformance-fixture.ts
// inline through the seam, with a workspace-planted copy of
// workspace-discovery-canary.ts, and asserts three properties from that one
// session:
//
//   - InlineExtensionDeliveryRegistersATool: the delivered extension's
//     session_start handler ran (pi.registerTool() did not throw).
//   - HeadlessUIRefusesPromptlyInsteadOfHanging: the fixture's own UI
//     round-trip (which the runner has no reason to recognize) resolved
//     promptly as a refusal (D3), never hanging.
//   - WorkspaceDiscoveryStaysDisabledUnderDelivery: the workspace-resident
//     canary never loaded, even with a seam delivery present in the same
//     argv (D2's "disabled, not merely gated" half).
func TestPiExtensionSeam_RealPiBinaryConformance(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end agent-run smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	afh.SkipIfToolMissing(t, "git", "the bare fixture repo this smoke clones is created with git")

	sourceDir := requirePiExtensionSeamSource(t)
	embedder := buildPiSeamEmbedderHarness(t, sourceDir)

	if nodeBinDir() == "" {
		afh.DeclineLive(t, "node not on PATH — pi is a node script and cannot exec, "+
			"so the real-pi extension-seam conformance checks have nothing to drive")
	}
	piBinDir := resolvePiBinDir(t)

	fixtureSource, err := os.ReadFile(filepath.Join(sourceDir, "provider", "harness", "pi", "testdata", "conformance-fixture.ts"))
	if err != nil {
		t.Fatalf("read donmai's own conformance-fixture.ts from the located checkout: %v", err)
	}
	canarySource, err := os.ReadFile(filepath.Join(sourceDir, "provider", "harness", "pi", "testdata", "workspace-discovery-canary.ts"))
	if err != nil {
		t.Fatalf("read donmai's own workspace-discovery-canary.ts from the located checkout: %v", err)
	}

	markerPath := filepath.Join(t.TempDir(), "fixture-marker.json")
	canaryMarkerPath := filepath.Join(t.TempDir(), "canary-marker.json")

	// The canary is committed into the fixture repo at the exact
	// auto-discovery location (docs/extensions.md: <cwd>/.pi/extensions/) so
	// it lands in the cloned workarea for free — no need to guess or race
	// the clone's dynamic checkout path.
	repository := makeSeededBareFixtureRepoStep21(t, "pi-seam-real-binary-repo", map[string]string{
		filepath.Join(".pi", "extensions", "canary.ts"): string(canarySource),
	})
	fixture := setupPiSeamFixture(t, embedder, "real-binary", repository)

	extraEnv := map[string]string{
		"SMOKE_EXT_ID":          "smoke-conformance-fixture",
		"SMOKE_EXT_KIND":        "inline",
		"SMOKE_EXT_BASENAME":    "conformance-fixture-inline.ts",
		"SMOKE_EXT_SOURCE_FILE": writeTempFileStep21(t, "conformance-fixture.ts", fixtureSource),
		"SMOKE_EXT_DIGEST":      sha256HexStep21(fixtureSource),
		"DONMAI_FIXTURE_MARKER": markerPath,
		"DONMAI_CANARY_MARKER":  canaryMarkerPath,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stdout, stderr, _ := fixture.run(t, ctx, extraEnv, piBinDir)
	// The run is bounded by the daemon fixture's short stageBudget and is not
	// expected to reach a natural terminal (no model credential supplied,
	// same posture as step20's real-binary lanes) — runErr is deliberately
	// ignored; only the marker files below matter.

	if detail := afh.ExplainAdaptationDenial(stdout + stderr); detail != "" {
		t.Fatalf("the session was denied before spawn, so the real pi binary was never reached.\n\n%s\n"+
			"--- stdout ---\n%s\n--- stderr ---\n%s", detail, stdout, stderr)
	}

	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("conformance-fixture.ts's marker was never written at %s — the delivered extension's "+
			"session_start handler never ran.\n--- stdout ---\n%s\n--- stderr ---\n%s", markerPath, stdout, stderr)
	}
	var marker struct {
		Loaded             bool   `json:"loaded"`
		HasUI              *bool  `json:"hasUI"`
		UIRoundTripSettled bool   `json:"uiRoundTripSettled"`
		UIRoundTripMs      int64  `json:"uiRoundTripMs"`
		UIReply            any    `json:"uiReply"`
		UIError            string `json:"uiError"`
	}
	if err := json.Unmarshal(markerBytes, &marker); err != nil {
		t.Fatalf("unmarshal fixture marker: %v (raw: %s)", err, markerBytes)
	}

	t.Run("InlineExtensionDeliveryRegistersATool", func(t *testing.T) {
		if !marker.Loaded {
			t.Fatal("fixture marker reports loaded=false — the inline-delivered extension's session_start " +
				"handler never ran, meaning pi.registerTool() for donmai_fixture_tool either threw or the " +
				"extension never loaded through the seam")
		}
	})

	t.Run("HeadlessUIRefusesPromptlyInsteadOfHanging", func(t *testing.T) {
		if marker.HasUI == nil || !*marker.HasUI {
			t.Fatalf("fixture marker hasUI = %v; want true in the headless RPC lane (pi's own documented "+
				"ctx.hasUI behavior)", marker.HasUI)
		}
		if !marker.UIRoundTripSettled {
			t.Fatal("the fixture's UI round-trip never settled — this is exactly the hang D3 forbids: a UI " +
				"call from an extension the runner does not recognize must resolve promptly as a refusal, never hang")
		}
		if marker.UIRoundTripMs > 10000 {
			t.Errorf("UI round-trip took %dms to settle; want a prompt cancellation, not something "+
				"indistinguishable from a hang", marker.UIRoundTripMs)
		}
		if marker.UIError == "" && marker.UIReply != nil {
			t.Errorf("UI round-trip resolved to a non-null reply (%v) with no error — an unrecognized "+
				"extension's round-trip must come back refused, never as if answered", marker.UIReply)
		}
	})

	t.Run("WorkspaceDiscoveryStaysDisabledUnderDelivery", func(t *testing.T) {
		if _, err := os.Stat(canaryMarkerPath); err == nil {
			t.Fatal("the workspace-discovered canary extension LOADED even with a seam delivery present in " +
				"the same argv — --no-extensions failed to disable auto-discovery (D2's 'disabled, not merely gated' half)")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat canary marker: %v", err)
		}
	})
}
