package smokes

// step2_af_daemon_command_surface_test.go — live-daemon end-to-end smoke
// for the four daemon-targeted command surfaces migrated to
// agentfactory-tui in Wave 9 (ADR-2026-05-07-daemon-http-control-api.md).
//
// This is the af-binary mirror of rensei-smokes' TestRenseiHostDaemonCommandSurface.
// The substitutions are clean:
//
//   rensei host provider list  → af provider list
//   rensei host kit list       → af kit list
//   rensei host workarea list  → af workarea list
//   rensei routing show        → af routing show
//
// The af binary surfaces these as top-level subcommands (no `host` parent),
// matching the OSS-pure shape declared by ADR-2026-05-07. The wantFragments
// and regression-guard lists port verbatim — same daemon, same JSON shape,
// same family enum values.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestAfDaemonCommandSurface exercises each of the four migrated
// daemon-targeted command surfaces against a real `af daemon run` process.
//
// Skipped under -short and when RENSEI_SMOKES_SKIP_LIVE_DAEMON=1 is set.
func TestAfDaemonCommandSurface(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end live-daemon test; skipped under -short")
	}
	if os.Getenv("RENSEI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("RENSEI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of the live-daemon smoke")
	}

	live, afBinary, logBuf := setupLiveDaemon(t)

	// Per-command HOME so any af-side config write (there shouldn't be
	// any for these read-only commands, but defence in depth) stays
	// scoped to a fresh tmp dir.
	commandHome := t.TempDir()
	cfgDir := filepath.Join(commandHome, ".config", "rensei")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir command home cfg dir: %v", err)
	}

	type assertion struct {
		name string
		args []string
		// wantFragments — at least one of these strings must appear in
		// the rendered output. OR-composed because the daemon's response
		// shape varies with whether it has data yet.
		wantFragments []string
	}

	cases := []assertion{
		{
			name: "provider list",
			args: []string{"provider", "list"},
			// A daemon started with the Wave 9 provider registry surfaces
			// at least one AgentRuntime provider (claude/codex/etc.).
			// "agent-runtime" is the family-section header; provider names
			// (claude, codex, stub) render in the table. Either signal is
			// enough — the daemon is honest about which families it
			// populates today (only AgentRuntime).
			wantFragments: []string{
				"agent-runtime",
				"AgentRuntime",
				"claude",
				"codex",
				"stub",
				"No providers registered",
			},
		},
		{
			name: "kit list",
			args: []string{"kit", "list"},
			// Fresh daemon HOME has no kits installed — the friendly
			// empty-state line is expected. If the daemon's bundled-kit
			// scan ever populates this we'll match the kit-id form too.
			wantFragments: []string{
				"No kits installed",
				"id",
				"version",
				"status",
			},
		},
		{
			name: "workarea list",
			args: []string{"workarea", "list"},
			// Fresh daemon HOME has neither active members nor archives.
			wantFragments: []string{
				"No workareas found",
				"workarea",
				"active",
				"archived",
			},
		},
		{
			name: "routing show",
			args: []string{"routing", "show", "--plain"},
			// The routing config endpoint always returns a non-empty
			// envelope (weights, capability filters), so the plain
			// output's `weights:` line is always present even on a
			// fresh daemon with no recent decisions.
			wantFragments: []string{
				"weights:",
				"sandbox-providers",
				"llm-providers",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmdCtx, cmdCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cmdCancel()

			out, err := afh.RunHermeticAgainstDaemon(cmdCtx, afh.HermeticRunOptions{
				Binary:          afBinary,
				Args:            tc.args,
				HomeDir:         commandHome,
				DaemonURLEnvVar: "RENSEI_DAEMON_URL",
				DaemonURL:       live.URL,
			})
			if err != nil {
				t.Fatalf("af %s failed: %v\n--- output ---\n%s\n--- daemon log tail ---\n%s",
					strings.Join(tc.args, " "), err, out, logBuf.String())
			}

			if strings.TrimSpace(out) == "" {
				t.Errorf("af %s produced empty output\n--- daemon log tail ---\n%s",
					strings.Join(tc.args, " "), logBuf.String())
			}

			afh.AssertOutputContainsAny(t, out, tc.wantFragments)

			// Regression guards — the rendered output must not look
			// like the pre-Wave-9 mis-routing symptoms. /v1/* paths
			// are explicitly retired per ADR-2026-05-07 § D1.
			afh.AssertOutputDoesNotContain(t, out, []string{
				"404 not found",
				"session auth required",
				"/v1/",
			})
		})
	}
}
