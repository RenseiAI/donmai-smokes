package smokes

// step16_arch_assess_native_test.go — end-to-end smoke for `donmai arch assess`,
// the native Go arch-intelligence Layer-1 (diff-only) pipeline.
//
// This exercises the OSS-shipped arch-intel surface that replaced the legacy
// `af-arch` TypeScript shim. The OSS binary ships ONLY the Layer-1
// `native-diff-only` path — the LLM/baseline Layer-2 lane is platform-owned and
// no longer shipped in OSS. The smoke builds the donmai binary, runs `donmai
// arch assess` over a FIXTURE PR diff, and asserts the two load-bearing
// behaviours of the native diff-only path:
//
//	(a) Observations — the native diff-fetch produces real architectural
//	    observations from the PR's changed files + patches + title/body. This
//	    proves the diff-fetch wire (arch_difffetch.go FetchPRDiff →
//	    ReadDiffObservations) runs on actual content, not the old empty stub.
//	(b) Gate — the drift gate triggers and clears correctly across policies
//	    (none / zero-deviations / max:N), with the process exit code mirroring
//	    the gated flag (0 clean, 1 gated).
//
// # OSS boundary
//
// This smoke has NO platform dependencies. It uses only:
//   - the donmai binary (built from a sibling donmai checkout),
//   - a FAKE `gh` shim on PATH that returns a fixture PR view + diff (no
//     network, no GitHub token; `gh` as a build dep is explicitly permitted by
//     AGENTS.md § Boundary).
//
// No WorkOS, no Linear, no `/api/cli/*`, no `rsk_*` tokens, no platform
// endpoints. A forked OSS deployment runs this unchanged.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// archGoNativeSourceDir is the donmai checkout the arch-assess smoke builds from.
// The native arch-intel diff-only pipeline (afclient/codeintel/arch_*.go) ships
// from the donmai module; DONMAI_ARCH_SOURCE_DIR lets an operator point the
// build at a specific worktree (e.g. donmai.wt/af-arch-deprecation during the
// port). It defaults to the sibling "../donmai" checkout that the other steps
// build from.
func archGoNativeSourceDir() string {
	if v := strings.TrimSpace(os.Getenv("DONMAI_ARCH_SOURCE_DIR")); v != "" {
		return v
	}
	return "../donmai"
}

// fixturePRURL is the GitHub PR URL the smoke assesses. The fake `gh` shim
// responds to `gh pr view <url>` and `gh pr diff <url>` for this exact ref, so
// the binary's diff-fetch resolves it without any network access.
const fixturePRURL = "https://github.com/acme/widgets/pull/42"

// writeFakeGh installs a POSIX `gh` shim at dir/gh that returns a deterministic
// fixture PR view (title/body/files JSON) and unified diff, so the donmai
// binary's native diff-fetch (FetchPRDiff) runs on real content offline.
//
// The fixture is engineered to exercise every diff-reader signal class:
//   - zone patterns: an auth file, a db/migrations file, and an api/handlers file
//     → three "pattern" observations,
//   - a decision signal in the PR body ("chose bcrypt over argon2")
//     → one "decision" observation.
//
// The script exits non-zero for any other invocation so an unexpected `gh` call
// surfaces loudly rather than silently succeeding.
func writeFakeGh(t *testing.T, dir string) {
	t.Helper()
	const script = `#!/bin/sh
# Fake gh for the arch-assess smoke. Handles only:
#   gh pr view <ref> --json title,body,files
#   gh pr diff <ref>
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s' '{"title":"Add auth middleware and DB migration","body":"Chose bcrypt over argon2 for password hashing.","files":[{"path":"src/auth/middleware.ts","additions":40,"deletions":0},{"path":"src/db/migrations/0001_users.sql","additions":12,"deletions":0},{"path":"src/api/handlers/login.ts","additions":25,"deletions":3}]}'
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "diff" ]; then
  printf '%s\n' 'diff --git a/src/auth/middleware.ts b/src/auth/middleware.ts'
  printf '%s\n' 'index 0000000..1111111 100644'
  printf '%s\n' '--- /dev/null'
  printf '%s\n' '+++ b/src/auth/middleware.ts'
  printf '%s\n' '@@ -0,0 +1,3 @@'
  printf '%s\n' '+export const requireAuth = async (req, res) => {'
  printf '%s\n' '+  await verifyToken(req)'
  printf '%s\n' '+}'
  exit 0
fi
exit 1
`
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
}

// archAssessResult is the subset of `donmai arch assess` JSON output the smoke
// asserts against. The OSS binary emits the native diff-only report
// (arch_native.go NativeDriftReport).
type archAssessResult struct {
	Mode         string `json:"mode"`
	Gated        bool   `json:"gated"`
	Observations []struct {
		Kind       string  `json:"kind"`
		Confidence float64 `json:"confidence"`
	} `json:"observations"`
}

// archRun is the outcome of one `donmai arch assess` invocation.
type archRun struct {
	stdout   string
	exitCode int
	combined string
	timedOut bool // true when the process exceeded the call timeout
}

// runArchAssess invokes the donmai binary's `arch assess` subcommand with
// fakeBinDir prepended to PATH (so it resolves the fake `gh` shim) and the
// supplied extra args/env, bounded by timeout.
//
// The exit code is extracted from *exec.ExitError so the gate-trigger assertion
// can distinguish clean (0) from gated (1) from error (2) — the contract
// documented in `donmai arch assess --help`. A context-deadline overrun is
// surfaced via timedOut rather than killing the test, so a hung invocation is
// diagnosed precisely rather than aborting the whole test binary.
func runArchAssess(
	t *testing.T,
	binary, fakeBinDir string,
	timeout time.Duration,
	extraEnv []string,
	args ...string,
) archRun {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	full := append([]string{"arch", "assess", fixturePRURL}, args...)
	cmd := exec.CommandContext(ctx, binary, full...) //nolint:gosec // binary + flags are test-controlled.

	// Prepend the fake-bin dir so the binary resolves our shims, not real tools.
	env := append([]string{}, os.Environ()...)
	env = append(env, "PATH="+fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = append(env, extraEnv...)
	cmd.Env = env

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	out := archRun{
		stdout:   strings.TrimRight(outBuf.String(), "\n"),
		combined: outBuf.String() + errBuf.String(),
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.timedOut = true
		out.exitCode = -1
		return out
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		out.exitCode = exitErr.ExitCode()
	} else if runErr != nil {
		// A non-ExitError that is not a deadline (e.g. binary not executable) is
		// a harness failure, not a gate signal.
		t.Fatalf("donmai %s: %v\n--- stdout ---\n%s\n--- stderr ---\n%s",
			strings.Join(full, " "), runErr, outBuf.String(), errBuf.String())
	}
	return out
}

// TestArchAssessNativeEndToEnd builds the donmai binary and drives
// `donmai arch assess` over a fixture PR diff, asserting the native arch-intel
// diff-only pipeline's observation emission and gate behaviour.
//
// Skipped under -short because building the binary takes 60-90s cold.
func TestArchAssessNativeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end arch-assess smoke; skipped under -short")
	}

	// Build the donmai binary from the arch-go-native source (or the sibling
	// donmai checkout). GOWORK is cleared so the build resolves donmai's own
	// go.mod rather than an org-root workspace overlay.
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()

	binDir := t.TempDir()
	donmaiBinary, err := afh.BuildBinary(buildCtx, afh.BuildOptions{
		SourceDir:  archGoNativeSourceDir(),
		EntryPoint: "./cmd/donmai",
		OutputPath: filepath.Join(binDir, "donmai"),
		// GOWORK=off fully decouples the build from the org-root go.work
		// overlay. A bare "GOWORK=" leaves go on auto-discovery, which finds
		// the workspace at the org root and rejects building the donmai module
		// when it is not listed there (e.g. an arch-go-native worktree). "off"
		// makes the build resolve donmai's own go.mod, matching `make test`'s
		// GOWORK=off discipline.
		Env: append(os.Environ(), "GOWORK=off"),
	})
	if err != nil {
		if strings.Contains(err.Error(), "resolve ../") ||
			strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("donmai binary unavailable (source %q): %v", archGoNativeSourceDir(), err)
		}
		t.Fatalf("build donmai binary: %v", err)
	}

	// Capability probe: the native arch-intel diff-only pipeline ships the
	// `arch assess` subcommand. A binary that predates the native port (e.g. the
	// canonical donmai checkout before the port lands) lacks it and has no native
	// diff-fetch/gate to assert against — skip cleanly so the gate stays green
	// until the port merges. DONMAI_ARCH_SOURCE_DIR points the build at the
	// af-arch-deprecation worktree to exercise the real surface.
	if !binaryHasNativeArchAssess(t, donmaiBinary) {
		t.Skipf("donmai binary at %q predates the native arch-intel port "+
			"(no native `arch assess` surface) — build from the af-arch-deprecation "+
			"worktree via DONMAI_ARCH_SOURCE_DIR to exercise this smoke", donmaiBinary)
	}

	// Fake `gh` shim — fixture PR view + diff, no network.
	fakeBinDir := t.TempDir()
	writeFakeGh(t, fakeBinDir)

	// ── (a) Observations: native diff-fetch produces real observations ────────
	//
	// Run with --no-llm + gate-policy none so the result is the deterministic
	// diff-only path with no gate side effects. The fixture's three zone files +
	// one decision signal MUST surface as observations — proving FetchPRDiff fed
	// real content into ReadDiffObservations (not the old empty PrDiff{} stub).
	t.Run("observations_from_native_diff_fetch", func(t *testing.T) {
		r := runArchAssess(t, donmaiBinary, fakeBinDir, 60*time.Second, nil,
			"--no-llm", "--gate-policy", "none")
		if r.exitCode != 0 {
			t.Fatalf("expected exit 0 (gate none), got %d\n%s", r.exitCode, r.combined)
		}

		var res archAssessResult
		if err := afh.JSONUnmarshal(r.stdout, &res); err != nil {
			t.Fatalf("decode arch assess JSON: %v\n%s", err, r.stdout)
		}

		if res.Mode != "native-diff-only" {
			t.Errorf("expected mode native-diff-only with --no-llm, got %q\n%s", res.Mode, r.stdout)
		}
		if len(res.Observations) == 0 {
			t.Fatalf("native diff-fetch produced ZERO observations — the diff-fetch "+
				"wire is broken (empty PrDiff regression)\n%s", r.stdout)
		}

		// The fixture deterministically yields the auth/database/api zone
		// patterns plus the bcrypt-over-argon2 decision. Assert the signal mix
		// rather than an exact count so future diff-reader additions don't make
		// this brittle, but require at least one pattern AND the decision.
		var patterns, decisions int
		for _, o := range res.Observations {
			switch o.Kind {
			case "pattern":
				patterns++
			case "decision":
				decisions++
			}
		}
		if patterns < 1 {
			t.Errorf("expected >=1 zone-pattern observation from the fixture files, got %d\n%s",
				patterns, r.stdout)
		}
		if decisions < 1 {
			t.Errorf("expected >=1 decision observation from the PR body "+
				"(chose bcrypt over argon2), got %d\n%s", decisions, r.stdout)
		}
	})

	// ── (b) Gate triggers and clears correctly ────────────────────────────────
	//
	// Same fixture, varying only the gate policy. The exit code MUST mirror the
	// gated flag: 0 when clear, 1 when gated. This is the contract every CI gate
	// downstream of `donmai arch assess` relies on.
	t.Run("gate_triggers_and_clears", func(t *testing.T) {
		cases := []struct {
			name      string
			policy    string
			wantGated bool
		}{
			// none never gates.
			{"none_clears", "none", false},
			// zero-deviations gates on ANY observation (the fixture has several).
			{"zero_deviations_triggers", "zero-deviations", true},
			// max:1 gates when observation count > 1 (the fixture has >1).
			{"max_below_count_triggers", "max:1", true},
			// max:50 clears — observation count is well under 50.
			{"max_above_count_clears", "max:50", false},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				r := runArchAssess(t, donmaiBinary, fakeBinDir, 60*time.Second, nil,
					"--no-llm", "--gate-policy", tc.policy)

				var res archAssessResult
				if err := afh.JSONUnmarshal(r.stdout, &res); err != nil {
					t.Fatalf("decode arch assess JSON (policy %s): %v\n%s", tc.policy, err, r.stdout)
				}

				if res.Gated != tc.wantGated {
					t.Errorf("policy %s: gated=%v, want %v\n%s",
						tc.policy, res.Gated, tc.wantGated, r.stdout)
				}

				// Exit code MUST mirror the gated flag (0 clean / 1 gated).
				wantExit := 0
				if tc.wantGated {
					wantExit = 1
				}
				if r.exitCode != wantExit {
					t.Errorf("policy %s: exit code %d, want %d (must mirror gated=%v)\n%s",
						tc.policy, r.exitCode, wantExit, tc.wantGated, r.combined)
				}
			})
		}
	})

	// Defence in depth: --summary mode emits human text (not JSON) and still
	// mirrors the gate in the exit code.
	t.Run("summary_mode_mirrors_gate", func(t *testing.T) {
		r := runArchAssess(t, donmaiBinary, fakeBinDir, 60*time.Second, nil,
			"--no-llm", "--gate-policy", "zero-deviations", "--summary")
		text := r.stdout
		if r.exitCode != 1 {
			t.Errorf("summary + zero-deviations: expected exit 1 (gated), got %d\n%s", r.exitCode, r.combined)
		}
		if strings.TrimSpace(text) == "" {
			t.Errorf("summary mode produced empty text\n%s", r.combined)
		}
		if strings.HasPrefix(strings.TrimSpace(text), "{") {
			t.Errorf("summary mode should emit plain text, not JSON\n%s", text)
		}
		if !strings.Contains(text, "BLOCKED by gate policy") {
			t.Errorf("summary text should announce the gate block, got:\n%s", text)
		}
	})
}

// binaryHasNativeArchAssess reports whether the donmai binary ships the native
// arch-intel `arch assess` surface. The `--no-llm` flag is present on the native
// port (afcli/arch.go newArchAssessCmd) and forces the OSS-shipped diff-only
// path; binaries that predate the port omit it. The probe runs
// `arch assess --help` and looks for the flag.
//
// This lets the smoke skip cleanly (rather than fail) when built against a
// donmai checkout where the native pipeline has not yet merged.
func binaryHasNativeArchAssess(t *testing.T, binary string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, binary, "arch", "assess", "--help").CombinedOutput() //nolint:gosec // binary is test-built.
	return strings.Contains(string(out), "--no-llm")
}
