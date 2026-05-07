package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAfOptions is the rensei-smokes-equivalent shape but binary-agnostic
// — used here by the test code to fixture an "af" binary advertising
// `af daemon run` so DaemonAvailable's subcommand probe matches.
func fakeAfOptions() DaemonProbeOptions {
	return DaemonProbeOptions{
		Binary:         "af",
		SubcommandPath: []string{"daemon", "run"},
		UsageMarker:    "af daemon run [",
		// af today has no legacy standalone binary; LegacyBinary stays
		// empty so the probe falls cleanly through to absent.
	}
}

// fakeRenseiOptions builds the equivalent shape for the legacy probe
// case — used to verify the LegacyBinary-on-PATH detection path.
func fakeRenseiOptions() DaemonProbeOptions {
	return DaemonProbeOptions{
		Binary:         "rensei",
		SubcommandPath: []string{"host", "run"},
		UsageMarker:    "rensei host run [",
		LegacyBinary:   "rensei-daemon",
	}
}

// TestDaemonAvailable_SubcommandPresent_Af verifies DaemonAvailable
// returns (true, DaemonModeSubcommand) when the binary's subcommand
// probe matches the configured UsageMarker. Uses an "af" fixture to
// exercise the parameterisation against a non-rensei spelling.
func TestDaemonAvailable_SubcommandPresent_Af(t *testing.T) {
	dir := t.TempDir()
	WriteFakeBinaryAdvertisingSubcommand(t, dir, FakeBinarySubcommandFixture{
		BinaryName:        "af",
		SubcommandPath:    []string{"daemon", "run"},
		SubcommandPresent: true,
		UsageMarker:       "af daemon run [",
	})
	t.Setenv("PATH", dir)

	present, mode := DaemonAvailable(fakeAfOptions())
	if !present {
		t.Fatal("DaemonAvailable should report present when subcommand probe matches")
	}
	if mode != DaemonModeSubcommand {
		t.Errorf("expected mode=%q, got %q", DaemonModeSubcommand, mode)
	}
}

// TestDaemonAvailable_SubcommandPresent_Rensei mirrors the af test using
// the rensei probe shape, confirming the same parameterisation works
// for both spellings.
func TestDaemonAvailable_SubcommandPresent_Rensei(t *testing.T) {
	dir := t.TempDir()
	WriteFakeBinaryAdvertisingSubcommand(t, dir, FakeBinarySubcommandFixture{
		BinaryName:        "rensei",
		SubcommandPath:    []string{"host", "run"},
		SubcommandPresent: true,
		UsageMarker:       "rensei host run [",
	})
	t.Setenv("PATH", dir)

	present, mode := DaemonAvailable(fakeRenseiOptions())
	if !present {
		t.Fatal("DaemonAvailable should report present when subcommand probe matches")
	}
	if mode != DaemonModeSubcommand {
		t.Errorf("expected mode=%q, got %q", DaemonModeSubcommand, mode)
	}
}

// TestDaemonAvailable_BinaryFallback verifies DaemonAvailable falls back
// to the legacy standalone binary when the subcommand probe fails.
func TestDaemonAvailable_BinaryFallback(t *testing.T) {
	dir := t.TempDir()
	WriteFakeBinaryAdvertisingSubcommand(t, dir, FakeBinarySubcommandFixture{
		BinaryName:        "rensei",
		SubcommandPath:    []string{"host", "run"},
		SubcommandPresent: false, // older rensei without run subcommand
		UsageMarker:       "rensei host run [",
	})
	// Add a fake rensei-daemon binary alongside.
	fakeDaemon := filepath.Join(dir, "rensei-daemon")
	if err := os.WriteFile(fakeDaemon, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake daemon: %v", err)
	}
	t.Setenv("PATH", dir)

	present, mode := DaemonAvailable(fakeRenseiOptions())
	if !present {
		t.Fatal("DaemonAvailable should report present when LegacyBinary is on PATH")
	}
	if mode != DaemonModeBinary {
		t.Errorf("expected mode=%q, got %q", DaemonModeBinary, mode)
	}
}

// TestDaemonAvailable_Absent verifies DaemonAvailable returns
// (false, DaemonModeAbsent) when neither the subcommand nor the legacy
// binary is reachable.
func TestDaemonAvailable_Absent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	present, mode := DaemonAvailable(fakeRenseiOptions())
	if present {
		t.Error("DaemonAvailable should report absent on empty PATH")
	}
	if mode != DaemonModeAbsent {
		t.Errorf("expected mode=%q, got %q", DaemonModeAbsent, mode)
	}
}

// TestDaemonAvailable_SubcommandTakesPrecedence verifies that when BOTH
// the subcommand and the legacy binary are present, subcommand mode
// wins (it is the canonical post-migration shape).
func TestDaemonAvailable_SubcommandTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	WriteFakeBinaryAdvertisingSubcommand(t, dir, FakeBinarySubcommandFixture{
		BinaryName:        "rensei",
		SubcommandPath:    []string{"host", "run"},
		SubcommandPresent: true,
		UsageMarker:       "rensei host run [",
	})
	fakeDaemon := filepath.Join(dir, "rensei-daemon")
	if err := os.WriteFile(fakeDaemon, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake daemon: %v", err)
	}
	t.Setenv("PATH", dir)

	_, mode := DaemonAvailable(fakeRenseiOptions())
	if mode != DaemonModeSubcommand {
		t.Errorf("subcommand should take precedence over binary; got %q", mode)
	}
}

// TestDaemonAvailable_NoBinaryConfigured verifies that DaemonAvailable
// returns absent when the configured Binary itself is missing from PATH
// AND no LegacyBinary is configured (the af case where there's no
// fallback).
func TestDaemonAvailable_NoBinaryConfigured_NoLegacy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	present, mode := DaemonAvailable(fakeAfOptions())
	if present {
		t.Error("DaemonAvailable should report absent when Binary is missing and no LegacyBinary")
	}
	if mode != DaemonModeAbsent {
		t.Errorf("expected mode=%q, got %q", DaemonModeAbsent, mode)
	}
}

// TestDaemonModeLog verifies the log strings include the mode keyword
// and a stable human-readable phrase suitable for the detect-mode log
// header.
func TestDaemonModeLog(t *testing.T) {
	cases := []struct {
		mode  DaemonMode
		opts  DaemonProbeOptions
		needs []string
	}{
		{
			mode:  DaemonModeSubcommand,
			opts:  fakeRenseiOptions(),
			needs: []string{"subcommand", "rensei host run"},
		},
		{
			mode:  DaemonModeBinary,
			opts:  fakeRenseiOptions(),
			needs: []string{"binary", "rensei-daemon"},
		},
		{
			mode:  DaemonModeAbsent,
			opts:  fakeRenseiOptions(),
			needs: []string{"absent"},
		},
		{
			// af shape: no LegacyBinary; absent log should still be
			// well-formed (no empty string in middle).
			mode:  DaemonModeAbsent,
			opts:  fakeAfOptions(),
			needs: []string{"absent", "af daemon run"},
		},
	}
	for _, tc := range cases {
		got := DaemonModeLog(tc.mode, tc.opts)
		for _, n := range tc.needs {
			if !strings.Contains(got, n) {
				t.Errorf("DaemonModeLog(%q, %+v) = %q; missing %q", tc.mode, tc.opts, got, n)
			}
		}
	}
}
