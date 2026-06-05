package smokes

// step3_af_help_deprecation_guard_test.go — pins that the af binary's
// help surface carries the Wave 9 command tree in the expected shape.
//
// This test is purposefully different from rensei-smokes'
// TestRenseiHelpMirrorsAfForMigratedSurfaces, which is a divergence
// detector across two binaries (it asserts rensei's help is sourced
// from afcli upstream). This test is a self-pin against the af binary
// alone: future commits that drop a Wave 9 verb or rename a subcommand
// fail here even if the rensei mirror is still in sync, because the
// expected shape lives in the test fixture rather than being copied
// from the af side at runtime.
//
// Per Phase 10 dispatch (Q5 resolution), both tests are valuable:
// neither is redundant with the other. They share the same
// `harness.ParseHelpSubcommands` parser.

import (
	"context"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestAfHelpDeprecationGuard builds the af binary, captures its top-
// level `--help` output and the per-surface `--help` output for each
// of the four Wave 9 surfaces, and asserts each subcommand set matches
// the hardcoded baseline below.
//
// The baseline encodes the Wave 9 verb shape per the agentfactory-tui
// v0.7.0 CHANGELOG entry. A regression that drops a verb or renames a
// subcommand fires here.
//
// Skipped under -short because building the binary takes 60-90s on a
// cold cache.
func TestAfHelpDeprecationGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end help-output diff; skipped under -short")
	}

	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()

	binDir := t.TempDir()
	donmaiBinary, err := afh.BuildDonmaiBinary(buildCtx, afh.BuildOptions{
		OutputPath: binDir + "/donmai",
		Env:        append(os.Environ(), "GOWORK="),
	})
	if err != nil {
		if strings.Contains(err.Error(), "resolve ../") ||
			strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("donmai binary unavailable: %v", err)
		}
		t.Fatalf("build donmai binary: %v", err)
	}

	// Top-level surface — the four Wave 9 surfaces must be present
	// alongside the pre-existing daemon/agent/dashboard/etc set. The
	// expected names below are the union of the v0.6.x and v0.7.0
	// shapes — every entry the af top-level help advertised at v0.7.0.
	t.Run("top-level", func(t *testing.T) {
		helpCtx, helpCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer helpCancel()

		got, out, err := afh.ParseHelpSubcommands(helpCtx, donmaiBinary)
		if err != nil {
			t.Fatalf("af --help: %v\n%s", err, out)
		}
		if len(got) == 0 {
			t.Fatalf("af --help produced no Available Commands section\n%s", out)
		}

		// Pre-existing surface (pre-Wave-9): every command MUST be
		// present.
		preexisting := []string{
			"admin", "agent", "arch", "code", "completion", "daemon",
			"dashboard", "fleet", "governor", "help", "linear", "logs",
			"orchestrator", "project", "session", "status", "worker",
		}
		// Wave 9 migrated surface: provider, kit, workarea, routing.
		// These are the four daemon-targeted command trees the binary
		// MUST advertise at v0.7.0+.
		wave9 := []string{"provider", "kit", "workarea", "routing"}

		for _, name := range append(preexisting, wave9...) {
			if _, ok := got[name]; !ok {
				t.Errorf("af --help missing required top-level subcommand %q\n--- output ---\n%s",
					name, out)
			}
		}
	})

	// Per-Wave-9-surface subcommand shape. Each surface has a
	// hardcoded subcommand list per agentfactory-tui's v0.7.0
	// CHANGELOG entry — a future commit that drops or renames a verb
	// fires here.
	type surfaceCase struct {
		surface  string
		expected []string
	}
	cases := []surfaceCase{
		{
			// `af provider --help` — list, show. Per ADR-2026-05-07 §D2
			// + agentfactory-tui v0.7.0 CHANGELOG.
			surface:  "provider",
			expected: []string{"list", "show"},
		},
		{
			// `af kit --help` — disable, enable, install, list, show,
			// sources, verify. Per ADR-2026-05-07 §D3 + CHANGELOG.
			surface:  "kit",
			expected: []string{"disable", "enable", "install", "list", "show", "sources", "verify"},
		},
		{
			// `af workarea --help` — diff, list, restore, show. Per
			// ADR-2026-05-07 §D4 + CHANGELOG.
			surface:  "workarea",
			expected: []string{"diff", "list", "restore", "show"},
		},
		{
			// `af routing --help` — explain, show. Per ADR-2026-05-07
			// §D5 + CHANGELOG.
			surface:  "routing",
			expected: []string{"explain", "show"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.surface, func(t *testing.T) {
			helpCtx, helpCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer helpCancel()

			got, out, err := afh.ParseHelpSubcommands(helpCtx, donmaiBinary, tc.surface)
			if err != nil {
				t.Fatalf("af %s --help: %v\n%s", tc.surface, err, out)
			}
			if len(got) == 0 {
				t.Fatalf("af %s --help has no Available Commands section\n%s",
					tc.surface, out)
			}

			gotNames := afh.SortedKeys(got)
			expected := append([]string{}, tc.expected...)
			sort.Strings(expected)

			if !reflect.DeepEqual(gotNames, expected) {
				t.Errorf("af %s subcommands diverge from baseline\n  expected: %v\n  got:      %v\n--- help output ---\n%s",
					tc.surface, expected, gotNames, out)
			}

			// Defence in depth: every advertised subcommand must have a
			// non-empty short description. A regression that wires up a
			// command but forgets the Short field would render a blank
			// row in `--help` and trip here.
			for name, desc := range got {
				if strings.TrimSpace(desc) == "" {
					t.Errorf("af %s subcommand %q has empty short description\n--- help output ---\n%s",
						tc.surface, name, out)
				}
			}
		})
	}
}

